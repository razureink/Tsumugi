package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestWALVersionCompat 校验版本策略：
//   - 新写的 WAL 头是 v4（远端 v3 + 1）；
//   - 把头部改成远端 v3 后，仍然可以被完整读出（跨版本兼容）。
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

	// 头部版本应为 v4
	path := filepath.Join(dir, "wal")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if got := binary.BigEndian.Uint32(data[len(WALMagic):]); got != WALVersion4 {
		t.Fatalf("written WAL version = %d, want %d", got, WALVersion4)
	}

	// 把版本改回远端 v3：模拟"远端仓库写出的文件"，必须仍能读出
	binary.BigEndian.PutUint32(data[len(WALMagic):], WALVersion3)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("rewrite wal: %v", err)
	}
	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen v3 file: %v", err)
	}
	defer db2.Close()
	tbl := db2.getTable("w")
	if tbl == nil {
		t.Fatal("table missing after loading v3 WAL")
	}
	if got := tbl.pkTree.Size(); got != 200 {
		t.Fatalf("rows after loading v3 WAL = %d want 200", got)
	}
	row, ok := tbl.SelectByPK(7)
	if !ok || row["v"] != int64(14) {
		t.Fatalf("row 7 = %v/%v", row, ok)
	}
}
