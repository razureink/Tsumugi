# Tsumugi

Tsumugi (つむぎ, Japanese for "spinning thread") is an embedded relational database implemented from scratch using **only the Go standard library** — zero third-party dependencies. It features a home-grown red-black tree index, WAL crash recovery, transactions, TTL, permissions, and backups, plus a Material Design 3 Expressive real-time monitoring dashboard with a built-in stress tester.

> An educational / learning-oriented project: compact code, no external dependencies, and a complete walkthrough of a storage engine, WAL, transactions, and monitoring.

---

## ✨ Features

- **Pure standard library**: build and run with a single `go build`.
- **Core storage engine**: custom Left-leaning Red-Black Tree (LLRB), primary-key index + generic secondary indexes.
- **Performance**: PK point-lookups skip full scans (O(log n)); buffered WAL writes with periodic fsync; hand-rolled byte encoding replaces reflection; WAL (version 1) uses varint + table-ID compression, `COMPACT` rewrites the WAL to reclaim disk space, and concurrent writes funnel through a single-writer FIFO queue (group commit in fsync mode) to eliminate lock contention.
- **Fast startup**: WAL recovery reads the file into memory in one pass (no per-field syscalls), replays records in parallel per table, and stores already-encoded rows directly. Measured: 1.24M rows / 81MB WAL restored in ~3s.
- **WAL crash recovery**: append-only Write-Ahead Log with CRC32 checksums, replayed on restart; truncated trailing records are discarded instead of blocking startup.
- **Transactions**: BEGIN / COMMIT / ROLLBACK with MVCC-style version-based conflict detection.
- **Group commit**: batches transaction fsyncs to improve write throughput.
- **TTL**: per-row expiration with a background cleanup loop.
- **Permissions**: GRANT / REVOKE table-level privileges (SELECT / INSERT / UPDATE / DELETE / DDL).
- **Backup**: one-shot snapshot of the WAL and privilege file into a backup directory.
- **Real-time monitoring**: HTTP `/metrics` (Prometheus format), `/api/stats` (JSON), and `/dashboard` (MD3 Expressive console). Five clickable rings (CPU / memory / QPS / TPS / disk-write-rate) expand into live curves; trend charts, system info, command table and stress tool included.
- **Data manager**: `/admin` (MD3 Expressive, phpMyAdmin-style) — table list, paginated row browser, SQL console, visual table creation; **login required** (token auth).
- **Unified console**: `/dashboard` and `/admin` share one sidebar layout and design language.
- **MySQL protocol**: optional (enable in the Settings page or via `mysql_enabled` in `config/tsumugi.json`), supports handshake + `mysql_native_password` auth + `COM_QUERY`; any MySQL client can connect and run SQL.
- **First-run wizard**: on first visit, a setup wizard walks you through creating the admin account; there is no default password.
- **Built-in stress test**: `/stress?duration=10&workers=4` to hammer the engine and watch QPS / TPS live.
- **System tables**: `__tables`, `__indexes`, `__stats` queryable via SELECT.

---

## 📦 Project Layout

```
tsumugi/
├── main.go          # Entry point
├── config.go        # Configuration loading
├── logger.go        # Logging utilities
├── db.go            # Storage engine: red-black trees, tables, WAL, txn, indexes, TTL, privileges, backup
├── server.go        # TCP server and binary command protocol handling
├── metrics.go       # Stats, Prometheus / JSON APIs, dashboard, stress test
├── go.mod           # Go module
└── examples/
    └── client.go    # Example client demonstrating the full protocol
```

---

## 🚀 Quick Start

```bash
# Start the server
go run .

# In another terminal, run the example client
go run ./examples
```

On first visit to <http://localhost:10232/> you'll be guided through a setup wizard to create the admin account, then sign in.

## Deployment

### Option 1: Docker Compose (recommended)

A Dockerfile and docker-compose.yml are included:

```bash
docker compose up -d --build
```

| Port | Purpose |
| ---- | ---- |
| 9999 | Binary protocol |
| 10232 | Dashboard / admin panel |
| 3309 | MySQL protocol |

Data is stored in `./data` (mounted as a volume, survives container removal). Useful commands:

```bash
docker compose logs -f          # follow logs
docker compose restart           # restart
docker compose down              # stop (data stays in ./data)
docker compose down -v           # stop and wipe data
```

MySQL protocol is enabled by default in `docker-compose.yml` (`TSUMUGI_MYSQL=true`), with a health check configured.

### Option 2: Build and run directly

Requires Go 1.21+:

```bash
# build (Linux)
go build -o tsumugi .

# run
./tsumugi
```

On Windows you get `tsumugi.exe` — run it from a terminal or double-click. `data/` and `config/` are auto-created next to the binary.

Or just:

```bash
go run .
```

### Option 3: Custom builds

```bash
# static build, portable to any machine
CGO_ENABLED=0 go build -o tsumugi .

# cross-compile (e.g. build a Linux binary on Windows)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tsumugi .
```

### Production notes

- **Ports**: expose 9999 and 10232 only to your intranet or behind a reverse proxy (Nginx/Caddy); open 3309 only if you need remote MySQL access.
- **Persistence**: `data/` holds the WAL plus user/privilege files — back it up. With Docker, make sure the `./data` volume is mounted.
- **Durability**: use `batch` for max throughput (may lose up to one flush interval, default 100ms, on crash); use `fsync` if you cannot afford to lose writes. Changes apply hot, no restart needed.
- **WAL compaction**: auto `COMPACT` is on by default (idle periods, WAL over 64MB); you can also trigger it manually in the Settings page.
- **Reverse proxy**: for HTTPS, forward `/`, `/dashboard`, `/admin` and `/api/*` to port 10232 — nothing extra to configure.
- **First-run flow**: start → visit 10232 → create the admin via the wizard → sign in. `data/users.json` stores SHA-256 hashes; there is no default password.

Expected output:

```
[ok] AUTH
[ok] CREATE TABLE users
[ok] INSERT id=1 name=Alice age=30
[ok] INSERT id=2 name=Bob age=25
[ok] INSERT id=3 name=Carol age=28
[rows] total=3
  - id=1 name=Alice age=30 (version=0)
  - id=2 name=Bob age=25 (version=0)
  - id=3 name=Carol age=28 (version=0)
[done]
```

### Monitoring Dashboard

After startup, visit:

- **Dashboard**: <http://localhost:10232/dashboard>
- **JSON API**: <http://localhost:10232/api/stats>
- **Prometheus metrics**: <http://localhost:10232/metrics>

The dashboard's header renders **four MD3 Expressive circular progress gauges** with live metrics:

| Gauge | Source |
| ---- | ---- |
| CPU usage | `runtime/metrics` (cumulative process CPU-time delta; cross-platform, stdlib only) |
| Memory usage | `runtime.ReadMemStats` (HeapAlloc / Sys) |
| QPS | all commands within the last 1-second sliding window |
| TPS | write commands within the last 1-second window (INSERT/UPDATE/DELETE/BATCH) |

Info chips below show total commands, errors, hottest command, uptime, and goroutines / cores, followed by a per-command table.

Click **Start Stress Test** on the dashboard, or trigger it directly:

```
curl "http://localhost:10232/stress?duration=10&workers=4"
```

The dashboard refreshes QPS / TPS and per-command stats every 2 seconds; watch the gauges and throughput change live while the stress test runs.

---

## ⚙️ Configuration

All settings live in `loadConfig()` inside `config.go`:

| Field | Default | Description |
| --- | --- | --- |
| `Port` | `9999` | TCP server port |
| `WALDir` | `./data` | Data directory |
| `WALFile` | `tsumugi.wal` | WAL file name |
| `PrivilegeFile` | `privileges.json` | Privilege store file |
| `User` / `Password` | created by setup wizard | Admin account (no default; set during first-run wizard) |
| `FlushInterval` | `100ms` | Periodic WAL fsync interval (fsync runs outside the write lock, so it never stalls writers) |
| `GroupCommitInterval` | `2ms` | Group-commit batching window |
| `TTLCleanInterval` | `30s` | TTL cleanup period |
| `IdleTimeout` | `60s` | Connection idle timeout |
| `BackupDir` | `./backup` | Backup directory |
| `MetricsPort` | `10232` | Monitoring HTTP port |
| `EnableChecksum` | `true` | CRC32 checksums in WAL |
| `Durability` | `batch` | Persistence mode: `batch` (periodic batched fsync, max throughput, may lose up to one flush-interval of writes on crash); `fsync` (fsync every write, real-time durability, throughput capped by disk fsync speed) |

> `Durability` can also be set via the `TSUMUGI_DURABILITY` environment variable (`batch` / `fsync`) without recompiling.

---

## 📡 Binary Protocol

Clients communicate over TCP using a big-endian binary protocol. Every request starts with a **1-byte command code**.

### Command Codes

| Command | Code | Command | Code |
| --- | --- | --- | --- |
| AUTH | 10 | BEGIN | 40 |
| PING | 11 | COMMIT | 41 |
| CREATE TABLE | 20 | ROLLBACK | 42 |
| DROP TABLE | 21 | CREATE INDEX | 50 |
| DESCRIBE | 22 | BATCH | 55 |
| ALTER TABLE | 23 | BACKUP | 56 |
| INSERT | 30 | CREATE PROC / CALL | 60 / 61 |
| SELECT | 31 | CREATE VIEW | 70 |
| UPDATE | 32 | CREATE TRIGGER | 80 |
| DELETE | 33 | GRANT | 90 |
| | | REVOKE | 91 |

### Response Codes

| Response | Code | Description |
| --- | --- | --- |
| OK | 1 | Success |
| ERROR | 2 | Failure (`uint32` length + UTF-8 message) |
| VALUE | 3 | Single value |
| NOT_FOUND | 4 | Not found |
| ROWS | 5 | Row set (`uint32` count; each row = `uint32` length + encoded data) |
| TXN_ID | 6 | Transaction ID (`uint64`) |

### Field Types (wire encoding)

| Type | Byte value |
| --- | --- |
| TypeInt (int64) | `0` |
| TypeVarchar | `1` |
| TypeBool | `2` |

> See `examples/client.go` for a complete reference implementation of AUTH / CREATE TABLE / INSERT / SELECT encoding and decoding.

---

## 🏗️ Architecture

```
┌──────────────────────────────────────────────┐
│                 TCP Client                    │
└──────────────────┬───────────────────────────┘
                   │ binary protocol (big-endian)
┌──────────────────▼───────────────────────────┐
│              server.go (Server)               │
│  Session · Auth · command dispatch · encode   │
└──────────────────┬───────────────────────────┘
                   │
┌──────────────────▼───────────────────────────┐
│                db.go (DB engine)              │
│  ┌────────┐  ┌────────┐  ┌──────────────┐    │
│  │Catalog │  │ Tables │  │  Transaction │    │
│  │  meta  │  │  set   │  │  management  │    │
│  └────────┘  └───┬────┘  └──────────────┘    │
│                  │                            │
│   ┌──────────────▼──────────────┐             │
│   │          Table              │             │
│   │  pkTree: IntRBTree (PK)     │             │
│   │  idxTrees: RBTree (2nd idx) │             │
│   │  writeLog → WAL (CRC32)     │             │
│   └─────────────────────────────┘             │
│    WAL replay · TTL cleanup · group commit    │
│    · backup                                   │
└──────────────────┬───────────────────────────┘
                   │
┌──────────────────▼───────────────────────────┐
│            metrics.go (monitoring)            │
│  Stats · /metrics · /api/stats · /dashboard  │
│  /stress concurrency test (read/write mix)   │
└──────────────────────────────────────────────┘
```

### Data Files

- `data/tsumugi.wal`: WAL file with header (magic `TSUMUGI` + version + reserved). Compact format (version 1): varint encoding + table IDs (via a table registry, replacing repeated table-name strings); data records are `[cmd][tableID][txnID][pk][len][data][CRC32]`, catalog records use `cmd=100`; fixed per-row overhead drops from ~38B to ~10B. Reads and writes use this single compact format; reads also accept files written during the v2-labeled window (same format).
- `data/privileges.json`: privilege data.
- `backup/tsumugi_<timestamp>.wal` and `backup/privileges_<timestamp>.json`: backup snapshots.

---

## 🔌 Command Overview

| Command | Description |
| --- | --- |
| `AUTH(user, pass)` | Authenticate; returns OK or error |
| `CREATE TABLE(meta)` | Create a table (fields, PK, indexes); persists catalog |
| `DROP TABLE(name)` | Drop a table |
| `DESCRIBE(name)` | Return schema and indexes |
| `ALTER TABLE(name, field, type)` | Add a column |
| `INSERT(name, pk, ttl, fields...)` | Insert a row, supports TTL |
| `SELECT(name, conditions, minKey, maxKey)` | Conditional query (secondary index first) |
| `UPDATE(name, pk, ttl, fields...)` | Update (bumps row version) |
| `DELETE(name, pk)` | Delete a row |
| `BEGIN` / `COMMIT` / `ROLLBACK` | Transaction control |
| `CREATE INDEX(table, idxName, field)` | Create an index and backfill it |
| `BACKUP` | Snapshot backup |
| `GRANT` / `REVOKE(user, table, perm)` | Privilege management |
| SELECT `__tables` / `__indexes` / `__stats` | System-table queries |

> `BATCH`, stored procedures, views, and triggers are protocol placeholders that currently return `not implemented`.

---

## 🧪 Verification

Validated on `go1.26.5` / Windows:

1. `go vet ./...` passes with no warnings.
2. End-to-end smoke test: AUTH → CREATE TABLE → INSERT ×3 → SELECT, all correct.
3. WAL recovery: after restart, schema and data are fully restored (a duplicate-PK error on re-insert proves data survived).
4. Stress test: `/stress?duration=3&workers=4` produced ~26k commands with 0 errors.
5. `/metrics`, `/api/stats`, and `/dashboard` all respond correctly.

---

## 📝 Notes

- This is an educational / learning project. WAL uses a **single-writer queue**: all writes are handed to one writer goroutine over a FIFO channel and serialized there, so the hot path has no global mutex; in `fsync` mode the writer does **group commit** (concurrent writers share one fsync). Measured ~900k QPS in batch mode at 1000 threads, and ~30x higher QPS in fsync mode vs. per-write flush.
- Stored procedures / views / triggers are protocol placeholders ready to be implemented.
- To run under Docker / Linux: `docker run --rm -v "$PWD":/app -w /app golang:1.24 go run .`
