// Package db 管理多个 GaussDB 实例的连接池，并在事务层强制只读。
//
// GaussDB 分布式版仅支持事务级只读设置，因此所有查询统一在显式只读事务中
// 执行：BEGIN → SET LOCAL TRANSACTION READ ONLY → 查询 → COMMIT（出错则
// ROLLBACK），由服务端拒绝事务内的一切写操作。该方式在集中式/主备与分布
// 式实例上通用。这是只读防护的最终兜底：即使 SQL 校验被绕过，服务端也会
// 拒绝写入。入池连接仅设置会话级 statement_timeout。
package db

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"
	"github.com/HuaweiCloudDeveloper/gaussdb-go/gaussdbconn"
	"github.com/HuaweiCloudDeveloper/gaussdb-go/gaussdbxpool"

	"gaussdb-ro-mcp/internal/config"
	"gaussdb-ro-mcp/internal/guard"
)

// hasConnectTimeoutInDSN 判断用户是否在 DSN 或 options 中显式给出了 connect_timeout。
// 同时覆盖 keyword=value（空白分隔字段）与 URL 查询参数（?connect_timeout=1）两种形式。
func hasConnectTimeoutInDSN(icfg *config.Instance) bool {
	if hasParamKeyIn(icfg.DSN, "connect_timeout") {
		return true
	}
	if strings.Contains(icfg.DSN, "://") {
		if u, err := url.Parse(icfg.DSN); err == nil && u.Query().Has("connect_timeout") {
			return true
		}
	}
	for _, o := range icfg.Options {
		if strings.HasPrefix(o, "connect_timeout=") {
			return true
		}
	}
	return false
}

// hasParamKeyIn 判断 keyword=value 形式的连接串中是否含指定键（键前为串首或空白）。
func hasParamKeyIn(dsn, key string) bool {
	for _, field := range strings.Fields(dsn) {
		if strings.HasPrefix(field, key+"=") {
			return true
		}
	}
	return false
}

// Instance 是一个已就绪的数据源：连接池 + 该实例生效的配置。
type Instance struct {
	Name             string
	pool             *gaussdbxpool.Pool
	MaxRows          int
	MaxRowsCap       int
	StatementTimeout time.Duration

	partitionMode bool
	// partitionResolved 仅在探测成功后置位：探测出错（ctx 取消、连接抖动）
	// 不缓存结果，下次调用重试，避免分区支持被永久错判。
	partitionMu       sync.Mutex
	partitionResolved bool
}

// Manager 持有全部实例。
type Manager struct {
	guard   *guard.Guard
	defName string
	insts   map[string]*Instance
}

// NewManager 为配置中的每个实例创建连接池。
// 连接池创建不等待真实连接；首次查询时才建连。
func NewManager(ctx context.Context, cfg *config.Config) (*Manager, error) {
	g := guard.New(cfg.Server.BlockedFunctions)
	m := &Manager{
		guard:   g,
		defName: cfg.DefaultInstance,
		insts:   map[string]*Instance{},
	}
	for _, icfg := range cfg.Instances {
		inst, err := newInstance(ctx, icfg, cfg)
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("初始化实例 %q 失败: %w", icfg.Name, err)
		}
		m.insts[icfg.Name] = inst
	}
	return m, nil
}

func newInstance(ctx context.Context, icfg *config.Instance, cfg *config.Config) (*Instance, error) {
	poolCfg, err := gaussdbxpool.ParseConfig(icfg.BuildDSN())
	if err != nil {
		return nil, fmt.Errorf("解析连接串失败: %w", err)
	}
	poolCfg.MaxConns = int32(icfg.PoolMaxConns)
	poolCfg.MinConns = 0
	// 连接超时优先级：实例级 connect_timeout > DSN/options 中显式给出的
	// connect_timeout（ParseConfig 已解析进 ConnectTimeout，不覆盖）> 服务级默认。
	switch {
	case icfg.ConnectTimeout > 0:
		poolCfg.ConnConfig.ConnectTimeout = time.Duration(icfg.ConnectTimeout)
	case !hasConnectTimeoutInDSN(icfg):
		poolCfg.ConnConfig.ConnectTimeout = time.Duration(cfg.Server.ConnectTimeout)
	}

	// 入池连接仅设置会话级 statement_timeout；只读由每个查询的显式
	// 只读事务保证（见 queryReadOnly）。
	poolCfg.AfterConnect = func(ctx context.Context, conn *gaussdbgo.Conn) error {
		return setStatementTimeout(ctx, conn, time.Duration(icfg.StatementTimeout))
	}

	pool, err := gaussdbxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}
	return &Instance{
		Name:             icfg.Name,
		pool:             pool,
		MaxRows:          icfg.MaxRows,
		MaxRowsCap:       cfg.Server.MaxRowsCap,
		StatementTimeout: time.Duration(icfg.StatementTimeout),
	}, nil
}

// setStatementTimeout 设置会话级语句超时；亚毫秒时长向下取整会得到 0（PG 语义
// 为禁用超时），与配置意图相反，故钳制为最小 1ms。
func setStatementTimeout(ctx context.Context, conn *gaussdbgo.Conn, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	ms := d.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", ms)); err != nil {
		return fmt.Errorf("设置 statement_timeout 失败: %w", err)
	}
	return nil
}

// Resolve 按名称取实例；name 为空时返回默认实例。
func (m *Manager) Resolve(name string) (*Instance, error) {
	if name == "" {
		name = m.defName
	}
	inst, ok := m.insts[name]
	if !ok {
		return nil, fmt.Errorf("未知的实例 %q，可用实例: %s", name, m.InstanceNames())
	}
	return inst, nil
}

// InstanceNames 返回全部实例名。
func (m *Manager) InstanceNames() string {
	names := make([]string, 0, len(m.insts))
	for n := range m.insts {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// DefaultInstanceName 返回默认实例名。
func (m *Manager) DefaultInstanceName() string { return m.defName }

// Guard 返回共享的 SQL 校验器。
func (m *Manager) Guard() *guard.Guard { return m.guard }

// Close 关闭全部连接池。
func (m *Manager) Close() {
	for _, inst := range m.insts {
		inst.pool.Close()
	}
}

// SelectResult 是一次 SELECT 的结果。
type SelectResult struct {
	Columns    []string
	Rows       []map[string]any
	RowCount   int
	Truncated  bool
	DurationMS int64
}

// Select 执行一条已通过 guard 校验的 SELECT，返回至多 maxRows 行。
// 查询在显式只读事务中执行；ctx 需已带超时，服务端另有 statement_timeout 兜底。
func (inst *Instance) Select(ctx context.Context, sql string, maxRows int) (*SelectResult, error) {
	start := time.Now()
	res := &SelectResult{}
	err := inst.queryReadOnly(ctx, sql, nil, func(rows gaussdbgo.Rows) error {
		cols := dedupColumns(columnNames(rows.FieldDescriptions()))
		res.Columns = cols
		var err error
		res.Rows, res.Truncated, err = collectRows(rows, cols, maxRows)
		return err
	})
	if err != nil {
		return nil, err
	}
	res.RowCount = len(res.Rows)
	res.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}

// Query 执行元数据小查询并返回 map 行列表（供 meta 查询复用）。
// 查询在显式只读事务中执行。
func (inst *Instance) Query(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	var out []map[string]any
	err := inst.queryReadOnly(ctx, sql, args, func(rows gaussdbgo.Rows) error {
		cols := dedupColumns(columnNames(rows.FieldDescriptions()))
		var err error
		out, _, err = collectRows(rows, cols, math.MaxInt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// queryReadOnly 在显式只读事务中执行查询并收集结果：
//
//	BEGIN → SET LOCAL TRANSACTION READ ONLY → 查询 → COMMIT（出错 ROLLBACK）
//
// GaussDB 分布式版仅支持事务级只读设置，该方式在集中式/主备与分布式上通用；
// 只读事务由服务端拒绝事务内的一切写操作。collect 在事务内消费完整结果。
func (inst *Instance) queryReadOnly(ctx context.Context, sql string, args []any, collect func(gaussdbgo.Rows) error) error {
	c, err := inst.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("获取连接失败: %w", err)
	}
	defer c.Release()

	begin := func() error {
		_, err := c.Exec(ctx, "BEGIN")
		return err
	}
	if err := begin(); err != nil {
		// 服务端复用的会话可能残留未结束/中止的事务（任何命令都会报错）：
		// ROLLBACK 清理后重试一次。
		if _, rbErr := c.Exec(ctx, "ROLLBACK"); rbErr != nil {
			return fmt.Errorf("开启只读事务失败: %w", err)
		}
		if err := begin(); err != nil {
			return fmt.Errorf("开启只读事务失败: %w", err)
		}
	}
	if _, err := c.Exec(ctx, "SET LOCAL TRANSACTION READ ONLY"); err != nil {
		_, _ = c.Exec(ctx, "ROLLBACK")
		return fmt.Errorf("设置事务只读失败: %w", err)
	}

	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		_, _ = c.Exec(ctx, "ROLLBACK")
		return err
	}
	collectErr := func() (err error) {
		defer func() {
			rows.Close()
		}()
		if err := collect(rows); err != nil {
			return err
		}
		return rows.Err()
	}()
	if collectErr != nil {
		_, _ = c.Exec(ctx, "ROLLBACK")
		return collectErr
	}
	if _, err := c.Exec(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("提交只读事务失败: %w", err)
	}
	return nil
}

// columnNames 提取结果集列名。
func columnNames(fields []gaussdbconn.FieldDescription) []string {
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = string(f.Name)
	}
	return cols
}

// dedupColumns 为重复列名生成唯一显示名（id、id_2、…），保证行 map 的键与
// Columns 清单一一对应，避免重复列名时数据被静默覆盖。
func dedupColumns(cols []string) []string {
	seen := make(map[string]int, len(cols))
	out := make([]string, len(cols))
	for i, c := range cols {
		name := c
		for {
			n := seen[name]
			seen[name] = n + 1
			if n == 0 {
				break
			}
			name = fmt.Sprintf("%s_%d", c, n+1)
		}
		out[i] = name
	}
	return out
}

// collectRows 读取剩余行并按（已去重的）列名建键；超过 maxRows 行时停止并
// 返回 truncated=true。始终返回非 nil 切片，JSON 序列化为 [] 而非 null。
func collectRows(rows gaussdbgo.Rows, cols []string, maxRows int) ([]map[string]any, bool, error) {
	out := []map[string]any{}
	truncated := false
	for rows.Next() {
		if len(out) >= maxRows {
			truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return nil, false, err
		}
		row := make(map[string]any, len(cols))
		for i, v := range values {
			if i < len(cols) {
				row[cols[i]] = NormalizeValue(v)
			}
		}
		out = append(out, row)
	}
	return out, truncated, rows.Err()
}

// normalizeFloat 把 NaN/±Inf 转为字符串：它们不是合法 JSON 数字，直接输出会让
// 整个工具结果序列化失败。
func normalizeFloat(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return f
}

// NormalizeValue 将驱动返回值转换为可 JSON 序列化的值。
func NormalizeValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string, bool, int64,
		int, int8, int16, int32,
		uint, uint8, uint16, uint32, uint64, time.Time:
		return t
	case float64:
		return normalizeFloat(t)
	case float32:
		return normalizeFloat(float64(t))
	case []byte:
		if utf8.Valid(t) {
			return string(t)
		}
		return "0x" + hex.EncodeToString(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = NormalizeValue(e)
		}
		return out
	case map[string]any: // hstore 等
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = NormalizeValue(e)
		}
		return out
	case driver.Valuer: // gaussdbtype 数值/区间等自定义类型
		dv, err := t.Value()
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return NormalizeValue(dv)
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}
