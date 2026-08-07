# Tsumugi

Tsumugi（つむぎ，日语"纺线"）是一个用 Go 标准库从零写的内存关系型数据库，零第三方依赖。内置红黑树索引、WAL 崩溃恢复、事务、TTL、权限、备份，带一个 Material Design 3 Expressive 风格的实时监控面板和内置压测工具。

项目定位是教学 / 学习向，代码精简、没有外部依赖，适合顺着读一遍存储引擎、WAL、事务和监控的实现。

## 特性

- 纯标准库，`go build` 一条命令跑起来
- 自研 Left-leaning Red-Black Tree（LLRB）做主键索引，支持通用二级索引
- 主键点查直接命中（O(log n)），不做全表扫描
- WAL 追加写 + CRC32 校验，重启自动重放恢复；尾部残缺记录自动截断而不是拒绝启动
- WAL 用 varint + 表 ID 压缩编码，小行写入固定开销从 ~38B 降到 ~10B；支持 `COMPACT` 重写 WAL 回收空间
- 单写者 channel 队列串行落盘，消除热路径锁竞争；fsync 模式按批组提交
- WAL 恢复一次读入内存解析 + 按表并行重放，124 万行 / 81MB 恢复约 3 秒
- 事务：BEGIN / COMMIT / ROLLBACK，基于 MVCC 版本号冲突检测
- 行级 TTL，后台协程自动清理
- GRANT / REVOKE 表级权限（SELECT / INSERT / UPDATE / DELETE / DDL）
- 一键备份 WAL 与权限文件到备份目录
- 实时监控：`/metrics`（Prometheus 格式）、`/api/stats`（JSON）、`/dashboard`（MD3 Expressive 面板，5 个可点击环形指标 + 实时曲线）
- 数据管理面板 `/admin`：表列表、分页浏览、SQL 控制台、可视化建表，与 dashboard 共用侧边栏布局
- MySQL 协议兼容：握手 + mysql_native_password 认证 + COM_QUERY，任意 MySQL 客户端可连
- 内置压测：`/stress?duration=10&workers=4`，边压边看 QPS / TPS
- 系统表：`__tables`、`__indexes`、`__stats` 可直接 SELECT

## 快速开始

首次访问 <http://localhost:10232/> 会进入安装向导，按提示创建管理员账号后即可登录控制台。

## 部署

### 方式一：Docker Compose（推荐）

仓库自带 `Dockerfile` 与 `docker-compose.yml`，一条命令起服务：

```bash
docker compose up -d --build
```

| 端口 | 用途 |
| ---- | ---- |
| 9999 | 二进制协议 |
| 10232 | 监控 / 管理面板 |
| 3309 | MySQL 兼容协议 |

数据落在 `./data` 目录（容器内挂载为 volume，删除容器不丢数据）。常用操作：

```bash
docker compose logs -f          # 看日志
docker compose restart           # 重启
docker compose down              # 停止（数据保留在 ./data）
docker compose down -v           # 停止并清空数据
```

`docker-compose.yml` 里已默认开启 MySQL 协议（`TSUMUGI_MYSQL=true`）并设置健康检查。

### 方式二：直接编译运行

需要 Go 1.21+：

```bash
# 编译（Linux 直接 build 即可）
go build -o tsumugi .

# 启动
./tsumugi
```

Windows 上编译出来的是 `tsumugi.exe`，双击或命令行运行都行。数据、配置在程序所在目录的 `data/`、`config/` 下自动生成。

也可以不编译直接跑：

```bash
go run .
```

### 方式三：自定义编译（如去掉 MySQL 或调整构建参数）

```bash
# 静态编译，适合塞进小镜像 / 拷贝到任意机器
CGO_ENABLED=0 go build -o tsumugi .

# 跨平台编译（比如在 Windows 上编译 Linux 版）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tsumugi .
```

### 部署到生产环境的建议

- **端口**：二进制协议 9999、面板 10232 建议只对内网或加反向代理（Nginx/Caddy）暴露；MySQL 3309 按需开放。
- **数据持久化**：`data/` 里是 WAL 文件 + 用户/权限配置，务必做好备份；Docker 部署时确认 `./data` 卷挂载成功。
- **刷盘策略**：追求性能用 `durability=batch`（崩溃最多丢一个刷盘周期，默认 100ms）；数据不能丢用 `fsync`。写配置可热生效，不用重启。
- **WAL 整理**：默认开启自动 `COMPACT`（低峰期、WAL 超 64MB 时触发），也可在设置页手动"立即整理 WAL"。
- **反向代理**：面板走 HTTPS 时，把 `/`、`/dashboard`、`/admin`、`/api/*` 都转发到 10232 即可，无需额外配置。
- **首次部署流程**：启动 → 访问 10232 → 安装向导创建管理员 → 进控制台。`data/users.json` 里存的是 SHA-256 哈希，不会出现默认口令。

### 监控面板

- Dashboard：<http://localhost:10232/dashboard>
- JSON API：<http://localhost:10232/api/stats>
- Prometheus：<http://localhost:10232/metrics>
- 数据管理：<http://localhost:10232/admin>

Dashboard 的 5 个环形指标（CPU / 内存 / QPS / TPS / 磁盘写入）都可以点击，展开最近 60 秒的实时曲线，含当前值 / 峰值 / 均值 / 最低 / 采样点数。面板还有实时趋势区、系统信息、命令明细和压测工具，每 2 秒自动刷新。

## 配置

### config/tsumugi.json

服务启动时自动生成 `config/tsumugi.json`，是配置的唯一事实来源，也可在设置页里改（热生效并落盘）。

主要字段：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `port` | `9999` | 二进制协议端口 |
| `wal_dir` | `./data` | 数据目录 |
| `wal_file` | `tsumugi.wal` | WAL 文件名 |
| `privilege_file` | `privileges.json` | 权限存储文件 |
| `flush_interval_ms` | `100` | WAL 定时 fsync 间隔 |
| `group_commit_ms` | `2` | 组提交合并窗口 |
| `ttl_clean_ms` | `30000` | TTL 清理周期 |
| `idle_timeout_s` | `60` | 连接空闲超时 |
| `backup_dir` | `./backup` | 备份目录 |
| `metrics_port` | `10232` | 监控 / 管理面板端口 |
| `enable_checksum` | `true` | WAL CRC32 校验 |
| `durability` | `batch` | `batch` 定时批量 fsync，吞吐高，崩溃最多丢一个刷盘周期；`fsync` 每条写立即落盘，实时持久化 |
| `mysql_enabled` | `false` | 是否开启 MySQL 兼容协议 |
| `mysql_port` | `3309` | MySQL 协议监听端口 |

兼容旧的 `.env` 写法，启动时若发现 `.env` 会把它迁移进 `config/tsumugi.json` 然后删掉 `.env`，之后的配置统一走文件。

### MySQL 协议兼容

设置页里勾选"启用 MySQL 协议服务"，或直接把 `config/tsumugi.json` 里 `mysql_enabled` 改为 `true` 并重启，服务会多监听一个 3309 端口：

```bash
mysql -h 127.0.0.1 -P 3309 -u <管理员账号> -p
mysql -e "CREATE TABLE t(id INT PRIMARY KEY, name VARCHAR(20)); INSERT INTO t VALUES (1,'a'); SELECT * FROM t;"
```

数据与 TCP 二进制协议共用同一存储引擎和 WAL，两边互相可见。

## 二进制协议

客户端与服务器用 TCP 大端序通信，每个请求以 1 字节命令码开头。

### 命令码

| 命令 | 码 | 命令 | 码 |
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

### 响应码

| 响应 | 码 | 说明 |
| --- | --- | --- |
| OK | 1 | 成功 |
| ERROR | 2 | 失败（uint32 长度 + UTF-8 消息） |
| VALUE | 3 | 单值 |
| NOT_FOUND | 4 | 未找到 |
| ROWS | 5 | 行集（uint32 行数，每行 uint32 长度 + 编码数据） |
| TXN_ID | 6 | 事务 ID（uint64） |

### 字段类型

| 类型 | 字节值 |
| --- | --- |
| TypeInt (int64) | `0` |
| TypeVarchar | `1` |
| TypeBool | `2` |

完整编解码参考 `examples/client.go`。

## 架构

```
┌──────────────────────────────────────────────┐
│                 TCP Client                    │
└──────────────────┬───────────────────────────┘
                   │ 二进制协议 (大端序)
┌──────────────────▼───────────────────────────┐
│              server.go (Server)               │
│  Session · Auth · 命令分发 · 响应编码         │
└──────────────────┬───────────────────────────┘
                   │
┌──────────────────▼───────────────────────────┐
│                db.go (DB 引擎)                │
│  ┌────────┐  ┌────────┐  ┌──────────────┐    │
│  │Catalog │  │ Tables │  │  Transaction │    │
│  │(表元数据)│  │ 表集合  │  │   事务管理    │    │
│  └────────┘  └───┬────┘  └──────────────┘    │
│                  │                            │
│   ┌──────────────▼──────────────┐             │
│   │          Table              │             │
│   │  pkTree: IntRBTree (主键)    │             │
│   │  idxTrees: RBTree  (二级索引) │             │
│   │  writeLog → WAL (CRC32)     │             │
│   └─────────────────────────────┘             │
│    WAL 重放 · TTL 清理 · 组提交 · 备份         │
└──────────────────┬───────────────────────────┘
                   │
┌──────────────────▼───────────────────────────┐
│            metrics.go (监控)                  │
│  Stats · /metrics · /api/stats · /dashboard  │
│  /stress 压力测试 (并发读写)                   │
└──────────────────────────────────────────────┘
```

### 数据文件

- `data/tsumugi.wal`：WAL 文件，文件头（magic `TSUMUGI` + version + reserved）+ 紧凑记录（varint 变长整数 + 表 ID + 主键 + 数据长度 + 数据 + CRC32），Catalog 记录用 cmd=100 标记
- `data/users.json`：管理员账号（SHA-256 哈希存储）
- `data/privileges.json`：表权限
- `backup/tsumugi_<时间戳>.wal`、`backup/privileges_<时间戳>.json`：备份快照

## 主要 API

| 命令 | 功能 |
| --- | --- |
| `AUTH(user, pass)` | 登录认证 |
| `CREATE TABLE(meta)` | 建表（字段、主键、索引），持久化 Catalog |
| `DROP TABLE(name)` | 删表 |
| `DESCRIBE(name)` | 返回表结构与索引 |
| `ALTER TABLE(name, field, type)` | 增加列 |
| `INSERT(name, pk, ttl, fields...)` | 插入，支持 TTL |
| `SELECT(name, conditions, minKey, maxKey)` | 条件查询（二级索引优先） |
| `UPDATE(name, pk, ttl, fields...)` | 更新（版本号递增） |
| `DELETE(name, pk)` | 删除 |
| `BEGIN` / `COMMIT` / `ROLLBACK` | 事务控制 |
| `CREATE INDEX(table, idxName, field)` | 建二级索引并回填 |
| `BACKUP` | 快照备份 |
| `GRANT` / `REVOKE(user, table, perm)` | 权限管理 |
| SELECT `__tables` / `__indexes` / `__stats` | 系统表查询 |

`BATCH`、存储过程、视图、触发器目前是占位实现，返回 `not implemented`。

## 测试

在 `go1.26.5` / Windows 下验证过：

- `go vet ./...` 无告警
- 端到端冒烟：AUTH → CREATE TABLE → INSERT ×3 → SELECT
- WAL 恢复：重启后表结构与数据完整恢复
- 压测：`/stress?duration=3&workers=4` 约 2.6 万条命令，错误数 0
- `/metrics`、`/api/stats`、`/dashboard` 均正常

## 协议

MIT，见 [LICENSE](LICENSE)。

## 说明

- WAL 采用单写者队列：所有写记录经 FIFO 通道交给唯一 writer goroutine 串行落盘，热路径无全局互斥锁；fsync 模式下 writer 对并发写者做组提交（同批共享一次落盘）。实测 1000 并发读写 batch 模式 QPS ~90 万，fsync 模式比每写必落的朴素实现 QPS 提升约 30 倍。
- 存储过程 / 视图 / 触发器为协议占位，可按需扩展。
