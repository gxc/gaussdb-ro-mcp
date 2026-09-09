package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestLoadMissingFile 覆盖配置文件不存在的错误分支。
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent-dir/nope.yaml"); err == nil ||
		!strings.Contains(err.Error(), "读取配置文件失败") {
		t.Fatalf("文件不存在应报读取失败: %v", err)
	}
}

// TestBuildDSNWithOptions 覆盖 options 与 URL/kv 连接串的合并。
func TestBuildDSNWithOptions(t *testing.T) {
	urlInst := Instance{
		DSN:     "gaussdb://u:p@h:5432/db?sslmode=disable",
		Options: []string{"application_name=mcp"},
	}
	got := urlInst.BuildDSN()
	if !strings.Contains(got, "application_name=mcp") || !strings.Contains(got, "sslmode=disable") {
		t.Errorf("URL DSN 应合并 options: %s", got)
	}

	kvInst := Instance{
		Host:     "h",
		Port:     5432,
		Database: "db",
		User:     "u",
		Options:  []string{"application_name=mcp"},
	}
	got = kvInst.BuildDSN()
	if !strings.HasSuffix(got, "application_name=mcp") {
		t.Errorf("kv DSN 应追加 options: %s", got)
	}
}

// TestEnsureSSLModeDirect 覆盖 sslmode 追加的全部分支。
func TestEnsureSSLModeDirect(t *testing.T) {
	cases := []struct {
		dsn, mode, want string
	}{
		{"gaussdb://u@h/db", "require", "gaussdb://u@h/db?sslmode=require"},
		{"gaussdb://u@h/db?foo=1", "require", "gaussdb://u@h/db?foo=1&sslmode=require"},
		{"host=h sslmode=disable", "require", "host=h sslmode=disable"},
		{"host=h", "", "host=h sslmode=disable"},
	}
	for _, c := range cases {
		if got := ensureSSLMode(c.dsn, c.mode); got != c.want {
			t.Errorf("ensureSSLMode(%q, %q) = %q, want %q", c.dsn, c.mode, got, c.want)
		}
	}
}

// TestJoinOptionsDirect 覆盖 kv 追加的辅助函数。
func TestJoinOptionsDirect(t *testing.T) {
	if got := joinOptions2("a=1", []string{"b=2"}); got != "a=1 b=2" {
		t.Errorf("joinOptions2 = %q", got)
	}
	if got := joinOptions([]string{"b=2"}); got != " b=2" {
		t.Errorf("joinOptions = %q", got)
	}
	if got := joinOptions(nil); got != "" {
		t.Errorf("空 options 应返回空串: %q", got)
	}
}

// TestDurationUnmarshalNumeric 覆盖纯数字（秒）形式的时长解析。
func TestDurationUnmarshalNumeric(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte("1.5"), &d); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if time.Duration(d) != 1500*time.Millisecond {
		t.Errorf("1.5 应解析为 1.5s，实际 %s", time.Duration(d))
	}
}

// TestPortDefault 覆盖端口缺省为 5432 的分支。
func TestPortDefault(t *testing.T) {
	cfg, err := Load(writeTemp(t, "instances:\n  - name: a\n    host: h\n    database: d\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Instances[0].BuildDSN(), "port=5432") {
		t.Errorf("端口应缺省为 5432: %s", cfg.Instances[0].BuildDSN())
	}
}
