package db

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gaussdb-ro-mcp/internal/config"
	"gaussdb-ro-mcp/internal/dbtest"
)

// newMockInstance 创建指向 mock 的实例（不建连，首次使用时触发 AfterConnect）。
func newMockInstance(t *testing.T, opts ...dbtest.Option) (*Instance, *dbtest.Server) {
	t.Helper()
	f := dbtest.Start(t, opts...)
	cfg := f.NewConfig()
	inst, err := newInstance(context.Background(), cfg.Instances[0], cfg)
	if err != nil {
		t.Fatalf("创建实例失败: %v", err)
	}
	t.Cleanup(inst.pool.Close)
	return inst, f
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

	t.Run("URL形式DSN内connect_timeout优先", func(t *testing.T) {
		inst := mkInst(&config.Instance{
			Name: "mock", DSN: "gaussdb://u:p@127.0.0.1:15432/db?sslmode=disable&connect_timeout=1",
			PoolMaxConns: 1, StatementTimeout: config.Duration(5 * time.Second),
		})
		if got := inst.pool.Config().ConnConfig.ConnectTimeout; got != time.Second {
			t.Errorf("URL 查询串内 connect_timeout=1 应生效（不被服务级 10s 覆盖），实际 %s", got)
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

// TestQueryReadOnlyTransactionSequence 验证所有查询都在显式只读事务中执行：
// BEGIN → SET LOCAL TRANSACTION READ ONLY → 查询 → COMMIT。
// GaussDB 分布式版仅支持事务级只读设置，该方式在集中式/分布式上通用。
func TestQueryReadOnlyTransactionSequence(t *testing.T) {
	var mu sync.Mutex
	var order []string
	inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		mu.Lock()
		order = append(order, q)
		mu.Unlock()
		return nil // 记录后仍走默认应答
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := inst.Query(ctx, "SELECT 1"); err != nil {
		t.Fatalf("Query 失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// 入池连接可能先有 statement_timeout 会话设置；其后必须紧跟
	// BEGIN → SET LOCAL TRANSACTION READ ONLY → 查询 → COMMIT。
	want := []string{"BEGIN", "SET LOCAL TRANSACTION READ ONLY", "SELECT 1", "COMMIT"}
	start := -1
	for i := range order {
		if order[i] == "BEGIN" {
			start = i
			break
		}
	}
	if start == -1 || len(order)-start < len(want) {
		t.Fatalf("缺少只读事务序列: %v", order)
	}
	for k := range want {
		if order[start+k] != want[k] {
			t.Fatalf("只读事务序列错误: got %v, want %v 起始于 %d", order, want, start)
		}
	}
}

// TestQueryReadOnlyErrorPaths 覆盖只读事务各失败分支的错误与回滚行为。
func TestQueryReadOnlyErrorPaths(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("BEGIN失败清理后重试", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		beginFailed := false
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, q)
			if q == "BEGIN" && !beginFailed { // 模拟残留事务导致首次 BEGIN 失败
				beginFailed = true
				return dbtest.ErrorResult("mock: 残留事务")
			}
			return nil
		}))

		if _, err := inst.Query(ctx, "SELECT 1"); err != nil {
			t.Fatalf("清理残留事务后应重试成功: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if got := strings.Join(order, " | "); !strings.Contains(got, "ROLLBACK | BEGIN") {
			t.Errorf("应先 ROLLBACK 清理再重试 BEGIN: %v", order)
		}
	})

	t.Run("SETLOCAL失败回滚并报错", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.HasPrefix(q, "SET LOCAL") {
				return dbtest.ErrorResult("mock: 拒绝事务只读")
			}
			return nil
		}))
		_, err := inst.Query(ctx, "SELECT 1")
		if err == nil || !strings.Contains(err.Error(), "设置事务只读失败") {
			t.Fatalf("SET LOCAL 失败应报错: %v", err)
		}
	})

	t.Run("查询失败回滚并上抛", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if strings.Contains(q, "boom") {
				return dbtest.ErrorResult("mock: 查询失败")
			}
			return nil
		}))
		if _, err := inst.Query(ctx, "SELECT 'boom' AS x"); err == nil {
			t.Fatal("查询失败应上抛")
		}
	})

	t.Run("COMMIT失败报错", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if q == "COMMIT" {
				return dbtest.ErrorResult("mock: 提交失败")
			}
			return nil
		}))
		_, err := inst.Query(ctx, "SELECT 1")
		if err == nil || !strings.Contains(err.Error(), "提交只读事务失败") {
			t.Fatalf("COMMIT 失败应报错: %v", err)
		}
	})
}

// TestStatementTimeoutViaAfterConnect 验证入池连接的会话级 statement_timeout：
// 亚毫秒钳制为 1ms（回归 issue #13），零值不下发。
func TestStatementTimeoutViaAfterConnect(t *testing.T) {
	var mu sync.Mutex
	var setTimeout string
	hook := func(q string) *dbtest.Result {
		if strings.HasPrefix(q, "SET statement_timeout") {
			mu.Lock()
			setTimeout = q
			mu.Unlock()
		}
		return nil
	}

	t.Run("亚毫秒钳制为1ms", func(t *testing.T) {
		setTimeout = ""
		f := dbtest.Start(t, dbtest.WithQueryHook(hook))
		cfg := f.NewConfig()
		cfg.Instances[0].StatementTimeout = config.Duration(100 * time.Microsecond)
		inst, err := newInstance(context.Background(), cfg.Instances[0], cfg)
		if err != nil {
			t.Fatalf("创建实例失败: %v", err)
		}
		defer inst.pool.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := inst.Query(ctx, "SELECT 1"); err != nil {
			t.Fatalf("Query 失败: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasSuffix(setTimeout, "= 1") {
			t.Errorf("亚毫秒超时应钳制为 1ms，实际: %q", setTimeout)
		}
	})

	t.Run("零值不下发", func(t *testing.T) {
		setTimeout = ""
		f := dbtest.Start(t, dbtest.WithQueryHook(hook))
		cfg := f.NewConfig()
		cfg.Instances[0].StatementTimeout = 0
		inst, err := newInstance(context.Background(), cfg.Instances[0], cfg)
		if err != nil {
			t.Fatalf("创建实例失败: %v", err)
		}
		defer inst.pool.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := inst.Query(ctx, "SELECT 1"); err != nil {
			t.Fatalf("Query 失败: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if setTimeout != "" {
			t.Errorf("零值不应下发 SET statement_timeout: %q", setTimeout)
		}
	})
}
