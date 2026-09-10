package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"

	"gaussdb-ro-mcp/internal/config"
)

// testDSN 由环境变量 GAUSSDB_RO_MCP_TEST_DSN 提供（key=value 形式）。
// 未设置时跳过集成测试。
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GAUSSDB_RO_MCP_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 GAUSSDB_RO_MCP_TEST_DSN，跳过数据库集成测试")
	}
	return dsn
}

// beginReadOnly 在裸连接上开启事务级只读事务（与 queryReadOnly 同一机制）。
func beginReadOnly(ctx context.Context, t *testing.T, conn *gaussdbgo.Conn) {
	t.Helper()
	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("BEGIN 失败: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET LOCAL TRANSACTION READ ONLY"); err != nil {
		t.Fatalf("SET LOCAL TRANSACTION READ ONLY 失败: %v", err)
	}
}

// TestReadOnlyTransactionRejectsWrites 验证事务级防护：即使 SQL 校验层被绕过，
// 服务端的只读事务也会拒绝一切写操作。注意：本驱动使用 GaussDB 扩展协议，
// 需要真实的 GaussDB / openGauss 实例。
func TestReadOnlyTransactionRejectsWrites(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := gaussdbgo.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close(ctx)

	beginReadOnly(ctx, t, conn)

	// 只读查询应当正常。
	var got int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&got); err != nil || got != 1 {
		t.Fatalf("只读事务内 SELECT 失败: %v", err)
	}

	// 各类写操作都应被服务端拒绝。
	writes := []string{
		"CREATE TABLE __ro_tx_test (id int)",
		"INSERT INTO __ro_tx_test VALUES (1)",
		"DROP TABLE IF EXISTS __ro_tx_test",
		"SELECT nextval('nonexistent_seq')",
	}
	for _, sql := range writes {
		if _, err := conn.Exec(ctx, sql); err == nil {
			t.Errorf("写语句未被拒绝: %s", sql)
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Logf("写语句 %s 被拒绝，原因: %v", sql, err)
		}
	}

	if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("ROLLBACK 失败: %v", err)
	}
}

// TestSelectRejectsWritesInReadOnlyTransaction 验证经连接池（queryReadOnly）
// 的查询跑在只读事务中：db 层直通写语句（绕过 tools 的 SQL 校验层）时，
// 服务端仍然拒绝写入；正常查询不受影响。
func TestSelectRejectsWritesInReadOnlyTransaction(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr, err := NewManager(ctx, &config.Config{
		Server: config.Server{MaxRowsCap: 10000, ConnectTimeout: config.Duration(10 * time.Second)},
		Instances: []*config.Instance{{
			Name: "test", DSN: dsn, PoolMaxConns: 2, StatementTimeout: config.Duration(5 * time.Second),
		}},
	})
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	defer mgr.Close()

	inst, err := mgr.Resolve("test")
	if err != nil {
		t.Fatalf("解析实例失败: %v", err)
	}

	if _, err := inst.Select(ctx, "DROP TABLE IF EXISTS __ro_tx_guard_bypass", 10); err == nil {
		t.Fatal("db 层直通 DROP 应被服务端只读事务拒绝")
	}
	if _, err := inst.Query(ctx, "INSERT INTO __ro_tx_guard_bypass VALUES (1)"); err == nil {
		t.Fatal("db 层直通 INSERT 应被服务端只读事务拒绝")
	}

	res, err := inst.Select(ctx, "SELECT 1 AS one", 10)
	if err != nil || res.RowCount != 1 {
		t.Fatalf("只读事务内 SELECT 失败: %v", err)
	}
}
