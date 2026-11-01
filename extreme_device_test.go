package main

// ==================== 极端设备行为回归 ====================
// 低端设备（SD 卡 / U 盘 / 机械盘 / 电池供电）上，空闲期的无意义写入
// 与 fsync 会带来真实磨损和功耗。这里锁定两个行为：
//   1. 空闲时定时 flush 不再产生 fsync；
//   2. 有真实写入后必须恢复 fsync（不能为了省电牺牲持久性）。

import (
	"testing"
	"time"
)

func TestIdleFlushSkipsFsync(t *testing.T) {
	SetLogMute(true)
	cfg := walTestConfig(t.TempDir())
	cfg.FlushInterval = 30 * time.Millisecond
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	mustRun(t, db, "CREATE TABLE idle_t (id INT PRIMARY KEY, v INT)")
	mustRun(t, db, "INSERT INTO idle_t VALUES (1,1)")
	if err := db.flushWAL(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	base := db.stats.Snapshot()["fsync_count"].(uint64)

	// 空转 8 个 flush 周期：不应产生任何 fsync
	time.Sleep(250 * time.Millisecond)
	afterIdle := db.stats.Snapshot()["fsync_count"].(uint64)
	if afterIdle != base {
		t.Fatalf("idle flush issued %d fsync(s); want 0", afterIdle-base)
	}

	// 有真实写入后必须恢复 fsync
	mustRun(t, db, "INSERT INTO idle_t VALUES (2,2)")
	if err := db.flushWAL(); err != nil {
		t.Fatalf("flush after write: %v", err)
	}
	if got := db.stats.Snapshot()["fsync_count"].(uint64); got <= afterIdle {
		t.Fatalf("write did not trigger fsync (count stayed %d)", got)
	}
}
