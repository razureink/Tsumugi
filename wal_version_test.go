package main

import (
	"encoding/binary"
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

	// 头部版本应为 v5
	path := filepath.Join(dir, "wal")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if got := binary.BigEndian.Uint32(data[len(WALMagic):]); got != WALVersion5 {
		t.Fatalf("written WAL version = %d, want %d", got, WALVersion5)
	}

	// 依次把版本改小到 v4（上一代本项目格式）与 v3（远端格式），都必须能读出。
	for _, ver := range []uint32{WALVersion4, WALVersion3} {
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
