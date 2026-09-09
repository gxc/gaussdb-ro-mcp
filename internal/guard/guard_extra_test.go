package guard

import (
	"strings"
	"testing"
)

// TestTokenHelpers 覆盖词元辅助函数的全部分支。
func TestTokenHelpers(t *testing.T) {
	if isWordToken("") {
		t.Error("空 token 不是词")
	}
	if isWordToken("0") {
		t.Error("占位符不是词")
	}
	if isWordToken("(") {
		t.Error("括号不是词")
	}
	for _, tok := range []string{"_a", "abc", "ABC"} {
		if !isWordToken(tok) {
			t.Errorf("%q 应是词", tok)
		}
	}
	if got := tokenTail("pg_catalog.dblink"); got != "dblink" {
		t.Errorf("tokenTail 应取尾段: %q", got)
	}
	if got := tokenTail("setval"); got != "setval" {
		t.Errorf("无点 token 应原样返回: %q", got)
	}
}

// TestIsValidDollarTag 覆盖 dollar-quote 标签的合法性判断。
func TestIsValidDollarTag(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"", true},
		{"a", true},
		{"_a1", true},
		{"a9_", true},
		{"1a", false},  // 数字开头是位置参数
		{"-a", false},  // 非法首字符
		{"a-b", false}, // 含非法字符
	}
	for _, c := range cases {
		if got := isValidDollarTag(c.tag); got != c.want {
			t.Errorf("isValidDollarTag(%q) = %v, want %v", c.tag, got, c.want)
		}
	}
}

// TestSqlHeadText 覆盖错误信息里的语句头截断。
func TestSqlHeadText(t *testing.T) {
	long := "DELETE FROM " + strings.Repeat("a", 60)
	got := sqlHeadText(long)
	if !strings.HasSuffix(got, "...") || len(got) != 43 {
		t.Errorf("长语句应截断为 40 字符加省略号: %q", got)
	}
	if got := sqlHeadText("(  DELETE x"); got != "DELETE x" {
		t.Errorf("应跳过左括号与空白: %q", got)
	}
}

// TestValidateSelectLexicalErrors 覆盖词法分析的各错误与边界分支。
func TestValidateSelectLexicalErrors(t *testing.T) {
	g := newGuard()
	errors := []struct {
		sql string
		why string
	}{
		{"SELECT /* 未闭合", "未闭合的块注释"},
		{"SELECT '未闭合", "未闭合的字符串字面量"},
		{`SELECT "未闭合`, "未闭合的引号标识符"},
		{"SELECT $$abc", "未闭合的 dollar-quoted 字符串"},
		{"SELECT 1; DROP TABLE t", "多语句"},
		{"SELECT 1; 1", "分号后有内容"},
		{"SELECT 1; /* 未闭合", "分号后注释未闭合"},
		{"(((", "只有括号"},
	}
	for _, c := range errors {
		if err := g.ValidateSelect(c.sql); err == nil {
			t.Errorf("[%s] 应当报错: %q", c.why, c.sql)
		}
	}

	allowed := []string{
		"SELECT 1; -- 分号后仅注释",
		"SELECT 1; /* 分号后仅块注释 */",
		"SELECT $a1$ body ; with $nested$ inside $a1$",
		"SELECT $1a$",     // 非法 tag 按位置参数处理，不报错
		"SELECT 值 FROM 表", // UTF-8 标识符
		"SELECT 1e5, 1e-2, 1+1, 1-2",
		"SELECT U&'x\\y'",
		"SELECT 1 (SELECT 2)",
	}
	for _, sql := range allowed {
		if err := g.ValidateSelect(sql); err != nil {
			t.Errorf("应当放行 %q: %v", sql, err)
		}
	}
}

// TestValidateSelectMoreBranches 覆盖语句结构校验的剩余分支。
func TestValidateSelectMoreBranches(t *testing.T) {
	g := newGuard()

	if err := g.ValidateSelect("   \t\n "); err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("空白 SQL 应报为空: %v", err)
	}
	if err := g.ValidateSelect("((("); err == nil || !strings.Contains(err.Error(), "未找到有效") {
		t.Errorf("只有括号应报未找到查询: %v", err)
	}
	// 锁子句的各种形态。
	for _, sql := range []string{
		"SELECT * FROM t FOR UPDATE",
		"SELECT * FROM t FOR SHARE",
		"SELECT * FROM t FOR NO KEY UPDATE",
		"SELECT * FROM t FOR KEY SHARE",
		"SELECT * FROM t INTO new_t",
		"WITH x AS (DELETE FROM t) SELECT * FROM x",
		"SELECT set_config('a', 'b', true)",
		"SELECT pg_catalog.nextval('s')",
	} {
		if err := g.ValidateSelect(sql); err == nil {
			t.Errorf("应拦截: %q", sql)
		}
	}
	// 自定义黑名单（覆盖 New 的覆盖分支与通配/精确匹配分支）。
	g2 := New([]string{"my_*", "evil"})
	if err := g2.ValidateSelect("SELECT my_func(1)"); err == nil {
		t.Error("通配黑名单应命中 my_func")
	}
	if err := g2.ValidateSelect("SELECT evil(1)"); err == nil {
		t.Error("精确黑名单应命中 evil")
	}
	if err := g2.ValidateSelect("SELECT set_config('a','b',true)"); err != nil {
		t.Errorf("自定义黑名单替换默认后 set_config 不应被拦截: %v", err)
	}
	if len(DefaultBlockedFunctions()) == 0 {
		t.Error("默认黑名单不应为空")
	}
}
