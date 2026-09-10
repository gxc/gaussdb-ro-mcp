package db

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gaussdb-ro-mcp/internal/config"
	"gaussdb-ro-mcp/internal/dbtest"
)

// newMockInstance 创建指向 mock 的实例（不建连，首次使用时触发 AfterConnect）。
func newMockInstance(t *testing.T, opts ...dbtest.Option) (*Instance, *dbtest.Server) {
	t.Helper()
	return newMockInstanceCfg(t, nil, opts...)
}

// newMockInstanceCfg 在 newMockInstance 基础上允许在建实例前修改配置
// （如覆盖 StatementTimeout），避免各测试手写装配序列。
func newMockInstanceCfg(t *testing.T, mutate func(*config.Config), opts ...dbtest.Option) (*Instance, *dbtest.Server) {
	t.Helper()
	f := dbtest.Start(t, opts...)
	cfg := f.NewConfig()
	if mutate != nil {
		mutate(cfg)
	}
	inst, err := newInstance(context.Background(), cfg.Instances[0], cfg)
	if err != nil {
		t.Fatalf("创建实例失败: %v", err)
	}
	t.Cleanup(inst.pool.Close)
	return inst, f
}

// TestConnectTimeoutPrecedence 回归 issue #7：连接超时的三级优先级
// （DSN/options 显式值 > 实例级字段 > 服务级默认），与配置注释/示例一致，
// 不再无条件覆盖、也不再让实例级字段反超 DSN 显式值。
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

	t.Run("DSN显式值优先于实例级字段", func(t *testing.T) {
		// 与配置注释/示例一致：DSN/options 显式写出的 connect_timeout 优先级最高，
		// 实例级字段只在 DSN 未给出时生效（回归：旧实现实例级字段反超 DSN）。
		inst := mkInst(&config.Instance{
			Name: "mock", Host: "127.0.0.1", Port: 15432, Database: "db",
			User: "u", Password: "p", SSLMode: "disable", PoolMaxConns: 1,
			StatementTimeout: config.Duration(5 * time.Second),
			Options:          []string{"connect_timeout=1"},
			ConnectTimeout:   config.Duration(3 * time.Second),
		})
		if got := inst.pool.Config().ConnConfig.ConnectTimeout; got != time.Second {
			t.Errorf("DSN/options 显式 connect_timeout=1 应优先于实例级 3s，实际 %s", got)
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
// BEGIN → SET LOCAL TRANSACTION READ ONLY → 回读校验 → 查询 → COMMIT。
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
	// BEGIN → SET LOCAL → 回读校验 SHOW → 查询 → COMMIT。
	want := []string{
		"BEGIN",
		"SET LOCAL TRANSACTION READ ONLY",
		"SHOW transaction_read_only",
		"SELECT 1",
		"COMMIT",
	}
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

// TestQueryReadOnlyVerifiesSetLocal 回归：SET LOCAL 之后必须回读校验，只读
// 未生效（如被中间代理剥离）时拒绝执行业务查询（fail-closed）。
func TestQueryReadOnlyVerifiesSetLocal(t *testing.T) {
	var mu sync.Mutex
	sawBusinessQuery := false
	inst, _ := newMockInstance(t,
		dbtest.WithShowValues("off"), // 模拟 SET LOCAL 未生效
		dbtest.WithQueryHook(func(q string) *dbtest.Result {
			mu.Lock()
			defer mu.Unlock()
			if q == "SELECT secret FROM t" {
				sawBusinessQuery = true
			}
			return nil
		}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := inst.Query(ctx, "SELECT secret FROM t")
	if err == nil || !strings.Contains(err.Error(), "只读事务校验失败") {
		t.Fatalf("只读未生效应拒绝执行查询，实际: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawBusinessQuery {
		t.Fatal("校验失败后业务查询不应被执行")
	}
}

// TestEnsureIdleTxCleansLingering 验证 BEGIN 前的残留事务清理：对活动事务
// 直接 BEGIN 只产生 WARNING，查询会被并入外来事务。
func TestEnsureIdleTxCleansLingering(t *testing.T) {
	inst, _ := newMockInstance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := inst.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("获取连接失败: %v", err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("制造残留事务失败: %v", err)
	}
	if got := c.Conn().GaussdbConn().TxStatus(); got != 'T' {
		t.Fatalf("期望会话处于事务中（TxStatus=T），实际 %q", got)
	}
	if err := ensureIdleTx(ctx, c); err != nil {
		t.Fatalf("清理残留事务应成功: %v", err)
	}
	if got := c.Conn().GaussdbConn().TxStatus(); got != 'I' {
		t.Fatalf("期望残留事务被清理（TxStatus=I），实际 %q", got)
	}
}

// TestSelectKeepsResultsOnCommitFailure 回归：结果已完整读取、仅提交失败时，
// Select 保留结果并附警告，而不是丢弃已取回的数据。
func TestSelectKeepsResultsOnCommitFailure(t *testing.T) {
	inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		switch {
		case q == "COMMIT":
			return dbtest.ErrorResult("mock: 提交失败")
		case strings.Contains(q, "AS one"):
			return dbtest.Rows([]dbtest.Col{dbtest.Int8("one", 1)}, []string{"1"})
		}
		return nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := inst.Select(ctx, "SELECT 1 AS one", 10)
	if err != nil {
		t.Fatalf("仅提交失败时 Select 应返回结果与警告: %v", err)
	}
	if res.RowCount != 1 || len(res.Rows) != 1 || res.Rows[0]["one"] == nil {
		t.Errorf("已取回的行不应丢弃: %+v", res)
	}
	if res.Warning == "" || !strings.Contains(res.Warning, "提交只读事务失败") {
		t.Errorf("应附提交失败警告: %q", res.Warning)
	}
	// 覆盖 errCommitFailed 的错误链展开（供 errors.Is/As 透出底层驱动错误）。
	inner := errors.New("boom")
	if !errors.Is(&errCommitFailed{inner}, inner) {
		t.Error("errCommitFailed 应透传底层错误")
	}
}

// TestDetectPartitionSupportSingleflight 验证并发探测只触发一次网络往返：
// 后来的调用等待首个探测的结果，而不是各自重复探测或被互斥锁串行阻塞。
func TestDetectPartitionSupportSingleflight(t *testing.T) {
	probeEntered := make(chan struct{})
	probeRelease := make(chan struct{})
	var probes int32
	inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
		if strings.Contains(q, "pg_attribute") && strings.Contains(q, "parttype") {
			if atomic.CompareAndSwapInt32(&probes, 0, 1) {
				close(probeEntered)
				<-probeRelease // 让首个探测驻留，制造并发窗口
			}
		}
		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan bool, 1)
	go func() { done <- inst.detectPartitionSupport(ctx) }()
	<-probeEntered // 首个探测已在途

	waiter := make(chan bool, 1)
	go func() { waiter <- inst.detectPartitionSupport(ctx) }()
	// 给等待者进入等待分支的机会，再放行首个探测。
	time.Sleep(50 * time.Millisecond)
	close(probeRelease)

	first := <-done
	second := <-waiter
	if first != second {
		t.Errorf("并发探测结果应一致: %v vs %v", first, second)
	}
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Errorf("探测应只执行一次，实际 %d 次", n)
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

	t.Run("BEGIN与ROLLBACK都失败则报错", func(t *testing.T) {
		inst, _ := newMockInstance(t, dbtest.WithQueryHook(func(q string) *dbtest.Result {
			if q == "BEGIN" || q == "ROLLBACK" {
				return dbtest.ErrorResult("mock: 清理失败")
			}
			return nil
		}))
		_, err := inst.Query(ctx, "SELECT 1")
		if err == nil || !strings.Contains(err.Error(), "开启只读事务失败") {
			t.Fatalf("BEGIN 且清理失败应报错: %v", err)
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
		inst, _ := newMockInstanceCfg(t, func(cfg *config.Config) {
			cfg.Instances[0].StatementTimeout = config.Duration(100 * time.Microsecond)
		}, dbtest.WithQueryHook(hook))

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
		inst, _ := newMockInstanceCfg(t, func(cfg *config.Config) {
			cfg.Instances[0].StatementTimeout = 0
		}, dbtest.WithQueryHook(hook))

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
