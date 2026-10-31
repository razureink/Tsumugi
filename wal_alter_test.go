package main

// ALTER 原子性回归：结构重建走单条 WAL 记录，任何截断都必须回退到旧表。

import (
	"os"
	"path/filepath"
	"testing"
)

// 正常路径：ALTER 后重启，新结构 + 行内容都要正确。
func TestAlterAtomicReplay(t *testing.T) {
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, db, "CREATE TABLE ar2(id INT PRIMARY KEY, a VARCHAR, b INT)")
	mustRun(t, db, "INSERT INTO ar2 VALUES (1,'x',10),(2,'y',20),(3,NULL,30)")
	if _, _, _, _, err := db.runSQL("ALTER TABLE ar2 DROP COLUMN b"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	db.Close()

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	rows := rowsOf(t, db2, "SELECT id, a FROM ar2 ORDER BY id")
	if len(rows) != 3 || rows[0][1] != "x" || rows[2][1] != nil {
		t.Fatalf("after restart rows=%v", rows)
	}
	// 旧列不应再存在
	if _, _, _, _, err := db2.runSQL("SELECT b FROM ar2"); err == nil {
		t.Fatal("dropped column still selectable after restart")
	}
}

// 崩溃路径：把 ALTER 记录在任意字节处截断，重放都必须回到旧结构 + 全部旧行。
// 这是"单条记录原子生效"的核心保证：不存在半新半旧的中间态。
func TestAlterAtomicTornTail(t *testing.T) {
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, db, "CREATE TABLE at2(id INT PRIMARY KEY, a VARCHAR, b INT)")
	mustRun(t, db, "INSERT INTO at2 VALUES (1,'x',10),(2,'y',20)")
	// batch 模式提交是异步的：先显式落盘，确保 base 偏移覆盖建表+插入。
	if err := db.flushWAL(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, cfg.WALFile))
	if err != nil {
		t.Fatal(err)
	}
	base := fi.Size() // ALTER 记录起始偏移
	if _, _, _, _, err := db.runSQL("ALTER TABLE at2 DROP COLUMN b"); err != nil {
		t.Fatalf("alter: %v", err)
	}
	db.Close()
	path := filepath.Join(dir, cfg.WALFile)
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	full := fi2.Size()
	tmpl, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if full-base < 8 {
		t.Fatalf("alter record too small (%d bytes) to test truncation", full-base)
	}
	for cut := base + 1; cut < full; cut++ {
		d2 := t.TempDir()
		cfg2 := walTestConfig(d2)
		if err := os.WriteFile(filepath.Join(d2, cfg2.WALFile), tmpl[:cut], 0600); err != nil {
			t.Fatal(err)
		}
		db2, err := NewDB(cfg2)
		if err != nil {
			t.Fatalf("cut=%d (+%d): replay must not fail: %v", cut, cut-base, err)
		}
		// 旧结构必须仍可用，且两行都在
		_, rows, _, _, e := db2.runSQL("SELECT id, a, b FROM at2 ORDER BY id")
		if e != nil {
			db2.Close()
			t.Fatalf("cut=%d (+%d): old schema unusable: %v", cut, cut-base, e)
		}
		if len(rows) != 2 || rows[0][2] != int64(10) || rows[1][2] != int64(20) {
			db2.Close()
			t.Fatalf("cut=%d (+%d): rows=%v", cut, cut-base, rows)
		}
		db2.Close()
	}
}

// 重建期间禁止写入：否则"扫描之后写入的行"会在新表里丢失。
func TestAlterBlocksWrites(t *testing.T) {
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustRun(t, db, "CREATE TABLE aw(id INT PRIMARY KEY, a VARCHAR, b INT)")
	mustRun(t, db, "INSERT INTO aw VALUES (1,'x',1)")
	tbl := db.getTable("aw")
	tbl.migrating.Store(true)
	_, _, _, _, err = db.runSQL("INSERT INTO aw VALUES (2,'y',2)")
	tbl.migrating.Store(false)
	if err == nil {
		t.Fatal("write during migration should be rejected")
	}
	if _, _, _, _, e := db.runSQL("INSERT INTO aw VALUES (2,'y',2)"); e != nil {
		t.Fatalf("write after migration should succeed: %v", e)
	}
}
