package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMultiInstance(t *testing.T) {
	path := writeTemp(t, `
server:
  max_rows: 100
  statement_timeout: 15s
instances:
  - name: prod
    host: 192.168.1.10
    port: 8000
    database: postgres
    user: ro_user
    password: secret
  - name: dev
    dsn: gaussdb://ro_user:secret@127.0.0.1:5433/devdb
    max_rows: 50
default_instance: dev
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(cfg.Instances) != 2 {
		t.Fatalf("应解析出 2 个实例，实际 %d", len(cfg.Instances))
	}
	if cfg.DefaultInstance != "dev" {
		t.Errorf("默认实例应为 dev，实际 %q", cfg.DefaultInstance)
	}
	if cfg.Server.MaxRows != 100 || time.Duration(cfg.Server.StatementTimeout) != 15*time.Second {
		t.Errorf("服务级默认值解析错误: %+v", cfg.Server)
	}
	// 拆分字段实例：sslmode 默认 disable，端口缺省 5432
	prod := cfg.Instances[0]
	if prod.SSLMode != "disable" || prod.Port != 8000 {
		t.Errorf("prod 实例默认值错误: %+v", prod)
	}
	dsn := prod.BuildDSN()
	want := "host=192.168.1.10 port=8000 dbname=postgres user=ro_user password=secret sslmode=disable"
	if dsn != want {
		t.Errorf("prod DSN 错误:\n got  %s\n want %s", dsn, want)
	}
	// DSN 实例：未显式给 sslmode 时自动追加 disable
	dev := cfg.Instances[1]
	if got := dev.BuildDSN(); got != "gaussdb://ro_user:secret@127.0.0.1:5433/devdb?sslmode=disable" {
		t.Errorf("dev DSN 错误: %s", got)
	}
	if dev.MaxRows != 50 {
		t.Errorf("dev max_rows 应为 50，实际 %d", dev.MaxRows)
	}
}

func TestLoadKeepsExplicitSSLMode(t *testing.T) {
	path := writeTemp(t, `
instances:
  - name: secure
    dsn: gaussdb://u:p@h:5432/db?sslmode=verify-ca
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Instances[0].BuildDSN(); got != "gaussdb://u:p@h:5432/db?sslmode=verify-ca" {
		t.Errorf("不应覆盖显式 sslmode: %s", got)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, content := range map[string]string{
		"无实例":   "instances: []",
		"缺host": "instances:\n  - name: a\n    database: db",
		"重名":    "instances:\n  - name: a\n    host: h\n    database: d\n  - name: a\n    host: h2\n    database: d",
		"默认不存在": "instances:\n  - name: a\n    host: h\n    database: d\ndefault_instance: nope",
		"非法时长":  "instances:\n  - name: a\n    host: h\n    database: d\nserver:\n  statement_timeout: xyz",
		"空实例名":  "instances:\n  - host: h\n    database: d",
	} {
		if _, err := Load(writeTemp(t, content)); err == nil {
			t.Errorf("[%s] 应当报错", name)
		}
	}
}

func TestMaxRowsCapClamp(t *testing.T) {
	path := writeTemp(t, `
server:
  max_rows_cap: 100
instances:
  - name: a
    host: h
    database: d
    max_rows: 5000
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instances[0].MaxRows != 100 {
		t.Errorf("实例 max_rows 应被钳制到 cap 100，实际 %d", cfg.Instances[0].MaxRows)
	}
}

func TestURLOptionsMergedIntoQuery(t *testing.T) {
	path := writeTemp(t, `
instances:
  - name: a
    dsn: "gaussdb://u:p@h:5432/db?sslmode=disable"
    options:
      - "application_name=gaussdb-ro-mcp"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Instances[0].BuildDSN()
	if !strings.HasPrefix(got, "gaussdb://u:p@h:5432/db?") {
		t.Fatalf("DSN 应保持 URL 形式: %s", got)
	}
	if !strings.Contains(got, "sslmode=disable") || !strings.Contains(got, "application_name=gaussdb-ro-mcp") {
		t.Errorf("URL 查询串应同时包含 sslmode 与 application_name: %s", got)
	}
}
