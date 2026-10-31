package main

// ==================== 极端设备自适应调优 ====================
// 同一套参数在两种极端机器上都不合适：
//   - 机械盘 / 网络盘：一次 fsync 十几毫秒，2ms 的组提交窗口意味着
//     "只攒到一两条就落一次盘"，吞吐被 fsync 次数上限死死压住；
//   - 小内存 / 单核机器：8MB 重放段 + 1MB 批量缓冲在几十 MB 内存的
//     环境下可能直接把进程推到 OOM；
//   - 64 核以上：按核数起等价数量的重放 worker，调度与 cache 争用反而变慢。
//
// 启动时做一次很轻量的设备探测（临时文件写 + fsync，测一次落盘延迟），
// 再结合核数把全局写入/重放参数调到合适的量级。所有参数都可以用环境变量
// 覆盖，便于在没有探测能力的容器里手工设定。

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 可被探测结果覆盖的运行时参数（默认值面向普通 SSD + 8 核以上）。
var (
	// replayWorkers 重放并行 worker 上限（0 = 按核数自动）
	replayWorkers = 0
	// replayPipeline 是否用"解析/回填流水线"。单核机器上流水线只有调度开销，
	// 关掉后改为分段同步回填，内存同样有界。
	replayPipeline = true
	// lowDiskSpace 启动时 WAL 目录剩余空间过低的提示级别标记（0=正常）
	lowDiskSpace bool
	// maxScanRows 单条查询最多物化的行数。SQL 路径会把结果集整体物化成
	// [][]interface{}，超大结果集会直接吃光内存（极端小内存设备上是 OOM 主因）。
	// 超过上限时明确报错并提示加 LIMIT，而不是让进程被 OOM-killer 杀掉。
	maxScanRows = 2000000
)

const (
	// 单段重放的硬下限：否则流水线被切得过碎，调度开销反超收益
	minReplaySegmentOps   = 4096
	minReplaySegmentBytes = 1 << 20
	// 重放 worker 的合理上限：再多只是增加调度与 cache 争用
	maxReplayWorkers = 16
)

// capBuf 把 *v 压到不超过 limit（只压不涨，避免把大机器参数改小）。
func capBuf(v *int, limit int) {
	if *v > limit {
		*v = limit
	}
}

// applyMemoryBudget 按物理内存总量缩放缓冲与并行度（0 = 探测失败，保持默认）。
// 抽成独立函数是为了让"极端小内存"分支可以被单元测试直接覆盖。
func applyMemoryBudget(totalRAM uint64) {
	if totalRAM <= 0 {
		return
	}
	switch {
	case totalRAM < 256<<20: // < 256MB：只保留最小可用缓冲，单 worker、无流水线
		maxScanRows = 50000
		capBuf(&walWriteBufBytes, 32<<10)
		capBuf(&walBatchMaxBytes, 128<<10)
		capBuf(&replaySegmentBytes, 1<<20)
		if replaySegmentOps > 8192 {
			replaySegmentOps = 8192
		}
		replayWorkers = 1
		replayPipeline = false
	case totalRAM < 1<<30: // < 1GB
		maxScanRows = 200000
		capBuf(&walWriteBufBytes, 64<<10)
		capBuf(&walBatchMaxBytes, 256<<10)
		capBuf(&replaySegmentBytes, 2<<20)
		if replaySegmentOps > 16384 {
			replaySegmentOps = 16384
		}
		if replayWorkers > 2 {
			replayWorkers = 2
		}
	case totalRAM < 4<<30: // < 4GB
		maxScanRows = 1000000
		capBuf(&walWriteBufBytes, 256<<10)
		capBuf(&walBatchMaxBytes, 1<<20)
		capBuf(&replaySegmentBytes, 8<<20)
	}
}

// replayWorkerCount 返回本次重放应使用的 worker 数。
func replayWorkerCount(tables int) int {
	if tables <= 0 {
		return 1
	}
	n := replayWorkers
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	if n > maxReplayWorkers {
		n = maxReplayWorkers
	}
	if n > tables {
		n = tables
	}
	if n < 1 {
		n = 1
	}
	return n
}

// deviceTuning 一次探测得到的设备画像（仅用于日志与调参）。
type deviceTuning struct {
	FsyncLatency time.Duration
	Cores        int
	Class        string
	CommitMin    time.Duration
	CommitMax    time.Duration
	// 以下为极端设备信息：探测不到时为 0
	TotalRAM  uint64
	FreeDisk  uint64
	MemoryCap uint64 // 已设置的软内存上限（0 = 未设置）
}

// probeFsyncLatency 在 WAL 目录上测一次"写 64KB + fsync"的耗时。
// 取三次中的最小值：其它进程的抖动只会让测量偏大，最小值最接近设备真实能力。
// 结果按进程缓存：一个进程只服务一个数据目录（既有设计约束），
// 反复探测既慢又无意义（测试里每次 NewDB 都会走这里）。
var (
	probeOnce   sync.Once
	probeResult time.Duration
)

func probeFsyncLatency(dir string) time.Duration {
	probeOnce.Do(func() { probeResult = probeFsyncOnce(dir) })
	return probeResult
}

func probeFsyncOnce(dir string) time.Duration {
	const probeSize = 64 << 10
	best := time.Duration(0)
	buf := make([]byte, probeSize)
	for i := 0; i < 3; i++ {
		f, err := os.CreateTemp(dir, ".tsumugi_probe_*")
		if err != nil {
			return 0
		}
		begin := time.Now()
		_, werr := f.Write(buf)
		if werr == nil {
			werr = f.Sync()
		}
		cost := time.Since(begin)
		name := f.Name()
		f.Close()
		os.Remove(name)
		if werr != nil {
			return best
		}
		if best == 0 || cost < best {
			best = cost
		}
	}
	return best
}

// envInt 读取整型环境变量覆盖值。
func envInt(name string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// applyTuning 探测设备并按结果调整全局写入/重放参数。
// 必须在 WAL writer goroutine 启动前调用（writer 在创建 bufio 时读缓冲大小）。
func applyTuning(cfg *Config) deviceTuning {
	cores := runtime.GOMAXPROCS(0)
	t := deviceTuning{Cores: cores}

	// 1) 落盘延迟 → 设备分类与组提交窗口
	fsync := probeFsyncLatency(cfg.WALDir)
	t.FsyncLatency = fsync
	switch {
	case fsync == 0:
		// 探测失败（只读目录/受限容器）：保持默认，靠自适应窗口自己收敛
		t.Class = "unknown"
		t.CommitMin, t.CommitMax = gcWindowMin, gcWindowMax
	case fsync >= 8*time.Millisecond:
		// 机械盘 / 网络盘：一次落盘极贵，值得多等一会儿攒大批
		t.Class = "slow"
		t.CommitMin = 1 * time.Millisecond
		t.CommitMax = 4 * fsync
		if t.CommitMax > 50*time.Millisecond {
			t.CommitMax = 50 * time.Millisecond
		}
		walWriteBufBytes = 1 << 20 // 1MB：摊薄系统调用与写放大
		walBatchMaxBytes = 4 << 20
		walBatchMaxRecords = 512
	case fsync >= 1*time.Millisecond:
		// 普通 SATA SSD：中度窗口
		t.Class = "ssd"
		t.CommitMin = 500 * time.Microsecond
		t.CommitMax = 20 * time.Millisecond
		walWriteBufBytes = 256 << 10
	case fsync >= 200*time.Microsecond:
		// NVMe：落盘便宜，窗口不必大
		t.Class = "fast"
		t.CommitMin = 200 * time.Microsecond
		t.CommitMax = 10 * time.Millisecond
		walWriteBufBytes = 256 << 10
	default:
		// 内存盘 / tmpfs：落盘几乎免费，窗口越短越好
		t.Class = "memory"
		t.CommitMin = 100 * time.Microsecond
		t.CommitMax = 5 * time.Millisecond
		walWriteBufBytes = 128 << 10
		walBatchMaxRecords = 128
	}

	// 2) 核数 → 重放并行度与段大小
	switch {
	case cores <= 1:
		// 单核：流水线的"解析/回填重叠"不可能发生，只剩 goroutine 与通道开销，
		// 改为分段同步回填（内存同样有界），并缩小段以降低峰值占用。
		replayWorkers = 1
		replayPipeline = false
		replaySegmentOps = 16384
		if replaySegmentBytes > 2<<20 {
			replaySegmentBytes = 2 << 20
		}
	case cores <= 2:
		// 双核：并行度天然受限，段切小一点降低峰值内存与首次可见延迟
		replayWorkers = cores
		replaySegmentOps = 16384
		if replaySegmentBytes > 2<<20 {
			replaySegmentBytes = 2 << 20
		}
	case cores >= 32:
		// 大机器：限制 worker 数，避免调度与 cache 争用
		replayWorkers = maxReplayWorkers
	default:
		replayWorkers = cores
	}

	// 3) 内存预算：小内存机器按比例缩小所有缓冲，并设软上限避免被 OOM 杀掉。
	//    这是"极端设备"最关键的一档：256MB 容器里 8MB 段 × 3 + 1MB 批次会直接打爆。
	t.TotalRAM = totalMemoryBytes()
	if t.TotalRAM > 0 {
		applyMemoryBudget(t.TotalRAM)
		// 软内存上限：让 GC 提前加频，而不是等内核 OOM-killer 杀进程。
		// 只在中小内存机器上设置——大内存机器上设上限反而会干扰内存型数据库
		// 的正常驻留行为。用户显式设了 GOMEMLIMIT 时不覆盖。
		if os.Getenv("GOMEMLIMIT") == "" && t.TotalRAM < 16<<30 {
			limit := int64(t.TotalRAM / 10 * 7) // 70%
			if limit > 256<<20 {                // 太小会导致 GC 抖动
				debug.SetMemoryLimit(limit)
				t.MemoryCap = uint64(limit)
			}
		}
	}

	// 4) 磁盘余量：WAL 会持续增长，空间紧张时提前告警（压缩策略由 autoCompact 负责）。
	// 阈值取"绝对下限 2GB"或"小机器上剩余空间已小于内存"——后者意味着
	// WAL/备份很容易把盘写满。
	t.FreeDisk = freeDiskBytes(cfg.WALDir)
	if t.FreeDisk > 0 {
		if t.FreeDisk < 2<<30 || (t.TotalRAM > 0 && t.TotalRAM <= 4<<30 && t.FreeDisk < t.TotalRAM) {
			lowDiskSpace = true
		}
	}
	if replaySegmentOps < minReplaySegmentOps {
		replaySegmentOps = minReplaySegmentOps
	}
	if replaySegmentBytes < minReplaySegmentBytes {
		replaySegmentBytes = minReplaySegmentBytes
	}

	// 5) 环境变量覆盖（优先级最高，便于容器/受限环境手工调参）
	if n, ok := envInt("TSUMUGI_WAL_BUF_KB"); ok {
		walWriteBufBytes = n << 10
	}
	if n, ok := envInt("TSUMUGI_WAL_BATCH_RECORDS"); ok {
		walBatchMaxRecords = n
	}
	if n, ok := envInt("TSUMUGI_WAL_BATCH_MB"); ok {
		walBatchMaxBytes = n << 20
	}
	if n, ok := envInt("TSUMUGI_REPLAY_SEGMENT_OPS"); ok {
		replaySegmentOps = n
	}
	if n, ok := envInt("TSUMUGI_REPLAY_SEGMENT_MB"); ok {
		replaySegmentBytes = n << 20
	}
	if n, ok := envInt("TSUMUGI_REPLAY_WORKERS"); ok {
		replayWorkers = n
	}
	if n, ok := envInt("TSUMUGI_MAX_SCAN_ROWS"); ok {
		maxScanRows = n
	}

	logf(LOG_VERB,
		"device profile: class=%s fsync≈%v cores=%d ram=%s disk_free=%s | commit window %v..%v, wal buf %dKB, batch %d rec/%dKB, replay %d ops/%dMB x %d workers (pipeline=%v), max scan rows %d, mem limit %s",
		t.Class, t.FsyncLatency, cores, fmtBytes(t.TotalRAM), fmtBytes(t.FreeDisk),
		t.CommitMin, t.CommitMax,
		walWriteBufBytes>>10, walBatchMaxRecords, walBatchMaxBytes>>10,
		replaySegmentOps, replaySegmentBytes>>20, replayWorkerCount(1<<8), replayPipeline,
		maxScanRows, fmtBytes(t.MemoryCap))

	// 慢盘 + 逐条 fsync 的组合无论怎么调都不可能快：明确提示用户改批量模式
	if t.Class == "slow" && cfg.Durability.Fsync() {
		logf(LOG_WARN,
			"slow storage detected (fsync≈%v) with durability=fsync: each commit pays a full fsync; "+
				"switch durability to batch for high write throughput", t.FsyncLatency)
	}
	// 磁盘空间紧张：WAL 会持续增长，提醒缩短压缩间隔
	if lowDiskSpace {
		logf(LOG_WARN,
			"low disk space on WAL dir (%s free): WAL grows continuously; "+
				"reduce compact_min_wal_mb / compact_idle_seconds to reclaim space sooner",
			fmtBytes(t.FreeDisk))
	}
	if t.TotalRAM > 0 && t.TotalRAM < 1<<30 {
		logf(LOG_WARN,
			"small memory detected (%s): buffers scaled down and a soft memory limit was applied; "+
				"avoid large result sets / unbounded scans",
			fmtBytes(t.TotalRAM))
	}
	return t
}

// fmtBytes 把字节数格式化成人类可读形式（0 表示探测失败）。
func fmtBytes(n uint64) string {
	const unit = 1024
	if n == 0 {
		return "unknown"
	}
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTP"[exp])
}
