// Package guard 实现只读防护的第一层：SQL 语句静态校验。
//
// 防护分多层（纵深防御）：
//  1. 本包：仅放行单条 SELECT/WITH 查询；拦截 CTE 内 DML、SELECT ... INTO、
//     FOR UPDATE/SHARE 锁子句、多语句、以及危险函数调用（dblink、set_config 等）。
//  2. 事务层（internal/db）：所有查询在显式只读事务（SET LOCAL TRANSACTION
//     READ ONLY）中执行，由服务端兜底拒绝事务内一切写操作。
//  3. 部署层（README）：建议使用仅授予 SELECT 权限的数据库账号。
//
// 词法分析会跳过字符串字面量、注释与引号标识符，因此拦截只针对
// 语句结构本身，不会对包含敏感单词的字符串值产生误报。
package guard

import (
	"fmt"
	"strings"
)

// 默认危险函数黑名单（小写，尾段匹配，支持 * 前缀通配）。
// 覆盖：跨库执行（dblink）、会话配置篡改（set_config）、序列推进（nextval/setval）、
// 大对象写、文件读取、后台终止/取消、复制槽与日志/备份等管理操作。
var defaultBlockedFunctions = []string{
	"set_config", "setval", "nextval",
	"dblink*",
	"lo_import*", "lo_export", "lo_creat", "lo_create", "lo_put", "lo_unlink", "lo_truncate",
	"pg_read_file*", "pg_read_binary_file*", "pg_ls_dir", "pg_stat_file",
	"pg_terminate_backend", "pg_cancel_backend",
	"pg_drop_replication_slot", "pg_create_physical_replication_slot", "pg_create_logical_replication_slot",
	"pg_replication_origin_*", "pg_replication_slot_advance",
	"pg_stat_reset*",
	"pg_advisory*", "pg_try_advisory*", // 会话级咨询锁在只读事务中合法，必须整体拦截（含 pg_try_advisory_* / *_shared）
	"pg_rotate_logfile", "pg_reload_conf", "pg_switch_wal",
	"pg_wal_replay_pause", "pg_wal_replay_resume",
	"pg_backup_start", "pg_backup_stop",
}

// 只允许作为查询词出现的 DML 关键字（含 CTE 内 DML）。
// 全部为保留字，不会与列名/表名冲突。
// 其余危险语句（CREATE/COPY/SET/...）只能出现在语句开头，
// 已被"首词必须是 SELECT/WITH"拦截，不再词级匹配以降低误报。
var dmlKeywords = map[string]bool{
	"insert": true, "update": true, "delete": true, "merge": true,
}

// ValidateSelect 校验 sql 是否为一条安全的只读 SELECT 查询。
func (g *Guard) ValidateSelect(sql string) error {
	toks, err := tokenize(sql)
	if err != nil {
		return err
	}
	if len(toks) == 0 {
		return fmt.Errorf("SQL 为空")
	}

	// 跳过开头的左括号，找到第一个实质词。
	i := 0
	for i < len(toks) && toks[i] == "(" {
		i++
	}
	if i >= len(toks) {
		return fmt.Errorf("SQL 中未找到有效的查询语句")
	}
	head := toks[i]
	if head != "select" && head != "with" {
		return fmt.Errorf("只允许执行 SELECT 查询，当前语句以 %q 开头", sqlHeadText(sql))
	}

	for j := i; j < len(toks); j++ {
		t := toks[j]
		if dmlKeywords[t] {
			return fmt.Errorf("检测到写操作关键字 %q：只读模式禁止 DML（含 CTE 内的写语句）", strings.ToUpper(t))
		}
		// SELECT ... INTO：读语句形态的写操作（建表/临时表）。
		if t == "into" {
			return fmt.Errorf(`检测到 "SELECT ... INTO"：只读模式禁止通过查询创建表`)
		}
		// FOR UPDATE / FOR SHARE / FOR NO KEY UPDATE ...：锁子句。
		if t == "for" && j+1 < len(toks) {
			switch toks[j+1] {
			case "update", "share", "no", "key":
				return fmt.Errorf("检测到行锁定子句 FOR %s：只读模式禁止锁行", strings.ToUpper(toks[j+1]))
			}
		}
		// 函数调用黑名单：word( 形式（含引号标识符 "fn"( )。
		// 引号标识符需剥去 \x00 前缀后再比对，否则 "dblink"( 可绕过黑名单；
		// 同时回溯拼接 "." 连接的限定全名（如 pg_catalog.dblink）。
		if j+1 < len(toks) && toks[j+1] == "(" {
			name := strings.TrimPrefix(t, "\x00")
			if isWordToken(t) || name != t {
				tail := tokenTail(name)
				full := buildFullName(toks, j, name)
				if g.isBlockedFunction(full, tail) {
					return fmt.Errorf("函数 %q 在只读模式下被禁止调用", tail)
				}
			}
		}
	}
	return nil
}

// Guard 是可配置的只读校验器。
type Guard struct {
	blockedFunctions []string
}

// New 创建 Guard；blockedFunctions 非空时覆盖默认黑名单。
func New(blockedFunctions []string) *Guard {
	if len(blockedFunctions) == 0 {
		blockedFunctions = defaultBlockedFunctions
	}
	lowered := make([]string, len(blockedFunctions))
	for i, f := range blockedFunctions {
		lowered[i] = strings.ToLower(f)
	}
	return &Guard{blockedFunctions: lowered}
}

// DefaultBlockedFunctions 返回默认黑名单（供文档展示）。
func DefaultBlockedFunctions() []string {
	out := make([]string, len(defaultBlockedFunctions))
	copy(out, defaultBlockedFunctions)
	return out
}

func (g *Guard) isBlockedFunction(full, tail string) bool {
	for _, pat := range g.blockedFunctions {
		// 含 schema 限定的条目（如 pg_catalog.dblink）按全名匹配，其余按尾段匹配。
		name := tail
		if strings.Contains(pat, ".") {
			name = full
		}
		if strings.HasSuffix(pat, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(pat, "*")) {
				return true
			}
			continue
		}
		if name == pat {
			return true
		}
	}
	return false
}

// isWordToken 判断 token 是否为标识符/关键字（而非括号或占位符）。
func isWordToken(tok string) bool {
	if tok == "" {
		return false
	}
	c := tok[0]
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// tokenTail 取 "schema.func" 形式的最后一段。
func tokenTail(tok string) string {
	if idx := strings.LastIndexByte(tok, '.'); idx >= 0 {
		return tok[idx+1:]
	}
	return tok
}

// tokenize 将 SQL 归一化为小写 token 流：
//   - 字符串字面量（'...'、E'...'、$tag$...$tag$）→ 占位符 "0"
//   - 引号标识符（"..."）→ 内容保留为 token（作为标识符，不参与关键字匹配）
//   - 注释（-- 与嵌套 /* */）→ 丢弃
//   - 标识符保留 "schema.table" 中的点，其余标点仅保留 "(" ")"
//
// 返回的流中若仍含 ";" 即多语句。
func tokenize(sql string) ([]string, error) {
	var toks []string
	s := sql
	n := len(s)
	i := 0
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++

		case c == '-' && i+1 < n && s[i+1] == '-': // 行注释
			for i < n && s[i] != '\n' {
				i++
			}

		case c == '/' && i+1 < n && s[i+1] == '*': // 嵌套块注释
			depth := 1
			i += 2
			for i < n && depth > 0 {
				if i+1 < n && s[i] == '/' && s[i+1] == '*' {
					depth++
					i += 2
				} else if i+1 < n && s[i] == '*' && s[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth > 0 {
				return nil, fmt.Errorf("SQL 存在未闭合的块注释")
			}

		case c == '\'': // 字符串字面量，'' 为转义；紧邻的 E''/U&'' 前缀中 \' 亦为转义
			backslash := escapePrefixAt(s, i)
			i++
			closed := false
			for i < n {
				if backslash && s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '\'' {
					if i+1 < n && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("SQL 存在未闭合的字符串字面量")
			}
			toks = append(toks, "0")

		case c == '"': // 引号标识符，"" 为转义；加前缀使其不参与关键字匹配
			if escapePrefixAt(s, i) {
				// U&"…" 的 Unicode 转义解码超出词法职责，且可用于伪造标识符
				// 绕过黑名单（如 U&"set_\0063onfig" → set_config），保守拒绝。
				return nil, fmt.Errorf(`SQL 含 U&"…" Unicode 转义标识符，只读模式拒绝执行`)
			}
			i++
			var sb strings.Builder
			closed := false
			for i < n {
				if s[i] == '"' {
					if i+1 < n && s[i+1] == '"' {
						sb.WriteByte('"')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				sb.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("SQL 存在未闭合的引号标识符")
			}
			toks = append(toks, "\x00"+strings.ToLower(sb.String()))

		case c == '$': // 位置参数 $1 或 dollar-quoted 字符串 $tag$...$tag$
			j := i + 1
			for j < n && isTagChar(s[j]) { // tag 字符不含 $，避免吞掉界定符
				j++
			}
			if j < n && s[j] == '$' && isValidDollarTag(s[i+1:j]) { // $tag$
				tag := s[i : j+1]
				end := strings.Index(s[j+1:], tag)
				if end < 0 {
					return nil, fmt.Errorf("SQL 存在未闭合的 dollar-quoted 字符串")
				}
				i = j + 1 + end + len(tag)
				toks = append(toks, "0")
			} else { // 位置参数 $1
				i = j
			}

		case isIdentStart(c): // 标识符/关键字（含 UTF-8 高位字节与 $）
			j := i
			for j < n && isIdentChar(s[j]) {
				j++
			}
			toks = append(toks, strings.ToLower(s[i:j]))
			i = j

		case c >= '0' && c <= '9': // 数字字面量
			j := i
			for j < n && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' ||
				s[j] == 'e' || s[j] == 'E' || s[j] == '+' || s[j] == '-') {
				// 保守：e/E/-/+ 仅在数字上下文里是指数部分；简单吞掉连续数字字符
				if s[j] == '+' || s[j] == '-' || s[j] == 'e' || s[j] == 'E' {
					// 仅当紧跟数字时并入，避免把 "1-" 后的词吃进去
					if j+1 >= n || s[j+1] < '0' || s[j+1] > '9' {
						break
					}
				}
				j++
			}
			toks = append(toks, "0")
			i = j

		default: // 其余标点：仅保留括号与点（点用于回溯 schema 限定的函数全名）
			if c == '(' || c == ')' || c == '.' {
				toks = append(toks, string(c))
			}
			if c == ';' {
				// 单语句约束：分号之后仅允许空白与注释。
				i++
				for i < n {
					switch {
					case s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f' || s[i] == '\v':
						i++
					case s[i] == '-' && i+1 < n && s[i+1] == '-':
						for i < n && s[i] != '\n' {
							i++
						}
					case s[i] == '/' && i+1 < n && s[i+1] == '*':
						depth := 1
						i += 2
						for i < n && depth > 0 {
							if i+1 < n && s[i] == '/' && s[i+1] == '*' {
								depth++
								i += 2
							} else if i+1 < n && s[i] == '*' && s[i+1] == '/' {
								depth--
								i += 2
							} else {
								i++
							}
						}
						if depth > 0 {
							return nil, fmt.Errorf("SQL 存在未闭合的块注释")
						}
					default:
						return nil, fmt.Errorf("仅允许单条语句，禁止多语句执行")
					}
				}
			}
			i++
		}
	}
	return toks, nil
}

// escapePrefixAt 判断 s[i] 处的引号是否紧邻 E / U& 前缀（E”、U&” 转义字符串语法）。
// 前缀必须与引号逐字符相邻：列名/别名等标识符（如 "SELECT e, '\' FROM t" 中的 e）
// 与引号之间隔着标点或空白时不得启用转义语义，否则 guard 与服务端对字符串边界的
// 认定会错位，可被用于把危险函数调用藏进 guard 认定的"字符串"里。
func escapePrefixAt(s string, i int) bool {
	if i <= 0 {
		return false
	}
	switch s[i-1] {
	case 'e', 'E':
		return i-1 == 0 || !isIdentChar(s[i-2])
	case '&':
		if i < 2 {
			return false
		}
		switch s[i-2] {
		case 'u', 'U':
			return i-2 == 0 || !isIdentChar(s[i-3])
		}
	}
	return false
}

// isIdentStart 判断标识符起始字符。
// buildFullName 从 toks[j] 向前回溯，把 "." 与标识符/引号标识符连接成限定全名
// （如 pg_catalog.dblink）。遇到其他 token（关键字、括号等）即停止。
func buildFullName(toks []string, j int, name string) string {
	full := name
	k := j - 1
	for k >= 1 && toks[k] == "." && k-1 >= 0 {
		prev := strings.TrimPrefix(toks[k-1], "\x00")
		if prev == "" || (!isWordToken(toks[k-1]) && !strings.HasPrefix(toks[k-1], "\x00")) {
			break
		}
		full = prev + "." + full
		k -= 2
	}
	return full
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// isValidDollarTag：$tag$ 的 tag 为空或以字母/下划线开头（排除 $1$ 误判）。
func isValidDollarTag(tag string) bool {
	if tag == "" {
		return true
	}
	if !(tag[0] == '_' || tag[0] >= 'a' && tag[0] <= 'z' || tag[0] >= 'A' && tag[0] <= 'Z') {
		return false
	}
	for i := 0; i < len(tag); i++ {
		if !isTagChar(tag[i]) {
			return false
		}
	}
	return true
}

// isTagChar 是 dollar-quote tag 的合法字符：字母/数字/下划线（不含 $）。
func isTagChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
}

// sqlHeadText 提取语句开头片段用于错误信息展示。
func sqlHeadText(sql string) string {
	trimmed := strings.TrimSpace(strings.TrimLeft(sql, "( \t\r\n"))
	if len(trimmed) > 40 {
		trimmed = trimmed[:40] + "..."
	}
	return trimmed
}
