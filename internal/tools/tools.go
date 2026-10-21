// Package tools 注册并实现 MCP 工具。
package tools

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gaussdb-ro-mcp/internal/db"
)

// instanceArg 是所有工具共享的可选实例参数。
type instanceArg struct {
	// Instance 是数据源名称，省略时使用 default_instance。
	// 该静态描述仅作兜底：Register 会把它动态替换为内嵌实例清单的版本
	// （见 instanceParamDesc），agent 在 tools/list 阶段即可发现可选实例。
	Instance string `json:"instance,omitempty" jsonschema:"数据源名称（配置文件中 instance 的 name），省略时使用默认实例"`
}

// 各工具的入参类型：输入 schema 由这些类型推断（见 schemaFor），instance
// 参数描述在推断后注入实例清单。
type testConnectionIn struct{ instanceArg }

type listSchemasIn struct {
	instanceArg
	IncludeSystem bool `json:"include_system,omitempty" jsonschema:"是否包含系统模式（pg_catalog 等），默认 false"`
}

type listTablesIn struct {
	instanceArg
	Schema        string `json:"schema,omitempty" jsonschema:"限定模式名（schema），省略时扫描全部非系统模式"`
	IncludeSystem bool   `json:"include_system,omitempty" jsonschema:"是否包含系统模式，默认 false"`
}

type describeTableIn struct {
	instanceArg
	Schema string `json:"schema,omitempty" jsonschema:"模式名，可来自 list_schemas 的结果；省略时自动在非系统模式中查找（歧义时报错）"`
	Table  string `json:"table" jsonschema:"表名或视图名（必填），可来自 list_tables 的结果"`
}

type executeSelectIn struct {
	instanceArg
	SQL     string `json:"sql" jsonschema:"要执行的 SELECT 语句（必填），仅允许单条 SELECT/WITH 查询"`
	MaxRows int    `json:"max_rows,omitempty" jsonschema:"本次返回的最大行数，默认取实例配置，上限受服务端限制"`
}

// instanceParamDesc 生成 instance 参数的 schema 描述：内嵌按配置声明顺序的
// 全部实例名并标注默认实例，agent 无需靠"传错实例名看报错"来发现可选值。
func instanceParamDesc(m *db.Manager) string {
	names := m.InstanceNamesList()
	def := m.DefaultInstanceName()
	labeled := make([]string, 0, len(names))
	for _, n := range names {
		if n == def {
			n += "（默认）"
		}
		labeled = append(labeled, n)
	}
	return "数据源名称，省略时使用默认实例。可选实例：" + strings.Join(labeled, "、")
}

// schemaFor 按 In 类型推断输入 schema，并把 instance 参数描述替换为含实例
// 清单的动态版本：jsonschema 描述来自编译期 struct tag，无法内嵌运行期的
// 实例名，故推断后打补丁、显式交给 AddTool（SDK 对非 nil InputSchema 直接
// 采用；除该描述外与自动推断的 schema 逐字段一致，校验语义不变）。
func schemaFor[In any](instanceDesc string) *jsonschema.Schema {
	s, err := jsonschema.ForType(reflect.TypeFor[In](), &jsonschema.ForOptions{})
	if err != nil {
		// In 类型均为编译期已知的普通 struct，推断失败只可能是结构性错误，
		// 与 AddTool 自身的 panic 同样按注册期致命错误处理。
		panic(fmt.Sprintf("推断输入 schema 失败: %v", err))
	}
	if p := s.Properties["instance"]; p != nil {
		p.Description = instanceDesc
	}
	return s
}

// Register 向 MCP 服务器注册全部只读工具。
func Register(s *mcp.Server, m *db.Manager) {
	instDesc := instanceParamDesc(m)

	mcp.AddTool(s, &mcp.Tool{
		Name: "test_connection",
		Description: "测试 GaussDB 数据源连通性，返回服务器版本、当前数据库、用户、只读状态与连接耗时。" +
			"建议在会话开始或其他工具调用前先执行，确认数据源可达且只读事务生效；也用于排查连接、凭据或超时问题。" +
			"不指定 instance 时测试默认实例。",
		InputSchema: schemaFor[testConnectionIn](instDesc),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in testConnectionIn) (*mcp.CallToolResult, any, error) {
		return handleTestConnection(ctx, m, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_schemas",
		Description: "列出 GaussDB 数据库中的 schema（模式）清单，含每个模式下的对象数量与注释，默认排除系统模式。" +
			"适用场景：探查数据库的第一步——先确定业务模式名称，再用 list_tables 查看其中的表。" +
			"include_system 为 true 时包含 pg_catalog、information_schema 等系统模式（一般仅在排查系统对象时需要）。",
		InputSchema: schemaFor[listSchemasIn](instDesc),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listSchemasIn) (*mcp.CallToolResult, any, error) {
		return handleListSchemas(ctx, m, in.Instance, in.IncludeSystem)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_tables",
		Description: "列出 GaussDB 数据库中的表和视图清单：名称、类型（表/视图/物化视图/分区表/外表）、估算行数与注释。" +
			"schema 参数可过滤到指定模式（可来自 list_schemas 的结果），省略时扫描全部非系统模式。" +
			"适用场景：定位目标表后，用 describe_table 查看结构或用 execute_select 抽样查询。大型库建议传 schema 缩小扫描范围。",
		InputSchema: schemaFor[listTablesIn](instDesc),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listTablesIn) (*mcp.CallToolResult, any, error) {
		return handleListTables(ctx, m, in.Instance, in.Schema, in.IncludeSystem)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "describe_table",
		Description: "查看表/视图的完整结构：列清单（类型/可空/默认值/注释）、主键与约束、全部索引及其定义；" +
			"视图返回视图定义 SQL，分区表返回分区清单。" +
			"适用场景：编写或校对查询前了解表结构、关联键与索引设计。table 为必填（可来自 list_tables 的结果）；" +
			"schema 省略时自动在非系统模式中查找，存在同名表时请显式指定 schema。",
		InputSchema: schemaFor[describeTableIn](instDesc),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in describeTableIn) (*mcp.CallToolResult, any, error) {
		return handleDescribeTable(ctx, m, in.Instance, in.Schema, in.Table)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "execute_select",
		Description: "执行只读 SELECT 查询（本服务器唯一允许的 SQL 入口）。仅接受单条 SELECT/WITH 语句：" +
			"拒绝 DML/DDL、SELECT INTO、FOR UPDATE/SHARE 行锁、多语句与危险函数；查询在服务端强制的只读事务中执行。" +
			"适用场景：在 list_tables/describe_table 定位目标后做数据探查与统计（计数、聚合、抽样、排序取前 N 行）。" +
			"max_rows 控制本次返回行数（默认取实例配置，受服务端硬上限约束）；大结果集建议用 LIMIT 或聚合减少传输。",
		InputSchema: schemaFor[executeSelectIn](instDesc),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in executeSelectIn) (*mcp.CallToolResult, any, error) {
		return handleExecuteSelect(ctx, m, in.Instance, in.SQL, in.MaxRows)
	})
}

// handleTestConnection 连通性测试。
func handleTestConnection(ctx context.Context, m *db.Manager, in testConnectionIn) (*mcp.CallToolResult, any, error) {
	inst, err := m.Resolve(in.Instance)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	start := time.Now()
	info, err := inst.ServerInfo(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("实例 %q 连接失败: %w", inst.Name, err)
	}
	latency := time.Since(start).Milliseconds()
	// ReadOnlyStatus 在只读事务内回读并 fail-closed 校验：
	// 失败时返回错误（此时不提供任何结果），成功时必为 on。
	ro, roErr := inst.ReadOnlyStatus(ctx)
	if roErr != nil {
		return nil, nil, fmt.Errorf("实例 %q 只读事务校验失败: %w", inst.Name, roErr)
	}
	out := map[string]any{
		"ok":                    true,
		"instance":              inst.Name,
		"server_version":        info["version"],
		"database":              info["database"],
		"user":                  info["user"],
		"server_start_time":     info["server_start_time"],
		"latency_ms":            latency,
		"transaction_read_only": ro,
	}
	return nil, out, nil
}

func handleListSchemas(ctx context.Context, m *db.Manager, name string, includeSystem bool) (*mcp.CallToolResult, any, error) {
	inst, err := m.Resolve(name)
	if err != nil {
		return nil, nil, err
	}
	rows, err := inst.ListSchemas(ctx, includeSystem)
	if err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"instance": inst.Name, "count": len(rows), "schemas": rows}, nil
}

func handleListTables(ctx context.Context, m *db.Manager, name, schema string, includeSystem bool) (*mcp.CallToolResult, any, error) {
	inst, err := m.Resolve(name)
	if err != nil {
		return nil, nil, err
	}
	out, err := inst.ListTables(ctx, schema, includeSystem)
	if err != nil {
		return nil, nil, err
	}
	out["instance"] = inst.Name
	return nil, out, nil
}

func handleDescribeTable(ctx context.Context, m *db.Manager, name, schema, table string) (*mcp.CallToolResult, any, error) {
	if table == "" {
		return nil, nil, fmt.Errorf("table 参数不能为空")
	}
	inst, err := m.Resolve(name)
	if err != nil {
		return nil, nil, err
	}
	oid, ns, tbl, kind, err := inst.ResolveTable(ctx, schema, table)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{
		"instance": inst.Name,
		"schema":   ns,
		"table":    tbl,
		"kind":     kind,
	}
	// 各段尽力而为：失败时输出 <段名>_error 键，避免"无数据"与"查询失败"
	// 在结果里不可区分。
	if comment, est, err := inst.TableComment(ctx, oid); err == nil {
		out["comment"] = comment
		out["estimated_rows"] = est
	} else {
		out["comment_error"] = err.Error()
	}
	if cols, err := inst.DescribeColumns(ctx, oid); err == nil {
		out["columns"] = cols
	} else {
		out["columns_error"] = err.Error()
	}
	if cons, err := inst.DescribeConstraints(ctx, oid); err == nil {
		out["constraints"] = cons
	} else {
		out["constraints_error"] = err.Error()
	}
	if idxs, err := inst.DescribeIndexes(ctx, oid); err == nil {
		out["indexes"] = idxs
	} else {
		out["indexes_error"] = err.Error()
	}
	if kind == "view" || kind == "materialized view" {
		if viewdef, err := inst.ViewDefinition(ctx, oid); err == nil {
			out["view_definition"] = viewdef
		} else {
			out["view_definition_error"] = err.Error()
		}
	}
	// 分区信息：GaussDB/openGauss 专属，尽力而为。
	if parts, err := inst.ListPartitions(ctx, oid); err == nil && len(parts) > 0 {
		out["partitions"] = parts
	} else if err != nil {
		out["partitions_error"] = err.Error()
	}
	return nil, out, nil
}

func handleExecuteSelect(ctx context.Context, m *db.Manager, name, sql string, maxRows int) (*mcp.CallToolResult, any, error) {
	inst, err := m.Resolve(name)
	if err != nil {
		return nil, nil, err
	}
	// 第一层防护：静态 SQL 校验。
	if err := m.Guard().ValidateSelect(sql); err != nil {
		return nil, nil, err
	}
	if maxRows <= 0 {
		maxRows = inst.MaxRows
	}
	if maxRows > inst.MaxRowsCap {
		maxRows = inst.MaxRowsCap
	}
	ctx, cancel := context.WithTimeout(ctx, inst.StatementTimeout+5*time.Second)
	defer cancel()

	res, err := inst.Select(ctx, sql, maxRows)
	if err != nil {
		return nil, nil, fmt.Errorf("查询执行失败: %w", err)
	}
	out := map[string]any{
		"instance":    inst.Name,
		"columns":     res.Columns,
		"rows":        res.Rows,
		"row_count":   res.RowCount,
		"truncated":   res.Truncated,
		"duration_ms": res.DurationMS,
	}
	if res.Warning != "" {
		// 结果完整、仅收尾（提交）失败：附警告而非丢弃已取回的数据。
		out["warning"] = res.Warning
	}
	return nil, out, nil
}
