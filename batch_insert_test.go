package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// batchBenchSeq 基准用的全局主键序列：框架会先跑标定轮再跑正式轮，
// 若每轮都从 0 开始会与上一轮写入的行撞主键。
var batchBenchSeq atomic.Int64

// ==================== 批量写路径（多行 INSERT）测试与基准 ====================

// TestInsertBatchWALReplay 多行 INSERT 走批量 WAL 记录后，关库重开必须完整恢复
//（行数、字段值、二级索引都要对）——这是批量记录格式正确性的关键验证。
func TestInsertBatchWALReplay(t *testing.T) {
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
		"CREATE TABLE b (id INT PRIMARY KEY, grp INT, name VARCHAR(32), KEY idx_grp (grp))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 一条语句插入 500 行（命中批量路径）
	const n = 500
	var sb strings.Builder
	sb.WriteString("INSERT INTO b (id, grp, name) VALUES ")
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,%d,'n-%d')", i, i%7, i)
	}
	before0 := batchInsertCalls.Load()
	if _, _, _, _, err := db.runSQL(sb.String()); err != nil {
		t.Fatalf("batch insert: %v", err)
	}
	if batchInsertCalls.Load() == before0 {
		t.Fatal("batch insert path was not used")
	}
	tbl := db.getTable("b")
	if got := tbl.pkTree.Size(); got != n {
		t.Fatalf("rows after batch insert=%d want %d", got, n)
	}
	before := mustExec(t, db, "SELECT id FROM b WHERE grp = 3")
	db.Close()
	closed = true

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	tbl2 := db2.getTable("b")
	if tbl2 == nil {
		t.Fatal("table missing after replay")
	}
	if got := tbl2.pkTree.Size(); got != n {
		t.Fatalf("rows after replay=%d want %d (batch WAL record not parsed correctly?)", got, n)
	}
	// 逐行校验值
	for i := 1; i <= n; i++ {
		row, ok := tbl2.SelectByPK(int64(i))
		if !ok {
			t.Fatalf("row %d missing after replay", i)
		}
		if row["name"] != fmt.Sprintf("n-%d", i) {
			t.Fatalf("row %d name=%v", i, row["name"])
		}
	}
	// 二级索引（批量路径按桶合并写入）必须完好
	after := mustExec(t, db2, "SELECT id FROM b WHERE grp = 3")
	if len(after) != len(before) {
		t.Fatalf("indexed rows after replay=%d want %d", len(after), len(before))
	}
}

// TestInsertBatchConflictAtomic 批量路径下主键冲突仍必须整句回滚。
func TestInsertBatchConflictAtomic(t *testing.T) {
	db, closeDB := openIndexTestDB(t)
	defer closeDB()
	mustExec(t, db, "CREATE TABLE c (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "INSERT INTO c VALUES (1, 1)")
	// 第二行与已有行冲突：整句失败，且 2、3 行都不能落库
	if _, _, _, _, err := db.runSQL("INSERT INTO c VALUES (2,2),(1,9),(3,3)"); err == nil {
		t.Fatal("expected conflict error")
	}
	rows := mustExec(t, db, "SELECT id FROM c")
	if len(rows) != 1 {
		t.Fatalf("leaked rows after failed batch insert: %v", rows)
	}
}

// BenchmarkBatchInsert 对比"一条语句 1000 行"与"1000 条单行语句"的吞吐。
// 用一个 DB + 两张表，避免反复建库（WAL 通道是进程级单例）。
func BenchmarkBatchInsert(b *testing.B) {
	const perStmt = 1000
	SetLogMute(true)
	cfg := walTestConfig(b.TempDir())
	db, err := NewDB(cfg)
	if err != nil {
		b.Fatalf("NewDB: %v", err)
	}
	b.Cleanup(db.Close)
	for _, tbl := range []string{"bi_batch", "bi_single"} {
		if _, _, _, _, err := db.runSQL(
			"CREATE TABLE " + tbl + " (id INT PRIMARY KEY, grp INT, name VARCHAR(32), KEY idx_grp (grp))"); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("batch-stmt", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var sb strings.Builder
			sb.WriteString("INSERT INTO bi_batch (id, grp, name) VALUES ")
			for r := 0; r < perStmt; r++ {
				if r > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, "(%d,%d,'x')", batchBenchSeq.Add(1), r%50)
			}
			if _, _, _, _, err := db.runSQL(sb.String()); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(perStmt), "rows/op")
	})

	b.Run("single-stmt", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for r := 0; r < perStmt; r++ {
				q := fmt.Sprintf("INSERT INTO bi_single (id, grp, name) VALUES (%d,%d,'x')", batchBenchSeq.Add(1), r%50)
				if _, _, _, _, err := db.runSQL(q); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.ReportMetric(float64(perStmt), "rows/op")
	})
}
