# gaussdb-ro-mcp

**English** | [简体中文](README.md)

[![CI](https://github.com/gxc/gaussdb-ro-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/gxc/gaussdb-ro-mcp/actions/workflows/ci.yml)
[![Glama](https://glama.ai/mcp/servers/gxc/gaussdb-ro-mcp/badges/score.svg)](https://glama.ai/mcp/servers/gxc/gaussdb-ro-mcp)
[![Release](https://img.shields.io/github/v/release/gxc/gaussdb-ro-mcp)](https://github.com/gxc/gaussdb-ro-mcp/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)

A **read-only MCP server for GaussDB**, designed for coding agents (Claude Code, OpenCode, etc.).
It is built on the [official GaussDB Go driver](https://github.com/huaweicloud-samples/database-gaussdb-go)
(Huawei Cloud's open-source pgx v5 adapter; the source is vendored in `third_party/gaussdb-go`
for offline builds) and exposes database exploration tools over stdio transport.

Designed for **intranet, non-SSL environments**: `sslmode=disable` is the default, and the
config file supports managing multiple GaussDB instances at once.

## Read-Only Guarantee (Three Layers of Defense in Depth)

| Layer | Mechanism | Details |
| --- | --- | --- |
| 1. Static SQL validation | `internal/guard` | Only **single** `SELECT`/`WITH` statements are allowed. Rejects DML/DDL (including writes inside CTEs), `SELECT ... INTO`, `FOR UPDATE/SHARE` row locks, multi-statement input, and dangerous functions (`dblink*`, `set_config`, `setval`, `pg_read_file*`, `pg_terminate_backend`, `pg_advisory_*`, large-object writes, etc. — configurable). Lexical analysis correctly skips strings, comments, and quoted identifiers to avoid false positives |
| 2. Transaction-level enforcement | `internal/db` | Every query runs inside an explicit read-only transaction: `BEGIN` → `SET LOCAL TRANSACTION READ ONLY` → query → `COMMIT` (rollback on failure). The GaussDB distributed edition only supports transaction-level read-only settings, so this approach works on both centralized/standby and distributed instances; a `SET statement_timeout` is also applied. Even if layer 1 is bypassed, the server rejects any write inside the transaction (including writes performed inside functions) |
| 3. Deployment recommendation | README | Use a database account granted `SELECT` privileges only (see below) for least privilege |

## MCP Tools

| Tool | Purpose |
| --- | --- |
| `test_connection` | Connectivity test: server version, current database/user, read-only status, latency; optional `instance` parameter |
| `list_schemas` | List schemas: object counts and comments; `include_system` controls whether system schemas are included |
| `list_tables` | List tables/views: type (table/view/materialized view/partitioned table/foreign table), estimated row count, comments; filterable by `schema` |
| `describe_table` | Table structure: columns (type/nullability/default/comment), primary keys and constraints, **all indexes with definitions**; views return the view definition SQL; partitioned tables return the partition list (GaussDB `pg_partition`) |
| `execute_select` | Run a SELECT: accepts a single SELECT/WITH only, subject to `max_rows`/timeout limits; returns column names, rows, and a truncation flag |

All tools accept an optional `instance` parameter to select the data source; `default_instance` is used when omitted. The full instance list (with the default marked) is embedded in the parameter's schema description, so agents discover the available instances from the tool list without trial and error.

## How It Compares

| | gaussdb-ro-mcp (this project) | [GaussDB-MCP](https://github.com/mengqi1436/GaussDB-MCP) | [Multi DB MCP](https://github.com/the-Rings/universal-db-mcp) | Generic PostgreSQL MCP servers |
| --- | --- | --- | --- | --- |
| Positioning | **Read-only** exploration, GaussDB-specific | Full-featured GaussDB access: queries, writes, transactions, diagnostics, user administration | Universal connector for 17 databases (incl. GaussDB/openGauss), MCP + REST API | Vanilla PostgreSQL |
| GaussDB adaptation | Official Go driver; centralized/standby/distributed editions all covered | Official Node.js driver (gaussdb-node) | Via the generic PostgreSQL protocol | No adaptation for GaussDB-specific catalogs (e.g. `pg_partition`) |
| Read-only guarantee | Three-layer defense in depth: static SQL validation + transaction-level READ ONLY enforcement + least-privilege guidance | No read-only mode (default `root` admin account; writes are by design) | Application-level read-only by default | Varies by project |
| GaussDB-specific metadata | Partitioned tables (`pg_partition`), foreign tables, materialized views, view definition SQL, full index definitions | Partial | Probed via the PG protocol; partial | None |
| Runtime dependency | None (statically compiled single binary, offline-friendly) | Node.js ≥ 20 | Node.js (npx) | Varies |
| Multi-instance management | Centralized config for multiple instances, switchable via the `instance` parameter | Single instance (environment variables) | Single connection (startup args) | Usually single instance |
| Intranet non-SSL | `sslmode=disable` by default | Supported (SSL off by default) | Supported | Supported |
| License | Apache-2.0 | Not declared | Not declared | Varies |

These projects serve different purposes: a full-featured server is a fair choice when you
need complete read/write and operations capability. If the goal is letting a coding agent
explore data **safely** — especially on production databases — read-only constraints and
defense in depth are what this project is for. The information above is compiled from the
public READMEs of each project (Oct 2026); corrections are welcome via
[an issue](https://github.com/gxc/gaussdb-ro-mcp/issues).

## Installation

Download the binary for your platform from
[Releases](https://github.com/gxc/gaussdb-ro-mcp/releases)
(`linux-amd64` / `linux-arm64` / `darwin-arm64` / `windows-amd64.exe`; for other platforms or
air-gapped environments, build from source — see the next section).

**Linux**:

```bash
curl -LO https://github.com/gxc/gaussdb-ro-mcp/releases/latest/download/gaussdb-ro-mcp-linux-amd64
sudo install -Dm 755 gaussdb-ro-mcp-linux-amd64 /usr/local/bin/gaussdb-ro-mcp
gaussdb-ro-mcp --version
```

**macOS** (Apple Silicon):

```bash
curl -LO https://github.com/gxc/gaussdb-ro-mcp/releases/latest/download/gaussdb-ro-mcp-darwin-arm64
sudo install -Dm 755 gaussdb-ro-mcp-darwin-arm64 /usr/local/bin/gaussdb-ro-mcp
gaussdb-ro-mcp --version
```

**Windows** (PowerShell; once placed in a PATH directory it can be invoked directly):

```powershell
Invoke-WebRequest -Uri "https://github.com/gxc/gaussdb-ro-mcp/releases/latest/download/gaussdb-ro-mcp-windows-amd64.exe" -OutFile "gaussdb-ro-mcp.exe"
Move-Item .\gaussdb-ro-mcp.exe "$env:LOCALAPPDATA\Microsoft\WindowsApps\"  # this directory is on PATH by default
gaussdb-ro-mcp --version
```

Checksums for each artifact are available in `SHA256SUMS.txt` on the Release page.

## Building

Requires Go 1.26.10+ (matching go.mod). The driver source is vendored in `third_party/gaussdb-go`
(referenced via a `replace` directive); with network access, `go build` resolves the remaining
dependencies automatically. In a fully air-gapped environment, first run `go mod vendor` on a
networked machine, then carry the `vendor/` directory over and build with `go build -mod=vendor`.

```bash
go build -o gaussdb-ro-mcp ./cmd/gaussdb-ro-mcp
sudo install -Dm 755 gaussdb-ro-mcp /usr/local/bin/gaussdb-ro-mcp
gaussdb-ro-mcp --version
```

Cross-compiling for other platforms is straightforward (pure Go, just set `CGO_ENABLED=0`):
`CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go build -o ... ./cmd/gaussdb-ro-mcp`.

## Configuration

Copy [gaussdb-ro-mcp.example.yaml](gaussdb-ro-mcp.example.yaml) to `gaussdb-ro-mcp.yaml` and
edit it. Config file lookup order: `-config` flag > `GAUSSDB_RO_MCP_CONFIG` environment
variable > `./gaussdb-ro-mcp.yaml`.

```yaml
server:
  max_rows: 500          # default row limit for execute_select
  max_rows_cap: 10000    # hard cap that a single call may raise up to
  statement_timeout: 30s
  connect_timeout: 10s

instances:
  - name: prod                    # instance name, referenced via the instance parameter
    host: 192.168.0.10
    port: 8000
    database: postgres
    user: readonly_user
    password: "****"
    sslmode: disable              # default for intranet, non-SSL environments
  - name: dev
    dsn: "gaussdb://readonly_user:****@192.168.1.20:5432/appdb?sslmode=disable"

default_instance: prod
```

## Connecting to a Coding Agent

### Claude Code

Option 1: `.mcp.json` in the project root (or the `claude mcp add` command):

```json
{
  "mcpServers": {
    "gaussdb-readonly": {
      "command": "/usr/local/bin/gaussdb-ro-mcp",
      "args": ["-config", "/path/to/gaussdb-ro-mcp.yaml"]
    }
  }
}
```

```bash
claude mcp add gaussdb-readonly -- /usr/local/bin/gaussdb-ro-mcp -config /path/to/gaussdb-ro-mcp.yaml
```

### OpenCode

`opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "gaussdb-readonly": {
      "type": "local",
      "command": ["/usr/local/bin/gaussdb-ro-mcp", "-config", "/path/to/gaussdb-ro-mcp.yaml"]
    }
  }
}
```

## Read-Only Database Account (Strongly Recommended)

```sql
CREATE USER readonly_user WITH PASSWORD 'your-strong-password';
GRANT USAGE ON SCHEMA public TO readonly_user;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO readonly_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO readonly_user;
-- Grant additional schemas one by one as needed
```

Even with a database account restricted to read-only, this server's SQL validation and session
enforcement still block row locking (`FOR UPDATE`), `SELECT INTO`, dangerous function calls,
and long-running queries.

## Logging

All runtime logs are written to **stderr** (stdout is reserved for the MCP protocol channel),
including config loading, data source readiness, and session read-only check results —
convenient for troubleshooting in your agent client's MCP logs.

## Development and Testing

```bash
go test ./...                       # unit tests (SQL validation, config parsing)
GAUSSDB_RO_MCP_TEST_DSN="host=... port=... user=... password=... dbname=... sslmode=disable" \
  go test -count=1 ./...            # integration tests (require a GaussDB/openGauss instance)
```

You can quickly spin up an openGauss test instance with Docker and seed it with test data:

```bash
docker run -d --name opengauss-ro-test -e GS_PASSWORD='Gaussdb@123' \
  -p 127.0.0.1:15433:5432 --privileged docker.m.daocloud.io/enmotech/opengauss:latest

go run ./scripts/devseed "host=127.0.0.1 port=15433 user=gaussdb password=Gaussdb@123 dbname=postgres sslmode=disable"
```

Integration tests cover: read-only session enforcement (server-side rejection of writes),
end-to-end behavior of all 5 tools, and a smoke test against the real binary as a stdio
subprocess. Note: this driver uses the GaussDB extended protocol (3.51) and cannot connect to
vanilla PostgreSQL, so integration tests require a real GaussDB / openGauss instance.

## Repository Layout

```
cmd/gaussdb-ro-mcp/     entry point (stdio MCP server)
internal/config/        YAML config: multi-instance data sources, row limits/timeouts
internal/guard/         layer 1: static read-only SQL validator
internal/db/            multi-instance connection pool + layer 2: session-level READ ONLY enforcement, metadata queries
internal/tools/         MCP tool registration and implementation
scripts/devseed/        test data seeding tool for integration tests (development use)
third_party/gaussdb-go/ official GaussDB Go driver source (via go.mod replace)
```

## Feedback and Contributions

Issues and pull requests are welcome:

- Bug reports / feature suggestions: <https://github.com/gxc/gaussdb-ro-mcp/issues>
- Latest release: <https://github.com/gxc/gaussdb-ro-mcp/releases/latest>
- Before submitting a PR, make sure `go vet ./...` and `go test ./...` pass; changes touching
  the security layers (static SQL validation / read-only enforcement) should come with
  regression tests
