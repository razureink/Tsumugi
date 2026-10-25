# Tsumugi

A single-node, in-memory relational database written with the Go standard library only: data lives in RAM, durability comes from a WAL, and it ships with a monitoring/admin panel plus MySQL wire-protocol compatibility. No third-party dependencies — `go build` is all you need.

Built for **small-to-medium, single-node deployments** that want simple SQL, table-level ACLs and a built-in ops panel (cache/session/config/metrics-style data), and as a readable storage-engine reference.

[中文文档](README.md)

---

## Scope

**Good fit**

- Single node, working set fits in RAM (keep it under ~50% of available memory)
- Read-heavy workloads, or write throughput that one disk can sustain
- Internal systems that need table-level permissions, audit logs, a monitoring panel and a load generator
- Deployments that should be one binary + one data directory

**Not a fit**

- Datasets far larger than RAM (there is no disk tier; the WAL is recovery-only)
- Multi-replica / sharding / HA (single process, single-writer WAL)
- Analytical SQL: no `JOIN`, `GROUP BY`, `HAVING`, subqueries, window functions, foreign keys
- Serializable isolation: optimistic concurrency, same-row concurrent writes are **last-writer-wins**
- Row locks, `SELECT ... FOR UPDATE`, cursors

---

## Features

- **Zero dependencies**: pure standard library, `CGO_ENABLED=0` static builds
- **Custom LLRB indexes**: O(log n) primary-key lookups, copy-on-write secondary indexes with lock-free readers
- **WAL durability**: append-only + CRC32 + varint records, v3 batch framing (length + whole-batch checksum) for streaming replay, torn-tail truncation on startup, single-writer queue with group commit
- **Transactions**: `BEGIN / COMMIT / ROLLBACK` with per-row version conflict detection
- **Row TTL** with a background reaper
- **Table-level GRANT/REVOKE** (SELECT / INSERT / UPDATE / DELETE / DDL), enforced in the engine, shared by all entry points
- **MySQL protocol**: handshake, `mysql_native_password`, text protocol, prepared statements (binary rows), `COM_FIELD_LIST`, `COM_SET_OPTION`
- **Admin panel**: live metrics, row browser, SQL console, visual table builder, user management, backup and WAL compaction
- **Panel themes**: 7 tones plus a "high contrast" mode (pure black/white, 2px solid borders, no gradients or soft shadows); pick it under Settings → Tone, applies instantly and is remembered in the browser
- **Observability**: `/metrics` (Prometheus), `/api/stats` (JSON), `/healthz`, audit logs
- **Built-in load test**: `/stress` drives the real client protocols (native or MySQL)

---

## Quick start

### Docker Compose

```bash
docker compose up -d --build
```

Open <http://localhost:10232/> and complete the setup wizard (the setup endpoint only accepts local/private source addresses).

| Port | Purpose |
| ---- | ------- |
| 9999 | binary protocol |
| 10232 | panel / metrics |
| 3309 | MySQL protocol |

Data, config and backups live in `./data`, `./config`, `./backup`.

```bash
docker compose logs -f
docker compose restart     # SIGTERM, flushes the WAL
docker compose down
```

Install the binaries on the host:

```bash
sudo ./build.sh
```

### Build from source

```bash
# Go 1.21+ required (verified on go1.27)
go build -o tsumugi .
./tsumugi
```

On first start `config/tsumugi.json`, `data/` and `backup/` are created in the working directory.

### Client examples

```bash
go run ./examples                     # binary protocol client
mysql -h 127.0.0.1 -P 3309 -u root -p # MySQL protocol (enable it first)
```

---

## Production deployment

### Sizing

| Item | Guidance |
| --- | --- |
| Memory | Data size × 1.5–2 (row bytes + indexes + replay overhead) |
| Disk | WAL ≈ data size × 2–3 before compaction; dedicated SSD recommended |
| CPU | Reads scale with cores; writes are bounded by the single WAL writer and fsync speed |
| File descriptors | ≥ `max_conns` × 2 + headroom |
| RPO | `durability=fsync` for zero loss, `batch` tolerates losing the last flush interval (100 ms default) |

### Topology

```
        HTTPS (443)
   ┌────────▼─────────┐
   │ Nginx / Caddy    │  TLS termination, rate limit, IP allowlist
   └────────┬─────────┘
            │ http://127.0.0.1:10232
   ┌────────▼─────────┐
   │     Tsumugi      │  bind_host=127.0.0.1
   │  10232 panel     │
   │  9999  binary    │◄── private network only
   │  3309  mysql     │◄── private network only
   └────────┬─────────┘
            │
   /var/lib/tsumugi/{data,config,backup}
```

- Never expose the panel directly: set `bind_host: "127.0.0.1"` and terminate TLS in a proxy (`deploy/nginx-panel.conf.example`)
- Behind a proxy, set `trusted_proxies` so login rate limiting sees real client IPs
- The binary and MySQL listeners are raw TCP — HTTP reverse proxies cannot front them
- A hardened systemd unit is provided in `deploy/tsumugi.service`

**First-time setup**: open the panel, run the wizard. It only accepts local/private addresses, stores a PBKDF2 hash for the console and a SHA1 digest (`password_sha1`) for MySQL, and generates `metrics_token`.

### Configuration reference

`config/tsumugi.json` is the single source of truth (created on first start, editable from the settings page, hot-applied and persisted).

| Key | Default | Description |
| --- | --- | --- |
| `port` | `9999` | binary protocol port |
| `bind_host` | `""` | listen address; empty = all interfaces |
| `tls_cert` / `tls_key` | empty | panel HTTPS cert/key (both required) |
| `max_conns` | `512` | concurrent connection limit (binary + MySQL) |
| `wal_dir` | `./data` | data directory |
| `wal_file` | `tsumugi.wal` | WAL file name |
| `privilege_file` | `privileges.json` | table ACL file |
| `flush_interval_ms` | `100` | batch-mode fsync interval |
| `group_commit_ms` | `2` | group commit window |
| `ttl_clean_ms` | `30000` | TTL sweep interval |
| `idle_timeout_s` | `60` | idle connection timeout (unauthenticated: 30s) |
| `backup_dir` | `./backup` | backup directory |
| `metrics_port` | `10232` | panel port |
| `enable_checksum` | `true` | WAL CRC32 |
| `durability` | `batch` | `batch` (periodic fsync) or `fsync` (every commit) |
| `mysql_enabled` | `false` | enable MySQL protocol |
| `mysql_port` | `3309` | MySQL protocol port |
| `password_sha1` | empty | MySQL auth digest; empty rejects MySQL logins |
| `metrics_token` | generated | `/metrics` scrape token |
| `trusted_proxies` | `[]` | CIDRs allowed to supply `X-Forwarded-For` |
| `auto_compact` | `true` | compact the WAL during idle periods |
| `compact_idle_seconds` | `60` | compaction check interval |
| `compact_min_wal_mb` | `64` | minimum WAL size before auto compaction |
| `compact_peak_rate` | `50` | commands/s under which the instance counts as idle |
| `txn_conflict` | `error` | write-conflict policy: `error`=abort+rollback (optimistic concurrency); `last_write_wins`=overwrite (MySQL autocommit semantics, for ORMs that wrap every statement in an implicit transaction) |

Environment overrides: `TSUMUGI_PORT`, `TSUMUGI_BIND_HOST`, `TSUMUGI_METRICS_PORT`, `TSUMUGI_MYSQL`, `TSUMUGI_MYSQL_PORT`, `TSUMUGI_DURABILITY`, `TSUMUGI_MAX_CONNS`, `TSUMUGI_TLS_CERT`, `TSUMUGI_TLS_KEY`, `TSUMUGI_TRUSTED_PROXIES`, `TSUMUGI_AUTO_COMPACT`.

Startup validates the config: port conflicts abort, while plain HTTP / non-fsync / all-interface listeners emit warnings.

### Security checklist

- [ ] Strong admin password; unused wizard-created accounts removed
- [ ] `bind_host=127.0.0.1` + TLS reverse proxy (or `tls_cert`/`tls_key`)
- [ ] `trusted_proxies` configured when behind a proxy
- [ ] Ports 9999/3309 reachable only from trusted networks
- [ ] `/metrics` scraped with `metrics_token` stored in the monitoring system
- [ ] `data/`, `config/`, `backup/` are `0700` and owned by the service account
- [ ] Audit lines (`audit:`) shipped to your log platform
- [ ] Scheduled backups **with restore drills**
- [ ] `durability=fsync` if acknowledged writes must never be lost
- [ ] `enable_checksum` left enabled
- [ ] Panel icon font self-hosted (next section): offline installs without it fall back to Unicode glyphs

### Panel icon font (web first, graded fallback)

Panel icons use Material Symbols Outlined (the M3E icon set) and are still fetched from Google Fonts by default. When that fails:

1. **Google Fonts** — the original `<link>` stays untouched
2. **Mirror CDN** — `fonts.googleapis.cn`, then `fonts.loli.net`
3. **Self-hosted** — drop `material-symbols-outlined.woff2` into `assets/` and it is served from `/assets/material-symbols-outlined.woff2` (no restart needed)
4. **Monochrome glyphs** — only when everything above is unreachable; icon names become **text glyphs** (`add → ＋`, `chevron_left → ‹`), never emoji

Set `TSUMUGI_ICON_FALLBACK=off` to disable step 4 and keep the plain CDN behaviour. See `assets/README.md` for font sources; Docker images ship only the binary, so steps 1–2 still apply.

### Backup and restore

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" http://127.0.0.1:10232/api/admin/backup
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:10232/api/admin/backup   # list
```

Produces `backup/tsumugi_<ts>.wal` (full data snapshot) and `backup/privileges_<ts>.json` with mode `0600`.

```bash
systemctl stop tsumugi
cp backup/tsumugi_20260101_030000.wal /var/lib/tsumugi/data/tsumugi.wal
cp backup/privileges_20260101_030000.json /var/lib/tsumugi/data/privileges.json   # optional
systemctl start tsumugi
```

The snapshot reflects the moment the backup was taken; later writes are not included. The file is a WAL snapshot in the current format: this build reads v1/v2/v3, but **older binaries cannot read v3**, so export the data before downgrading.

### Monitoring

```bash
curl -H "Authorization: Bearer $METRICS_TOKEN" http://127.0.0.1:10232/metrics
```

Metrics: `tsumugi_total_commands`, `tsumugi_total_errors`, `tsumugi_qps`, `tsumugi_tps`, `tsumugi_wal_write_mb_s`, `tsumugi_wal_total_mb`, `tsumugi_fsync_per_s`, `tsumugi_cmd_<name>`. Scrape config: `deploy/prometheus.yml`.

Suggested alerts: `/healthz` failing; non-zero `rate(tsumugi_total_errors[5m])`; `tsumugi_fsync_per_s` dropping to zero while QPS > 0; WAL growth outpacing workload; `total_rows` approaching the sizing limit.

### Logs and audit

Logs go to stdout with timestamps and levels (`[OK] [INFO] [WARN] [ERR]`). Audit entries use the `audit:` prefix:

```
audit: user=admin action=create_table table=orders fields=5
audit: user=admin action=grant target=bob table=orders perm=1
login FAILED from 10.0.0.8 (user="root")
```

Failed logins are counted per (IP, user) with exponential backoff; failed binary/MySQL authentications close the connection.

### Upgrade and rollback

```bash
systemctl stop tsumugi
cp -a /var/lib/tsumugi/data /var/lib/tsumugi/data.bak.$(date +%F)
install -m 0755 tsumugi-new /usr/local/bin/tsumugi
systemctl start tsumugi
curl -fsS http://127.0.0.1:10232/healthz
```

New config keys always have defaults, and WAL reads are backward compatible (v1/v2/v3). However new files are written as v3, which older binaries reject — export the data before downgrading.

### Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| Startup: `port conflicts` | duplicate ports in config | change ports |
| Startup: `invalid WAL magic` / `unsupported WAL version` | corrupted or newer WAL | restore from backup; never hand-edit the WAL |
| `tail record truncated` in logs | crash left a partial record | expected; the tail is truncated automatically |
| Writes fail with `wal: ...` | disk full / permissions / IO error | fix disk or ownership, then restart |
| `too many connections` | `max_conns` exceeded | raise the limit or hunt connection leaks |
| Login keeps failing | backoff after repeated failures | wait (max 5 min) or restart |
| MySQL `Access denied` | empty `password_sha1` / not the configured root user | finish setup in the panel |
| `/metrics` 401 | missing token | use Bearer or `?token=` |
| Memory keeps growing | compaction not firing / data growth | check `wal_file_mb`, `total_rows`, compact manually |

### Performance baseline

Measured on a Xeon E5-2687W v2 with `go test -bench` (engine level, batch durability):

| Benchmark | Result |
| --- | --- |
| `BenchmarkPointSelect` | ~478 ns/op |
| `BenchmarkPointSelectParallel` | ~164 ns/op |
| `BenchmarkInsert` | ~3.3 µs/op |
| `BenchmarkUpdate` | ~11 µs/op |
| `BenchmarkSQLPointSelect` | ~2 µs/op |
| `BenchmarkMySQLQuery` | ~50 µs/op |

Tuning: `durability=fsync` + `group_commit_ms` 2–5 ms for safety; `batch` + larger `flush_interval_ms` for throughput; size `max_conns` with file descriptors and memory (~64 KB buffering per connection); shorten `compact_idle_seconds` / lower `compact_min_wal_mb` if the WAL grows fast.

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:10232/stress?duration=10&workers=4&protocol=mysql&mode=rw"
```

---

## HTTP endpoints

| Path | Auth | Notes |
| --- | --- | --- |
| `/healthz` | none | health probe |
| `/metrics` | metrics token / session | Prometheus |
| `/` `/dashboard` `/admin` `/users` | page public, APIs require token | console |
| `/api/login` `/api/logout` | — | Bearer session token, 24h TTL |
| `/api/setup/status` `/api/setup/complete` | local/private only | first-run wizard |
| `/api/stats` | session | monitoring JSON |
| `/api/admin/*` | session (admin for most) | tables, rows, SQL, settings, backup, compact, restart, users |
| `/stress` | session + `can_stress` | load generator |

Request bodies are capped at 1 MB; the HTTP server sets read-header/read/write/idle timeouts and security headers.

---

## SQL subset

Supported:

- **DDL**: `CREATE TABLE` (column attributes, table-level `PRIMARY KEY (...)`, `KEY`/`INDEX`/`UNIQUE`, `ENGINE=`/`CHARSET=` options), `CREATE DATABASE/INDEX`, `ALTER TABLE` (`ADD`/`DROP`/`MODIFY`/`CHANGE`/`RENAME COLUMN`, `ADD`/`DROP INDEX`, rename table), `DROP TABLE [IF EXISTS]`, `TRUNCATE`, `RENAME TABLE`
- **DML**: `INSERT` (multi-row is atomic), `INSERT IGNORE`, `REPLACE INTO`, `INSERT ... ON DUPLICATE KEY UPDATE` (incl. `VALUES(col)`), `UPDATE` (any `WHERE`, `SET col = expr`, `LIMIT`), `DELETE` (any `WHERE`, `LIMIT`)
- **SELECT**: projections/expressions/aliases, `DISTINCT`, `WHERE` (`AND`/`OR`/`NOT`/comparisons/`IN`/`LIKE`/`BETWEEN`/`IS [NOT] NULL`), `JOIN` (`INNER`/`LEFT`/`RIGHT`/`CROSS` with `ON`/`USING`, comma joins), `GROUP BY` + `HAVING`, aggregates (`COUNT`/`SUM`/`AVG`/`MIN`/`MAX`, `COUNT(DISTINCT)`), `ORDER BY` (incl. `ORDER BY 1`), `LIMIT`/`OFFSET`, `UNION` / `UNION ALL`
- **Functions**: string, numeric, conditional (`IF`/`IFNULL`/`NULLIF`/`COALESCE`/`CASE WHEN`), date (`NOW`/`CURDATE`/`YEAR`/`DATE_FORMAT`/`DATEDIFF`/`DATE_ADD`), info (`DATABASE`/`USER`/`VERSION`)
- **Statement-level compat**: `BEGIN`/`START TRANSACTION`/`COMMIT`/`ROLLBACK`/`SAVEPOINT`, `SET` (incl. `autocommit`), `USE`, `FLUSH`/`ANALYZE`/`OPTIMIZE`/`CHECK`/`REPAIR`/`LOCK`/`UNLOCK`/`KILL`/`DO`/`RESET`, `SHOW` (tables/columns/databases/index/status/variables/engines/processlist/master status/privileges…), `DESCRIBE`, `GRANT`/`REVOKE`, backtick identifiers, comments, system tables `__tables`/`__indexes`/`__stats` (admin only), read-only `information_schema.*` / `mysql.*` for phpMyAdmin compatibility.

Rejected explicitly (never silently mis-evaluated): subqueries/derived tables, window functions, recursive CTEs, foreign keys, `SELECT ... FOR UPDATE`, and stored procedures/triggers/views (`CALL` returns a clear error).

`JOIN` uses nested loops (no optimizer); `INT` columns have no NULL semantics (NULL stores as 0) — only `VARCHAR` can hold NULL.

---

## Protocol summary

Binary protocol: big-endian, one command byte. Commands: AUTH(10), PING(11), CREATE/DROP/DESCRIBE/ALTER TABLE(20/21/22/23), INSERT/SELECT/UPDATE/DELETE(30/31/32/33), QUERY SQL(35), BEGIN/COMMIT/ROLLBACK(40/41/42), CREATE INDEX(50), BACKUP(56), STATUS(57), COMPACT(58), SET DURABILITY(59), GRANT/REVOKE(90/91). Responses: OK(1), ERROR(2), VALUE(3), NOT_FOUND(4), ROWS(5), TXN_ID(6). Types: INT(0), VARCHAR(1, ≤65534 bytes), BOOL(2). See `docs/native-protocol.md`.

MySQL protocol: `mysql_native_password`, text protocol and prepared statements (binary row encoding), `COM_FIELD_LIST`, `COM_SET_OPTION`, `COM_INIT_DB`, 64 MB packet ceiling (64 KB before authentication). Only the configured root account can connect.

---

## Data files and crash recovery

| File | Contents | Mode |
| --- | --- | --- |
| `config/tsumugi.json` | configuration | 0600 |
| `data/tsumugi.wal` | WAL (magic + version header, batch frames with length + CRC32, varint records) | 0600 |
| `data/users.json` | console users (PBKDF2 + salt) | 0600 |
| `data/privileges.json` | table ACLs | 0600 |
| `backup/*` | snapshots | 0600 |

Recovery validates the header, streams v3 batch frames (verifying one CRC per batch), replays the varint records inside, truncates an incomplete tail batch instead of refusing to start, uses catalog records as replay barriers, and drops writes from transactions that never committed.

---

## Development

```bash
go build ./...
go vet ./...
gofmt -l .                    # CI requires empty output
go test ./... -count=1
go test -race ./... -count=1  # needs cgo/gcc
go test -run '^$' -bench . .
cd interop && go test ./...    # real MySQL driver interop
```

CI runs build / vet / gofmt / race tests / MySQL interop on Ubuntu (`.github/workflows/ci.yml`).

Performance regression check: mount a HEAD copy with `git worktree`, run fixed-iteration benchmarks on both trees alternately (`-benchtime 100000x -count=3`, 3–6 rounds, medians) to avoid the noise of time-based runs on Windows. SQL point lookups, range scans, inserts and transaction commits stay within ±3% of HEAD; range scans and transaction commits are ~12% and ~32% faster respectively.

Layout: `main.go` (startup), `db.go` (engine), `wal_v3.go` (batch framing + streaming replay), `wal_alter.go` (atomic whole-table rewrite for ALTER), `rb_tree.go`, `sql_parser.go`/`sql_expr.go`/`sql_exec.go`/`sql_dml.go`/`sql_alter.go`/`sql_stmt.go`, `server.go` (binary protocol), `mysql.go`, `acl.go`/`users.go`, `metrics.go`/`admin.go`/`app_html.go` (panel), `deploy/` (systemd, nginx, prometheus), `interop/` (driver tests), `docs/`.

---

## Known limitations

- Single-node, single-writer WAL; no replication or failover
- One instance per data directory (an exclusive file lock is taken on `data/` at startup; a second instance refuses to start). Two processes sharing a WAL overwrite each other — silent corruption. Scale out with MySQL/PostgreSQL instead
- Auto-increment primary keys are assigned by the engine: `INSERT` without a primary-key column (or with 0/NULL) gets a monotonic, never-reused id, returned through the MySQL protocol's last-insert-id so ORMs can read it back. Clients must not seed ids themselves with `MAX(pk)+1` — that collides across concurrent clients
- Entire dataset must fit in memory; restarts replay the whole WAL (seconds for millions of rows)
- SQL subset: no subqueries/derived tables, no window functions or recursive CTEs, no foreign keys; `JOIN` is a nested loop without an optimizer; no `SELECT ... FOR UPDATE`
- `INT` columns have no NULL semantics (NULL becomes 0); only `VARCHAR` holds NULL
- Optimistic concurrency with last-writer-wins (not snapshot isolation); conflicts are detected at COMMIT (`conflict: row N version changed`), so ORMs that wrap each statement in an implicit transaction never see a statement-level error - use `txn_conflict=last_write_wins` for MySQL-like autocommit semantics
- `VARCHAR` limited to 65534 bytes
- Single MySQL account; table grants apply to the native protocol
- Protocol ports are plain TCP — keep them on trusted networks
- Panel loads text web fonts (Roboto Flex / Fira Code) from a public CDN; icon font can be self-hosted under `assets/`, otherwise icons fall back to Unicode glyphs
- Stored procedures, views and triggers are protocol placeholders

---

## License

MIT — see [LICENSE](LICENSE).
