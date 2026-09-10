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
	"errors"
	"fmt"
	"math"
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
	// partitionProbeCh 实现 singleflight：探测在锁外执行，并发调用只触发
	// 一次网络往返、其余等待结果，避免持锁跨 I/O 把元数据调用全部串行化。
	partitionMu        sync.Mutex
	partitionResolved  bool
	partitionProbeDone chan struct{}
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
	dsn := icfg.BuildDSN()
	poolCfg, err := gaussdbxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析连接串失败: %w", err)
	}
	poolCfg.MaxConns = int32(icfg.PoolMaxConns)
	poolCfg.MinConns = 0
	// 连接超时优先级（与配置注释/示例一致）：DSN/options 中显式给出的
	// connect_timeout（ParseConfig 已解析进 ConnectTimeout，不覆盖）>
	// 实例级 connect_timeout 字段 > 服务级默认。
	switch {
	case config.DSNHasParam(dsn, "connect_timeout"):
		// DSN/options 显式给出：保留 ParseConfig 的解析结果。
	case icfg.ConnectTimeout > 0:
		poolCfg.ConnConfig.ConnectTimeout = time.Duration(icfg.ConnectTimeout)
	default:
		poolCfg.ConnConfig.ConnectTimeout = time.Duration(cfg.Server.ConnectTimeout)
	}

	// 入池连接仅设置会话级 statement_timeout；只读由每个查询的显式
	// 只读事务保证（见 queryReadOnly）。
	poolCfg.AfterConnect = func(ctx context.Context, conn *gaussdbgo.Conn) error {
		// 服务端复用交付的会话可能残留未结束/中止的事务（此后任何语句都会
		// 报错），先清理再设置会话参数，否则该连接永远无法入池。
		if conn.GaussdbConn().TxStatus() != 'I' {
			if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
				return fmt.Errorf("清理连接残留事务失败: %w", err)
			}
		}
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
	// Warning 非空表示结果已完整读取但收尾（提交）失败：只读数据本身有效，
	// 调用方应把结果连同警告一并呈现，而非丢弃已取回的数据。
	Warning string
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
		var ce *errCommitFailed
		// 结果已完整收集、仅提交失败：保留结果并附警告（回归 issue #4 类
		// “近超时查询在 COMMIT 阶段 ctx 耗尽导致已收行全丢”）。
		if errors.As(err, &ce) && res.Rows != nil {
			res.Warning = ce.Error()
			res.RowCount = len(res.Rows)
			res.DurationMS = time.Since(start).Milliseconds()
			return res, nil
		}
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

// errCommitFailed 标记“结果已完整收集、仅事务提交失败”的错误：只读查询
// 的数据不受提交成败影响，调用方（Select）可选择保留结果并附警告。
type errCommitFailed struct{ err error }

func (e *errCommitFailed) Error() string { return "提交只读事务失败: " + e.err.Error() }
func (e *errCommitFailed) Unwrap() error { return e.err }

// cleanupTx 尽力把连接清理回空闲状态。此时 ctx 可能已耗尽/取消，改用
// 不继承取消的派生 context（带独立短超时）执行 ROLLBACK，尽量不让
// 带事务的连接回到池里（池会将其销毁重建，代价更高）。
func cleanupTx(ctx context.Context, c *gaussdbxpool.Conn) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = c.Exec(cctx, "ROLLBACK")
}

// ensureIdleTx 确保连接不处于（活动或中止的）残留事务中：对活动事务执行
// BEGIN 只产生 WARNING 不报错，后续查询会被并入外来事务、COMMIT 替人提交，
// 故在开启只读事务前按协议状态位先行清理。
func ensureIdleTx(ctx context.Context, c *gaussdbxpool.Conn) error {
	if c.Conn().GaussdbConn().TxStatus() == 'I' {
		return nil
	}
	if _, err := c.Exec(ctx, "ROLLBACK"); err != nil {
		return fmt.Errorf("清理残留事务失败: %w", err)
	}
	return nil
}

// verifyReadOnlyTx 在当前事务内回读只读状态。SET 语句可能被中间代理剥离
// 或忽略，未验证生效前不得执行业务查询（fail-closed：宁可拒绝服务，
// 不可在未生效只读的事务里执行来路不明的 SQL）。
func verifyReadOnlyTx(ctx context.Context, c *gaussdbxpool.Conn) error {
	var ro string
	if err := c.QueryRow(ctx, "SHOW transaction_read_only").Scan(&ro); err != nil {
		return fmt.Errorf("校验事务只读状态失败: %w", err)
	}
	if !strings.EqualFold(ro, "on") {
		return fmt.Errorf("只读事务校验失败：transaction_read_only=%s，拒绝在该事务内执行查询", ro)
	}
	return nil
}

// queryReadOnly 在显式只读事务中执行查询并收集结果：
//
//	BEGIN → SET LOCAL TRANSACTION READ ONLY → 回读校验 → 查询 → COMMIT
//
// GaussDB 分布式版仅支持事务级只读设置，该方式在集中式/主备与分布式上通用；
// 只读事务由服务端拒绝事务内的一切写操作。collect 在事务内消费完整结果。
// 出错路径一律 ROLLBACK 清理（用独立 context，见 cleanupTx）。
func (inst *Instance) queryReadOnly(ctx context.Context, sql string, args []any, collect func(gaussdbgo.Rows) error) error {
	c, err := inst.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("获取连接失败: %w", err)
	}
	defer c.Release()

	// 服务端复用交付的会话可能残留未结束/中止的事务。对活动事务执行 BEGIN
	// 只产生 WARNING 不报错，查询会被并入外来事务、COMMIT 替人提交；按协议
	// 状态位先行清理。
	if err := ensureIdleTx(ctx, c); err != nil {
		return err
	}
	begin := func() error {
		_, err := c.Exec(ctx, "BEGIN")
		return err
	}
	if err := begin(); err != nil {
		// 兜底：状态位为空闲但 BEGIN 仍失败（如中止态未反映到状态位），
		// ROLLBACK 清理后重试一次。
		if _, rbErr := c.Exec(ctx, "ROLLBACK"); rbErr != nil {
			return fmt.Errorf("开启只读事务失败: %w", err)
		}
		if err := begin(); err != nil {
			return fmt.Errorf("开启只读事务失败: %w", err)
		}
	}
	if _, err := c.Exec(ctx, "SET LOCAL TRANSACTION READ ONLY"); err != nil {
		cleanupTx(ctx, c)
		return fmt.Errorf("设置事务只读失败: %w", err)
	}
	if err := verifyReadOnlyTx(ctx, c); err != nil {
		cleanupTx(ctx, c)
		return err
	}

	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		cleanupTx(ctx, c)
		return err
	}
	collectErr := func() error {
		defer rows.Close() // 幂等；关闭/排空阶段的错误会写回 rows.Err()
		if err := collect(rows); err != nil {
			return err
		}
		rows.Close() // 主动关闭（排空/关闭 portal）后再读取最终错误
		return rows.Err()
	}()
	if collectErr != nil {
		cleanupTx(ctx, c)
		return collectErr
	}
	if _, err := c.Exec(ctx, "COMMIT"); err != nil {
		cleanupTx(ctx, c)
		return &errCommitFailed{err}
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
		// 有限 float32 原样返回：先转 float64 会引入精度放大
		// （float4 列的 0.1 会变成 0.10000000149011612）；仅非有限值需转字符串。
		if math.IsNaN(float64(t)) || math.IsInf(float64(t), 0) {
			return normalizeFloat(float64(t))
		}
		return t
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
