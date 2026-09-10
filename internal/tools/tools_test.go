package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gaussdb-ro-mcp/internal/db"
	"gaussdb-ro-mcp/internal/dbtest"
)

// asMap 把 handler 返回的结果断言为 map。
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("结果应为 map，实际 %T: %v", v, v)
	}
	return m
}

// newMockManager 创建指向 mock 的 Manager（懒建连，AfterConnect 强制只读）。
func newMockManager(t *testing.T, opts ...dbtest.Option) *db.Manager {
	t.Helper()
	f := dbtest.Start(t, opts...)
	mgr, err := db.NewManager(context.Background(), f.NewConfig())
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	t.Cleanup(mgr.Close)
	return mgr
}

// TestRegister 验证 5 个工具可注册到 MCP 服务器，并经进程内 MCP 会话逐个调用，
// 覆盖 Register 注册的闭包与协议层参数解码。
func TestRegister(t *testing.T) {
	mgr := newMockManager(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	Register(server, mgr) // 不应 panic

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, st) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("连接 MCP 会话失败: %v", err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	if len(list.Tools) != 5 {
		t.Errorf("应有 5 个工具，实际 %d", len(list.Tools))
	}

	calls := []struct {
		name string
		args map[string]any
	}{
		{"test_connection", nil},
		{"list_schemas", map[string]any{"include_system": true}},
		{"list_tables", map[string]any{"schema": "public"}},
		{"describe_table", map[string]any{"schema": "public", "table": "mock_table"}},
		{"execute_select", map[string]any{"sql": "SELECT 1", "max_rows": 10}},
	}
	for _, c := range calls {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.name, Arguments: c.args})
		if err != nil {
			t.Errorf("调用 %s 失败: %v", c.name, err)
		} else if res.IsError {
			t.Errorf("调用 %s 不应报错: %v", c.name, res.Content)
		}
	}

	// 协议层错误路径：execute_select 拒绝写语句。
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "execute_select",
		Arguments: map[string]any{"sql": "DELETE FROM t"},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !res.IsError {
		t.Error("DELETE 应被拒绝")
	}
}

// TestHandleTestConnection 验证连通性测试工具。
func TestHandleTestConnection(t *testing.T) {
	mgr := newMockManager(t)
	ctx := context.Background()

	_, res, err := handleTestConnection(ctx, mgr, struct{ instanceArg }{})
	if err != nil {
		t.Fatalf("test_connection 失败: %v", err)
	}
	out := asMap(t, res)
	if out["ok"] != true || out["instance"] != "mock" {
		t.Errorf("结果错误: %v", out)
	}
	if out["transaction_read_only"] != "on" {
		t.Errorf("只读状态应为 on: %v", out["transaction_read_only"])
	}
	if out["server_version"] == nil || out["database"] == nil || out["user"] == nil {
		t.Errorf("缺少版本/库/用户信息: %v", out)
	}

	if _, _, err := handleTestConnection(ctx, mgr, struct{ instanceArg }{instanceArg{Instance: "nope"}}); err == nil {
		t.Error("未知实例应报错")
	}

	// ServerInfo 查询失败 → 整体报“连接失败”。
	bad := newMockManager(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		if strings.Contains(q, "version()") {
			return dbtest.ErrorResult("mock: version 查询失败")
		}
		return nil
	}))
	if _, _, err := handleTestConnection(ctx, bad, struct{ instanceArg }{}); err == nil ||
		!strings.Contains(err.Error(), "连接失败") {
		t.Errorf("ServerInfo 失败应报连接失败: %v", err)
	}
}

// TestHandleListSchemas 验证 schema 清单工具。
func TestHandleListSchemas(t *testing.T) {
	mgr := newMockManager(t)
	ctx := context.Background()

	_, res, err := handleListSchemas(ctx, mgr, "", true)
	if err != nil {
		t.Fatalf("list_schemas 失败: %v", err)
	}
	out := asMap(t, res)
	if out["instance"] != "mock" {
		t.Errorf("结果缺 instance: %v", out)
	}
	if _, ok := out["count"]; !ok {
		t.Errorf("结果缺 count: %v", out)
	}
	if _, _, err := handleListSchemas(ctx, mgr, "nope", false); err == nil {
		t.Error("未知实例应报错")
	}

	bad := newMockManager(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		if strings.Contains(q, "pg_namespace") {
			return dbtest.ErrorResult("mock: namespace 查询失败")
		}
		return nil
	}))
	if _, _, err := handleListSchemas(ctx, bad, "", false); err == nil {
		t.Error("ListSchemas 查询失败应上抛")
	}
}

// TestHandleListTables 验证表清单工具。
func TestHandleListTables(t *testing.T) {
	mgr := newMockManager(t)
	ctx := context.Background()

	_, res, err := handleListTables(ctx, mgr, "", "public", false)
	if err != nil {
		t.Fatalf("list_tables 失败: %v", err)
	}
	out := asMap(t, res)
	if out["instance"] != "mock" || out["count"] != 2 {
		t.Errorf("结果错误: %v", out)
	}
	if _, _, err := handleListTables(ctx, mgr, "nope", "", false); err == nil {
		t.Error("未知实例应报错")
	}

	bad := newMockManager(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		if strings.Contains(q, "pg_class") {
			return dbtest.ErrorResult("mock: class 查询失败")
		}
		return nil
	}))
	if _, _, err := handleListTables(ctx, bad, "", "", false); err == nil {
		t.Error("ListTables 查询失败应上抛")
	}
}

// TestHandleDescribeTable 覆盖 describe_table 的成功、视图与错误分支。
func TestHandleDescribeTable(t *testing.T) {
	ctx := context.Background()

	t.Run("参数为空", func(t *testing.T) {
		mgr := newMockManager(t)
		if _, _, err := handleDescribeTable(ctx, mgr, "", "", ""); err == nil ||
			!strings.Contains(err.Error(), "table 参数不能为空") {
			t.Fatalf("应报 table 参数为空: %v", err)
		}
	})

	t.Run("未知实例", func(t *testing.T) {
		mgr := newMockManager(t)
		if _, _, err := handleDescribeTable(ctx, mgr, "nope", "", "t"); err == nil {
			t.Fatal("未知实例应报错")
		}
	})

	t.Run("表不存在", func(t *testing.T) {
		mgr := newMockManager(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "n.nspname = $1") {
				return dbtest.Rows([]dbtest.Col{dbtest.Int8("oid", 1)})
			}
			return nil
		}))
		if _, _, err := handleDescribeTable(ctx, mgr, "", "public", "t"); err == nil ||
			!strings.Contains(err.Error(), "不存在") {
			t.Fatalf("应报不存在: %v", err)
		}
	})

	t.Run("普通表", func(t *testing.T) {
		mgr := newMockManager(t)
		_, res, err := handleDescribeTable(ctx, mgr, "", "public", "mock_table")
		if err != nil {
			t.Fatalf("describe_table 失败: %v", err)
		}
		out := asMap(t, res)
		for _, key := range []string{"schema", "table", "kind", "comment", "columns", "constraints", "indexes", "estimated_rows"} {
			if _, ok := out[key]; !ok {
				t.Errorf("结果缺 %s: %v", key, out)
			}
		}
		if _, ok := out["view_definition"]; ok {
			t.Errorf("普通表不应有视图定义: %v", out)
		}
	})

	t.Run("视图", func(t *testing.T) {
		mgr := newMockManager(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "AS kind") {
				return dbtest.Rows(
					[]dbtest.Col{dbtest.Int8("oid", 99), dbtest.Text("schema_name", "public"),
						dbtest.Text("table_name", "v"), dbtest.Text("kind", "view")},
					[]string{"99", "public", "v", "view"},
				)
			}
			return nil
		}))
		_, res, err := handleDescribeTable(ctx, mgr, "", "public", "v")
		if err != nil {
			t.Fatalf("describe view 失败: %v", err)
		}
		out := asMap(t, res)
		if out["kind"] != "view" {
			t.Errorf("kind 应为 view: %v", out["kind"])
		}
		if vd, ok := out["view_definition"]; !ok || vd != "SELECT 1" {
			t.Errorf("视图定义错误: %v", out["view_definition"])
		}
	})

	t.Run("分区表", func(t *testing.T) {
		mgr := newMockManager(t, dbtest.WithPartitionSupport())
		_, res, err := handleDescribeTable(ctx, mgr, "", "public", "pt")
		if err != nil {
			t.Fatalf("describe partitioned table 失败: %v", err)
		}
		out := asMap(t, res)
		if _, ok := out["partitions"]; !ok {
			t.Errorf("分区表应有分区清单: %v", out)
		}
	})

	t.Run("视图定义与分区查询失败", func(t *testing.T) {
		mgr := newMockManager(t, dbtest.WithPartitionSupport(),
			dbtest.WithQueryHook(func(q string) *dbtest.Result {
				switch {
				case strings.Contains(q, "pg_get_viewdef"):
					return dbtest.ErrorResult("mock: 视图定义失败")
				case strings.Contains(q, "pg_partition"):
					return dbtest.ErrorResult("mock: 分区查询失败")
				case strings.Contains(q, "AS kind"):
					return dbtest.Rows(
						[]dbtest.Col{dbtest.Int8("oid", 99), dbtest.Text("schema_name", "public"),
							dbtest.Text("table_name", "v"), dbtest.Text("kind", "view")},
						[]string{"99", "public", "v", "view"},
					)
				}
				return nil
			}))
		_, res, err := handleDescribeTable(ctx, mgr, "", "public", "v")
		if err != nil {
			t.Fatalf("describe 失败: %v", err)
		}
		out := asMap(t, res)
		if msg, ok := out["view_definition_error"].(string); !ok || !strings.Contains(msg, "视图定义失败") {
			t.Errorf("应输出 view_definition_error: %v", out)
		}
		if msg, ok := out["partitions_error"].(string); !ok || !strings.Contains(msg, "分区查询失败") {
			t.Errorf("应输出 partitions_error: %v", out)
		}
	})
}

// TestHandleExecuteSelect 覆盖 execute_select 的校验、行数与错误分支。
func TestHandleExecuteSelect(t *testing.T) {
	mgr := newMockManager(t)
	ctx := context.Background()

	t.Run("拒绝写语句", func(t *testing.T) {
		if _, _, err := handleExecuteSelect(ctx, mgr, "", "DELETE FROM t", 0); err == nil {
			t.Fatal("DELETE 应被 SQL 校验拦截")
		}
	})

	t.Run("未知实例", func(t *testing.T) {
		if _, _, err := handleExecuteSelect(ctx, mgr, "nope", "SELECT 1", 0); err == nil {
			t.Fatal("未知实例应报错")
		}
	})

	t.Run("默认与放宽行数", func(t *testing.T) {
		_, res, err := handleExecuteSelect(ctx, mgr, "", "SELECT 1", 0)
		if err != nil {
			t.Fatalf("execute_select 失败: %v", err)
		}
		out := asMap(t, res)
		if out["row_count"] != 2 || out["instance"] != "mock" {
			t.Errorf("结果错误: %v", out)
		}
		// 超过 max_rows_cap 时应被钳制：mock 恒返回 2 行，钳制后两行都返回。
		_, res2, err2 := handleExecuteSelect(ctx, mgr, "", "SELECT 1", 1)
		if err2 != nil || asMap(t, res2)["row_count"] != 1 {
			t.Errorf("max_rows=1 应截断为 1 行: %v, %v", err2, res2)
		}
		if _, res3, err3 := handleExecuteSelect(ctx, mgr, "", "SELECT 1", 1<<20); err3 != nil {
			t.Errorf("超上限 max_rows 不应报错: %v", err3)
		} else {
			_ = res3 // 仅覆盖钳制分支
		}
	})

	t.Run("查询失败", func(t *testing.T) {
		badMgr := newMockManager(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "boom") {
				return dbtest.ErrorResult("mock: 执行失败")
			}
			return nil
		}))
		if _, _, err := handleExecuteSelect(ctx, badMgr, "", "SELECT 'boom' AS x", 0); err == nil ||
			!strings.Contains(err.Error(), "查询执行失败") {
			t.Fatalf("执行失败应上抛: %v", err)
		}
	})

	t.Run("上下文超时不panic", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = handleExecuteSelect(ctx, mgr, "", "SELECT 1", 10)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("execute_select 卡死")
		}
	})
}

// TestHandleTestConnectionReadOnlyOff 回归 issue #9：只读未生效时不可装作正常。
// 事务级强制下，任何 SHOW transaction_read_only 非 on 的实例会在 queryReadOnly
// 的事务内校验处 fail-closed（拒绝执行查询），test_connection 如实报连接失败，
// 而不是给出 ok=true 的假象。
func TestHandleTestConnectionReadOnlyOff(t *testing.T) {
	// SHOW 返回 off：模拟 SET LOCAL 被剥离/未生效的形态。
	mgr := newMockManager(t, dbtest.WithShowValues("off"))
	_, _, err := handleTestConnection(context.Background(), mgr, struct{ instanceArg }{})
	if err == nil || !strings.Contains(err.Error(), "只读事务校验失败") {
		t.Fatalf("只读未生效时 test_connection 应报校验失败，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "off") {
		t.Errorf("错误信息应包含实际只读状态: %v", err)
	}

	// 只读校验查询本身失败：同样 fail-closed 报连接失败。
	ctx := context.Background()
	mgr2 := newMockManager(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		if strings.Contains(strings.ToUpper(q), "TRANSACTION_READ_ONLY") {
			return dbtest.ErrorResult("mock: show 失败")
		}
		return nil
	}))
	_, _, err2 := handleTestConnection(ctx, mgr2, struct{ instanceArg }{})
	if err2 == nil || !strings.Contains(err2.Error(), "校验事务只读状态失败") {
		t.Fatalf("SHOW 查询失败应报校验失败，实际: %v", err2)
	}
}

// TestHandleDescribeTablePartialErrors 回归 issue #15：各段失败都应有 <段名>_error 键。
func TestHandleDescribeTablePartialErrors(t *testing.T) {
	mgr := newMockManager(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		switch {
		case strings.Contains(q, "pg_constraint"):
			return dbtest.ErrorResult("mock: 约束查询失败")
		case strings.Contains(q, "pg_index"):
			return dbtest.ErrorResult("mock: 索引查询失败")
		case strings.Contains(q, "obj_description(c.oid)"):
			return dbtest.ErrorResult("mock: 注释查询失败")
		case strings.Contains(q, "pg_attribute"):
			return dbtest.ErrorResult("mock: 列查询失败")
		}
		return nil
	}))
	_, res, err := handleDescribeTable(context.Background(), mgr, "", "public", "mock_table")
	if err != nil {
		t.Fatalf("describe_table 不应整体失败: %v", err)
	}
	out := asMap(t, res)
	for _, key := range []string{"comment_error", "constraints_error", "indexes_error", "columns_error"} {
		if _, ok := out[key]; !ok {
			t.Errorf("缺少 %s 键（失败被静默吞掉）: %v", key, out)
		}
	}
}
