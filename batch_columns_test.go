package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestInsertBatchExplicitColumns 显式列清单（且列序与表不同）也必须走批量路径，
// 并在重开后保持一致——验证"列置换"的正确性。
func TestInsertBatchExplicitColumns(t *testing.T) {
	SetLogMute(true)
	cfg := walTestConfig(t.TempDir())
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
	}()
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE e (id INT PRIMARY KEY, name VARCHAR(32), grp INT, KEY idx_grp (grp))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 列顺序与表定义相反
	var sb strings.Builder
	sb.WriteString("INSERT INTO e (grp, name, id) VALUES ")
	const n = 300
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'nm-%d',%d)", i%11, i, i)
	}
	before0 := batchInsertCalls.Load()
	if _, _, _, _, err := db.runSQL(sb.String()); err != nil {
		t.Fatalf("batch insert with explicit columns: %v", err)
	}
	if batchInsertCalls.Load() == before0 {
		t.Fatal("batch insert path was not used for explicit column list")
	}
	tbl := db.getTable("e")
	if got := tbl.pkTree.Size(); got != n {
		t.Fatalf("rows=%d want %d", got, n)
	}
	// 列值必须落到正确字段（不能错位）
	for i := 1; i <= n; i++ {
		row, ok := tbl.SelectByPK(int64(i))
		if !ok {
			t.Fatalf("row %d missing", i)
		}
		if row["name"] != fmt.Sprintf("nm-%d", i) || row["grp"] != int64(i%11) {
			t.Fatalf("row %d misplaced: %v", i, row)
		}
	}
	before := mustExec(t, db, "SELECT id FROM e WHERE grp = 5")
	db.Close()
	closed = true

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	tbl2 := db2.getTable("e")
	if tbl2 == nil {
		t.Fatal("table missing after replay")
	}
	if got := tbl2.pkTree.Size(); got != n {
		t.Fatalf("rows after replay=%d want %d", got, n)
	}
	for i := 1; i <= n; i++ {
		row, ok := tbl2.SelectByPK(int64(i))
		if !ok || row["name"] != fmt.Sprintf("nm-%d", i) || row["grp"] != int64(i%11) {
			t.Fatalf("row %d wrong after replay: %v", i, row)
		}
	}
	after := mustExec(t, db2, "SELECT id FROM e WHERE grp = 5")
	if len(after) != len(before) {
		t.Fatalf("indexed rows after replay=%d want %d", len(after), len(before))
	}
}
