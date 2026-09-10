package config

import (
	"net/url"
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
	if !strings.Contains(got, "application_name=mcp") || !strings.HasPrefix(got, "gaussdb://") {
		t.Errorf("kv 实例应生成含 options 的 URL DSN: %s", got)
	}

	// keyword=value 形式的 DSN + options：走 joinOptions2 空格追加回退。
	kvDSN := Instance{
		DSN:     "host=h dbname=db user=u sslmode=disable",
		Options: []string{"application_name=mcp"},
	}
	got = kvDSN.BuildDSN()
	if !strings.HasSuffix(got, "application_name=mcp") {
		t.Errorf("kv DSN 应空格追加 options: %s", got)
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
		// 含控制字符的 URL 无法解析：按 kv 追加兜底，绝不返回不带 sslmode 的串。
		{"gaussdb://h/db\n?foo=1", "require", "gaussdb://h/db\n?foo=1 sslmode=require"},
		// 密码值中恰含 "://"：不是 URL 形式，sslmode 正常追加（回归）。
		{"host=h password=a://b", "require", "host=h password=a://b sslmode=require"},
		// 单引号密码值中含 " sslmode=" 伪键：不算已设置，仍追加默认值。
		{"host=h password='my pass sslmode=x'", "", "host=h password='my pass sslmode=x' sslmode=disable"},
	}
	for _, c := range cases {
		if got := ensureSSLMode(c.dsn, c.mode); got != c.want {
			t.Errorf("ensureSSLMode(%q, %q) = %q, want %q", c.dsn, c.mode, got, c.want)
		}
	}
}

// TestDSNHasParam 覆盖共享的连接串参数检测助手（URL 与 kv 两种形式）。
func TestDSNHasParam(t *testing.T) {
	cases := []struct {
		dsn, key string
		want     bool
	}{
		{"gaussdb://h/db?connect_timeout=1", "connect_timeout", true},
		{"gaussdb://h/db?sslmode=disable", "connect_timeout", false},
		{"host=h connect_timeout=1", "connect_timeout", true},
		{"host=h", "connect_timeout", false},
		// 值内部的伪键不算（无引号但位于其他值中间的场景亦不误判）。
		{"host=h password='my pass connect_timeout=1'", "connect_timeout", false},
		{"host=h password=a://b", "connect_timeout", false},
	}
	for _, c := range cases {
		if got := DSNHasParam(c.dsn, c.key); got != c.want {
			t.Errorf("DSNHasParam(%q, %q) = %v, want %v", c.dsn, c.key, got, c.want)
		}
	}
}

// TestJoinOptionsDirect 覆盖 kv 追加的辅助函数。
func TestJoinOptionsDirect(t *testing.T) {
	if got := joinOptions2("a=1", []string{"b=2"}); got != "a=1 b=2" {
		t.Errorf("joinOptions2 = %q", got)
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
	if !strings.Contains(cfg.Instances[0].BuildDSN(), ":5432/") {
		t.Errorf("端口应缺省为 5432: %s", cfg.Instances[0].BuildDSN())
	}
}

// TestLoadEmptyInstanceItem 回归 issue #6：空列表项应报错而非 panic。
func TestLoadEmptyInstanceItem(t *testing.T) {
	_, err := Load(writeTemp(t, "instances:\n  -\n  - name: ok\n    host: h\n    database: d\n"))
	if err == nil || !strings.Contains(err.Error(), "空项") {
		t.Fatalf("空列表项应报错而非 panic: %v", err)
	}
}

// TestLoadMissingDatabase 覆盖 validate 的“缺少 database”分支。
func TestLoadMissingDatabase(t *testing.T) {
	_, err := Load(writeTemp(t, "instances:\n  - name: a\n    host: h\n"))
	if err == nil || !strings.Contains(err.Error(), "缺少 database") {
		t.Fatalf("缺 database 应报错: %v", err)
	}
}

// TestLoadRejectsUnknownKeys 回归 issue #12：拼错的键应报错而非静默用默认值。
func TestLoadRejectsUnknownKeys(t *testing.T) {
	content := `
server:
  max_row: 5000
instances:
  - name: a
    host: h
    database: d
`
	_, err := Load(writeTemp(t, content))
	if err == nil {
		t.Fatal("未知键应报错")
	}
	if !strings.Contains(err.Error(), "max_row") {
		t.Errorf("错误信息应指出未知键: %v", err)
	}
}

// TestBuildDSNPasswordEscaping 回归 issue #10：密码含空格/@ 时 DSN 不应损坏。
func TestBuildDSNPasswordEscaping(t *testing.T) {
	inst := Instance{
		Host:     "h",
		Port:     5432,
		Database: "db",
		User:     "u",
		Password: "pass word@2026",
	}
	got := inst.BuildDSN()
	if !strings.Contains(got, "pass%20word%402026") {
		t.Errorf("密码应被 URL 转义: %s", got)
	}
	// 生成的 DSN 应能被 URL 解析还原出原密码。
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("DSN 应可解析: %v", err)
	}
	if pw, _ := u.User.Password(); pw != "pass word@2026" {
		t.Errorf("解析出的密码应还原: %q", pw)
	}
}

// TestEnsureSSLModePasswordSubstring 回归 issue #10：密码含 "sslmode=" 子串时
// 不应抑制默认 sslmode 追加。
func TestEnsureSSLModePasswordSubstring(t *testing.T) {
	inst := Instance{
		Host:     "h",
		Port:     5432,
		Database: "db",
		User:     "u",
		Password: "xK3sslmode=9q",
	}
	got := inst.BuildDSN()
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("应追加默认 sslmode=disable: %s", got)
	}
}

// TestDurationUnmarshalRejectsBadNumbers 回归 issue #16：NaN/负数/溢出时长应报错。
func TestDurationUnmarshalRejectsBadNumbers(t *testing.T) {
	for _, in := range []string{"nan", "inf", "-5", "1e18", "-5s", "-1m"} {
		var d Duration
		if err := yaml.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("%q 应报非法时长", in)
		}
	}
	// 溢出边界回归：9223372036.8547758 秒 ×1e9 的 float64 乘积恰等于 2^63，
	// 旧实现（> MaxInt64 判溢出）会放过它并回绕成 MinInt64。
	var overflow Duration
	if err := yaml.Unmarshal([]byte("9223372036.8547758"), &overflow); err == nil {
		t.Errorf("溢出边界的秒数应报非法时长，实际得到 %d", time.Duration(overflow))
	}
	// 合法边界：恰好 MaxInt64 纳秒以内。
	var d Duration
	if err := yaml.Unmarshal([]byte("9223372036"), &d); err != nil { // ≈292 年，秒数 ×1e9 仍在 int64 内
		t.Errorf("大但合法的秒数不应报错: %v", err)
	}
}

// TestLoadEmptyFile 覆盖空/纯注释配置：应给出可操作错误而非裸 EOF。
func TestLoadEmptyFile(t *testing.T) {
	for name, content := range map[string]string{
		"空文件":     "",
		"纯注释":     "# 只有一行注释\n",
		"仅空白":     "\n\n  \n",
	} {
		_, err := Load(writeTemp(t, content))
		if err == nil || !strings.Contains(err.Error(), "配置文件为空") {
			t.Errorf("%s：应报“配置文件为空”，实际: %v", name, err)
		}
	}
}

// TestLoadRejectsOversizedPool 回归 pool_max_conns 溢出：超界值写入 int32
// 池配置前应显式报错，而不是静默回绕。
func TestLoadRejectsOversizedPool(t *testing.T) {
	content := `
instances:
  - name: a
    host: h
    database: d
    pool_max_conns: 4294967300
`
	_, err := Load(writeTemp(t, content))
	if err == nil || !strings.Contains(err.Error(), "pool_max_conns") {
		t.Fatalf("超界 pool_max_conns 应报错: %v", err)
	}
}
