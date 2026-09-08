package guard

import "testing"

func newGuard() *Guard { return New(nil) }

func TestValidateSelectAllowed(t *testing.T) {
	g := newGuard()
	allowed := []string{
		"SELECT 1",
		"select * from users where name = 'admin'",
		"SELECT * FROM t ORDER BY id LIMIT 10;",
		"  with x as (select 1) select * from x",
		"SELECT count(*) FROM orders o JOIN users u ON u.id = o.user_id",
		"SELECT (select max(id) from t) as mx, 2",
		"SELECT * FROM t WHERE comment = 'insert into x'", // 字符串里的关键字不误报
		`SELECT "into" FROM t`,                            // 引号标识符不误报
		"SELECT col1, col2 FROM schema1.tbl WHERE a > $1 AND b < 100",
		"SELECT * FROM t WHERE created_at > now() - interval '7 days'",
		"SELECT array_agg(distinct status) FROM tasks",
		"SELECT CASE WHEN a=1 THEN 'one' ELSE 'other' END FROM t",
		"SELECT 1 -- trailing comment",
		"/* leading comment */ SELECT 1",
		"SELECT /* nested /* deep */ comment */ 1",
		"SELECT E'it\\'s' FROM t",
		"SELECT $$it's got semicolons; inside$$",
		"SELECT $tag$no; semicolons matter$tag$",
		"SELECT * FROM t WHERE id = $1",
		"SELECT u.id FROM users AS u WHERE u.name LIKE 'a%b'",
		"SELECT generate_series(1, 10)",
		"WITH RECURSIVE r AS (SELECT 1 UNION ALL SELECT 1 FROM r) SELECT * FROM r",
	}
	for _, sql := range allowed {
		if err := g.ValidateSelect(sql); err != nil {
			t.Errorf("应当放行 %q: %v", sql, err)
		}
	}
}

func TestValidateSelectBlocked(t *testing.T) {
	g := newGuard()
	cases := []struct {
		sql string
		why string
	}{
		{"", "空语句"},
		{"   ", "空白语句"},
		{"INSERT INTO t VALUES (1)", "直接 INSERT"},
		{"UPDATE t SET a = 1", "直接 UPDATE"},
		{"DELETE FROM t", "直接 DELETE"},
		{"TRUNCATE TABLE t", "TRUNCATE"},
		{"CREATE TABLE x (id int)", "DDL"},
		{"DROP TABLE t", "DROP"},
		{"ALTER TABLE t ADD COLUMN c int", "ALTER"},
		{"GRANT ALL ON t TO PUBLIC", "GRANT"},
		{"COPY t TO STDOUT", "COPY"},
		{"SET statement_timeout = 0", "会话参数"},
		{"RESET statement_timeout", "会话参数"},
		{"SELECT 1; DROP TABLE t", "多语句"},
		{"SELECT 1;DROP TABLE t", "多语句"},
		{"SELECT * INTO new_t FROM t", "SELECT INTO"},
		{"with x as (select 1) select * into new_t from x", "CTE + SELECT INTO"},
		{"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x", "CTE 内 DELETE"},
		{"WITH x AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM x", "CTE 内 INSERT"},
		{"WITH x AS (UPDATE t SET a=1 RETURNING *) SELECT * FROM x", "CTE 内 UPDATE"},
		{"SELECT * FROM t FOR UPDATE", "行锁"},
		{"SELECT * FROM t FOR SHARE", "行锁"},
		{"SELECT * FROM t FOR NO KEY UPDATE", "行锁"},
		{"SELECT * FROM t FOR KEY SHARE", "行锁"},
		{"SELECT dblink('dbname=x', 'select 1')", "dblink"},
		{"SELECT pg_catalog.dblink_exec('conn', 'insert into t values(1)')", "schema 限定 dblink"},
		{"SELECT set_config('search_path', 'evil', false)", "set_config 篡改会话"},
		{"SELECT pg_catalog.set_config('transaction_read_only', 'off', false)", "尝试关闭只读"},
		{"SELECT setval('seq', 100)", "setval"},
		{"SELECT pg_terminate_backend(12345)", "终止后端"},
		{"SELECT pg_read_file('/etc/passwd')", "读文件"},
		{"SELECT lo_import('/tmp/evil')", "大对象写"},
		{"BEGIN; SELECT 1; COMMIT", "事务控制"},
		{"EXPLAIN ANALYZE SELECT 1", "EXPLAIN（analyze 会真实执行）"},
		{"CALL proc(1)", "存储过程调用"},
		{"SELECT 1 -- \n; DROP TABLE t", "注释夹带多语句"},
		{"SELECT 'a'; SELECT 2", "多语句（字符串后）"},
		{"LOCK TABLE t", "显式锁表"},
		{"LISTEN chan", "LISTEN"},
		{"NOTIFY chan", "NOTIFY"},
		{"VACUUM t", "VACUUM"},
	}
	for _, c := range cases {
		if err := g.ValidateSelect(c.sql); err == nil {
			t.Errorf("应当拦截 [%s] %q", c.why, c.sql)
		}
	}
}

func TestValidateSelectUnbalanced(t *testing.T) {
	g := newGuard()
	for _, sql := range []string{
		"SELECT 'unclosed",
		`SELECT "unclosed`,
		"SELECT $tag$unclosed",
		"SELECT /* unclosed",
	} {
		if err := g.ValidateSelect(sql); err == nil {
			t.Errorf("应当拦截未闭合语法: %q", sql)
		}
	}
}

func TestCustomBlockedFunctions(t *testing.T) {
	g := New([]string{"my_write_func", "gs_*"})
	if err := g.ValidateSelect("SELECT my_write_func(1)"); err == nil {
		t.Error("自定义黑名单未生效: my_write_func")
	}
	if err := g.ValidateSelect("SELECT gs_backup('x')"); err == nil {
		t.Error("自定义黑名单通配未生效: gs_backup")
	}
	// 默认黑名单被覆盖后不再拦截
	if err := g.ValidateSelect("SELECT dblink('x','y')"); err != nil {
		t.Errorf("覆盖后不应拦截默认项: %v", err)
	}
}

func TestDefaultBlockedFunctionsExported(t *testing.T) {
	if len(DefaultBlockedFunctions()) == 0 {
		t.Fatal("默认黑名单不应为空")
	}
}
