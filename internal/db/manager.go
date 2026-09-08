// Package db 管理多个 GaussDB 实例的连接池，并在会话层强制只读。
//
// 每条新连接建立后立即执行：
//
//	SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY
//	SET statement_timeout = <ms>
//
// 并回读 SHOW transaction_read_only 验证确为 on，否则拒绝该连接入池。
// 这是只读防护的最终兜底：即使 SQL 校验被绕过，服务端也会拒绝写入。
package db

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"
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
	partitionOnce sync.Once
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
	poolCfg.ConnConfig.ConnectTimeout = time.Duration(cfg.Server.ConnectTimeout)

	// 会话级只读强制：对每条入池连接生效，并由服务端验证。
	poolCfg.AfterConnect = func(ctx context.Context, conn *gaussdbgo.Conn) error {
		return enforceReadOnly(ctx, conn, time.Duration(icfg.StatementTimeout))
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

// enforceReadOnly 将连接设为只读会话并验证。
func enforceReadOnly(ctx context.Context, conn *gaussdbgo.Conn, stmtTimeout time.Duration) error {
	if _, err := conn.Exec(ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY"); err != nil {
		return fmt.Errorf("设置只读会话失败: %w", err)
	}
	if stmtTimeout > 0 {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", stmtTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("设置 statement_timeout 失败: %w", err)
		}
	}
	var ro string
	if err := conn.QueryRow(ctx, "SHOW transaction_read_only").Scan(&ro); err != nil {
		return fmt.Errorf("验证只读状态失败: %w", err)
	}
	if !strings.EqualFold(ro, "on") {
		return fmt.Errorf("只读会话验证失败：transaction_read_only=%s，拒绝该连接", ro)
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
// ctx 需已带超时；服务端另有 statement_timeout 兜底。
func (inst *Instance) Select(ctx context.Context, sql string, maxRows int) (*SelectResult, error) {
	start := time.Now()
	rows, err := inst.pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = string(f.Name)
	}

	res := &SelectResult{Columns: cols}
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, v := range values {
			if i < len(cols) {
				row[cols[i]] = NormalizeValue(v)
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.RowCount = len(res.Rows)
	res.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}

// QueryRow 执行元数据小查询并返回 map 行列表（供 meta 查询复用）。
func (inst *Instance) Query(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	rows, err := inst.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = string(f.Name)
	}
	var out []map[string]any
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, v := range values {
			row[cols[i]] = NormalizeValue(v)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// NormalizeValue 将驱动返回值转换为可 JSON 序列化的值。
func NormalizeValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string, bool, int64, float64,
		int, int8, int16, int32,
		uint, uint8, uint16, uint32, uint64, float32, time.Time:
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
