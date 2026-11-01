package main

// ==================== WAL v6：帧级压缩 ====================
// v6 的帧长度 varint 最低位是压缩标志；payload 为 [varint 原始长度][deflate]。
// 这组测试验证：压缩往返、Compact 压缩、帧标志真的带上、截断尾部仍能启动、
// 以及磁盘满的稳定错误映射（极端设备上空间不足是常态）。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type walFrameInfo struct {
	start      int
	storedLen  int
	compressed bool
}

func walV6FrameInfos(t *testing.T, data []byte) []walFrameInfo {
	t.Helper()
	ver := binary.BigEndian.Uint32(data[len(WALMagic):])
	if ver < WALVersion6 {
		t.Fatalf("expected v6+, got v%d", ver)
	}
	var out []walFrameInfo
	pos := len(WALMagic) + 8
	for pos < len(data) {
		start := pos
		raw, nb := binary.Uvarint(data[pos:])
		if nb <= 0 {
			t.Fatalf("bad frame length at %d", start)
		}
		pos += nb
		stored, compressed, err := decodeFrameLen(raw, true)
		if err != nil {
			t.Fatalf("frame at %d: %v", start, err)
		}
		if pos+walBatchCRCBytes+stored > len(data) {
			t.Fatalf("frame at %d truncated", start)
		}
		out = append(out, walFrameInfo{start: start, storedLen: stored, compressed: compressed})
		pos += walBatchCRCBytes + stored
	}
	return out
}

// seedCompressible 写入大量可压缩数据（重复文本 + 递增整数）。
func seedCompressible(t *testing.T, db *DB, rows int) {
	t.Helper()
	mustRun(t, db, "CREATE TABLE comp (id INT PRIMARY KEY, s TEXT, n INT)")
	blob := strings.Repeat("abcdefgh", 32) // 256B，重复度高
	for base := 1; base <= rows; base += 50 {
		var sb strings.Builder
		sb.WriteString("INSERT INTO comp VALUES ")
		for i := 0; i < 50 && base+i <= rows; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "(%d,'%s',%d)", base+i, blob, (base+i)*3)
		}
		mustRun(t, db, sb.String())
	}
}

func walSize(t *testing.T, dir string) int64 {
	t.Helper()
	st, err := os.Stat(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	return st.Size()
}

// TestWALV6CompressionRoundTrip 压缩写入 + Compact + 重开验证。
func TestWALV6CompressionRoundTrip(t *testing.T) {
	SetLogMute(true)
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	cfg.WALCompress = "on"

	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	seedCompressible(t, db, 2000)
	if _, err := db.WALPosition(); err != nil {
		t.Fatalf("position: %v", err)
	}
	// Compact 走 walBatchWriter，同样必须写 v6 帧并压缩
	if err := db.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	db.Close()

	data, err := os.ReadFile(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if got := binary.BigEndian.Uint32(data[len(WALMagic):]); got != WALVersion6 {
		t.Fatalf("version=%d want %d", got, WALVersion6)
	}
	frames := walV6FrameInfos(t, data)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	compressed := 0
	for _, f := range frames {
		if f.compressed {
			compressed++
		}
	}
	if compressed == 0 {
		t.Fatal("expected at least one compressed frame (WALCompress=on)")
	}

	// 重开必须逐行还原
	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if n := countRows(t, db2, "comp"); n != 2000 {
		t.Fatalf("rows after reopen=%d want 2000", n)
	}
	row, ok := db2.getTable("comp").SelectByPK(777)
	if !ok || row["n"] != int64(777*3) {
		t.Fatalf("row 777 = %v/%v", row, ok)
	}
}

// TestWALV6CompressionShrinksFile 压缩文件应明显小于同数据的未压缩文件。
func TestWALV6CompressionShrinksFile(t *testing.T) {
	SetLogMute(true)
	dirOff := t.TempDir()
	cfgOff := walTestConfig(dirOff)
	cfgOff.WALCompress = "off"
	dbOff, err := NewDB(cfgOff)
	if err != nil {
		t.Fatalf("off NewDB: %v", err)
	}
	seedCompressible(t, dbOff, 1000)
	dbOff.Close()
	offSize := walSize(t, dirOff)

	dirOn := t.TempDir()
	cfgOn := walTestConfig(dirOn)
	cfgOn.WALCompress = "on"
	dbOn, err := NewDB(cfgOn)
	if err != nil {
		t.Fatalf("on NewDB: %v", err)
	}
	seedCompressible(t, dbOn, 1000)
	dbOn.Close()
	onSize := walSize(t, dirOn)

	if onSize >= offSize {
		t.Fatalf("compressed WAL %d bytes is not smaller than %d", onSize, offSize)
	}
	// 重复度这么高的数据，至少应省 20%
	if onSize > offSize*8/10 {
		t.Fatalf("compression too weak: on=%d off=%d", onSize, offSize)
	}
	t.Logf("WAL size: uncompressed=%d bytes, compressed=%d bytes (%.1f%% saved)",
		offSize, onSize, 100*(1-float64(onSize)/float64(offSize)))
}

// TestWALV6TruncatedCompressedTail 压缩帧被截断时按帧边界丢弃，仍能启动。
func TestWALV6TruncatedCompressedTail(t *testing.T) {
	SetLogMute(true)
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	cfg.WALCompress = "on"
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	// 周期性 flush 强制切成多帧：这样才能验证"只丢最后一帧"，
	// 而不是整份 WAL 只有一个大帧（截断尾部会把所有行一起丢掉）。
	mustRun(t, db, "CREATE TABLE comp (id INT PRIMARY KEY, s TEXT, n INT)")
	blob := strings.Repeat("abcdefgh", 32)
	for i := 1; i <= 400; i++ {
		mustRun(t, db, fmt.Sprintf("INSERT INTO comp VALUES (%d,'%s',%d)", i, blob, i*3))
		if i%100 == 0 {
			if _, err := db.WALPosition(); err != nil {
				t.Fatalf("flush: %v", err)
			}
		}
	}
	db.Close()

	path := filepath.Join(dir, "wal")
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(full) < 200 {
		t.Fatalf("wal too small: %d", len(full))
	}
	// 砍掉最后一帧的一部分（模拟断电半写）
	truncated := full[:len(full)-17]
	if err := os.WriteFile(path, truncated, 0600); err != nil {
		t.Fatalf("write truncated: %v", err)
	}

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen truncated: %v", err)
	}
	defer db2.Close()
	n := countRows(t, db2, "comp")
	if n <= 0 || n > 400 {
		t.Fatalf("rows after truncated tail = %d, want 1..400", n)
	}
	// 尾部已被截断到最后一个完整帧
	st, _ := os.Stat(path)
	if st.Size() > int64(len(truncated)) {
		t.Fatalf("wal was not truncated: %d > %d", st.Size(), len(truncated))
	}
}

// TestDiskFullFaultIsStickyAndMaps1021 磁盘满进入只读故障态，且映射 MySQL 1021。
func TestDiskFullFaultIsStickyAndMaps1021(t *testing.T) {
	defer walFaultReset()
	if !isDiskFull(errors.New("write wal: no space left on device")) {
		t.Fatal("isDiskFull should recognize the error text")
	}
	if isDiskFull(errors.New("permission denied")) {
		t.Fatal("isDiskFull must not match unrelated errors")
	}
	walFail(errors.New("write wal: no space left on device"))
	if !walDiskFull.Load() {
		t.Fatal("walDiskFull not set")
	}
	if err := appendWAL(nil, false); !errors.Is(err, errDiskFull) {
		t.Fatalf("appendWAL returned %v, want errDiskFull", err)
	}
	if code := mysqlErrno(errDiskFull); code != ERDiskFull {
		t.Fatalf("errno=%d want %d", code, ERDiskFull)
	}
	walFaultReset()
	if walDiskFull.Load() {
		t.Fatal("walFaultReset must clear disk-full flag")
	}
}
