package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gaussdb-ro-mcp/internal/config"
	"gaussdb-ro-mcp/internal/db"
)

// 进程内端到端测试：真实驱动 + 真实数据库 + MCP 协议（InMemory 传输）。
// 通过 GAUSSDB_RO_MCP_TEST_DSN 启用（key=value 连接串，目标库需含 sales.orders 测试数据）。

func e2eSetup(t *testing.T) (*mcp.ClientSession, func()) {
	t.Helper()
	dsn := os.Getenv("GAUSSDB_RO_MCP_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 GAUSSDB_RO_MCP_TEST_DSN，跳过集成测试")
	}
	kv := parseKV(dsn)

	cfg := &config.Config{
		Server: config.Server{MaxRows: 100, MaxRowsCap: 200, StatementTimeout: config.Duration(10 * time.Second), ConnectTimeout: config.Duration(5 * time.Second)},
		Instances: []*config.Instance{
			// 拆分字段方式 + 默认实例
			{Name: "main", Host: kv["host"], Port: atoi(kv["port"]), Database: kv["dbname"], User: kv["user"], Password: kv["password"]},
			// DSN 方式
			{Name: "second", DSN: dsn + " application_name=e2e-test"},
		},
		DefaultInstance: "main",
	}
	cfg.SetDefaults()
	mgr, err := db.NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Manager 初始化失败: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	Register(server, mgr)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		cancel()
		mgr.Close()
		t.Fatalf("server.Connect 失败: %v", err)
	}
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		cancel()
		mgr.Close()
		t.Fatalf("client.Connect 失败: %v", err)
	}
	return cs, func() {
		cs.Close()
		ss.Close()
		mgr.Close()
		cancel()
	}
}

// callTool 调用工具并返回结构化输出；若结果为错误则 t.Fatal。
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s 传输层错误: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s 返回工具错误: %s", name, resultText(res))
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s 结构化输出类型异常: %T", name, res.StructuredContent)
	}
	return m
}

// callToolErr 断言工具返回 IsError 错误，并返回错误文本。
func callToolErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s 传输层错误: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("%s 应当返回错误，实际成功: %s", name, resultText(res))
	}
	return resultText(res)
}

func resultText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	if res.IsError {
		return "<isError=true, 无文本>"
	}
	return ""
}

func TestE2EListTools(t *testing.T) {
	cs, done := e2eSetup(t)
	defer done()
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"test_connection": false, "list_schemas": false, "list_tables": false,
		"describe_table": false, "execute_select": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("缺少工具 %s", name)
		}
	}
}

func TestE2ETestConnection(t *testing.T) {
	cs, done := e2eSetup(t)
	defer done()
	out := callTool(t, cs, "test_connection", nil)
	if out["ok"] != true {
		t.Errorf("ok 应为 true: %v", out)
	}
	if out["transaction_read_only"] != "on" {
		t.Errorf("transaction_read_only 应为 on: %v", out)
	}
	if ver, _ := out["server_version"].(string); !strings.Contains(strings.ToLower(ver), "postgres") && !strings.Contains(strings.ToLower(ver), "gauss") {
		t.Errorf("server_version 异常: %v", out["server_version"])
	}
	if out["instance"] != "main" {
		t.Errorf("默认实例应为 main: %v", out["instance"])
	}
	// 显式指定实例
	out2 := callTool(t, cs, "test_connection", map[string]any{"instance": "second"})
	if out2["instance"] != "second" {
		t.Errorf("instance 参数未生效: %v", out2["instance"])
	}
}

func TestE2EListSchemasAndTables(t *testing.T) {
	cs, done := e2eSetup(t)
	defer done()

	out := callTool(t, cs, "list_schemas", nil)
	schemas := fmt.Sprint(out["schemas"])
	if !strings.Contains(schemas, "public") || !strings.Contains(schemas, "sales") {
		t.Errorf("schema 清单缺少 public/sales: %s", schemas)
	}
	if strings.Contains(schemas, "pg_catalog") {
		t.Errorf("默认应排除系统模式: %s", schemas)
	}
	outSys := callTool(t, cs, "list_schemas", map[string]any{"include_system": true})
	if !strings.Contains(fmt.Sprint(outSys["schemas"]), "pg_catalog") {
		t.Errorf("include_system 应包含 pg_catalog")
	}

	outT := callTool(t, cs, "list_tables", nil)
	tables := fmt.Sprint(outT["tables"])
	for _, want := range []string{"users", "orders", "active_users", "events"} {
		if !strings.Contains(tables, want) {
			t.Errorf("表清单缺少 %s: %s", want, tables)
		}
	}
	if !strings.Contains(tables, "partitioned table") || !strings.Contains(tables, "view") {
		t.Errorf("表清单应包含对象类型标注: %s", tables)
	}
	// 按 schema 过滤
	outS := callTool(t, cs, "list_tables", map[string]any{"schema": "sales"})
	if strings.Contains(fmt.Sprint(outS["tables"]), "users") {
		t.Errorf("schema 过滤失效: %v", outS["tables"])
	}
	if !strings.Contains(fmt.Sprint(outS["tables"]), "orders") {
		t.Errorf("sales.orders 应在清单中: %v", outS["tables"])
	}
}

func TestE2EDescribeTable(t *testing.T) {
	cs, done := e2eSetup(t)
	defer done()

	out := callTool(t, cs, "describe_table", map[string]any{"table": "users"})
	if out["schema"] != "public" {
		t.Errorf("应解析到 public 模式: %v", out["schema"])
	}
	cols := fmt.Sprint(out["columns"])
	for _, want := range []string{"id", "name", "email", "created_at", "integer", "text"} {
		if !strings.Contains(cols, want) {
			t.Errorf("列信息缺少 %s: %s", want, cols)
		}
	}
	idx := fmt.Sprint(out["indexes"])
	if !strings.Contains(idx, "idx_users_name") || !strings.Contains(idx, "users_pkey") {
		t.Errorf("索引信息不完整: %s", idx)
	}
	cons := fmt.Sprint(out["constraints"])
	if !strings.Contains(cons, "primary key") {
		t.Errorf("约束信息缺少主键: %s", cons)
	}
	if fmt.Sprint(out["comment"]) != "用户表" {
		t.Errorf("表注释错误: %v", out["comment"])
	}

	// 视图定义
	outV := callTool(t, cs, "describe_table", map[string]any{"table": "active_users"})
	viewdef := fmt.Sprint(outV["view_definition"])
	if !strings.Contains(strings.ToUpper(viewdef), "SELECT") || !strings.Contains(viewdef, "users") {
		t.Errorf("视图定义异常: %q", viewdef)
	}
	// schema 限定 + 跨模式
	outO := callTool(t, cs, "describe_table", map[string]any{"schema": "sales", "table": "orders"})
	if !strings.Contains(fmt.Sprint(outO["columns"]), "amount") {
		t.Errorf("sales.orders 列信息异常: %v", outO["columns"])
	}
	// 歧义/不存在
	if msg := callToolErr(t, cs, "describe_table", map[string]any{"table": "__no_such__"}); msg == "" {
		t.Error("查询不存在的表应返回错误信息")
	}
}

func TestE2EExecuteSelect(t *testing.T) {
	cs, done := e2eSetup(t)
	defer done()

	out := callTool(t, cs, "execute_select", map[string]any{"sql": "SELECT id, name FROM public.users ORDER BY id"})
	if out["row_count"] != float64(2) {
		t.Errorf("应返回 2 行: %v", out["row_count"])
	}
	rows := fmt.Sprint(out["rows"])
	if !strings.Contains(rows, "alice") || !strings.Contains(rows, "bob") {
		t.Errorf("数据内容异常: %s", rows)
	}
	if out["truncated"] != false {
		t.Errorf("不应截断: %v", out["truncated"])
	}
	// CTE
	out2 := callTool(t, cs, "execute_select", map[string]any{"sql": "WITH x AS (SELECT count(*) c FROM sales.orders) SELECT * FROM x"})
	if out2["row_count"] != float64(1) {
		t.Errorf("CTE 查询失败: %v", out2)
	}
	// 截断
	out3 := callTool(t, cs, "execute_select", map[string]any{"sql": "SELECT * FROM sales.orders", "max_rows": 1})
	if out3["truncated"] != true || out3["row_count"] != float64(1) {
		t.Errorf("max_rows 截断失败: %v", out3)
	}
	// 指定实例
	out4 := callTool(t, cs, "execute_select", map[string]any{"sql": "SELECT 1 AS one", "instance": "second"})
	if out4["instance"] != "second" {
		t.Errorf("instance 参数未生效: %v", out4["instance"])
	}
}

func TestE2EExecuteSelectRejectsWrites(t *testing.T) {
	cs, done := e2eSetup(t)
	defer done()

	blocked := []string{
		"INSERT INTO public.users (name) VALUES ('evil')",
		"UPDATE public.users SET name = 'evil'",
		"DELETE FROM public.users",
		"TRUNCATE public.users",
		"CREATE TABLE evil (id int)",
		"SELECT 1; DROP TABLE public.users",
		"SELECT * FROM public.users FOR UPDATE",
		"SELECT * INTO evil_t FROM public.users",
		"WITH x AS (DELETE FROM public.users RETURNING *) SELECT * FROM x",
		"SELECT set_config('search_path', 'public,evil', false)",
		"SELECT dblink('host=127.0.0.1 port=5432 dbname=testdb user=ro_test password=test123', 'INSERT INTO users VALUES (9, ''evil'')')",
		"copy public.users to stdout",
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity",
	}
	for _, sql := range blocked {
		msg := callToolErr(t, cs, "execute_select", map[string]any{"sql": sql})
		if msg == "" {
			t.Errorf("[%s] 错误信息为空", sql)
		}
	}
	// 验证数据未被篡改
	out := callTool(t, cs, "execute_select", map[string]any{"sql": "SELECT count(*) AS c FROM public.users"})
	if out["row_count"] != float64(1) {
		t.Fatalf("计数查询失败: %v", out)
	}
	if c := fmt.Sprint(out["rows"]); !strings.Contains(c, "2") {
		t.Errorf("users 行数应为 2（防护生效，数据未变）: %s", c)
	}
	// 未知实例
	callToolErr(t, cs, "execute_select", map[string]any{"sql": "SELECT 1", "instance": "nope"})
}

// parseKV 解析 "host=x port=5432" 形式的连接串。
func parseKV(dsn string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Fields(dsn) {
		if i := strings.IndexByte(part, '='); i > 0 {
			out[part[:i]] = part[i+1:]
		}
	}
	return out
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}
