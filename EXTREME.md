# 极端设备优化

目标场景：**1~2 核、512MB~2GB 内存、SD 卡 / U 盘 / 机械盘、小容量磁盘、
电池供电**。同一套代码在这些设备上不能靠"加大缓冲/加线程"取胜，反而要
反向收缩。所有参数在启动时按四维探测自动决定，并可用环境变量覆盖。

## 1. 设备探测（启动时一次）

| 探测项 | 依据 | 影响 |
| --- | --- | --- |
| fsync 延迟 | WAL 目录写 64KB + `Sync`，取 3 次最小值 | 组提交窗口、WAL 缓冲、批次大小、压缩开关 |
| 核数 | `runtime.GOMAXPROCS(0)` | 重放 worker 数、是否启用解析/应用流水线 |
| 总内存 | 系统信息 | 所有缓冲按比例缩放 + 软内存上限 |
| 磁盘余量 | WAL 目录所在卷 | 低空间告警 / 紧急压缩 / 强制压缩 |

日志会打印一行 `device profile: ...`，直接看它就知道当前档位。

## 2. WAL 帧级压缩（v6）

**格式**：v6 的帧长度 varint 最低位是压缩标志：

```
[varint (存盘长度<<1 | 压缩位)][u32 crc(存盘字节)][payload]
压缩时 payload = [varint 原始长度][deflate(BestSpeed) 流]
```

- 读取侧兼容 v1~v6；v5 旧文件保持原帧布局追加，不会被误读。
- CRC 覆盖**存盘字节**（压缩流），先校验再解压；解压长度严格校验，防解压炸弹。
- 压缩收益不足 8 字节时自动退回原始 payload（不可压缩数据不做无效功）。
- Compact 重写的 WAL 同样是 v6，并按当前策略压缩。

**策略**（`wal_compress`）：

| 值 | 行为 |
| --- | --- |
| `off` | 从不压缩 |
| `auto`（默认） | fsync ≥ 1ms 且核数 ≥ 2 时开启；磁盘余量紧张时强制开启；单核默认关闭 |
| `on` | 总是压缩 |

环境变量覆盖：`TSUMUGI_WAL_COMPRESS=1|0`。

**实测**（本仓库 `TestWALV6CompressionShrinksFile`，高重复文本）：

```
WAL size: uncompressed=296210 bytes, compressed=12593 bytes (95.7% saved)
```

这是最好情况；真实业务（数字、短字符串、混合列）通常在 **2~4×**。
单核设备上 CPU 可能成为新瓶颈，所以 `auto` 默认不压。

## 3. 磁盘写满（极端设备的常态）

- WAL 写入遇到 `ENOSPC / EDQUOT`（Windows：`ERROR_DISK_FULL` 等）时进入
  **只读故障态**，所有后续写返回稳定错误，映射 MySQL errno **1021**。
- 不会静默丢弃或返回"成功但没落盘"。
- 故障是**粘性**的：清空磁盘后需要重启实例恢复（避免半失败状态的
  bufio/句柄被继续使用）。监控看 `/api/stats` 的 `disk_full`。
- 崩溃/半写帧仍按帧边界截断，启动不会因为尾部残帧失败。

## 4. 空闲期不做无意义 fsync

`flushLoop` 每 `flush_interval` 醒一次，但**只有自上次 Sync 以来有新字节**
才真正 `Flush + Sync`。空闲实例不再每秒做 10 次空 fsync——这在 SD 卡上是
真实的写入寿命与功耗消耗。有真实写入后立即恢复 fsync（回归测试
`TestIdleFlushSkipsFsync`）。

## 5. 内存与缓冲（按内存分档）

| 参数 | 行为 |
| --- | --- |
| `walWriteBufBytes` | 慢盘 1MB / 普通 256KB / 内存盘 128KB |
| `walBatchMaxBytes/Records` | 随设备档位与内存缩放 |
| `connBufBytes` | 8 / 16 / 32KB 三档，按内存与连接数上限缩放 |
| 重放段大小 | 1 核 2MB、双核 2MB，避免峰值内存过高 |
| `debug.SetMemoryLimit` | < 16GB 机器设 70% 软上限，让 GC 提前介入而不是被 OOM-killer 杀 |
| `maxScanRows` | 全量物化扫描的硬上限，防止一条 `SELECT *` 打爆小内存 |

## 6. 重放与启动

- ≤ 1 核：关闭解析/应用流水线（流水线在这类设备上只有 goroutine 开销）；
- 2 核：段切小，降低首次可见延迟与峰值内存；
- 大机器：worker 上限钳制，避免调度与 cache 争用。

## 7. 推荐配置（512MB / 1 核 / SD 卡）

```json
{
  "durability": "batch",
  "flush_interval_ms": 200,
  "wal_compress": "on",
  "auto_compact": true,
  "compact_min_wal_mb": 16,
  "max_conns": 32
}
```

说明：单核也建议显式 `on`，因为 SD 卡的随机写延迟通常远高于压缩的 CPU 成本；
若 CPU 确实吃紧，改回 `auto` 即可。`durability=batch` 允许崩溃丢失一个
flush 窗口（默认 200ms），换取 SD 卡寿命；不能接受时用 `fsync`。

## 8. 如何验证

```bash
go test -run "TestWALV6|TestIdleFlushSkipsFsync|TestDiskFull" -v
go test -race ./...        # 需要 cgo/gcc，见 RACE.md
```

真实设备上建议看：

- 启动日志的 `device profile` 与 `wal frame compression`；
- `/api/stats` 的 `wal_file_mb`、`fsync_per_s`、`config.wal_compress`、`disk_full`；
- 空闲 1 分钟后 `dsync_per_s` 应接近 0。
