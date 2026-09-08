package main

// 子进程冒烟测试：构建真实二进制，通过 MCP CommandTransport（stdio）调用，
// 验证配置文件加载、工具清单与只读执行链路。
// 通过 GAUSSDB_RO_MCP_TEST_DSN 启用。

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gaussdb-ro-mcp")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("构建二进制失败: %v", err)
	}
	return bin
}

func TestSubprocessStdio(t *testing.T) {
	dsn := os.Getenv("GAUSSDB_RO_MCP_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 GAUSSDB_RO_MCP_TEST_DSN，跳过集成测试")
	}
	bin := buildBinary(t)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "server:\n  name: smoke-test\ninstances:\n  - name: it\n    dsn: \"" + dsn + "\"\ndefault_instance: it\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.Command(bin, "-config", cfgPath)
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("连接子进程失败: %v", err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	if len(list.Tools) != 5 {
		t.Errorf("应有 5 个工具，实际 %d", len(list.Tools))
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "execute_select",
		Arguments: map[string]any{"sql": "SELECT 'stdio-e2e' AS tag"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("SELECT 不应报错: %v", res.Content)
	}
	if !strings.Contains(fmtSprint(res.StructuredContent), "stdio-e2e") {
		t.Errorf("结果缺少预期值: %v", res.StructuredContent)
	}

	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "execute_select",
		Arguments: map[string]any{"sql": "DROP TABLE IF EXISTS __smoke__"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.IsError {
		t.Error("DROP 语句应被拒绝")
	}
}

func fmtSprint(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
