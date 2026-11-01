package main

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// TestWALVersionCompat 校验版本策略：
//   - 新写的 WAL 头是 v5（v4 + 1，v4 又是远端 v3 + 1）；
//   - 把头部改成 v4 / v3 后，仍然可以被完整读出（跨版本兼容）。
//
// v3/v4/v5 的分帧布局相同（[varint len][u32 crc][payload]），版本号只标识
// 记录语义。因此"降版本号再读"是该格式最直接的兼容性验证方式。
func TestWALVersionCompat(t *testing.T) {
	SetLogMute(true)
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	cfg.WALCompress = "off" // 本测试验证版本/帧兼容性，固定不压缩

	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE TABLE w (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 200; i++ {
		if _, _, _, _, err := db.runSQL("INSERT INTO w VALUES (" + itoa(i) + "," + itoa(i*2) + ")"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	db.Close()

	// 头部版本应为 v6（v5 + 帧级压缩标志）
	path := filepath.Join(dir, "wal")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if got := binary.BigEndian.Uint32(data[len(WALMagic):]); got != WALVersion6 {
		t.Fatalf("written WAL version = %d, want %d", got, WALVersion6)
	}

	// v6 的帧长度带压缩标志，不能只改头版本号。先把帧重写回 v5 布局
	// （长度左移掉标志位），再降版本号验证 v5/v4/v3 都能读。
	data = downgradeWALV6Frames(t, data)
	for _, ver := range []uint32{WALVersion5, WALVersion4, WALVersion3} {
		binary.BigEndian.PutUint32(data[len(WALMagic):], ver)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatalf("rewrite wal as v%d: %v", ver, err)
		}
		db2, err := NewDB(cfg)
		if err != nil {
			t.Fatalf("reopen v%d file: %v", ver, err)
		}
		tbl := db2.getTable("w")
		if tbl == nil {
			db2.Close()
			t.Fatalf("table missing after loading v%d WAL", ver)
		}
		if got := tbl.pkTree.Size(); got != 200 {
			db2.Close()
			t.Fatalf("rows after loading v%d WAL = %d want 200", ver, got)
		}
		row, ok := tbl.SelectByPK(7)
		if !ok || row["v"] != int64(14) {
			db2.Close()
			t.Fatalf("v%d row 7 = %v/%v", ver, row, ok)
		}
		db2.Close()
	}
}

// downgradeWALV6Frames 把 v6 帧重写为 v5 布局（长度不再左移、无压缩标志），
// 用于验证 v5/v4/v3 读取路径。v6 的 CRC 覆盖存盘字节；若帧被压缩，
// 解压后需要按解压内容重算 CRC。
func downgradeWALV6Frames(t *testing.T, data []byte) []byte {
	t.Helper()
	headerLen := len(WALMagic) + 8
	if len(data) < headerLen {
		t.Fatalf("wal too short: %d bytes", len(data))
	}
	out := append([]byte(nil), data[:headerLen]...)
	pos := headerLen
	for pos < len(data) {
		start := pos
		raw, nb := binary.Uvarint(data[pos:])
		if nb <= 0 {
			t.Fatalf("invalid frame length at %d", start)
		}
		pos += nb
		storedLen, compressed, err := decodeFrameLen(raw, true)
		if err != nil {
			t.Fatalf("frame at %d: %v", start, err)
		}
		if pos+walBatchCRCBytes+storedLen > len(data) {
			t.Fatalf("frame at %d truncated", start)
		}
		crc := append([]byte(nil), data[pos:pos+walBatchCRCBytes]...)
		payload := data[pos+walBatchCRCBytes : pos+walBatchCRCBytes+storedLen]
		pos += walBatchCRCBytes + storedLen
		if compressed {
			inflated, derr := decompressWALPayload(payload)
			if derr != nil {
				t.Fatalf("frame at %d: %v", start, derr)
			}
			payload = inflated
			binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(payload))
		}
		out = appendFrameLen(out, len(payload), false, false)
		out = append(out, crc...)
		out = append(out, payload...)
	}
	return out
}
