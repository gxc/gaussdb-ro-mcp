package config

import (
	"path/filepath"
	"testing"
)

// TestExampleYAMLLoads 保障仓库自带示例配置在 KnownFields 严格模式下可加载，
// 用户直接复制使用不会踩到未知键报错。
func TestExampleYAMLLoads(t *testing.T) {
	path := filepath.Join("..", "..", "gaussdb-ro-mcp.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("example.yaml 应可加载: %v", err)
	}
	if len(cfg.Instances) != 3 || cfg.DefaultInstance != "prod" {
		t.Errorf("example.yaml 内容异常: %d 实例, 默认 %q", len(cfg.Instances), cfg.DefaultInstance)
	}
}
