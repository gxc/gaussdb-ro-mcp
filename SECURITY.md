# 安全策略

[简体中文] | [English](#english)

## 支持的版本

项目处于 0.x 快速迭代阶段，安全修复只发布到**最新版本**，请始终使用
[最新 Release](https://github.com/gxc/gaussdb-ro-mcp/releases/latest)。

| 版本 | 支持状态 |
| --- | --- |
| 最新版（0.2.x） | ✅ 接收安全修复 |
| 更早版本 | ❌ 请先升级 |

## 报告漏洞

**请勿通过公开 Issue 报告安全漏洞。**

请使用 GitHub 的
[私有漏洞报告](https://github.com/gxc/gaussdb-ro-mcp/security/advisories/new)
功能提交，该渠道在披露前对报告内容保密。报告时请尽量包含：

- 影响的版本（`gaussdb-ro-mcp --version`）与部署形态（stdio / 接入的 Agent 客户端）
- 数据库环境：GaussDB 集中式 / 主备 / 分布式，或 openGauss 版本
- 复现步骤：完整的配置片段（**请脱敏口令**）与触发问题的 SQL 输入
- 影响评估：泄露了什么数据、能执行什么写入、越过了哪一层防御

目标在 **3 个工作日内**确认收到，并在评估后给出修复计划；修复发布时会同步更新本页。

## 征集范围

特别欢迎以下方向的安全报告：

- **SQL 静态校验绕过**（`internal/guard`）：任何能让 DML/DDL、多语句、
  `SELECT ... INTO`、`FOR UPDATE/SHARE`、危险函数（`dblink*`、`set_config`、
  `setval`、`pg_read_file*`、`pg_advisory_*`、大对象写等）通过校验并到达数据库的输入，
  包括借助注释、字符串、引号标识符、编码、CTE、函数调用等构造的绕过
- **只读事务强制失效**（`internal/db`）：在只读事务内实际发生写入的路径
- **资源限制绕过**：绕过 `max_rows`、`statement_timeout` 造成数据库端资源耗尽
- **凭据泄露**：配置加载、日志输出等环节出现口令或 DSN 泄露

**不在征集范围内**（请勿报告，或应报告给相应方）：

- 数据库账号自身的口令强度、授权范围等部署问题（属第 3 层防御，见 README 的账号建议）
- 需要数据库管理员权限才能实施的攻击
- 针对运行主机、操作系统的攻击
- GaussDB / openGauss 数据库自身的漏洞（请报告给华为云或 openGauss 社区）

## 威胁模型与边界

理解本项目的防御边界，有助于正确部署：

1. **三层防御是纵深防御，不是安全边界**。第 1 层 SQL 静态校验可能存在未知的绕过方式；
   第 2 层事务级 `READ ONLY` 由数据库服务端执行，是更可靠的兜底，但仍建议与
   第 3 层（仅授予 `SELECT` 权限的专用账号）叠加使用。任何单层都不应被当作唯一防线。
2. **本地 stdio 进程模型**。本服务假设运行环境与配置文件不被不可信用户访问。
   配置文件内含数据库口令，请将文件权限设为 `600`，不要提交到代码仓库。
3. **日志**。运行日志输出到 stderr，当前仅包含配置路径、实例名、超时参数等元信息，
   **不包含口令与 DSN**。若发现日志中出现敏感信息，请按上述渠道报告。
4. **产物完整性**。安装时请核对 Release 页 `SHA256SUMS.txt` 中的校验值。

## 部署加固建议

- 使用仅授予 `SELECT` 权限的专用数据库账号（见 README「数据库侧只读账号」）
- 配置文件权限 `600`，通过环境变量 `GAUSSDB_RO_MCP_CONFIG` 指定时同样注意路径可读性
- 在数据库侧限制该账号可访问的 schema，遵循最小授权
- 订阅 [Releases](https://github.com/gxc/gaussdb-ro-mcp/releases) 以便及时获取安全修复

## 致谢

（暂无）报告经确认并修复后，将在本节致谢（可要求匿名）。

---

<a id="english"></a>
# Security Policy

## Supported Versions

The project is in rapid 0.x iteration; security fixes are only released for the
**latest version**. Always use the
[latest release](https://github.com/gxc/gaussdb-ro-mcp/releases/latest).

| Version | Supported |
| --- | --- |
| latest (0.2.x) | ✅ security fixes |
| older | ❌ please upgrade |

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Use GitHub
[private vulnerability reporting](https://github.com/gxc/gaussdb-ro-mcp/security/advisories/new)
instead; reports stay confidential until disclosure. Please include:

- Affected version (`gaussdb-ro-mcp --version`) and deployment shape (stdio / agent client)
- Database environment: GaussDB centralized / standby / distributed, or openGauss version
- Reproduction: full config snippet (**redact passwords**) and the offending SQL input
- Impact: what data is exposed, what writes are possible, which defense layer is bypassed

Reports are acknowledged within **3 business days**; the fix plan follows after triage
and this page is updated when the fix ships.

## Scope

Reports are especially welcome for:

- **Static SQL validation bypass** (`internal/guard`): any input that lets DML/DDL,
  multi-statements, `SELECT ... INTO`, `FOR UPDATE/SHARE`, or dangerous functions
  (`dblink*`, `set_config`, `setval`, `pg_read_file*`, `pg_advisory_*`, large-object
  writes, etc.) pass validation and reach the database — including bypasses built from
  comments, strings, quoted identifiers, encodings, CTEs, or function calls
- **Read-only transaction enforcement failure** (`internal/db`): any path where a write
  actually occurs inside the read-only transaction
- **Resource-limit bypass**: circumventing `max_rows` or `statement_timeout` to exhaust
  database resources
- **Credential leakage**: passwords or DSNs appearing in config loading or log output

**Out of scope** (do not report here, or report to the appropriate party):

- Deployment issues with the database account itself (password strength, grant scope —
  this is layer 3, see the README account guidance)
- Attacks requiring database administrator privileges
- Attacks against the host or operating system
- Vulnerabilities in GaussDB / openGauss itself (report to Huawei Cloud or the
  openGauss community)

## Threat Model and Boundaries

1. **The three layers are defense in depth, not a security boundary.** Layer 1 (static
   SQL validation) may have unknown bypasses; layer 2 (transaction-level `READ ONLY`)
   is enforced by the database server and is the more reliable backstop, but should
   still be combined with layer 3 (a dedicated account granted `SELECT` only). No
   single layer should be treated as the sole line of defense.
2. **Local stdio process model.** The server assumes its runtime environment and config
   file are not accessible to untrusted users. The config file contains database
   passwords: set its permissions to `600` and never commit it to a repository.
3. **Logging.** Runtime logs go to stderr and currently contain only metadata (config
   path, instance names, timeout parameters) — **no passwords or DSNs**. If sensitive
   information ever appears in logs, please report it via the channel above.
4. **Artifact integrity.** Verify checksums against `SHA256SUMS.txt` on the Release page.

## Hardening Recommendations

- Use a dedicated database account granted `SELECT` privileges only (see the README
  section on read-only accounts)
- Set config file permissions to `600`; the same applies when pointing to it via the
  `GAUSSDB_RO_MCP_CONFIG` environment variable
- Restrict which schemas the account can access on the database side (least privilege)
- Watch [Releases](https://github.com/gxc/gaussdb-ro-mcp/releases) so you pick up
  security fixes promptly

## Acknowledgments

(None yet.) Confirmed and fixed reports will be acknowledged here (anonymous
acknowledgment is fine).
