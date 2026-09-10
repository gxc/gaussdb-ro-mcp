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

// TestEnforceReadOnlyBlocksWrites 验证会话层防护：即使 SQL 校验层被绕过，
// 服务端的 READ ONLY 会话也会拒绝一切写操作。
// 注意：此处用裸 DSN 直连（未带启动参数），会走 enforceReadOnly 的
// "回读校验未生效 → ROLLBACK + 会话级 SET 回退" 分支。
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

// TestEnforceReadOnlyRecoversFromLingeringTransaction 模拟 55P02 的真实触发场景：
// 服务端复用会话交付时残留未结束事务（GaussDB 禁止在事务中修改
// default_transaction_read_only），enforceReadOnly 应 ROLLBACK 清理后回退 SET 成功。
// 前提：测试库 default_transaction_read_only 为默认值 off。
func TestEnforceReadOnlyRecoversFromLingeringTransaction(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := gaussdbgo.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("制造残留事务失败: %v", err)
	}
	if conn.GaussdbConn().TxStatus() != 'T' {
		t.Fatalf("期望会话处于事务中（TxStatus=T），实际 %q", conn.GaussdbConn().TxStatus())
	}

	if err := enforceReadOnly(ctx, conn, 5*time.Second); err != nil {
		t.Fatalf("残留事务应被清理并通过只读校验: %v", err)
	}
	if conn.GaussdbConn().TxStatus() != 'I' {
		t.Fatalf("期望残留事务被 ROLLBACK（TxStatus=I），实际 %q", conn.GaussdbConn().TxStatus())
	}

	var ro string
	if err := conn.QueryRow(ctx, "SHOW transaction_read_only").Scan(&ro); err != nil || !strings.EqualFold(ro, "on") {
		t.Fatalf("transaction_read_only=%q err=%v，应为 on", ro, err)
	}
}

// TestManagerPoolEnforcesReadOnly 覆盖连接池全链路（NewManager → AfterConnect）：
// 池上 SELECT 正常返回，写操作被服务端只读会话拒绝。
func TestManagerPoolEnforcesReadOnly(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := &config.Config{
		Server: config.Server{
			MaxRowsCap:     10000,
			ConnectTimeout: config.Duration(10 * time.Second),
		},
		Instances: []*config.Instance{{
			Name:             "test",
			DSN:              dsn,
			PoolMaxConns:     2,
			StatementTimeout: config.Duration(5 * time.Second),
		}},
	}
	mgr, err := NewManager(ctx, cfg)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	defer mgr.Close()

	inst, err := mgr.Resolve("test")
	if err != nil {
		t.Fatalf("解析实例失败: %v", err)
	}

	res, err := inst.Select(ctx, "SELECT 1 AS one", 10)
	if err != nil {
		t.Fatalf("池内 SELECT 失败: %v", err)
	}
	if res.RowCount != 1 {
		t.Fatalf("期望返回 1 行，实际 %d 行", res.RowCount)
	}

	// 写操作应被服务端只读会话兜底拒绝（TEMP 表亦不允许）。
	if _, err := inst.pool.Exec(ctx, "CREATE TEMP TABLE __ro_pool_t (i int)"); err == nil {
		t.Error("池上写语句未被拒绝")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Logf("池上写语句被拒绝，原因: %v", err)
	}
}

// TestEnforceReadOnlyRecoversFromAbortedTransaction 回归 issue #4 场景 a：
// 中止的残留事务里任何 SET 都会报 25P02，enforceReadOnly 应先 ROLLBACK 清理
// 再回退会话级 SET（statement_timeout 在只读校验完成后设置）。
func TestEnforceReadOnlyRecoversFromAbortedTransaction(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := gaussdbgo.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("除零应失败从而使事务进入中止状态")
	}
	if got := conn.GaussdbConn().TxStatus(); got != 'E' {
		t.Fatalf("期望会话处于中止事务（TxStatus=E），实际 %q", got)
	}

	if err := enforceReadOnly(ctx, conn, 5*time.Second); err != nil {
		t.Fatalf("中止事务应被清理并通过只读校验: %v", err)
	}
	if got := conn.GaussdbConn().TxStatus(); got != 'I' {
		t.Fatalf("期望中止事务被 ROLLBACK（TxStatus=I），实际 %q", got)
	}
}
