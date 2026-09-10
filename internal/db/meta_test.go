package db

import (
	"context"
	"database/sql/driver"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"

	"gaussdb-ro-mcp/internal/config"
	"gaussdb-ro-mcp/internal/dbtest"
)

// TestRelKindExpr 验证分区表达式切换。
func TestRelKindExpr(t *testing.T) {
	if got := relKindExpr(false); !strings.Contains(got, "WHEN 'r'") {
		t.Errorf("非分区模式应使用 relkind 基础表达式: %s", got)
	}
	if got := relKindExpr(true); !strings.Contains(got, "parttype = 'p'") {
		t.Errorf("分区模式应使用 parttype 表达式: %s", got)
	}
}

// TestAsStringAsInt64 覆盖类型转换辅助函数。
func TestAsStringAsInt64(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"abc", "abc"},
		{42, "42"},
		{fmt.Errorf("x"), "x"},
	}
	for _, c := range cases {
		if got := asString(c.in); got != c.want {
			t.Errorf("asString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	intCases := []struct {
		in   any
		want int64
	}{
		{int64(7), 7},
		{uint64(8), 8},
		{int32(9), 9},
		{uint32(10), 10},
		{int(11), 11},
		{float64(12.9), 12},
		{"13", 13},
		{nil, 0},
		{struct{}{}, 0},
		{"not-a-number", 0},
	}
	for _, c := range intCases {
		if got := asInt64(c.in); got != c.want {
			t.Errorf("asInt64(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

type stringerImpl struct{}

func (stringerImpl) String() string { return "stringer" }

// TestNormalizeValue 覆盖驱动值到 JSON 安全值的全部转换分支。
func TestNormalizeValue(t *testing.T) {
	valuerOK := valuerStub{v: int64(5), err: nil}
	valuerErr := valuerStub{v: nil, err: fmt.Errorf("boom")}

	cases := []struct {
		name string
		in   any
		want any
	}{
		{"nil", nil, nil},
		{"string", "s", "s"},
		{"bool", true, true},
		{"int64", int64(1), int64(1)},
		{"int", int(2), int(2)},
		{"int8", int8(3), int8(3)},
		{"int16", int16(4), int16(4)},
		{"int32", int32(5), int32(5)},
		{"uint", uint(6), uint(6)},
		{"uint8", uint8(7), uint8(7)},
		{"uint16", uint16(8), uint16(8)},
		{"uint32", uint32(9), uint32(9)},
		{"uint64", uint64(10), uint64(10)},
		{"float32", float32(1.5), float32(1.5)},
		{"float64", float64(2.5), float64(2.5)},
		{"time", time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC()},
		{"bytes-utf8", []byte("hi"), "hi"},
		{"bytes-binary", []byte{0xff, 0xfe}, "0xfffe"},
		{"slice", []any{int64(1), nil}, []any{int64(1), nil}},
		{"map", map[string]any{"a": "b"}, map[string]any{"a": "b"}},
		{"valuer-ok", valuerOK, int64(5)},
		{"valuer-err", valuerErr, "valuer-stub"},
		{"stringer", stringerImpl{}, "stringer"},
		{"default", complex(1, 2), fmt.Sprintf("%v", complex(1, 2))},
	}
	for _, c := range cases {
		if got := NormalizeValue(c.in); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("[%s] NormalizeValue = %v (%T), want %v (%T)", c.name, got, got, c.want, c.want)
		}
	}
}

type valuerStub struct {
	v   driver.Value
	err error
}

// Value 实现 driver.Valuer；String 使错误分支的 %v 输出确定化。
func (s valuerStub) Value() (driver.Value, error) { return s.v, s.err }

func (s valuerStub) String() string { return "valuer-stub" }

// TestManagerBasics 覆盖 Resolve/InstanceNames/DefaultInstanceName/Guard/Close。
func TestManagerBasics(t *testing.T) {
	f := dbtest.Start(t)
	mgr, err := NewManager(context.Background(), f.NewConfig())
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	defer mgr.Close()

	inst, err := mgr.Resolve("mock")
	if err != nil || inst == nil || inst.Name != "mock" {
		t.Fatalf("Resolve(mock) 失败: %v", err)
	}
	if inst, err := mgr.Resolve(""); err != nil || inst.Name != "mock" {
		t.Fatalf("Resolve(\"\") 应回退默认实例: %v", err)
	}
	if _, err := mgr.Resolve("nope"); err == nil || !strings.Contains(err.Error(), "未知的实例") {
		t.Fatalf("未知实例应报错: %v", err)
	}
	if got := mgr.InstanceNames(); got != "mock" {
		t.Errorf("InstanceNames = %q", got)
	}
	if got := mgr.DefaultInstanceName(); got != "mock" {
		t.Errorf("DefaultInstanceName = %q", got)
	}
	if mgr.Guard() == nil {
		t.Error("Guard 不应为 nil")
	}
}

// TestNewManagerBadDSN 覆盖 NewManager 的错误路径（非法连接串）。
func TestNewManagerBadDSN(t *testing.T) {
	cfg := fNewConfigWithBadDSN()
	mgr, err := NewManager(context.Background(), cfg)
	if err == nil {
		mgr.Close()
		t.Fatal("非法 sslmode 应导致初始化失败")
	}
	if !strings.Contains(err.Error(), "初始化实例") {
		t.Fatalf("错误信息应包含初始化实例: %v", err)
	}
}

// fNewConfigWithBadDSN 构造携带非法 sslmode 的配置。
func fNewConfigWithBadDSN() *config.Config {
	cfg := &config.Config{
		Server: config.Server{
			MaxRows:        500,
			MaxRowsCap:     10000,
			ConnectTimeout: config.Duration(5 * time.Second),
		},
		Instances: []*config.Instance{{
			Name:             "bad",
			DSN:              "gaussdb://u:p@h/db?sslmode=invalid",
			StatementTimeout: config.Duration(5 * time.Second),
			PoolMaxConns:     1,
		}},
		DefaultInstance: "bad",
	}
	return cfg
}

// ---- 元数据查询（经 mock 通用结果集） ----

// TestMetaQueries 覆盖全部元数据查询函数的正常路径。
func TestMetaQueries(t *testing.T) {
	ctx := context.Background()
	inst, _ := newMockInstance(t)

	if rows, err := inst.ListSchemas(ctx, false); err != nil || len(rows) != 2 {
		t.Errorf("ListSchemas 失败: %v, rows=%d", err, len(rows))
	}
	if rows, err := inst.ListSchemas(ctx, true); err != nil || len(rows) == 0 {
		t.Errorf("ListSchemas(includeSystem) 失败: %v", err)
	}

	lt, err := inst.ListTables(ctx, "", false)
	if err != nil || lt["count"] != 2 || lt["truncated"] != false {
		t.Errorf("ListTables 失败: %v, %v", err, lt)
	}
	if _, err := inst.ListTables(ctx, "public", false); err != nil {
		t.Errorf("ListTables(schema) 失败: %v", err)
	}
	if _, err := inst.ListTables(ctx, "", true); err != nil {
		t.Errorf("ListTables(includeSystem) 失败: %v", err)
	}

	if _, ns, name, kind, err := inst.ResolveTable(ctx, "public", "t"); err != nil ||
		ns != "public" || name != "mock_table" || kind != "table" {
		t.Errorf("ResolveTable(schema) 失败: %v, %s %s %s", err, ns, name, kind)
	}
	if cols, err := inst.DescribeColumns(ctx, 1); err != nil || len(cols) != 2 {
		t.Errorf("DescribeColumns 失败: %v", err)
	}
	if cons, err := inst.DescribeConstraints(ctx, 1); err != nil || len(cons) != 2 {
		t.Errorf("DescribeConstraints 失败: %v", err)
	}
	if idxs, err := inst.DescribeIndexes(ctx, 1); err != nil || len(idxs) != 2 {
		t.Errorf("DescribeIndexes 失败: %v", err)
	}
	if vd, err := inst.ViewDefinition(ctx, 1); err != nil || vd != "SELECT 1" {
		t.Errorf("ViewDefinition 失败: %v, %q", err, vd)
	}
	comment, est, err := inst.TableComment(ctx, 1)
	if err != nil || comment != "mock comment" || est != 42 {
		t.Errorf("TableComment 失败: %v, %q %d", err, comment, est)
	}
	if info, err := inst.ServerInfo(ctx); err != nil || info["version"] == nil {
		t.Errorf("ServerInfo 失败: %v", err)
	}
	if ro, err := inst.ReadOnlyStatus(ctx); err != nil || ro != "on" {
		t.Errorf("ReadOnlyStatus 失败: %v, %q", err, ro)
	}

	// 分区模式关闭时 ListPartitions 返回 nil。
	if parts, err := inst.ListPartitions(ctx, 1); err != nil || parts != nil {
		t.Errorf("非分区模式 ListPartitions 应为 nil: %v, %v", err, parts)
	}
}

// TestResolveTableErrors 覆盖 ResolveTable 的各错误分支。
func TestResolveTableErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("带schema不存在", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "n.nspname = $1") {
				return dbtest.Rows(genericColsForHook())
			}
			return nil
		}))
		if _, _, _, _, err := inst.ResolveTable(ctx, "public", "t"); err == nil ||
			!strings.Contains(err.Error(), "不存在") {
			t.Fatalf("应报不存在: %v", err)
		}
	})

	t.Run("无schema唯一命中", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "LIMIT 2") {
				return dbtest.Rows(
					[]dbtest.Col{dbtest.Int8("oid", 16384), dbtest.Text("schema_name", "public"),
						dbtest.Text("table_name", "mock_table"), dbtest.Text("kind", "table")},
					[]string{"16384", "public", "mock_table", "table"},
				)
			}
			return nil
		}))
		oid, ns, name, kind, err := inst.ResolveTable(ctx, "", "t")
		if err != nil || oid != 16384 || ns != "public" || name != "mock_table" || kind != "table" {
			t.Fatalf("唯一命中应成功: %v, %d %s %s %s", err, oid, ns, name, kind)
		}
	})

	t.Run("无schema歧义", func(t *testing.T) {
		inst, _ := newMockInstance(t)
		if _, _, _, _, err := inst.ResolveTable(ctx, "", "t"); err == nil ||
			!strings.Contains(err.Error(), "多个模式") {
			t.Fatalf("应报歧义: %v", err)
		}
	})

	t.Run("无schema不存在", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "LIMIT 2") {
				return dbtest.Rows(genericColsForHook())
			}
			return nil
		}))
		if _, _, _, _, err := inst.ResolveTable(ctx, "", "t"); err == nil ||
			!strings.Contains(err.Error(), "不存在") {
			t.Fatalf("应报不存在: %v", err)
		}
	})
}

// genericColsForHook 提供 0 行结果集的列描述（与默认列一致的首列即可）。
func genericColsForHook() []dbtest.Col {
	return []dbtest.Col{dbtest.Int8("oid", 16384), dbtest.Text("schema_name", "public")}
}

// TestMetaEdgeCases 覆盖 0 行结果与查询错误的分支。
func TestMetaEdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("空结果", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "pg_get_viewdef") || strings.Contains(q, "obj_description") ||
				strings.Contains(q, "version()") {
				return dbtest.Rows(nil)
			}
			return nil
		}))
		if vd, err := inst.ViewDefinition(ctx, 1); err != nil || vd != "" {
			t.Errorf("0 行 ViewDefinition 应返回空串: %v %q", err, vd)
		}
		if c, n, err := inst.TableComment(ctx, 1); err != nil || c != "" || n != 0 {
			t.Errorf("0 行 TableComment 应返回零值: %v %q %d", err, c, n)
		}
		if _, err := inst.ServerInfo(ctx); err == nil || !strings.Contains(err.Error(), "空结果") {
			t.Errorf("0 行 ServerInfo 应报空结果: %v", err)
		}
	})

	t.Run("查询错误", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "pg_catalog") {
				return dbtest.ErrorResult("mock: catalog 查询失败")
			}
			return nil
		}))
		if _, err := inst.ListSchemas(ctx, false); err == nil {
			t.Error("查询错误应上抛")
		}
		if _, err := inst.ListTables(ctx, "", false); err == nil {
			t.Error("ListTables 查询错误应上抛")
		}
		if _, err := inst.ViewDefinition(ctx, 1); err == nil {
			t.Error("查询错误应上抛")
		}
		if _, _, err := inst.TableComment(ctx, 1); err == nil {
			t.Error("查询错误应上抛")
		}
		if _, err := inst.ServerInfo(ctx); err == nil {
			t.Error("查询错误应上抛")
		}
		if _, _, _, _, err := inst.ResolveTable(ctx, "public", "t"); err == nil {
			t.Error("ResolveTable(schema) 查询错误应上抛")
		}
		if _, _, _, _, err := inst.ResolveTable(ctx, "", "t"); err == nil {
			t.Error("ResolveTable(无 schema) 查询错误应上抛")
		}
	})

	t.Run("ListTables截断", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "pg_class") && strings.Contains(q, "LIMIT 5001") {
				cols := []dbtest.Col{
					dbtest.Text("schema_name", "public"), dbtest.Text("table_name", "t"),
					dbtest.Text("kind", "table"), dbtest.Int8("estimated_rows", 1),
					dbtest.Text("comment", ""),
				}
				rows := make([][]string, 5001)
				for i := range rows {
					rows[i] = []string{"public", "t", "table", "1", ""}
				}
				return dbtest.Rows(cols, rows...)
			}
			return nil
		}))
		lt, err := inst.ListTables(ctx, "", false)
		if err != nil {
			t.Fatalf("ListTables 失败: %v", err)
		}
		if lt["truncated"] != true || lt["count"] != 5000 {
			t.Errorf("应截断为 5000 行并标记 truncated: count=%v truncated=%v", lt["count"], lt["truncated"])
		}
	})
}

// TestDetectPartitionSupport 覆盖分区探测的两个分支及 ListPartitions。
func TestDetectPartitionSupport(t *testing.T) {
	ctx := context.Background()

	t.Run("不支持分区", func(t *testing.T) {
		inst, _ := newMockInstance(t)
		if inst.detectPartitionSupport(ctx) {
			t.Fatal("默认 mock 不应支持分区")
		}
		if parts, err := inst.ListPartitions(ctx, 1); err != nil || parts != nil {
			t.Errorf("非分区模式 ListPartitions 应为 nil: %v", err)
		}
	})

	t.Run("支持分区", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithPartitionSupport())
		if !inst.detectPartitionSupport(ctx) {
			t.Fatal("mock 应支持分区")
		}
		if inst.partitionMode != true {
			t.Error("partitionMode 应为 true")
		}
		parts, err := inst.ListPartitions(ctx, 1)
		if err != nil || len(parts) != 2 {
			t.Errorf("ListPartitions 失败: %v, %d", err, len(parts))
		}
		// sync.Once：第二次调用不再查询，直接返回缓存结果。
		if !inst.detectPartitionSupport(ctx) {
			t.Error("探测结果应被缓存")
		}
	})
}

// TestSelectAndQuery 覆盖 Select 的行数上限/截断与 Query 参数化路径。
func TestSelectAndQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inst, _ := newMockInstance(t)

	res, err := inst.Select(ctx, "SELECT 1", 1)
	if err != nil {
		t.Fatalf("Select 失败: %v", err)
	}
	if res.RowCount != 1 || !res.Truncated || len(res.Columns) == 0 {
		t.Errorf("截断结果错误: %+v", res)
	}
	if res.Rows[0]["n"] != nil {
		t.Logf("首行: %v", res.Rows[0])
	}

	res, err = inst.Select(ctx, "SELECT 1", 100)
	if err != nil || res.RowCount != 2 || res.Truncated {
		t.Errorf("不截断结果错误: %v, %+v", err, res)
	}

	rows, err := inst.Query(ctx, "SELECT $1::int AS v", 7)
	if err != nil || len(rows) == 0 {
		t.Errorf("Query 失败: %v", err)
	}

	// 查询错误上抛。
	badInst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		return dbtest.ErrorResult("mock: 查询失败")
	}))
	if _, err := badInst.Select(ctx, "SELECT 1", 10); err == nil {
		t.Error("Select 查询错误应上抛")
	}
	if _, err := badInst.Query(ctx, "SELECT 1"); err == nil {
		t.Error("Query 查询错误应上抛")
	}
}

// TestEnforceReadOnlyReSetsTimeoutAfterFallback 回归 issue #4：
// 回退路径（ROLLBACK + 会话级 SET）之后必须重新设置 statement_timeout，
// 否则打开事务里的事务作用域 SET 会被一并回滚，连接无服务端超时入池。
func TestEnforceReadOnlyReSetsTimeoutAfterFallback(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var f *dbtest.Server
	f = dbtest.Start(t, dbtest.WithShowValues("off", "on"),
		dbtest.WithQueryHook(func(q string) *dbtest.Result {
			mu.Lock()
			order = append(order, q)
			mu.Unlock()
			return nil // 记录后仍走默认应答
		}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := gaussdbgo.Connect(ctx, f.DSN())
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close(ctx)

	if err := enforceReadOnly(ctx, conn, 3*time.Second); err != nil {
		t.Fatalf("回退应成功: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	rollbackIdx, setTimeoutIdx := -1, -1
	for i, q := range order {
		if strings.HasPrefix(q, "ROLLBACK") && rollbackIdx == -1 {
			rollbackIdx = i
		}
		if strings.HasPrefix(q, "SET statement_timeout") {
			setTimeoutIdx = i
		}
	}
	if rollbackIdx == -1 || setTimeoutIdx == -1 {
		t.Fatalf("应先 ROLLBACK 再设 statement_timeout，实际顺序: %v", order)
	}
	if setTimeoutIdx < rollbackIdx {
		t.Fatalf("statement_timeout 应在 ROLLBACK 之后设置: %v", order)
	}
}

// TestSetStatementTimeoutClampSubMillisecond 回归 issue #13：亚毫秒时长钳制为 1ms。
func TestSetStatementTimeoutClampSubMillisecond(t *testing.T) {
	var mu sync.Mutex
	var setTimeout string
	f := dbtest.Start(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		if strings.HasPrefix(q, "SET statement_timeout") {
			mu.Lock()
			setTimeout = q
			mu.Unlock()
		}
		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := gaussdbgo.Connect(ctx, f.DSN())
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close(ctx)

	if err := enforceReadOnly(ctx, conn, 100*time.Microsecond); err != nil {
		t.Fatalf("enforceReadOnly 失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasSuffix(setTimeout, "= 1") {
		t.Errorf("亚毫秒超时应钳制为 1ms，实际: %q", setTimeout)
	}

	// d<=0 时不发送 SET（零值由配置层校验兜底，这里只验证不误发）。
	before := setTimeout
	if err := enforceReadOnly(ctx, conn, 0); err != nil {
		t.Fatalf("d=0 不应报错: %v", err)
	}
	if setTimeout != before {
		t.Errorf("d=0 不应下发 SET statement_timeout: %q", setTimeout)
	}
}

// TestDedupColumnsAndDuplicateSelect 回归 issue #8：重复列名生成唯一键且数据不丢。
func TestDedupColumnsAndDuplicateSelect(t *testing.T) {
	if got := dedupColumns([]string{"id", "id", "id_2"}); fmt.Sprint(got) != "[id id_2 id_2_2]" {
		t.Errorf("dedupColumns = %v", got)
	}

	inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		if !strings.Contains(q, "AS id") {
			return nil // 其余查询走默认应答
		}
		return dbtest.Rows(
			[]dbtest.Col{dbtest.Text("id", "first"), dbtest.Text("id", "second")},
			[]string{"first", "second"},
		)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := inst.Select(ctx, "SELECT 1 AS id, 2 AS id", 10)
	if err != nil {
		t.Fatalf("Select 失败: %v", err)
	}
	if fmt.Sprint(res.Columns) != "[id id_2]" {
		t.Errorf("列应去重: %v", res.Columns)
	}
	if len(res.Rows) != 1 || res.Rows[0]["id"] != "first" || res.Rows[0]["id_2"] != "second" {
		t.Errorf("两列数据都应保留: %v", res.Rows[0])
	}
}

// TestNormalizeFloatNonFinite 回归 issue #16：NaN/±Inf 转字符串避免 JSON 序列化失败。
func TestNormalizeFloatNonFinite(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{math.NaN(), "NaN"},
		{math.Inf(1), "+Inf"},
		{math.Inf(-1), "-Inf"},
		{float32(math.NaN()), "NaN"},
		{float64(1.5), "1.5"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(NormalizeValue(c.in)); got != c.want {
			t.Errorf("NormalizeValue(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReadOnlyStatusFailureModes 覆盖 ReadOnlyStatus 的查询错误与空结果分支。
// 计数器让入池校验的 SHOW 正常通过（第 1 次计算），目标查询走到失败分支（第 2 次）。
func TestReadOnlyStatusFailureModes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("查询错误", func(t *testing.T) {
		calls := 0
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(strings.ToUpper(q), "TRANSACTION_READ_ONLY") {
				calls++
				if calls >= 2 {
					return dbtest.ErrorResult("mock: show 失败")
				}
			}
			return nil
		}))
		if _, err := inst.ReadOnlyStatus(ctx); err == nil {
			t.Fatal("SHOW 失败应上抛")
		}
	})

	t.Run("空结果", func(t *testing.T) {
		calls := 0
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(strings.ToUpper(q), "TRANSACTION_READ_ONLY") {
				calls++
				if calls >= 2 {
					return dbtest.Rows(nil)
				}
			}
			return nil
		}))
		if _, err := inst.ReadOnlyStatus(ctx); err == nil || !strings.Contains(err.Error(), "空结果") {
			t.Fatalf("0 行 SHOW 应报空结果: %v", err)
		}
	})
}

// TestDetectPartitionSupportRetriesAfterError 回归 issue #14：探测出错不缓存，下次重试。
func TestDetectPartitionSupportRetriesAfterError(t *testing.T) {
	fail := true
	inst, _ := newMockInstance(t, dbtest.WithPartitionSupport(),
		dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "parttype") && fail {
				return dbtest.ErrorResult("mock: 瞬时错误")
			}
			return nil
		}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if inst.detectPartitionSupport(ctx) {
		t.Fatal("探测失败不应报告支持分区")
	}
	if inst.partitionResolved {
		t.Fatal("探测失败不应缓存结果")
	}

	fail = false // 瞬时错误恢复
	if !inst.detectPartitionSupport(ctx) {
		t.Fatal("恢复后重试应成功")
	}
	if !inst.partitionResolved || !inst.partitionMode {
		t.Fatal("成功后应缓存结果")
	}
}

// TestConnectTimeoutPrecedence 回归 issue #7：连接超时的三级优先级
// （实例级字段 > DSN/options 显式值 > 服务级默认），不再无条件覆盖。
func TestConnectTimeoutPrecedence(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Server: config.Server{
				MaxRows:        500,
				MaxRowsCap:     10000,
				ConnectTimeout: config.Duration(10 * time.Second),
			},
			DefaultInstance: "mock",
		}
	}
	mkInst := func(inst *config.Instance) *Instance {
		cfg := base()
		cfg.Instances = []*config.Instance{inst}
		in, err := newInstance(context.Background(), inst, cfg)
		if err != nil {
			t.Fatalf("newInstance 失败: %v", err)
		}
		t.Cleanup(in.pool.Close)
		return in
	}

	t.Run("DSN显式connect_timeout优先", func(t *testing.T) {
		inst := mkInst(&config.Instance{
			Name: "mock", Host: "127.0.0.1", Port: 15432, Database: "db",
			User: "u", Password: "p", SSLMode: "disable", PoolMaxConns: 1,
			StatementTimeout: config.Duration(5 * time.Second),
			Options:          []string{"connect_timeout=1"},
		})
		if got := inst.pool.Config().ConnConfig.ConnectTimeout; got != time.Second {
			t.Errorf("connect_timeout=1 应生效（不被服务级 10s 覆盖），实际 %s", got)
		}
	})

	t.Run("DSN内connect_timeout优先", func(t *testing.T) {
		inst := mkInst(&config.Instance{
			Name: "mock", DSN: "host=127.0.0.1 port=15432 dbname=db user=u password=p connect_timeout=1 sslmode=disable",
			PoolMaxConns: 1, StatementTimeout: config.Duration(5 * time.Second),
		})
		if got := inst.pool.Config().ConnConfig.ConnectTimeout; got != time.Second {
			t.Errorf("DSN 内 connect_timeout=1 应生效（不被服务级 10s 覆盖），实际 %s", got)
		}
	})

	t.Run("实例级字段优先于服务级", func(t *testing.T) {
		inst := mkInst(&config.Instance{
			Name: "mock", Host: "127.0.0.1", Port: 15432, Database: "db",
			User: "u", Password: "p", SSLMode: "disable", PoolMaxConns: 1,
			StatementTimeout: config.Duration(5 * time.Second),
			ConnectTimeout:   config.Duration(3 * time.Second),
		})
		if got := inst.pool.Config().ConnConfig.ConnectTimeout; got != 3*time.Second {
			t.Errorf("实例级 connect_timeout 应生效，实际 %s", got)
		}
	})

	t.Run("未设置时用服务级默认", func(t *testing.T) {
		inst := mkInst(&config.Instance{
			Name: "mock", Host: "127.0.0.1", Port: 15432, Database: "db",
			User: "u", Password: "p", SSLMode: "disable", PoolMaxConns: 1,
			StatementTimeout: config.Duration(5 * time.Second),
		})
		if got := inst.pool.Config().ConnConfig.ConnectTimeout; got != 10*time.Second {
			t.Errorf("应回退服务级 connect_timeout，实际 %s", got)
		}
	})
}
