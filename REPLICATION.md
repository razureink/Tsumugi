# 复制（C1）：物理 WAL 传送

## 为什么不是 binlog

MySQL 原生复制需要实现 `COM_BINLOG_DUMP` + ROW/GTID 事件流，是一套独立
协议栈。本实现不复制这套协议，而是复用引擎唯一的变更真相源——**WAL**。

- WAL 里已经是按序、可校验（CRC）、可重放的全部变更；
- 偏移量天然就是 LSN；
- 从库用主库 WAL 的**一份前缀副本**做初始状态，之后按偏移拉增量，
  两边 LSN 严格对齐；
- 不需要第二套变更表示，主库写什么从库就应用什么，语义不可能漂移。

代价：只支持 **Tsumugi → Tsumugi**，不能把 MySQL 原生从库挂上来。

## 配置

主库 `config/tsumugi.json`：

```json
{
  "replication_token": "change-me",
  "auto_compact": false
}
```

- `replication_token`：从库拉取 WAL 的凭据。设置后本实例即处于"主库模式"，
  `Compact` 与自动压缩会被**拒绝**（重写 WAL 会让所有从库偏移失效）。
- 也可以用 `metrics_token` 鉴权（不推荐，语义不清晰）。

从库：

```json
{
  "replica_of": "http://10.0.0.1:10232",
  "replica_token": "change-me",
  "replica_poll_ms": 200
}
```

从库默认**只读**：写语句返回 MySQL errno **1290**
（`ER_OPTION_PREVENTS_STATEMENT`）。

## 初始化（bootstrap）

必须用**物理副本**（逻辑 dump 会重建表 ID，LSN 对不上）：

1. 在主库执行一次备份（管理接口 `POST /api/admin/backup`，或调用 `DB.Backup()`）。
   备份会先 flush WAL 再复制，因此副本末尾可能是半帧；从库启动时按崩溃
   残留截断到最后一个完整帧，得到一个干净前缀。
2. 把备份的 `*.wal` 作为从库数据目录里的 WAL 文件；同时复制
   `views.json`（视图定义不在 WAL 里）与 `privileges_*.json`。
3. 以 `replica_of` 配置启动从库。它的初始 LSN = 本地 WAL 长度，
   启动后立即开始拉取增量。

主库无需停机：备份之后写入的帧会在第 3 步被拉下来。

## 监控

SQL：

```sql
SHOW MASTER STATUS;   -- File / Position（主库当前 WAL 末尾）
SHOW SLAVE STATUS;    -- 从库自身：状态 / 主库位置 / 延迟 / 最后错误
SHOW SLAVE HOSTS;     -- 主库视角：每个从库最后回传的 Offset / Age_s
```

从库每次成功应用后会 `POST /replication/ack` 回传 `{id, offset}`（单调不回退），
主库据此展示各从库进度。**这只用于监控，不是半同步**：主库提交不等待 ack。

HTTP（管理员）：

```
GET /api/stats        -> snap["replication"] = {
                          replica_id, offset, lag_ms, status, error,
                          master, read_only, promoted,
                          replicas: [{id, host, offset, age_s}]
                        }
```

`status` 为 `running | stopped | error`；`error` 时原因在 `error` 字段。
拉取端点是 `GET /replication/wal?offset=N`，响应头带 `X-WAL-End`、
`X-WAL-Checksum` 与 `X-WAL-Version`（v6 压缩帧需要）。

## 故障切换（failover）

```sql
-- 在从库执行
PROMOTE;
```

`PROMOTE` 会：

1. 与增量应用互斥（等待在途应用完成）；
2. 永久设置 `promoted`，停止拉取循环；
3. 解除只读，开始接受写入。

之后把客户端指向新主库即可。**没有自动选主/ fencing**：旧主库若仍在运行，
必须人工停掉或改配置，否则会出现双写（这是运维边界，不是代码能替你决定的）。

## 明确的限制

- 仅 Tsumugi→Tsumugi；无 GTID、无半同步、无多源、无自动故障切换。
- 主库与从库必须使用**兼容的 WAL 版本**，且从库的初始 WAL 必须来自
  同一个主库（表 ID 历史一致）。
- 复制激活期间两侧都**禁止 Compact**（代码强制）。
- 主库 WAL 无限增长：复制场景下需要靠更大磁盘或定期重建从库来做保留策略。
- 同一张表的增量按表串行应用（`applySegment` 每表一个 worker），
  不同表之间并行。
- 主库若开启 WAL 帧压缩（v6），从库通过响应头 `X-WAL-Version` 自动识别，
  压缩帧对复制透明（增量原样追加，CRC 在解压前校验）。
