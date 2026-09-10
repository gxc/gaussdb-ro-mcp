# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

面向 Coding Agent（Claude Code、OpenCode 等）的 GaussDB 只读 MCP 服务器，Go 语言实现，stdio 传输。代码注释、错误信息、文档均使用中文——新增代码保持一致。

## 常用命令

```bash
go build -o gaussdb-ro-mcp ./cmd/gaussdb-ro-mcp   # 构建（version 用 -ldflags 注入）
go test ./...                                       # 单元测试（无需数据库）
go test ./internal/guard -run TestValidateSelect    # 运行单个测试
go vet ./...

# 集成测试（需真实 GaussDB/openGauss 实例 + sales 测试数据）：
GAUSSDB_RO_MCP_TEST_DSN="host=127.0.0.1 port=15433 user=gaussdb password=... dbname=postgres sslmode=disable" \
  go test -count=1 ./...
```

依赖已 vendor（`vendor/`）；纯内网环境用 `go build -mod=vendor`。

集成测试环境搭建：

```bash
docker run -d --name opengauss-ro-test -e GS_PASSWORD='Gaussdb@123' \
  -p 127.0.0.1:15433:5432 --privileged docker.m.daocloud.io/enmotech/opengauss:latest
go run ./scripts/devseed "host=127.0.0.1 port=15433 user=gaussdb password=Gaussdb@123 dbname=postgres sslmode=disable"
```

## 架构：三层只读纵深防御

这是本仓库的核心设计，改动任何一层前先理解整体：

1. **SQL 静态校验**（`internal/guard`）：`Guard.ValidateSelect` 只放行单条 SELECT/WITH。手写词法分析器 `tokenize` 把 SQL 归一化为小写 token 流——字符串字面量/dollar-quoted 替换为占位符 `"0"`，注释丢弃，引号标识符加 `\x00` 前缀使其不参与关键字匹配。拦截规则：首词必须 select/with（跳过前导括号）、任意位置出现 DML 关键字（含 CTE 内）、`INTO`、`FOR UPDATE/SHARE`、`;` 后再有内容（多语句）、危险函数黑名单（`dblink*` 等，尾段匹配以兼容 `pg_catalog.dblink(...)`，支持 `*` 前缀通配，可用配置覆盖）。

2. **事务级强制**（`internal/db`）：GaussDB 分布式版仅支持事务级只读设置，所有查询统一在显式只读事务中执行——`queryReadOnly`（manager.go）按 `BEGIN` → `SET LOCAL TRANSACTION READ ONLY` → 查询 → `COMMIT` 的顺序执行（出错 ROLLBACK；BEGIN 失败时先 ROLLBACK 清理服务端复用会话的残留事务再重试一次），由服务端拒绝事务内一切写入。`AfterConnect` 仅设置 `statement_timeout`（亚毫秒钳制为 1ms）。

3. **部署层**（README）：建议使用仅授予 SELECT 权限的数据库账号。

## 包结构与数据流

```
cmd/gaussdb-ro-mcp/main.go   入口：加载配置 → db.NewManager → mcp.NewServer(Stdio) → tools.Register
internal/config              YAML 配置：多实例（DSN 或拆分字段，DSN 优先）、Duration 自定义类型、
                             SetDefaults + validate；BuildDSN 缺省追加 sslmode=disable（内网非 SSL 场景）
internal/db/manager.go       Manager（多实例连接池注册表）+ Instance（池 + 生效配置）+ Select/Query
internal/db/meta.go          pg_catalog 元数据查询（schemas/tables/columns/indexes/视图定义/分区）；
                             detectPartitionSupport 探测 pg_class.parttype 区分 GaussDB 与原生 PG
internal/tools/tools.go      5 个 MCP 工具（test_connection/list_schemas/list_tables/describe_table/
                             execute_select），全部接受可选 instance 参数；execute_select 是唯一 SQL
                             入口：guard 校验 → maxRows 钳制 → Instance.Select
internal/dbtest              测试用 GaussDB 线协议 mock 服务端（启动包断言、简单/扩展协议、
                             可配置 SHOW transaction_read_only 应答），使 db/tools/cmd 测试无需真实库
scripts/devseed              向测试实例灌入集成测试数据（sales schema）
third_party/gaussdb-go       GaussDB 官方 Go 驱动源码，经 go.mod replace 引用，支持离线构建
```

调用链：MCP 工具 → `Manager.Resolve(instance)`（空名取 `default_instance`）→ `Instance` 的元数据方法或 `Select`。`execute_select` 的 maxRows 缺省取实例配置，硬顶为服务级 `max_rows_cap`。

## 发布流程

1. 构建产物：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64|arm64 go build -trimpath -ldflags "-s -w -X main.version=vX.Y.Z" -o dist/gaussdb-ro-mcp-vX.Y.Z-linux-<arch> ./cmd/gaussdb-ro-mcp`，重新生成 `dist/SHA256SUMS.txt` 并 `sha256sum -c` 校验；本地 `dist/` 替换为当版产物（旧版产物保留在对应 Release 页面）。
2. 打 annotated tag 并推送：`git tag -a vX.Y.Z && git push origin vX.Y.Z`。
3. `gh release create vX.Y.Z dist/... --title "gaussdb-ro-mcp vX.Y.Z" --notes-file -`；说明沿用既有格式，安装命令用 `sudo install -Dm 755` 装到 `/usr/local/bin`。
4. **产物与文案约定**：架构名只用 `linux-amd64` / `linux-arm64`，ARM64 **不要**加"（鲲鹏/飞腾等）"之类的举例；含安全修复时在说明头部标注"建议所有用户尽快升级"。
5. Release 说明末尾附 compare 链接（`/compare/v上一版...vX.Y.Z`）。

## 关键约束

- **stdout 是 MCP 协议通道**，所有日志必须走 stderr（现有代码用 `log.New(os.Stderr, ...)`）。
- 驱动使用 GaussDB 扩展协议（3.51），**无法连接原生 PostgreSQL**——集成测试必须用真实 GaussDB/openGauss。
- 集成测试由 `GAUSSDB_RO_MCP_TEST_DSN` 环境变量门控（未设置则 skip），`internal/tools/e2e_test.go` 依赖 devseed 灌入的 `sales.orders` 数据。
- 仓库测试覆盖率维持在 90% 以上（当前：tools 100% / config 100% / guard 99.5% / db 97.8% / cmd 94.6%），新增功能需配套测试；无法连库的场景用 `internal/dbtest` 的线协议 mock。
