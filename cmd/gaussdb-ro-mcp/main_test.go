package main

import (
	"bytes"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gaussdb-ro-mcp/internal/dbtest"
)

// quiet 返回丢弃输出的 logger。
func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

// withStdinDevnull 把 os.Stdin 指向 /dev/null（stdio 传输立即读到 EOF），返回还原函数。
func withStdinDevnull(t *testing.T) {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("打开 /dev/null 失败: %v", err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
		f.Close()
	})
}

// captureStdout 捕获进程标准输出，返回还原与读取函数。
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	return func() string {
		w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		return string(out)
	}
}

// TestStartVersion 覆盖 -version 分支。
func TestStartVersion(t *testing.T) {
	read := captureStdout(t)
	err := start("", true, "v9.9-test", quiet())
	out := read()
	if err != nil {
		t.Fatalf("start 不应报错: %v", err)
	}
	if strings.TrimSpace(out) != "v9.9-test" {
		t.Errorf("应打印版本号，实际 %q", out)
	}
}

// TestStartConfigError 覆盖配置加载失败的错误分支。
func TestStartConfigError(t *testing.T) {
	err := start(filepath.Join(t.TempDir(), "nope.yaml"), false, "dev", quiet())
	if err == nil || !strings.Contains(err.Error(), "配置加载失败") {
		t.Fatalf("应报配置加载失败: %v", err)
	}
}

// TestStartDefaultConfigPath 覆盖空路径回退到 ./gaussdb-ro-mcp.yaml 的分支。
func TestStartDefaultConfigPath(t *testing.T) {
	tmp := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	err = start("", false, "dev", quiet())
	if err == nil || !strings.Contains(err.Error(), "配置加载失败") {
		t.Fatalf("默认路径不存在配置时应报配置加载失败: %v", err)
	}
}

// TestStartBadDSN 覆盖数据源初始化失败的错误分支。
func TestStartBadDSN(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "instances:\n  - name: bad\n    dsn: \"gaussdb://u:p@h/db?sslmode=invalid\"\ndefault_instance: bad\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	err := start(cfgPath, false, "dev", quiet())
	if err == nil || !strings.Contains(err.Error(), "初始化数据源失败") {
		t.Fatalf("非法 DSN 应报初始化失败: %v", err)
	}
}

// TestStartServesUntilEOF 用 mock 数据源完整跑通启动流程；
// os.Stdin 指向 /dev/null 时 stdio 传输读到 EOF 后正常返回。
func TestStartServesUntilEOF(t *testing.T) {
	f := dbtest.Start(t)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "server:\n  name: test\ninstances:\n  - name: mock\n    dsn: \"" + f.DSN() + "\"\ndefault_instance: mock\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	withStdinDevnull(t)
	if err := start(cfgPath, false, "test", quiet()); err != nil {
		t.Fatalf("start 不应报错: %v", err)
	}
}

// TestMainGracefulShutdownOnSignal 经 main() 入口完整执行一次：运行中向自身发送
// SIGINT，验证信号触发的 context.Canceled 被识别为正常关闭（回归 issue #5，
// 不走 Fatalf/os.Exit(1)）。注意：main 会重复定义 flag，整个测试进程只调用一次。
func TestMainGracefulShutdownOnSignal(t *testing.T) {
	f := dbtest.Start(t)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "server:\n  name: test\ninstances:\n  - name: mock\n    dsn: \"" + f.DSN() + "\"\ndefault_instance: mock\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{"gaussdb-ro-mcp", "-config", cfgPath}
	defer func() { os.Args = oldArgs }()

	// stdin 用保持打开的管道（不发送数据），让 server.Run 阻塞直到信号到达。
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = oldStdin
		r.Close()
		w.Close()
	}()

	go func() {
		time.Sleep(500 * time.Millisecond)
		// NotifyContext 已接管 SIGINT，默认终止行为被屏蔽，仅取消 ctx。
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()

	main() // 信号触发优雅退出：不应 os.Exit(1)
}

// TestPrintUsage 验证帮助信息包含版本、参数与反馈/最新版本地址。
func TestPrintUsage(t *testing.T) {
	flagSet := flag.NewFlagSet("gaussdb-ro-mcp", flag.ContinueOnError)
	registerFlags(flagSet)
	var buf bytes.Buffer
	printUsage(&buf, "v9.9-test", flagSet)
	out := buf.String()
	for _, want := range []string{
		"v9.9-test",
		"-config",
		"-version",
		"https://github.com/gxc/gaussdb-ro-mcp/issues",
		"https://github.com/gxc/gaussdb-ro-mcp/releases/latest",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("帮助信息缺少 %q:\n%s", want, out)
		}
	}
}
