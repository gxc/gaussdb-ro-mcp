// Package db — meta.go：schema/表/列/索引/视图定义等元数据查询。
// 全部使用 pg_catalog 固定查询，天然只读，兼容 openGauss / GaussDB 与 PostgreSQL。
package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// 系统模式清单与模式匹配规则：默认从结果中排除。
// LIKE 转义使用 ESCAPE '$'，避免服务端 standard_conforming_strings 取值影响匹配。
const systemSchemaFilter = `n.nspname NOT IN ('pg_catalog','information_schema','pg_toast','snapshot',
	'cstore','db4ai','sqladvisor','pkg_service','pkg_util','dbe_perf','dbe_pldebugger','dbe_pldeveloper','dbe_sql_util')
	AND n.nspname NOT LIKE 'pg$_%' ESCAPE '$' AND n.nspname NOT LIKE 'dbe$_%' ESCAPE '$'`

// relKindBase 是仅按 relkind 判断对象类型的基础表达式（PostgreSQL 兼容）。
const relKindBase = `CASE c.relkind
	WHEN 'r' THEN 'table'
	WHEN 'v' THEN 'view'
	WHEN 'm' THEN 'materialized view'
	WHEN 'f' THEN 'foreign table'
	WHEN 'p' THEN 'partitioned table'
	ELSE c.relkind::text END`

// relKindExpr 返回对象类型表达式；openGauss/GaussDB 模式下
// 分区表以 pg_class.parttype='p' 标识（relkind 仍为 'r'）。
func relKindExpr(partitionMode bool) string {
	if !partitionMode {
		return relKindBase
	}
	return `CASE WHEN c.parttype = 'p' THEN 'partitioned table' ELSE ` + relKindBase + ` END`
}

// detectPartitionSupport 判断服务端是否为 openGauss/GaussDB
// （pg_class 含 parttype 列，原生 PostgreSQL 没有）。
func (inst *Instance) detectPartitionSupport(ctx context.Context) bool {
	inst.partitionOnce.Do(func() {
		rows, err := inst.Query(ctx, `SELECT count(*) AS n FROM pg_catalog.pg_attribute
			WHERE attrelid = 'pg_catalog.pg_class'::regclass AND attname = 'parttype'`)
		if err == nil && len(rows) > 0 {
			if n, ok := rows[0]["n"].(int64); ok && n > 0 {
				inst.partitionMode = true
			}
		}
	})
	return inst.partitionMode
}

// ListSchemas 返回模式清单及其对象数与注释。
func (inst *Instance) ListSchemas(ctx context.Context, includeSystem bool) ([]map[string]any, error) {
	filter := systemSchemaFilter
	if includeSystem {
		filter = "TRUE"
	}
	sql := fmt.Sprintf(`SELECT n.nspname AS schema_name,
		pg_catalog.obj_description(n.oid, 'pg_namespace') AS comment,
		(SELECT count(*) FROM pg_catalog.pg_class c
		 WHERE c.relnamespace = n.oid AND c.relkind IN ('r','v','m','f','p')) AS relations
	FROM pg_catalog.pg_namespace n
	WHERE %s
	ORDER BY 1`, filter)
	return inst.Query(ctx, sql)
}

// ListTables 返回表/视图清单。schema 为空时扫描全部非系统模式。
func (inst *Instance) ListTables(ctx context.Context, schema string, includeSystem bool) (map[string]any, error) {
	var cond string
	var args []any
	switch {
	case schema != "":
		cond = "n.nspname = $1"
		args = append(args, schema)
	case includeSystem:
		cond = "TRUE"
	default:
		cond = systemSchemaFilter
	}
	// 总数与清单一次查询；清单硬上限 5000 行防止超大库刷屏。
	sql := fmt.Sprintf(`SELECT n.nspname AS schema_name, c.relname AS table_name,
		%s AS kind,
		c.reltuples::bigint AS estimated_rows,
		pg_catalog.obj_description(c.oid) AS comment
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r','v','m','f','p') AND %s
	ORDER BY 1, 2`, relKindExpr(inst.detectPartitionSupport(ctx)), cond)

	rows, err := inst.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	const limit = 5000
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	return map[string]any{
		"tables":    rows,
		"count":     len(rows),
		"truncated": truncated,
	}, nil
}

// ResolveTable 按可选 schema + 表名定位对象，返回 (oid, schema, name, kind)。
// 未指定 schema 时在非系统模式内搜索：当前模式优先；命中多个则报歧义。
func (inst *Instance) ResolveTable(ctx context.Context, schema, table string) (oid int64, ns, name, kind string, err error) {
	kindExpr := relKindExpr(inst.detectPartitionSupport(ctx))
	if schema != "" {
		rows, err := inst.Query(ctx, fmt.Sprintf(`SELECT c.oid, n.nspname AS schema_name, c.relname AS table_name, %s AS kind
			FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2`, kindExpr), schema, table)
		if err != nil {
			return 0, "", "", "", err
		}
		if len(rows) == 0 {
			return 0, "", "", "", fmt.Errorf("表 %q.%q 不存在", schema, table)
		}
		r := rows[0]
		return asInt64(r["oid"]), asString(r["schema_name"]), asString(r["table_name"]), asString(r["kind"]), nil
	}
	rows, err := inst.Query(ctx, fmt.Sprintf(`SELECT c.oid, n.nspname AS schema_name, c.relname AS table_name, %s AS kind,
			(n.nspname = current_schema()) AS in_current
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = $1 AND %s
		ORDER BY in_current DESC, n.nspname LIMIT 2`, kindExpr, systemSchemaFilter), table)
	if err != nil {
		return 0, "", "", "", err
	}
	if len(rows) == 0 {
		return 0, "", "", "", fmt.Errorf("表 %q 在非系统模式下不存在", table)
	}
	if len(rows) > 1 {
		return 0, "", "", "", fmt.Errorf("表名 %q 在多个模式下存在（如 %s、%s），请指定 schema 参数",
			table, rows[0]["schema_name"], rows[1]["schema_name"])
	}
	r := rows[0]
	return asInt64(r["oid"]), asString(r["schema_name"]), asString(r["table_name"]), asString(r["kind"]), nil
}

// DescribeColumns 返回列清单（含类型/可空/默认值/注释/序号）。
func (inst *Instance) DescribeColumns(ctx context.Context, oid int64) ([]map[string]any, error) {
	return inst.Query(ctx, `SELECT a.attname AS column_name,
		pg_catalog.format_type(a.atttypid, a.atttypmod) AS data_type,
		NOT a.attnotnull AS nullable,
		pg_catalog.pg_get_expr(ad.adbin, ad.adrelid) AS default_value,
		pg_catalog.col_description(a.attrelid, a.attnum) AS comment,
		a.attnum AS position
	FROM pg_catalog.pg_attribute a
	LEFT JOIN pg_catalog.pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
	WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
	ORDER BY a.attnum`, oid)
}

// DescribeConstraints 返回主键/外键/唯一等约束定义。
func (inst *Instance) DescribeConstraints(ctx context.Context, oid int64) ([]map[string]any, error) {
	return inst.Query(ctx, `SELECT conname AS constraint_name,
		CASE contype WHEN 'p' THEN 'primary key' WHEN 'f' THEN 'foreign key'
			WHEN 'u' THEN 'unique' WHEN 'c' THEN 'check' ELSE contype::text END AS constraint_type,
		pg_catalog.pg_get_constraintdef(oid) AS definition
	FROM pg_catalog.pg_constraint
	WHERE conrelid = $1
	ORDER BY contype, conname`, oid)
}

// DescribeIndexes 返回索引清单及完整定义。
func (inst *Instance) DescribeIndexes(ctx context.Context, oid int64) ([]map[string]any, error) {
	return inst.Query(ctx, `SELECT idx.relname AS index_name,
		pg_catalog.pg_get_indexdef(ix.indexrelid) AS definition,
		ix.indisprimary AS is_primary,
		ix.indisunique AS is_unique,
		ix.indisvalid AS is_valid
	FROM pg_catalog.pg_index ix
	JOIN pg_catalog.pg_class c ON c.oid = ix.indrelid
	JOIN pg_catalog.pg_class idx ON idx.oid = ix.indexrelid
	WHERE c.oid = $1
	ORDER BY ix.indisprimary DESC, idx.relname`, oid)
}

// ViewDefinition 返回视图/物化视图定义（pretty 格式）。
func (inst *Instance) ViewDefinition(ctx context.Context, oid int64) (string, error) {
	rows, err := inst.Query(ctx, "SELECT pg_catalog.pg_get_viewdef($1::oid, true) AS viewdef", oid)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return asString(rows[0]["viewdef"]), nil
}

// TableComment 返回表级注释与估算行数。
func (inst *Instance) TableComment(ctx context.Context, oid int64) (comment string, estRows int64, err error) {
	rows, err := inst.Query(ctx, `SELECT pg_catalog.obj_description(c.oid) AS comment, c.reltuples::bigint AS est_rows
		FROM pg_catalog.pg_class c WHERE c.oid = $1`, oid)
	if err != nil || len(rows) == 0 {
		return "", 0, err
	}
	return asString(rows[0]["comment"]), asInt64(rows[0]["est_rows"]), nil
}

// ListPartitions 返回分区表的分区清单。
// openGauss/GaussDB：查 pg_partition（分区行 parttype='p'，boundaries 为 text[]）。
// 原生 PostgreSQL 没有 pg_partition，返回 nil。
func (inst *Instance) ListPartitions(ctx context.Context, oid int64) ([]map[string]any, error) {
	if !inst.detectPartitionSupport(ctx) {
		return nil, nil
	}
	sql := `SELECT relname AS partition_name, partstrategy::text AS part_strategy, boundaries
		FROM pg_catalog.pg_partition WHERE parentid = $1 AND parttype = 'p'
		ORDER BY relname`
	return inst.Query(ctx, sql, oid)
}

// ServerInfo 返回连通性信息。
func (inst *Instance) ServerInfo(ctx context.Context) (map[string]any, error) {
	rows, err := inst.Query(ctx, `SELECT version() AS version,
		current_database() AS database, current_user AS "user",
		pg_catalog.pg_postmaster_start_time() AS server_start_time`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("空结果")
	}
	return rows[0], nil
}

// ReadOnlyStatus 返回当前会话只读状态（应恒为 on）。
func (inst *Instance) ReadOnlyStatus(ctx context.Context) (string, error) {
	rows, err := inst.Query(ctx, "SHOW transaction_read_only")
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("空结果")
	}
	return asString(rows[0]["transaction_read_only"]), nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case uint64:
		return int64(t)
	case int32:
		return int64(t)
	case uint32:
		return int64(t)
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		return 0
	}
}

// QuoteIdent 以双引号安全包裹标识符。
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
