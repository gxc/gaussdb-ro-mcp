package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"
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

// TestEnforceReadOnlyBlocksWrites 验证会话层防护：即使 SQL 校验层被绕过，
// 服务端的 READ ONLY 会话也会拒绝一切写操作。
func TestEnforceReadOnlyBlocksWrites(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := gaussdbgo.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close(ctx)

	if err := enforceReadOnly(ctx, conn, 5*time.Second); err != nil {
		t.Fatalf("enforceReadOnly 失败: %v", err)
	}

	// 只读查询应当正常。
	var got int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&got); err != nil || got != 1 {
		t.Fatalf("只读会话内 SELECT 失败: %v", err)
	}

	// 各类写操作都应被服务端拒绝。
	// 注意：set_config 等会话变量操作由第 1 层 SQL 校验负责（guard 黑名单），
	// 服务端 READ ONLY 事务本身不拦截它；此处只验证第 2 层对"写"的兜底。
	writes := []string{
		"CREATE TABLE __ro_test_t (id int)",
		"INSERT INTO __ro_test_t VALUES (1)",
		"DROP TABLE IF EXISTS __ro_test_t",
		"SELECT nextval('nonexistent_seq')",
	}
	for _, sql := range writes {
		if _, err := conn.Exec(ctx, sql); err == nil {
			t.Errorf("写语句未被拒绝: %s", sql)
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Logf("写语句 %s 被拒绝，原因: %v", sql, err)
		}
	}

	// 验证回读状态。
	var ro string
	if err := conn.QueryRow(ctx, "SHOW transaction_read_only").Scan(&ro); err != nil || ro != "on" {
		t.Fatalf("transaction_read_only=%q err=%v，应为 on", ro, err)
	}
}
