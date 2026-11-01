package main

import (
	"fmt"
	"testing"
)

// ==================== 索引等值 + 主键范围 组合回归 ====================
// 缺陷：`WHERE 索引列 = 常量 AND 主键 > N` 会忽略主键范围，返回范围外的行。
// 根因（已定位）：SQL 层把主键范围折叠成 minKey/maxKey 并**不放进 filters**，
// 而覆盖索引 coveringRows 的签名里没有这两个参数，于是把它们整个丢掉了。
// 修复：coveringRows 接收 minKey/maxKey，并在等值/范围两个分支里都过滤主键。

func rangeBugDB(t *testing.T) (*DB, func()) {
	t.Helper()
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE r (id INT PRIMARY KEY, dept INT, KEY idx_dept (dept))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 20; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("INSERT INTO r VALUES (%d,%d)", i, i%3)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	closed := false
	return db, func() {
		if !closed {
			closed = true
			db.Close()
		}
	}
}

// TestIndexedEqualityPlusPKRange 索引等值与主键范围必须同时生效。
func TestIndexedEqualityPlusPKRange(t *testing.T) {
	db, closeDB := rangeBugDB(t)
	defer closeDB()

	// dept=0 → id ∈ {3,6,9,12,15,18}；再 id>3 → {6,9,12,15,18} = 5 行
	_, rows, _, _, err := db.runSQL("SELECT id FROM r WHERE dept = 0 AND id > 3")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("行数=%d want 5（主键范围被忽略）: %v", len(rows), rows)
	}
	for _, r := range rows {
		if id := r[0].(int64); id <= 3 {
			t.Fatalf("返回了范围外的行 id=%d", id)
		}
	}

	// BETWEEN：dept=1 → {1,4,7,10,13,16,19}，[5,10] → {7,10} = 2 行
	_, rows, _, _, err = db.runSQL("SELECT id FROM r WHERE dept = 1 AND id BETWEEN 5 AND 10")
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("BETWEEN 行数=%d want 2: %v", len(rows), rows)
	}

	// 上界：dept=2 → {2,5,8,11,14,17,20}，id<11 → {2,5,8} = 3 行
	_, rows, _, _, err = db.runSQL("SELECT id FROM r WHERE dept = 2 AND id < 11")
	if err != nil {
		t.Fatalf("upper bound: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("上界行数=%d want 3: %v", len(rows), rows)
	}
}

// TestIndexedEqualityPlusPKRangeWithLimit limit 必须在范围过滤之后生效。
func TestIndexedEqualityPlusPKRangeWithLimit(t *testing.T) {
	db, closeDB := rangeBugDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL("SELECT id FROM r WHERE dept = 0 AND id > 3 LIMIT 2")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("行数=%d want 2", len(rows))
	}
	for _, r := range rows {
		if id := r[0].(int64); id <= 3 {
			t.Fatalf("limit 后仍含范围外行 id=%d", id)
		}
	}
}

// TestIndexedEqualityPlusPKRangeProjection 投影含非索引列（回表路径）同样要正确。
func TestIndexedEqualityPlusPKRangeProjection(t *testing.T) {
	db, closeDB := rangeBugDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL("SELECT id, dept FROM r WHERE dept = 0 AND id > 3")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("行数=%d want 5", len(rows))
	}
}

// TestCoveringStillUsedWithoutRange 没有主键范围时覆盖索引不能被修坏。
func TestCoveringStillUsedWithoutRange(t *testing.T) {
	db, closeDB := rangeBugDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL("SELECT id FROM r WHERE dept = 0")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("行数=%d want 6", len(rows))
	}
}
