# gaussdb-ro-mcp

面向编码代理（Claude Code、OpenCode 等）的 **GaussDB 只读 MCP 服务器**。基于
[GaussDB 官方 Go 驱动](https://github.com/huaweicloud-samples/database-gaussdb-go)
（华为云官方开源的 pgx v5 适配版，源码随仓库内置于 `third_party/gaussdb-go`，支持离线构建），
通过 stdio 传输提供数据库只读探查工具。

专为**内网非 SSL 环境**设计：默认 `sslmode=disable`，配置文件支持同时管理多个 GaussDB 实例。

## 只读保障（三层纵深防御）

| 层级 | 机制 | 说明 |
| --- | --- | --- |
| 1. SQL 静态校验 | `internal/guard` | 仅放行**单条** `SELECT`/`WITH` 查询：拒绝 DML/DDL（含 CTE 内写语句）、`SELECT ... INTO`、`FOR UPDATE/SHARE` 行锁、多语句、危险函数（`dblink*`、`set_config`、`setval`、`pg_read_file*`、`pg_terminate_backend`、`pg_advisory_*`、大对象写等，可配置）。词法分析正确跳过字符串/注释/引号标识符，避免误报 |
| 2. 会话级强制 | `internal/db` | 每条连接入池前执行 `SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY` + `SET statement_timeout`，并回读 `SHOW transaction_read_only` 验证为 `on`，否则拒绝入池。即使第 1 层被绕过，服务端也会拒绝一切写入（包括函数内部的写） |
| 3. 部署建议 | README | 建议使用仅授予 `SELECT` 权限的数据库账号（见下文），实现权限最小化 |

## 提供的 MCP 工具

| 工具 | 功能 |
| --- | --- |
| `test_connection` | 连通性测试：服务器版本、当前库/用户、只读状态、延迟；可选 `instance` 参数 |
| `list_schemas` | schema（模式）清单：对象数、注释；`include_system` 控制是否含系统模式 |
| `list_tables` | 表/视图清单：类型（表/视图/物化视图/分区表/外表）、估算行数、注释；可按 `schema` 过滤 |
| `describe_table` | 表结构：列（类型/可空/默认值/注释）、主键与约束、**全部索引及定义**；视图返回视图定义 SQL；分区表返回分区清单（GaussDB `pg_partition`） |
| `execute_select` | 执行 SELECT：仅接受单条 SELECT/WITH，受 `max_rows`/超时限制，返回列名+行数据+是否截断 |

所有工具均接受可选 `instance` 参数以选择数据源，缺省使用 `default_instance`。

## 构建

要求 Go 1.21+。驱动源码已内置于 `third_party/gaussdb-go`（通过 `replace` 指令引用），
正常联网环境下 `go build` 会自动解析其余依赖；纯内网环境请先在有网环境执行 `go mod vendor`
后携带 `vendor/` 目录，用 `go build -mod=vendor` 构建。

```bash
go build -o gaussdb-ro-mcp ./cmd/gaussdb-ro-mcp
./gaussdb-ro-mcp --version
```

## 配置

复制 [gaussdb-ro-mcp.example.yaml](gaussdb-ro-mcp.example.yaml) 为 `gaussdb-ro-mcp.yaml` 并修改。
配置文件查找顺序：`-config` 参数 > 环境变量 `GAUSSDB_RO_MCP_CONFIG` > `./gaussdb-ro-mcp.yaml`。

```yaml
server:
  max_rows: 500          # execute_select 默认行数上限
  max_rows_cap: 10000    # 单次调用可放宽的硬上限
  statement_timeout: 30s
  connect_timeout: 10s

instances:
  - name: prod                    # 实例名称，工具调用时用 instance 参数引用
    host: 192.168.0.10
    port: 8000
    database: postgres
    user: readonly_user
    password: "****"
    sslmode: disable              # 内网非 SSL 默认值
  - name: dev
    dsn: "gaussdb://readonly_user:****@192.168.1.20:5432/appdb?sslmode=disable"

default_instance: prod
```

## 接入编码代理

### Claude Code

方式一：项目根目录 `.mcp.json`（或 `claude mcp add` 命令）：

```json
{
  "mcpServers": {
    "gaussdb-readonly": {
      "command": "/opt/gaussdb-ro-mcp/gaussdb-ro-mcp",
      "args": ["-config", "/opt/gaussdb-ro-mcp/gaussdb-ro-mcp.yaml"]
    }
  }
}
```

```bash
claude mcp add gaussdb-readonly -- /opt/gaussdb-ro-mcp/gaussdb-ro-mcp -config /opt/gaussdb-ro-mcp/gaussdb-ro-mcp.yaml
```

### OpenCode

`opencode.json`：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "gaussdb-readonly": {
      "type": "local",
      "command": ["/opt/gaussdb-ro-mcp/gaussdb-ro-mcp", "-config", "/opt/gaussdb-ro-mcp/gaussdb-ro-mcp.yaml"]
    }
  }
}
```

## 数据库侧只读账号（强烈建议）

```sql
CREATE USER readonly_user WITH PASSWORD 'your-strong-password';
GRANT USAGE ON SCHEMA public TO readonly_user;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO readonly_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO readonly_user;
-- 其他模式按需逐个授权
```

即使数据库账号被限定为只读，本服务的 SQL 校验与会话强制仍会拦截
锁行（`FOR UPDATE`）、`SELECT INTO`、危险函数调用、长查询等行为。

## 日志

所有运行日志输出到 **stderr**（stdout 为 MCP 协议通道），包括配置加载、
数据源就绪、会话只读校验结果等，便于在代理客户端的 MCP 日志中排障。

## 开发与测试

```bash
go test ./...                       # 单元测试（SQL 校验、配置解析）
GAUSSDB_RO_MCP_TEST_DSN="host=... port=... user=... password=... dbname=... sslmode=disable" \
  go test -count=1 ./...            # 集成测试（需 GaussDB/openGauss 实例）
```

可用 Docker 快速起一个 openGauss 测试实例并灌入测试数据：

```bash
docker run -d --name opengauss-ro-test -e GS_PASSWORD='Gaussdb@123' \
  -p 127.0.0.1:15433:5432 --privileged docker.m.daocloud.io/enmotech/opengauss:latest

go run ./scripts/devseed "host=127.0.0.1 port=15433 user=gaussdb password=Gaussdb@123 dbname=postgres sslmode=disable"
```

集成测试覆盖：只读会话强制（服务端拒绝写入）、5 个工具的端到端行为、
真实二进制 stdio 子进程冒烟。注意：本驱动使用 GaussDB 扩展协议（3.51），
无法连接原生 PostgreSQL，集成测试需要真实的 GaussDB / openGauss 实例。

## 目录结构

```
cmd/gaussdb-ro-mcp/     程序入口（stdio MCP 服务器）
internal/config/        YAML 配置：多实例数据源、行数/超时等
internal/guard/         第 1 层防护：SQL 只读静态校验器
internal/db/            多实例连接池 + 第 2 层防护：会话级 READ ONLY 强制、元数据查询
internal/tools/         MCP 工具注册与实现
scripts/devseed/        集成测试数据灌入工具（开发用）
third_party/gaussdb-go/ GaussDB 官方 Go 驱动源码（go.mod replace 引用）
```
