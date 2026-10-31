# Tsumugi

Go 标准库实现的**单机内存关系型数据库**：数据常驻内存、WAL 落盘持久化、自带监控与管理面板，并兼容 MySQL 线协议。零第三方依赖，`go build` 一条命令即可构建。

主要用于**中小规模、单机、需要简单 SQL 与自带运维面板**的场景（缓存/会话/配置/指标/元数据类数据），也适合作为存储引擎的学习样本。

---

## 目录

- [适用边界](#适用边界)
- [特性](#特性)
- [快速开始](#快速开始)
- [生产部署](#生产部署)
  - [容量与硬件规划](#容量与硬件规划)
  - [部署拓扑](#部署拓扑)
  - [配置参考](#配置参考)
  - [安全清单](#安全清单)
  - [持久化与备份恢复](#持久化与备份恢复)
  - [监控与告警](#监控与告警)
  - [日志与审计](#日志与审计)
  - [升级与回滚](#升级与回滚)
  - [故障排查](#故障排查)
  - [性能基线与调优](#性能基线与调优)
- [HTTP 接口](#http-接口)
- [SQL 与协议](#sql-与协议)
- [数据文件与崩溃恢复](#数据文件与崩溃恢复)
- [架构](#架构)
- [开发与测试](#开发与测试)
- [已知限制](#已知限制)
- [许可](#许可)

---

## 适用边界

**适合**

- 单机部署，数据量能完整放进内存（建议 ≤ 可用内存的 50%）
- 读多写少，或写吞吐在单机可承受范围（见[性能基线](#性能基线与调优)）
- 需要表级权限、审计日志、自带监控面板与压测工具的内部系统
- 希望一个二进制 + 一个数据目录就能跑起来，不引外部依赖的场景

**不适合**

- 数据量远超可用内存（当前没有磁盘冷数据层，WAL 只用于恢复）
- 需要多副本 / 分片 / 跨机房高可用（当前是单进程、单写者 WAL）
- 复杂分析型 SQL：不支持 `JOIN`、`GROUP BY`、`HAVING`、子查询、窗口函数、外键
- 需要严格可串行化隔离：当前是乐观并发控制，同一行并发写时**后写者胜出**，先提交方收到冲突并回滚
- 需要行级锁、`SELECT ... FOR UPDATE`、游标

---

## 特性

- **零依赖**：纯 Go 标准库，`CGO_ENABLED=0` 静态编译，可塞进 scratch/alpine 镜像
- **自研 LLRB 索引**：主键点查 O(log n)，二级索引按不可变快照发布，读路径无锁
- **WAL 持久化**：追加写 + CRC32 + varint 紧凑编码，v3 批次分帧（长度 + 整批校验和）支持流式重放；尾部残缺批次自动截断而不是拒绝启动；单写者队列 + 组提交
- **事务**：`BEGIN / COMMIT / ROLLBACK`，写时记录行版本，提交时检测冲突
- **行级 TTL**：后台协程清理过期行
- **权限**：表级 `GRANT / REVOKE`（SELECT / INSERT / UPDATE / DELETE / DDL），引擎层统一校验
- **MySQL 协议兼容**：握手、`mysql_native_password`、文本协议、预处理语句（二进制行协议）、`COM_FIELD_LIST`、`COM_SET_OPTION`
- **管理面板**：实时监控（5 个环形指标 + 曲线）、数据浏览、SQL 控制台、可视化建表、用户管理、备份与 WAL 整理
- **面板主题**：7 种色调 + 「高对比」模式（纯黑白、2px 实边、去除渐变与柔影）；入口在「设置 → 色调」，选择后立即生效并记在浏览器本地
- **可观测**：`/metrics`（Prometheus）、`/api/stats`（JSON）、`/healthz`、结构化审计日志
- **内置压测**：`/stress` 走真实客户端协议，覆盖 native 与 MySQL 两种链路

---

## 快速开始

### 方式一：Docker Compose

```bash
docker compose up -d --build
```

启动后访问 <http://localhost:10232/>，按向导创建管理员（安装接口只接受**本机/内网**来源）。

| 端口 | 用途 |
| ---- | ---- |
| 9999 | 二进制协议 |
| 10232 | 监控 / 管理面板 |
| 3309 | MySQL 兼容协议 |

数据落在 `./data`、配置在 `./config`、备份在 `./backup`（已挂载为 volume）。

```bash
docker compose logs -f      # 日志
docker compose restart      # 重启（SIGTERM，优雅冲刷 WAL）
docker compose down         # 停止，数据保留
docker compose down -v      # 停止并清空
```

把 `tsumugi` / `tsumugi-cli` 装到宿主机：

```bash
sudo ./build.sh
tsumugi --help
tsumugi-cli --help
```

### 方式二：直接编译

```bash
# 需要 Go 1.21+（本仓库在 go1.27 上验证）
go build -o tsumugi .
./tsumugi
```

首次启动会在当前目录生成 `config/tsumugi.json`、`data/`、`backup/`。

### 方式三：作为 Go 依赖阅读/改造

```bash
go run .
```

### 客户端示例

```bash
# 二进制协议：examples/client.go
go run ./examples

# MySQL 协议（需先在设置页或配置里开启）
mysql -h 127.0.0.1 -P 3309 -u <管理员账号> -p
mysql> CREATE TABLE t(id INT PRIMARY KEY, name VARCHAR);
mysql> INSERT INTO t VALUES (1, 'a');
mysql> SELECT * FROM t;
```

---

## 生产部署

### 容量与硬件规划

| 项 | 建议 |
| --- | --- |
| 内存 | 预留「数据量 × 1.5~2」：行字节 + 主键/二级索引 + WAL 恢复期临时占用 |
| 磁盘 | WAL 大小 ≈ 数据量 × 2~3（自动 COMPACT 前）；建议独立 SSD 数据盘，XFS/ext4 |
| CPU | 读路径并行度好；写路径受单写者 WAL 与 fsync 能力约束，4~8 核通常够用 |
| 文件句柄 | ≥ `max_conns` × 2 + 余量（systemd 示例用 65536） |
| 网卡 | 单机千兆足够；二进制协议单连接万级 QPS，多连接合计可达十万级（视硬件） |

`durability=fsync` 时写吞吐受磁盘 fsync 限制（普通 SSD 单盘约 1k~5k 次/秒，组提交会把并发写合并成一次 fsync）。写入数据不能丢时选 `fsync`；能容忍「崩溃丢最后 100ms」时选 `batch`（默认）。

### 部署拓扑

```
            公网 / 办公网
                  │ HTTPS (443)
          ┌───────▼────────┐
          │ Nginx / Caddy  │  TLS 终止、限流、IP 白名单
          └───────┬────────┘
                  │ http://127.0.0.1:10232
        ┌─────────▼──────────┐
        │      Tsumugi       │  bind_host=127.0.0.1
        │  10232 面板        │
        │  9999  二进制协议  │◄── 仅内网 / 安全组限制
        │  3309  MySQL 协议  │◄── 仅内网 / 安全组限制
        └─────────┬──────────┘
                  │
          /var/lib/tsumugi/{data,config,backup}
```

- 面板**不要**直接暴露公网：`bind_host: "127.0.0.1"` + 反向代理（示例见 `deploy/nginx-panel.conf.example`）
- 反代后设置 `trusted_proxies: ["127.0.0.1/32"]`，否则登录限速会按代理 IP 统计（所有用户共享配额）
- 二进制/MySQL 是裸 TCP，无法用 HTTP 反代；只在内网开放，必要时用四层代理 + ACL
- 系统服务用 `deploy/tsumugi.service`（已含 `ProtectSystem=strict`、`ReadWritePaths`、`NoNewPrivileges`）

```bash
sudo useradd --system --home /var/lib/tsumugi --shell /usr/sbin/nologin tsumugi
sudo install -d -o tsumugi -g tsumugi -m 0700 /var/lib/tsumugi
sudo install -m 0755 tsumugi /usr/local/bin/tsumugi
sudo install -m 0644 deploy/tsumugi.service /etc/systemd/system/tsumugi.service
sudo systemctl daemon-reload && sudo systemctl enable --now tsumugi
```

**首次初始化**：启动后访问面板 → 安装向导创建管理员。安装接口只接受本机/私网来源；管理员口令同时用于 MySQL 协议的 root 账号（只存 SHA1 摘要）。初始化完成后 `config/tsumugi.json` 中会写入 `password_sha1` 与自动生成的 `metrics_token`。

### 配置参考

唯一配置源：`config/tsumugi.json`（启动时自动生成，设置页可热更新并落盘）。

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `port` | `9999` | 二进制协议端口 |
| `bind_host` | `""` | 监听地址。空 = 所有网卡；生产建议 `127.0.0.1` + 反代 |
| `tls_cert` / `tls_key` | 空 | 面板 HTTPS 证书；两者都配置才生效（否则明文 HTTP） |
| `max_conns` | `512` | 并发连接上限（二进制与 MySQL 共用） |
| `wal_dir` | `./data` | 数据目录（含 WAL、users.json、privileges.json） |
| `wal_file` | `tsumugi.wal` | WAL 文件名 |
| `privilege_file` | `privileges.json` | 表级权限文件 |
| `flush_interval_ms` | `100` | batch 模式定时 fsync 间隔 |
| `group_commit_ms` | `2` | 组提交合并窗口 |
| `ttl_clean_ms` | `30000` | TTL 清理周期 |
| `idle_timeout_s` | `60` | 连接空闲超时（未认证连接固定 30s） |
| `backup_dir` | `./backup` | 备份目录 |
| `metrics_port` | `10232` | 面板端口 |
| `enable_checksum` | `true` | WAL CRC32 校验（建议保持开启） |
| `durability` | `batch` | `batch` 定时批刷 / `fsync` 每次提交落盘 |
| `mysql_enabled` | `false` | 是否开启 MySQL 协议 |
| `mysql_port` | `3309` | MySQL 协议端口 |
| `password_sha1` | 空 | MySQL 认证用摘要；为空时拒绝 MySQL 登录（初始化向导会写入） |
| `metrics_token` | 自动生成 | `/metrics` 抓取令牌 |
| `trusted_proxies` | `[]` | 可信反代 CIDR 列表，命中才采信 `X-Forwarded-For` |
| `auto_compact` | `true` | 低峰期自动整理 WAL |
| `compact_idle_seconds` | `60` | 自动整理检查间隔（秒） |
| `compact_min_wal_mb` | `64` | WAL 超过该大小才自动整理 |
| `compact_peak_rate` | `50` | 最近 1 秒命令数低于该值视为低峰 |
| `txn_conflict` | `error` | 事务写冲突策略：`error`=冲突报错回滚（乐观并发）；`last_write_wins`=后写者直接覆盖（MySQL autocommit 语义，适合 ORM 默认给每条语句套隐式事务的场景） |

环境变量可覆盖同名项（优先级：环境变量 > 配置文件）：

```
TSUMUGI_PORT TSUMUGI_BIND_HOST TSUMUGI_METRICS_PORT
TSUMUGI_MYSQL TSUMUGI_MYSQL_PORT TSUMUGI_DURABILITY
TSUMUGI_MAX_CONNS TSUMUGI_TLS_CERT TSUMUGI_TLS_KEY
TSUMUGI_TRUSTED_PROXIES TSUMUGI_AUTO_COMPACT TSUMUGI_TXN_CONFLICT
```

历史 `.env` 会在启动时迁入配置文件并删除；启动阶段会校验配置（端口冲突直接拒绝启动，明文/非 fsync/全网卡监听会打 WARN）。

### 安全清单

上生产前逐项确认：

- [ ] 管理员使用强口令，并已删除/禁用安装向导遗留账号
- [ ] `bind_host` 设为 `127.0.0.1`，面板由 TLS 反代暴露（或配置 `tls_cert`/`tls_key`）
- [ ] 配置 `trusted_proxies`，登录限速按真实客户端 IP 生效
- [ ] 9999 / 3309 只对可信网段开放（安全组/防火墙）
- [ ] `/metrics` 使用 `metrics_token` 抓取，令牌只存放在监控系统的凭据文件中
- [ ] `data/`、`config/`、`backup/` 权限 0700，所属用户为服务账号（WAL 与备份含全量数据）
- [ ] 采集审计日志（`audit:` 前缀），并接入日志平台
- [ ] 配置定时备份（`POST /api/admin/backup` 或 `BACKUP` 命令）并做**恢复演练**
- [ ] 明确 RPO：数据不能丢时 `durability=fsync`
- [ ] `enable_checksum` 保持 `true`
- [ ] 面板图标字体已自托管（见下节）：内网/离线环境不放字体文件会降级为 Unicode 字形

### 面板图标字体（网页优先，逐级兜底）

面板图标使用 Material Symbols Outlined（M3E 图标集），默认仍和以前一样从 Google Fonts 获取，取不到时按以下顺序补救：

1. **Google Fonts**：`fonts.googleapis.com`（页面里原有的 `<link>`，保持默认行为）
2. **镜像 CDN**：主站不通时自动追加 `fonts.googleapis.cn`，再不行用 `fonts.loli.net`；成功后图标外观与 Google 一致
3. **自托管字体**：把 `material-symbols-outlined.woff2` 放到 `assets/`，面板通过 `/assets/material-symbols-outlined.woff2` 加载；放置/删除都不需要重启
4. **单色字符**：以上全部不可用（纯内网/断网）时，把图标名换成**单色字符符号**（`add → ＋`、`chevron_left → ‹`），不使用 emoji，功能不受影响

用 `TSUMUGI_ICON_FALLBACK=off` 可以关掉第 4 步（完全保持原来的 CDN 行为，失败就显示原始连字文字）。字体获取方式见 `assets/README.md`；Docker 镜像只拷贝二进制，容器内没有 `assets/`，但第 1、2 步仍可用。

### 持久化与备份恢复

**写入语义**

| 模式 | 崩溃可能丢失 | 适用 |
| --- | --- | --- |
| `batch`（默认） | 最近一个 `flush_interval_ms`（默认 100ms）内已确认的写 | 可容忍少量丢失、追求吞吐 |
| `fsync` | 无（已确认写均落盘） | 数据不可丢 |

WAL 采用单写者队列：所有写记录经 FIFO 交给唯一 writer，fsync 模式对并发写者做组提交。writer 会把 `Write/Flush/Sync` 的错误回传给调用方，磁盘满/IO 错误不会伪装成提交成功；出错后进入故障态，后续写入返回错误。

WAL 文件格式为 v3：记录体仍是 varint 紧凑编码，但每批记录前有 `[varint 批次长度][CRC32(整批)]`。因此重放按批流式读入，内存占用只与**单批大小**（默认 ≤1MB）相关，而不是整个 WAL 文件；校验也按批一次完成。v1/v2 文件仍可直接读取，首次 COMPACT 后写成 v3。

**WAL 整理（COMPACT）**：把 WAL 重写为当前内存态的精简快照。默认在低峰且 WAL > 64MB 时自动执行，也可手动触发（面板按钮 / `POST /api/admin/compact`）。

**备份**

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" http://127.0.0.1:10232/api/admin/backup
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:10232/api/admin/backup   # 列出已有备份
```

生成 `backup/tsumugi_<时间戳>.wal`（WAL 快照，含全量数据）与 `backup/privileges_<时间戳>.json`，权限 0600。也可用 CLI `backup` 或二进制协议 `BACKUP(56)`。

**恢复**

```bash
systemctl stop tsumugi
cp backup/tsumugi_20260101_030000.wal /var/lib/tsumugi/data/tsumugi.wal
# 可选：恢复权限
cp backup/privileges_20260101_030000.json /var/lib/tsumugi/data/privileges.json
systemctl start tsumugi
```

恢复的是「备份时刻」的完整数据；备份之后的写入不在其中。备份文件就是同格式的 WAL 快照：当前版本读取 v1/v2/v3，但**旧版本二进制读不了 v3 文件**，降级前需先用旧版本可读的备份或导出 SQL。

### 监控与告警

```bash
curl -H "Authorization: Bearer $METRICS_TOKEN" http://127.0.0.1:10232/metrics
```

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `tsumugi_total_commands` | counter | 命令总数 |
| `tsumugi_total_errors` | counter | 错误总数 |
| `tsumugi_qps` / `tsumugi_tps` | gauge | 最近 1s 命令/写速率 |
| `tsumugi_wal_write_mb_s` | gauge | WAL 写入速率 |
| `tsumugi_wal_total_mb` | counter | 累计写入字节 |
| `tsumugi_fsync_per_s` | gauge | fsync 速率 |
| `tsumugi_cmd_<name>` | counter | 各命令计数 |

抓取配置见 `deploy/prometheus.yml`。建议告警：

- `/healthz` 连续失败（服务不可用）
- `rate(tsumugi_total_errors[5m])` 持续 > 0（写失败/权限异常）
- `durability=fsync` 时 `tsumugi_fsync_per_s` 掉到 0 而 QPS > 0（落盘异常）
- `tsumugi_wal_total_mb` 增长与业务量不匹配（COMPACT 未生效）
- 面板 `/api/stats` 的 `total_rows`、`wal_file_mb` 接近容量规划上限

### 日志与审计

日志输出到 stdout（容器请用 json-file/远端驱动采集），带时间戳与级别（`[OK] [INFO] [WARN] [ERR]`）。

审计事件（`audit:` 前缀）：

```
audit: user=admin action=create_table table=orders fields=5
audit: user=admin action=drop_table table=orders
audit: user=admin action=grant target=bob table=orders perm=1
audit: user=admin action=sql kind=4 sql="UPDATE orders SET ..."
login FAILED from 10.0.0.8 (user="root")
```

登录失败会按 (IP, 用户名) 计数并指数退避，二进制/MySQL 入口失败即断开连接。

### 升级与回滚

```bash
systemctl stop tsumugi
cp -a /var/lib/tsumugi/data /var/lib/tsumugi/data.bak.$(date +%F)
install -m 0755 tsumugi-new /usr/local/bin/tsumugi
systemctl start tsumugi
curl -fsS http://127.0.0.1:10232/healthz
```

- 配置新增字段都有默认值，旧配置文件可直接启动
- WAL 读取向后兼容（v1/v2/v3 均可读）；但新版本写出的是 v3，旧二进制不认识，**降级需先导出数据**（备份文件同样是 v3）
- 升级前建议先做一次 `POST /api/admin/backup`

### 故障排查

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| 启动即退出：`port conflicts` | `port`/`metrics_port`/`mysql_port` 重复 | 改端口后重启 |
| 启动失败：`invalid WAL magic` / `unsupported WAL version` | WAL 文件被覆盖或来自更高版本 | 用最近备份恢复；不要手工编辑 WAL |
| 启动时日志 `tail record truncated` | 崩溃导致 WAL 尾部残缺 | 正常现象，已自动截断；确认数据完整性后继续 |
| 写入报 `wal: ...` 错误 | 磁盘满 / 权限 / IO 故障 | 清理磁盘、修正目录属主；服务已进入故障态需重启 |
| 客户端 `too many connections` | 超过 `max_conns` | 提高上限或排查连接泄漏 |
| 登录返回 `bad_credentials` 且持续失败 | 失败次数触发退避 | 等待退避窗口（最长 5 分钟）或重启进程 |
| MySQL 连不上：`Access denied` | `password_sha1` 为空 / 账号不是配置里的 root | 通过面板完成初始化；MySQL 侧只有 root 账号 |
| 面板 401 | 会话过期（24h）/ 用户被删 | 重新登录 |
| 面板 403 `setup is only allowed from a private/local address` | 从公网访问安装接口 | 从内网/本机完成初始化，或经反代内网转发 |
| `/metrics` 401 | 未带 `metrics_token` | 用 Bearer 或 `?token=` |
| 内存持续上涨 | 自动 COMPACT 未触发 / 业务数据增长 | 看 `/api/stats` 的 `wal_file_mb`、`total_rows`，手动 COMPACT |

### 性能基线与调优

参考值（Xeon E5-2687W v2 单机、`go test -bench`，引擎层、batch 模式）：

| 基准 | 结果 |
| --- | --- |
| `BenchmarkPointSelect` | ~478 ns/op（单协程约 2M 次/秒） |
| `BenchmarkPointSelectParallel` | ~164 ns/op |
| `BenchmarkInsert` | ~3.3 µs/op |
| `BenchmarkUpdate` | ~11 µs/op |
| `BenchmarkSQLPointSelect` | ~2 µs/op（含 SQL 解析） |
| `BenchmarkMySQLQuery` | ~50 µs/op（文本协议 + 解析 + 一行结果） |

调优建议：

- 吞吐优先：`durability=batch`、适当增大 `flush_interval_ms`（可容忍的丢失窗口）
- 延迟/安全优先：`durability=fsync`，并用 `group_commit_ms` 把并发写合并（2~5ms）
- 连接数：`max_conns` 与文件句柄、内存一起规划（每连接 ~64KB 缓冲）
- WAL 增长快：缩短 `compact_idle_seconds`、降低 `compact_min_wal_mb`
- 大表扫描：给过滤列建二级索引（`CREATE INDEX`），避免全表扫描

#### 极端设备自适应

启动时会做一次很轻量的设备探测（临时文件写 64KB + fsync，按进程缓存），
结合核数自动调整写入与重放参数，并在日志里打印所选画像：

```
[INFO] device profile: class=ssd fsync≈1.55ms cores=32 | commit window 500µs..20ms,
       wal buf 256KB, batch 256 rec/1MB, replay 65536 ops/8MB x 16 workers
```

| 探测到的设备 | 自动调整 |
| --- | --- |
| 内存盘 / tmpfs（fsync < 200µs） | 窗口 100µs~5ms、WAL 缓冲 128KB、批次 128 条：落盘便宜，优先降延迟 |
| NVMe（< 1ms） | 窗口 200µs~10ms、缓冲 256KB |
| SATA SSD（1~8ms） | 窗口 500µs~20ms、缓冲 256KB |
| 机械盘 / 网络盘（≥ 8ms） | 窗口 1ms~4×fsync（上限 50ms）、缓冲 1MB、批次 512 条/4MB：一次落盘极贵，要攒大批 |
| **≤ 1 核** | 关闭重放流水线（改分段同步回填），worker=1，段 16k 条/2MB |
| **2 核** | worker=核数，段 16k 条/2MB，降低峰值内存 |
| **≥ 32 核** | 重放 worker 上限 16，避免调度与 cache 争用 |
| **内存 < 256MB** | 缓冲 32KB / 批次 128KB / 段 1MB，worker=1，关闭流水线 |
| **内存 < 1GB** | 缓冲 64KB / 批次 256KB / 段 2MB，worker ≤ 2 |
| **内存 < 4GB** | 缓冲 256KB / 批次 1MB / 段 8MB |
| **内存 < 16GB** | 设 70% 软内存上限（`debug.SetMemoryLimit`），让 GC 提前加频而不是被 OOM-killer 杀掉；用户设了 `GOMEMLIMIT` 则不覆盖 |
| **磁盘剩余 < 2GB**（或 ≤4GB 内存机器上剩余 < 内存） | 启动告警，提示缩短压缩间隔 |

启动日志会打印完整画像，便于确认实际生效值：

```
[INFO] device profile: class=ssd fsync≈1.03ms cores=32 ram=63.9GB disk_free=10.5GB |
       commit window 500µs..20ms, wal buf 256KB, batch 256 rec/1024KB,
       replay 65536 ops/8MB x 16 workers (pipeline=true), mem limit unknown
```

慢盘 + `durability=fsync` 时日志会告警：这种组合每次提交都要付一次完整 fsync，
吞吐上限就是 `1 / fsync 延迟`，高写入场景应改用 `batch`。

所有参数都可用环境变量覆盖（优先级最高，适合无法探测的容器环境）：

| 变量 | 含义 |
| --- | --- |
| `TSUMUGI_WAL_BUF_KB` | WAL 单写者 bufio 缓冲（KB） |
| `TSUMUGI_WAL_BATCH_RECORDS` | v3 分帧单批记录数上限 |
| `TSUMUGI_WAL_BATCH_MB` | v3 分帧单批字节上限（MB） |
| `TSUMUGI_REPLAY_SEGMENT_OPS` | 重放单段记录数 |
| `TSUMUGI_REPLAY_SEGMENT_MB` | 重放单段字节上限（MB） |
| `TSUMUGI_REPLAY_WORKERS` | 重放并行 worker 数 |

#### 索引结构（可切换）

主键索引与二级索引都走同一套有序映射接口，底层有两个**正交**的可选项：

**① 平衡结构 `tree_kind`**

| 取值 | 实现 | 特点 |
| --- | --- | --- |
| `llrb`（默认） | 左倾红黑树 | 成熟稳定；COW 每次写入克隆 ~19 个小节点 |
| `btree` | 写时复制 B 树（分支因子 6） | 克隆层数少（~11 层），但单节点更大 |

**② 并发模式 `tree_mode`**

| 取值 | 语义 | 适用 |
| --- | --- | --- |
| `cow`（默认） | 写时复制 + 原子换根，**读者完全无锁**、读到一致快照 | 读多写少、长扫描多 |
| `lock` | 读写锁 + **原地更新**，写入不再克隆 | 写多、可接受"长扫描阻塞写入" |
| `auto` | 自适应：单线程走原地更新，**检测到并发即单向升级为 COW** | 不确定负载形态，想自动取两者之长 |

`auto` 的工作原理（单向、不可逆，因此永远安全）：

1. 每次操作先用一个原子计数探测"当前是否只有我在操作这棵树"；
2. 独占时直接原地修改（无写时复制，写入快一个数量级，读也不需要加锁）；
3. 一旦发现别人也在操作，就把树标记为"升级中"，**等待在途操作全部退出**（此时刻不存在任何原地写者），
   再把树永久切换为 COW 语义，此后零额外开销。

升级只在 `inflight == 0` 时发生，所以 COW 读者绝不会看到被改了一半的节点。

切换方式：配置文件 `tree_kind` / `tree_mode`、环境变量 `TSUMUGI_TREE` / `TSUMUGI_TREE_MODE`，
或后台「设置 → 索引结构」。**必须重启生效**（索引在启动时从 WAL 重建，已建好的树无法原地更换结构或并发策略）。

实测（纯树层，20 万键，`go test -bench`）：

| 场景 | llrb + cow（默认） | llrb + lock | btree + lock |
| --- | --- | --- | --- |
| 插入 SetNX | 1370 ns / 19 allocs / 1276 B | 388 ns / 1 alloc | **289 ns / 0 alloc** |
| 主键点查 | 98.6 ns | **60.2 ns** | — |

`auto` 模式（btree 结构）实测：

| 场景 | cow | lock | **auto** |
| --- | --- | --- | --- |
| 单线程写入 SetNX | 1946 ns | 290 ns | **263 ns** |
| 并发读（32 P） | **30.5 ns** | 118 ns | **31.5 ns** |

也就是说 `auto` 同时拿到了 lock 的写入速度与 cow 的并发读速度。

结论：

- **`lock` 模式写入快 3.5~4.7 倍**，单线程读反而更快（不再产生 COW 垃圾节点、缓存局部性更好）；
  代价是长扫描持读锁期间写入被阻塞，且失去"扫描期间看到一致旧快照"的语义。
- **B 树在 COW 模式下比红黑树更慢**：写时复制成本 ≈ 层数 × 单节点大小，B 树层数少了但节点变大，
  总拷贝字节反而上升。默认仍是 `llrb + cow`；`btree + lock` 是写入最快的组合。

压测（走真实协议）：

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:10232/stress?duration=10&workers=4&protocol=native&mode=rw"
curl -X POST -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:10232/stress?duration=10&workers=4&protocol=mysql&mode=rw"
```

---

## HTTP 接口

| 路径 | 鉴权 | 说明 |
| --- | --- | --- |
| `/healthz` | 无 | 健康检查，返回 `{"ok":true}` |
| `/metrics` | metrics_token / 会话令牌 | Prometheus 指标 |
| `/` `/dashboard` `/admin` `/users` | 页面本身公开，数据接口需令牌 | 控制台 |
| `/api/login` `/api/logout` | — | 登录（返回 Bearer 令牌，24h 过期） |
| `/api/setup/status` `/api/setup/complete` | 仅本机/内网 | 首次初始化 |
| `/api/stats` | 会话 | 监控 JSON |
| `/api/admin/*` | 会话（部分要求管理员） | 表/行/SQL/设置/备份/压缩/重启/用户管理 |
| `/stress` | 会话 + `can_stress` | 内置压测 |

除 `/healthz` 外所有请求体上限 1MB；`http.Server` 设置了读头/读/写/空闲超时与安全响应头。

---

## SQL 与协议

### SQL 子集

支持：

- **DDL**：`CREATE TABLE`（列属性 `NOT NULL`/`DEFAULT`/`AUTO_INCREMENT`/`COMMENT`、表级 `PRIMARY KEY (...)`、`KEY`/`INDEX`/`UNIQUE` 索引、`ENGINE=`/`CHARSET=` 表选项）、`CREATE DATABASE`、`CREATE INDEX`、`ALTER TABLE`（`ADD COLUMN` / `DROP COLUMN` / `MODIFY` / `CHANGE` / `RENAME COLUMN` / `ADD INDEX` / `DROP INDEX` / 表重命名）、`DROP TABLE [IF EXISTS]`、`TRUNCATE [TABLE]`、`RENAME TABLE a TO b`
- **DML**：`INSERT`（单行/多行，多行整体原子）、`INSERT IGNORE`、`REPLACE INTO`、`INSERT ... ON DUPLICATE KEY UPDATE`（支持 `VALUES(col)`）、`UPDATE`（任意 WHERE、`SET col = 表达式`、`LIMIT`）、`DELETE`（任意 WHERE、`LIMIT`）
- **SELECT**：`*` / `列` / `表达式` / `AS 别名`、`DISTINCT`、`WHERE`（`AND`/`OR`/`NOT`/比较/`IN`/`NOT IN`/`LIKE`/`NOT LIKE`/`BETWEEN`/`IS [NOT] NULL`）、`JOIN`（`INNER`/`LEFT`/`RIGHT`/`CROSS`、`ON`/`USING`、逗号连接）、`GROUP BY` + `HAVING`、聚合（`COUNT`/`SUM`/`AVG`/`MIN`/`MAX`、`COUNT(DISTINCT)`）、`ORDER BY`（含 `ORDER BY 1`）、`LIMIT n` / `LIMIT offset,n` / `LIMIT n OFFSET m`、`UNION` / `UNION ALL`
- **函数**：字符串（`CONCAT`/`CONCAT_WS`/`SUBSTRING`/`LEFT`/`RIGHT`/`TRIM`/`REPLACE`/`UPPER`/`LOWER`/`LENGTH`/`LOCATE`/`LPAD`…）、数值（`ABS`/`ROUND`/`CEIL`/`FLOOR`/`MOD`/`POW`…）、条件（`IF`/`IFNULL`/`NULLIF`/`COALESCE`/`CASE WHEN`）、日期（`NOW`/`CURDATE`/`YEAR`/`MONTH`/`DAY`/`DATE_FORMAT`/`DATEDIFF`/`DATE_ADD`…）、信息（`DATABASE`/`USER`/`VERSION`/`CONNECTION_ID`）
- **语句级兼容**：`BEGIN`/`START TRANSACTION`/`COMMIT`/`ROLLBACK`/`SAVEPOINT`/`RELEASE`、`SET`（含 `autocommit`）、`USE`、`FLUSH`/`ANALYZE`/`OPTIMIZE`/`CHECK`/`REPAIR`/`LOCK`/`UNLOCK`/`KILL`/`DO`/`RESET`、`SHOW`（`TABLES`/`COLUMNS`/`DATABASES`/`INDEX`/`STATUS`/`VARIABLES`/`ENGINES`/`PROCESSLIST`/`MASTER STATUS`/`PRIVILEGES` 等）、`DESCRIBE`
- `GRANT` / `REVOKE`（表级：SELECT/INSERT/UPDATE/DELETE/DDL）
- 反引号标识符、单/双引号字符串与反斜杠转义、`--` `#` `/* */` 注释
- 系统表：`__tables`、`__indexes`、`__stats`（仅管理员）
- 虚拟表：`information_schema.*`、`mysql.*`（只读，兼容 phpMyAdmin）

不支持（显式报错，不静默返回错误结果）：子查询与派生表（`FROM (SELECT ...) t`）、窗口函数、`WITH ... AS` 递归 CTE、外键约束、`SELECT ... FOR UPDATE`、存储过程/触发器/视图（`CALL` 明确报错）。

`JOIN` 走嵌套循环（小表场景），大表连接建议先在内层表上建索引或改用应用层拼接。`INT` 列没有 NULL 语义（写入 NULL 落 0），只有 `VARCHAR` 能存 NULL。

### 二进制协议

大端序，1 字节命令码开头。完整编解码见 `examples/client.go` 与 `docs/native-protocol.md`。

| 命令 | 码 | 命令 | 码 |
| --- | --- | --- | --- |
| AUTH | 10 | BEGIN | 40 |
| PING | 11 | COMMIT | 41 |
| CREATE TABLE | 20 | ROLLBACK | 42 |
| DROP TABLE | 21 | CREATE INDEX | 50 |
| DESCRIBE | 22 | BACKUP | 56 |
| ALTER TABLE | 23 | STATUS | 57 |
| INSERT | 30 | COMPACT | 58 |
| SELECT | 31 | SET DURABILITY | 59 |
| UPDATE | 32 | GRANT / REVOKE | 90 / 91 |
| DELETE | 33 | QUERY SQL | 35 |

响应码：OK(1)、ERROR(2)、VALUE(3)、NOT_FOUND(4)、ROWS(5)、TXN_ID(6)。字段类型：INT(0, int64)、VARCHAR(1, ≤65534 字节)、BOOL(2)。

`BATCH`、存储过程、视图、触发器为协议占位，返回 `not implemented`。

### MySQL 协议

默认关闭。开启后在 `mysql_port`（默认 3309）监听，与二进制协议共用同一引擎与 WAL，数据互相可见。

- 认证：`mysql_native_password`（配置里只有 SHA1 摘要，不落明文）
- 文本协议 + 预处理语句（`COM_STMT_PREPARE/EXECUTE/SEND_LONG_DATA/RESET/CLOSE`，结果集为二进制行）
- `COM_FIELD_LIST`、`COM_SET_OPTION`、`COM_INIT_DB`、`COM_PING`
- 单包上限 64MB；认证前单包限制 64KB
- MySQL 侧只有单个 root 账号（表级 GRANT 只作用于原生协议）

```bash
mysql -h 127.0.0.1 -P 3309 -u admin -p
```

Go 驱动示例（互通测试见 `interop/`）：

```go
db, _ := sql.Open("mysql", "root:pass@tcp(127.0.0.1:3309)/?timeout=5s")
row := db.QueryRow("SELECT name FROM t WHERE id = ?", 1)
```

GORM：直接用 `gorm.io/driver/mysql`（Tsumugi 讲的是 MySQL 线协议，不需要专门的方言），
限制与"要不要自己写 dialector"的判断见 [`docs/gorm.md`](docs/gorm.md)。

---

## 数据文件与崩溃恢复

| 文件 | 内容 | 权限 |
| --- | --- | --- |
| `config/tsumugi.json` | 唯一配置源 | 0600 |
| `data/tsumugi.wal` | WAL：文件头（magic + 版本）+ 批次分帧（长度 + CRC32）+ varint 紧凑记录 | 0600 |
| `data/users.json` | 控制台用户（PBKDF2-HMAC-SHA256 + 随机盐） | 0600 |
| `data/privileges.json` | 表级权限 | 0600 |
| `backup/*.wal` / `backup/privileges_*.json` | 备份快照 | 0600 |

崩溃恢复：

1. 启动时校验文件头（magic `TSUMUGI` + 版本），按批读入并校验整批 CRC32，再按 varint 逐条解析记录
2. 尾部残缺/校验失败的**最后一批**视为未完成写入，按批次边界自动截断并继续启动（任意字节位置截断都能安全回退，有 `wal_alter_test.go` 逐个偏移验证）
3. Catalog 记录（建表/改表/删表/改名/**整表重建**）作为并行重放的屏障，保证「先创建后写入」的顺序
4. 改列结构（`DROP COLUMN` / 类型变更）以**一条** catalog 记录承载「新 meta + 全部行」，重放时原子生效——崩溃不会留下"旧字节按新 meta 解码"的错位数据
5. 事务：`COMMIT` 之前崩溃的写记录在重放时被丢弃（未提交数据不生效）

---

## 架构

```
              ┌──────────────── 客户端 ────────────────┐
              │  二进制协议   MySQL 协议   HTTP 面板    │
              └───────┬───────────┬───────────┬────────┘
                      │           │           │
              ┌───────▼───────────▼───────────▼────────┐
              │  server.go / mysql.go / metrics.go     │
              │  会话 · 鉴权 · 语句分析 · 结果编码      │
              └───────────────────┬────────────────────┘
                                  │
              ┌───────────────────▼────────────────────┐
              │              db.go（引擎）              │
              │  Catalog · Tables · Transaction         │
              │  ┌───────────────────────────────────┐  │
              │  │ Table: pkTree(LLRB) + idxTrees    │  │
              │  │ writeLog → WAL(单写者队列/组提交)  │  │
              │  └───────────────────────────────────┘  │
              │  WAL 重放 · TTL 清理 · 自动 COMPACT      │
              └─────────────────────────────────────────┘
```

读路径：写时复制索引，读者原子加载根指针后无锁遍历；主键点查直接命中，二级索引等值查询走索引树，否则全表扫描 + 类型化条件求值。写路径：先改内存树，再把编码后的行追加到 WAL 队列；`fsync` 模式等待落盘后返回。

---

## 开发与测试

```bash
go build ./...                       # 构建
go vet ./...                         # 静态检查
gofmt -l .                           # 格式检查（CI 要求为空）
go test ./... -count=1               # 单测 + 回归
go test -race ./... -count=1         # 并发检查（需要 cgo/gcc）
go test -run '^$' -bench . .         # 引擎基准

cd interop && go test ./... -v       # 真实 MySQL 驱动互通测试
```

`interop/` 用 `github.com/go-sql-driver/mysql` 覆盖：ping、文本协议 CRUD、预处理语句复用、NULL/Unicode、事务提交/回滚、`SHOW`/`DESCRIBE`/`VERSION()`、60KB 字段、>16MB 语句分片、8 并发写，以及 JOIN / GROUP BY+HAVING / UNION / `INSERT ... ON DUPLICATE KEY UPDATE` / 非主键 UPDATE 与 DELETE / DDL 变体。

CI（`.github/workflows/ci.yml`）在 Ubuntu 上跑 build / vet / gofmt / `-race` / interop。

性能回归验证：用 `git worktree` 挂一份 HEAD 副本，两边交替跑固定迭代基准（`-benchtime 100000x -count=3`，3~6 轮取中位数），避开 Windows 上按时间跑基准的抖动。安全/权限改造前后，SQL 点查、范围扫描、插入、事务提交都落在 ±3% 噪声带内；范围扫描与事务提交还分别快了约 12% / 32%。

目录结构：

```
main.go              启动、信号处理、配置校验
db.go                引擎：表、索引、WAL、事务、TTL、备份
wal_v3.go            WAL v3 批次分帧（长度 + 整批 CRC，流式重放）
wal_alter.go         ALTER 的整表原子重写（单条 catalog 记录承载新 meta + 全部行）
rb_tree.go           LLRB（写时复制）
sql_parser.go        分词 + 递归解析/执行（DDL/INSERT/SELECT 快路径）
sql_expr.go          表达式求值（运算符、标量函数、CASE）
sql_exec.go          通用查询执行（JOIN / GROUP BY / 排序 / 列校验）
sql_dml.go           UPDATE / DELETE（任意谓词 + SET 表达式）
sql_alter.go         ALTER TABLE 变体（增删列、改列、索引）
sql_stmt.go          TRUNCATE / RENAME / 事务控制 / 管理语句兼容
server.go            二进制协议
mysql.go             MySQL 协议兼容层
acl.go / users.go    权限与用户
metrics.go           HTTP 面板与指标
admin.go / app_html.go / i18n_*.go   管理面板与文案
deploy/              systemd / nginx / prometheus 示例
interop/             真实 MySQL 驱动互通测试（独立 module）
docs/                协议文档
```

---

## 已知限制

- **单机单写者**：WAL 是一个串行队列，写吞吐上限受单盘 fsync 能力约束；没有副本/高可用
- **一个数据目录只能跑一个实例**（启动时对 `data/` 加文件独占锁，第二个实例直接拒绝启动）：两个进程共享 WAL 会互相覆盖，属于静默数据损坏。需要水平扩容就换 MySQL/PostgreSQL；Tsumugi 适合单实例内网场景
- **自增主键由引擎分配**：`INSERT` 不带主键列（或给 0/NULL）时分配一个单调递增、不复用的 id，并通过 MySQL 协议的 last insert id 返回（ORM 可直接取回）。客户端不要再自己"MAX(pk)+1"播种——多客户端并发下会撞主键
- **数据必须在内存**：无冷热分层，重启需要重放整个 WAL（百万行级约数秒）
- **SQL 子集**：无子查询/派生表、窗口函数、递归 CTE、外键；`JOIN` 是嵌套循环，没有优化器与执行计划选择（不会自动选驱动表/索引）
- **无二级索引统计信息**：`WHERE` 只对主键做范围下推，二级索引等值查询会走索引树，但范围/排序仍可能全表扫描
- **改列结构的 ALTER 会整表重建**（`DROP COLUMN` / 类型变更）：重建结果以**一条 WAL 记录**原子生效（重启后要么是旧表、要么是新表，不存在半新半旧），代价是需要与表数据相当的临时内存（单次上限 1GB，超限会明确报错），重建期间该表拒绝写入
- **`UNION` 与预处理参数**：`UNION` 在 token 层拆分执行，两侧共享同一组绑定参数，带 `?` 的 `UNION` 语句请改写成两次查询
- **隔离级别**：乐观并发，同一行并发写「后写者胜出」，不是快照隔离；无 `SELECT ... FOR UPDATE`。冲突在 **COMMIT** 时判定（行版本被并发写覆盖 → 报 `conflict: row N version changed`），因此给每条语句套隐式事务的 ORM 看不到语句级错误；用 `txn_conflict=last_write_wins` 切成 MySQL autocommit 语义
- **`INT` 列无 NULL**：写入 NULL 落 0；只有 `VARCHAR` 有真正的 NULL 语义
- **VARCHAR 上限 65534 字节**，超限写入报错
- **MySQL 侧单账号**，无多用户/库级权限；`GRANT/REVOKE` 只作用于原生协议
- **协议端口无 TLS**：只在内网暴露，或用四层代理 + 网络隔离
- **面板正文字体引用公共 CDN**（Roboto Flex / Fira Code，缺失时回退系统字体）；**图标字体可自托管**到 `assets/`，未放置时自动降级为 Unicode 字形
- 存储过程、视图、触发器为占位实现（`CALL` 明确报错）

---

## 许可

MIT，见 [LICENSE](LICENSE)。
