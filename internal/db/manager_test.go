package db

import (
	"context"
	"strings"
	"testing"
	"time"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"

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

// TestStartupParamCarriesReadOnly 是 55P02 修复的回归测试：default_transaction_read_only=on
// 必须随启动包在会话初始化时下发（先于任何事务生效），而不能依赖建连后再 SET。
func TestStartupParamCarriesReadOnly(t *testing.T) {
	f := dbtest.Start(t)
	cfg := f.NewConfig()

	inst, err := newInstance(context.Background(), cfg.Instances[0], cfg)
	if err != nil {
		t.Fatalf("创建实例失败: %v", err)
	}
	defer inst.pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := inst.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("获取连接失败（AfterConnect 只读校验未通过？）: %v", err)
	}
	c.Release()

	select {
	case got := <-f.StartupParams():
		if got["default_transaction_read_only"] != "on" {
			t.Fatalf("启动包缺少 default_transaction_read_only=on，实际参数: %v", got)
		}
		if got["user"] == "" {
			t.Fatalf("启动包缺少 user，实际参数: %v", got)
		}
	case <-ctx.Done():
		t.Fatal("未收到启动包")
	}
}

// TestEnforceReadOnlyFallback 验证回退分支：启动参数未生效（首次 SHOW 为 off）时，
// 先 ROLLBACK 清理再会话级 SET 重新校验；复查仍 off 则拒绝该连接。
func TestEnforceReadOnlyFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("回退后生效", func(t *testing.T) {
		f := dbtest.Start(t, dbtest.WithShowValues("off", "on"))
		conn, err := gaussdbgo.Connect(ctx, f.DSN())
		if err != nil {
			t.Fatalf("连接 mock 失败: %v", err)
		}
		defer conn.Close(ctx)

		if err := enforceReadOnly(ctx, conn, 5*time.Second); err != nil {
			t.Fatalf("回退路径应成功: %v", err)
		}
	})

	t.Run("复查仍off则拒绝", func(t *testing.T) {
		f := dbtest.Start(t, dbtest.WithShowValues("off", "off"))
		conn, err := gaussdbgo.Connect(ctx, f.DSN())
		if err != nil {
			t.Fatalf("连接 mock 失败: %v", err)
		}
		defer conn.Close(ctx)

		err = enforceReadOnly(ctx, conn, 0)
		if err == nil || !strings.Contains(err.Error(), "拒绝该连接") {
			t.Fatalf("复查仍 off 应拒绝该连接，实际: %v", err)
		}
	})

	t.Run("statement_timeout设置失败", func(t *testing.T) {
		f := dbtest.Start(t, dbtest.WithQueryHook(func(query string) *dbtest.Result {
			if strings.Contains(strings.ToUpper(query), "STATEMENT_TIMEOUT") {
				return dbtest.ErrorResult("mock: statement_timeout 拒绝")
			}
			return nil
		}))
		conn, err := gaussdbgo.Connect(ctx, f.DSN())
		if err != nil {
			t.Fatalf("连接 mock 失败: %v", err)
		}
		defer conn.Close(ctx)

		err = enforceReadOnly(ctx, conn, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "设置 statement_timeout 失败") {
			t.Fatalf("statement_timeout 设置失败应报错，实际: %v", err)
		}
	})

	t.Run("show失败走回退后仍失败", func(t *testing.T) {
		f := dbtest.Start(t, dbtest.WithQueryHook(func(query string) *dbtest.Result {
			if strings.Contains(strings.ToUpper(query), "TRANSACTION_READ_ONLY") {
				return dbtest.ErrorResult("mock: show 被拒绝")
			}
			return nil
		}))
		conn, err := gaussdbgo.Connect(ctx, f.DSN())
		if err != nil {
			t.Fatalf("连接 mock 失败: %v", err)
		}
		defer conn.Close(ctx)

		err = enforceReadOnly(ctx, conn, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "验证只读状态失败") {
			t.Fatalf("SHOW 持续失败应报验证错误，实际: %v", err)
		}
	})
}
