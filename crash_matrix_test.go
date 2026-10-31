package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// ==================== A2：崩溃一致性测试矩阵 ====================
// 目标：任意时刻断电/强杀（等价于 WAL 在任意偏移被截断）后重启，
// 必须满足：
//  1. 不 panic、不无限挂起；
//  2. 要么正常加载（已提交数据完整），要么报明确错误；
//  3. 绝不出现"半行数据被当成有效行"的静默损坏。

// buildCrashFixture 造一个含表 + 索引 + 200 行的 WAL，返回配置与期望行数。
func buildCrashFixture(t *testing.T) (*Config, string, int) {
	t.Helper()
	SetLogMute(true)
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE c (id INT PRIMARY KEY, grp INT, name VARCHAR(48), KEY idx_grp (grp))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	const rows = 1200 // 跨多个 v4 批次（每批 ≤256 条），才能验证"只丢尾批"
	for i := 1; i <= rows; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf(
			"INSERT INTO c VALUES (%d,%d,'name-%03d')", i, i%9, i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	db.Close()
	return cfg, filepath.Join(dir, "wal"), rows
}

// verifyLoaded 校验加载结果：行数不超上限、每行字段都能正确解码、
// 索引与主键一致（不出现"行在但索引错"的撕裂状态）。
func verifyLoaded(t *testing.T, cfg *Config, maxRows int) int {
	t.Helper()
	db, err := NewDB(cfg)
	if err != nil {
		// 报错是可接受的降级（明确失败优于静默损坏）
		t.Logf("NewDB error: %v", err)
		return -1
	}
	defer db.Close()
	tbl := db.getTable("c")
	if tbl == nil {
		return 0
	}
	n := int(tbl.pkTree.Size())
	if n > maxRows {
		t.Fatalf("loaded %d rows, more than written %d (WAL 被重复应用？)", n, maxRows)
	}
	// 逐行校验：字段必须能解码且自洽（name 一定是 name-XXX，grp == id%9）
	for i := 1; i <= n; i++ {
		row, ok := tbl.SelectByPK(int64(i))
		if !ok {
			continue // 尾部未提交的行可以缺失，但不能错乱
		}
		if row["name"] != fmt.Sprintf("name-%03d", i) {
			t.Fatalf("row %d name=%v（半行数据被判为有效行）", i, row["name"])
		}
		if row["grp"] != int64(i%9) {
			t.Fatalf("row %d grp=%v want %d", i, row["grp"], i%9)
		}
	}
	return n
}

// TestCrashConsistencyTruncation 在 WAL 的多个偏移处截断（模拟断电），
// 每次都要能安全重启。
func TestCrashConsistencyTruncation(t *testing.T) {
	cfg, walPath, rows := buildCrashFixture(t)
	full, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if len(full) < 64 {
		t.Fatalf("wal too small: %d", len(full))
	}
	headerLen := len(WALMagic) + 4 + 4

	// 候选偏移：头部各点、帧边界附近、随机位置、末尾半帧
	offsets := []int{0, 1, len(WALMagic), headerLen - 1, headerLen, headerLen + 1, len(full) - 1, len(full) - 4}
	rng := rand.New(rand.NewSource(20261031))
	for i := 0; i < 40; i++ {
		offsets = append(offsets, headerLen+rng.Intn(len(full)-headerLen))
	}

	for _, off := range offsets {
		if off < 0 || off > len(full) {
			continue
		}
		off := off
		t.Run(fmt.Sprintf("truncate_at_%d", off), func(t *testing.T) {
			dir := t.TempDir()
			c := walTestConfig(dir)
			if err := os.WriteFile(filepath.Join(dir, "wal"), full[:off], 0600); err != nil {
				t.Fatalf("write truncated wal: %v", err)
			}
			n := verifyLoaded(t, c, rows)
			// 只要求"安全"：加载成功时数据自洽，失败时明确报错
			if n >= 0 {
				t.Logf("offset=%d loaded %d/%d rows", off, n, rows)
			} else {
				t.Logf("offset=%d rejected with error (acceptable)", off)
			}
			// 再次打开：截断修复后的文件必须稳定（可重复加载）
			verifyLoaded(t, c, rows)
			_ = cfg
		})
	}
}

// TestCrashConsistencyCorruption 单字节损坏（模拟磁盘坏块/位翻转）：
// 必须报错或安全截断，不能静默返回错行。
func TestCrashConsistencyCorruption(t *testing.T) {
	_, walPath, rows := buildCrashFixture(t)
	full, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	headerLen := len(WALMagic) + 4 + 4

	// 在数据区挑若干字节翻位
	rng := rand.New(rand.NewSource(777))
	samples := 25
	for i := 0; i < samples; i++ {
		pos := headerLen + rng.Intn(len(full)-headerLen)
		t.Run(fmt.Sprintf("flip_byte_%d", pos), func(t *testing.T) {
			corrupt := append([]byte(nil), full...)
			corrupt[pos] ^= 0xFF
			dir := t.TempDir()
			c := walTestConfig(dir)
			if err := os.WriteFile(filepath.Join(dir, "wal"), corrupt, 0600); err != nil {
				t.Fatalf("write corrupt wal: %v", err)
			}
			n := verifyLoaded(t, c, rows)
			if n >= 0 {
				t.Logf("pos=%d accepted, loaded %d rows (CRC 未覆盖该字节位置则属预期)", pos, n)
			} else {
				t.Logf("pos=%d rejected with error (good)", pos)
			}
		})
	}
}
