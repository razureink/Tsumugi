package main

import (
	"runtime"
	"testing"
	"time"
)

// saveTuningState 保存/恢复全局调优参数，避免测试之间互相污染。
func saveTuningState(t *testing.T) {
	t.Helper()
	buf, batchBytes, batchRecs := walWriteBufBytes, walBatchMaxBytes, walBatchMaxRecords
	segOps, segBytes, workers := replaySegmentOps, replaySegmentBytes, replayWorkers
	t.Cleanup(func() {
		walWriteBufBytes, walBatchMaxBytes, walBatchMaxRecords = buf, batchBytes, batchRecs
		replaySegmentOps, replaySegmentBytes, replayWorkers = segOps, segBytes, workers
	})
}

// TestTuningEnvOverrides 校验环境变量能覆盖探测结果（受限容器手工调参的入口）。
func TestTuningEnvOverrides(t *testing.T) {
	saveTuningState(t)
	SetLogMute(true)

	t.Setenv("TSUMUGI_WAL_BUF_KB", "512")
	t.Setenv("TSUMUGI_WAL_BATCH_RECORDS", "77")
	t.Setenv("TSUMUGI_WAL_BATCH_MB", "2")
	t.Setenv("TSUMUGI_REPLAY_SEGMENT_OPS", "12345")
	t.Setenv("TSUMUGI_REPLAY_SEGMENT_MB", "3")
	t.Setenv("TSUMUGI_REPLAY_WORKERS", "3")

	cfg := walTestConfig(t.TempDir())
	applyTuning(cfg)

	if walWriteBufBytes != 512<<10 {
		t.Fatalf("walWriteBufBytes=%d want %d", walWriteBufBytes, 512<<10)
	}
	if walBatchMaxRecords != 77 {
		t.Fatalf("walBatchMaxRecords=%d want 77", walBatchMaxRecords)
	}
	if walBatchMaxBytes != 2<<20 {
		t.Fatalf("walBatchMaxBytes=%d want %d", walBatchMaxBytes, 2<<20)
	}
	if replaySegmentOps != 12345 {
		t.Fatalf("replaySegmentOps=%d want 12345", replaySegmentOps)
	}
	if replaySegmentBytes != 3<<20 {
		t.Fatalf("replaySegmentBytes=%d want %d", replaySegmentBytes, 3<<20)
	}
	if replayWorkers != 3 {
		t.Fatalf("replayWorkers=%d want 3", replayWorkers)
	}
}

// TestTuningDefaultsSane 校验无环境变量时探测结果是合理量级（不会把参数调成 0）。
func TestTuningDefaultsSane(t *testing.T) {
	saveTuningState(t)
	SetLogMute(true)
	for _, k := range []string{
		"TSUMUGI_WAL_BUF_KB", "TSUMUGI_WAL_BATCH_RECORDS", "TSUMUGI_WAL_BATCH_MB",
		"TSUMUGI_REPLAY_SEGMENT_OPS", "TSUMUGI_REPLAY_SEGMENT_MB", "TSUMUGI_REPLAY_WORKERS",
	} {
		t.Setenv(k, "")
	}
	cfg := walTestConfig(t.TempDir())
	tune := applyTuning(cfg)

	if walWriteBufBytes < 64<<10 || walWriteBufBytes > 8<<20 {
		t.Fatalf("walWriteBufBytes out of range: %d", walWriteBufBytes)
	}
	if walBatchMaxRecords < 64 || walBatchMaxRecords > 4096 {
		t.Fatalf("walBatchMaxRecords out of range: %d", walBatchMaxRecords)
	}
	if walBatchMaxBytes < 1<<20 || walBatchMaxBytes > 64<<20 {
		t.Fatalf("walBatchMaxBytes out of range: %d", walBatchMaxBytes)
	}
	if replaySegmentOps < minReplaySegmentOps {
		t.Fatalf("replaySegmentOps below floor: %d", replaySegmentOps)
	}
	if replaySegmentBytes < minReplaySegmentBytes {
		t.Fatalf("replaySegmentBytes below floor: %d", replaySegmentBytes)
	}
	if tune.CommitMin <= 0 || tune.CommitMax < tune.CommitMin {
		t.Fatalf("commit window bounds invalid: %v..%v", tune.CommitMin, tune.CommitMax)
	}
	if tune.Cores != runtime.GOMAXPROCS(0) {
		t.Fatalf("cores=%d want %d", tune.Cores, runtime.GOMAXPROCS(0))
	}
	t.Logf("class=%s fsync≈%v commit=%v..%v buf=%dKB batch=%d rec",
		tune.Class, tune.FsyncLatency, tune.CommitMin, tune.CommitMax,
		walWriteBufBytes>>10, walBatchMaxRecords)
}

// TestReplayWorkerCountCapped 校验重放 worker 数量受限（大机器不做无脑并行）。
func TestReplayWorkerCountCapped(t *testing.T) {
	saveTuningState(t)
	replayWorkers = 1000
	if got := replayWorkerCount(1000); got > maxReplayWorkers {
		t.Fatalf("worker count %d exceeds cap %d", got, maxReplayWorkers)
	}
	replayWorkers = 8
	if got := replayWorkerCount(3); got != 3 {
		t.Fatalf("worker count %d want 3 (bounded by table count)", got)
	}
	replayWorkers = 0
	if got := replayWorkerCount(0); got != 1 {
		t.Fatalf("worker count %d want 1", got)
	}
}

// TestGroupCommitterWindowBounds 校验窗口自适应被限制在探测设定的上下界内。
func TestGroupCommitterWindowBounds(t *testing.T) {
	gc := &GroupCommitter{interval: 2 * time.Millisecond}
	gc.setWindowBounds(time.Millisecond, 5*time.Millisecond, 0)

	// 大量等待者：窗口应放大，但不得超过上界
	gc.waiters = make([]chan error, 100)
	gc.observe(100, time.Millisecond)
	if gc.adaptive > 5*time.Millisecond {
		t.Fatalf("adaptive window %v exceeds max", gc.adaptive)
	}
	// 单个等待者：窗口应缩小，但不得低于下界
	for i := 0; i < 40; i++ {
		gc.observe(1, time.Millisecond)
	}
	if gc.adaptive < time.Millisecond {
		t.Fatalf("adaptive window %v below min", gc.adaptive)
	}
}

// TestSysInfoProbe 校验内存/磁盘探测在当前平台可工作（失败时返回 0，不算错误）。
func TestSysInfoProbe(t *testing.T) {
	ram := totalMemoryBytes()
	disk := freeDiskBytes(t.TempDir())
	t.Logf("totalMemory=%s freeDisk=%s", fmtBytes(ram), fmtBytes(disk))
	if ram != 0 && ram < 64<<20 {
		t.Fatalf("suspicious total memory: %d", ram)
	}
	if disk != 0 && disk < 1<<20 {
		t.Fatalf("suspicious free disk: %d", disk)
	}
}

// TestFmtBytes 校验人类可读格式化。
func TestFmtBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "unknown"},
		{512, "512B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1 << 20, "1.0MB"},
		{3 << 30, "3.0GB"},
	}
	for _, c := range cases {
		if got := fmtBytes(c.in); got != c.want {
			t.Fatalf("fmtBytes(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestCapBufOnlyShrinks 校验 capBuf 只压不涨。
func TestCapBufOnlyShrinks(t *testing.T) {
	v := 1 << 20
	capBuf(&v, 64<<10)
	if v != 64<<10 {
		t.Fatalf("capBuf shrank to %d want %d", v, 64<<10)
	}
	v = 1024
	capBuf(&v, 64<<10)
	if v != 1024 {
		t.Fatalf("capBuf grew to %d, should stay 1024", v)
	}
}

// TestApplyMemoryBudget 校验小内存设备会按档位缩小缓冲（这是极端设备最关键的一档）。
func TestApplyMemoryBudget(t *testing.T) {
	saveTuningState(t)
	reset := func() {
		walWriteBufBytes = 1 << 20
		walBatchMaxBytes = 4 << 20
		replaySegmentBytes = 8 << 20
		replaySegmentOps = 65536
		replayWorkers = 16
		replayPipeline = true
	}

	// 256MB 以下：全部压到最小、单 worker、无流水线
	reset()
	applyMemoryBudget(192 << 20)
	if walWriteBufBytes > 32<<10 || walBatchMaxBytes > 128<<10 || replaySegmentBytes > 1<<20 {
		t.Fatalf("tiny RAM not capped: buf=%d batch=%d seg=%d",
			walWriteBufBytes, walBatchMaxBytes, replaySegmentBytes)
	}
	if replayPipeline || replayWorkers != 1 {
		t.Fatalf("tiny RAM should disable pipeline and use 1 worker: pipeline=%v workers=%d",
			replayPipeline, replayWorkers)
	}

	// 512MB：中等压缩
	reset()
	applyMemoryBudget(512 << 20)
	if walWriteBufBytes > 64<<10 || replaySegmentBytes > 2<<20 {
		t.Fatalf("small RAM not capped: buf=%d seg=%d", walWriteBufBytes, replaySegmentBytes)
	}
	if replayWorkers > 2 {
		t.Fatalf("small RAM workers=%d want <=2", replayWorkers)
	}

	// 2GB：轻度压缩，流水线保留
	reset()
	applyMemoryBudget(2 << 30)
	if walBatchMaxBytes > 1<<20 {
		t.Fatalf("2GB batch not capped: %d", walBatchMaxBytes)
	}
	if !replayPipeline {
		t.Fatal("2GB should keep the replay pipeline")
	}

	// 32GB：完全不动
	reset()
	applyMemoryBudget(32 << 30)
	if walWriteBufBytes != 1<<20 || walBatchMaxBytes != 4<<20 || replaySegmentBytes != 8<<20 {
		t.Fatalf("large RAM should be untouched: buf=%d batch=%d seg=%d",
			walWriteBufBytes, walBatchMaxBytes, replaySegmentBytes)
	}
}
