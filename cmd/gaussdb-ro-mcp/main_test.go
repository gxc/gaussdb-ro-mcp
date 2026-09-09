package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestMainFullRun 经 main() 入口完整执行一次（覆盖 flag 解析与 start 调用）。
// 注意：main 会重复定义 flag，整个测试进程只允许调用一次 main。
func TestMainFullRun(t *testing.T) {
	f := dbtest.Start(t)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "server:\n  name: test\ninstances:\n  - name: mock\n    dsn: \"" + f.DSN() + "\"\ndefault_instance: mock\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{"gaussdb-ro-mcp", "-config", cfgPath}
	withStdinDevnull(t)
	defer func() { os.Args = oldArgs }()

	// start 内部 server.Run 读到 EOF 后正常返回，main 不应触发 Fatalf（os.Exit）。
	main()
}
