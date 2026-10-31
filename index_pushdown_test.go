package main

import (
	"fmt"
	"testing"
)

// ==================== 二级索引下推 / 覆盖索引 测试 ====================
// 注意：WAL 通道是包级单例，同一进程内不能同时开两个 DB 实例，
// 需要对比的用例必须顺序执行（先跑 A、关掉，再跑 B）。

// openIndexTestDB 打开一个测试库，返回库和幂等的关闭函数。
func openIndexTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	closed := false
	return db, func() {
		if !closed {
			closed = true
			db.Close()
		}
	}
}

func mustExec(t *testing.T, db *DB, sql string) [][]interface{} {
	t.Helper()
	_, rows, _, _, err := db.runSQL(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return rows
}

// TestIndexRangePushdown 校验索引范围下推与全表扫描结果完全一致：
// 索引只是加速手段，命中集合与顺序都必须与不加索引时相同。
func TestIndexRangePushdown(t *testing.T) {
	const n = 500

	queries := []string{
		"SELECT id, age FROM t WHERE age > 40",
		"SELECT id, age FROM t WHERE age >= 40",
		"SELECT id, age FROM t WHERE age < 10",
		"SELECT id, age FROM t WHERE age <= 10",
		"SELECT id, age FROM t WHERE age BETWEEN 10 AND 20",
		"SELECT id, age FROM t WHERE age > 10 AND age < 20",
		"SELECT id, age FROM t WHERE age > 10 AND name = 'nm-005'",
		"SELECT id FROM t WHERE name > 'nm-030'",
		"SELECT id, age FROM t WHERE name BETWEEN 'nm-005' AND 'nm-012'",
		// 宽范围：候选数超过下推上限，应自动退回顺序扫描，结果仍需一致
		"SELECT id, age FROM t WHERE age >= 0",
		// 命中空集
		"SELECT id, age FROM t WHERE age > 1000",
		"SELECT id, age FROM t WHERE age BETWEEN 900 AND 1000",
	}

	build := func(indexed bool) (*DB, func()) {
		db, closeDB := openIndexTestDB(t)
		schema := "CREATE TABLE t (id INT PRIMARY KEY, age INT, name VARCHAR(32))"
		if indexed {
			schema = "CREATE TABLE t (id INT PRIMARY KEY, age INT, name VARCHAR(32), " +
				"KEY idx_age (age), KEY idx_name (name))"
		}
		mustExec(t, db, schema)
		for i := 1; i <= n; i++ {
			mustExec(t, db, fmt.Sprintf(
				"INSERT INTO t (id, age, name) VALUES (%d, %d, 'nm-%03d')", i, i%50, i%37))
		}
		return db, closeDB
	}

	// 1) 无索引：作为基准
	plain, closePlain := build(false)
	base := make([][][]interface{}, len(queries))
	for i, q := range queries {
		base[i] = mustExec(t, plain, q)
	}
	closePlain()

	// 2) 有索引：结果必须逐行相同
	indexed, closeIndexed := build(true)
	defer closeIndexed()
	for i, q := range queries {
		got := mustExec(t, indexed, q)
		if len(got) != len(base[i]) {
			t.Fatalf("%s: rows indexed=%d plain=%d", q, len(got), len(base[i]))
		}
		for r := range got {
			for c := range got[r] {
				if got[r][c] != base[i][r][c] {
					t.Fatalf("%s: row %d col %d indexed=%v plain=%v",
						q, r, c, got[r][c], base[i][r][c])
				}
			}
		}
	}
}

// TestIndexRangePushdownAfterAlter 校验运行期 ADD INDEX 后下推同样生效。
func TestIndexRangePushdownAfterAlter(t *testing.T) {
	db, closeDB := openIndexTestDB(t)
	defer closeDB()
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	for i := 1; i <= 300; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i%30))
	}
	before := mustExec(t, db, "SELECT id FROM t WHERE v > 20")
	mustExec(t, db, "ALTER TABLE t ADD INDEX idx_v (v)")
	after := mustExec(t, db, "SELECT id FROM t WHERE v > 20")
	if len(before) != len(after) || len(after) == 0 {
		t.Fatalf("rows before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i][0] != after[i][0] {
			t.Fatalf("row %d before=%v after=%v", i, before[i][0], after[i][0])
		}
	}
}

// TestCoveringIndex 校验覆盖索引路径：投影列只含索引列与主键时直接由索引产出，
// 且与回表结果一致（含多行同值、空结果、LIMIT）。
func TestCoveringIndex(t *testing.T) {
	db, closeDB := openIndexTestDB(t)
	defer closeDB()
	mustExec(t, db, "CREATE TABLE u (id INT PRIMARY KEY, email VARCHAR(64), city VARCHAR(32), "+
		"KEY idx_email (email))")
	for i := 1; i <= 200; i++ {
		mustExec(t, db, fmt.Sprintf(
			"INSERT INTO u (id, email, city) VALUES (%d, 'e-%03d@x', 'c%d')", i, i%40, i%5))
	}

	// 覆盖：SELECT id FROM u WHERE email = ...
	rows := mustExec(t, db, "SELECT id FROM u WHERE email = 'e-007@x'")
	if len(rows) != 5 { // 200/40
		t.Fatalf("covering id rows=%d want 5", len(rows))
	}
	seen := map[int64]bool{}
	for _, r := range rows {
		seen[r[0].(int64)] = true
	}
	for _, want := range []int64{7, 47, 87, 127, 167} {
		if !seen[want] {
			t.Fatalf("covering missing id %d (got %v)", want, seen)
		}
	}

	// 覆盖：索引列 + 主键都能直接给出
	rows = mustExec(t, db, "SELECT email, id FROM u WHERE email = 'e-003@x'")
	if len(rows) != 5 {
		t.Fatalf("covering email+id rows=%d want 5", len(rows))
	}
	for _, r := range rows {
		if r[0] != "e-003@x" {
			t.Fatalf("covering email value = %v", r[0])
		}
	}

	// 投影包含非覆盖列：必须回表，结果正确
	rows = mustExec(t, db, "SELECT city FROM u WHERE email = 'e-003@x'")
	if len(rows) != 5 {
		t.Fatalf("non-covering rows=%d want 5", len(rows))
	}

	// 空结果
	rows = mustExec(t, db, "SELECT id FROM u WHERE email = 'nobody@x'")
	if len(rows) != 0 {
		t.Fatalf("covering empty rows=%d want 0", len(rows))
	}

	// LIMIT
	rows = mustExec(t, db, "SELECT id FROM u WHERE email = 'e-001@x' LIMIT 2")
	if len(rows) != 2 {
		t.Fatalf("covering limit rows=%d want 2", len(rows))
	}
}

// TestCoveringIndexDisabledByTTL 校验表内出现过 TTL 行时覆盖索引自动停用，
// 避免跳过回表而漏掉过期判断。
func TestCoveringIndexDisabledByTTL(t *testing.T) {
	db, closeDB := openIndexTestDB(t)
	defer closeDB()
	mustExec(t, db, "CREATE TABLE tt (id INT PRIMARY KEY, email VARCHAR(64), KEY idx_tt_email (email))")
	mustExec(t, db, "INSERT INTO tt (id, email) VALUES (1, 'a@x')")
	tbl := db.getTable("tt")
	if tbl == nil {
		t.Fatal("table missing")
	}
	if tbl.ttlRows.Load() {
		t.Fatal("ttlRows should be false before any TTL row")
	}
	// 覆盖索引此时可用
	if _, ok := tbl.coveringRows(tbl.Meta(),
		map[string]interface{}{"email": "a@x"}, nil, []int{0}, 1, -1); !ok {
		t.Fatal("covering should be available without TTL rows")
	}
	// 写入一条带过期时间的行后，标记置位，覆盖索引必须停用
	if err := tbl.Insert(2, map[string]interface{}{"id": int64(2), "email": "b@x"}, 1, 0, false); err != nil {
		t.Fatalf("insert ttl row: %v", err)
	}
	if !tbl.ttlRows.Load() {
		t.Fatal("ttlRows should be set after inserting TTL row")
	}
	if _, ok := tbl.coveringRows(tbl.Meta(),
		map[string]interface{}{"email": "a@x"}, nil, []int{0}, 1, -1); ok {
		t.Fatal("covering must be disabled once table has TTL rows")
	}
	// 功能仍然正确（走回表路径）
	if rows := mustExec(t, db, "SELECT id FROM tt WHERE email = 'a@x'"); len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
}

// BenchmarkIndexedRangeScan 对比有/无二级索引的等值查询开销。
func BenchmarkIndexedRangeScan(b *testing.B) {
	for _, withIndex := range []bool{false, true} {
		name := "no-index"
		if withIndex {
			name = "with-index"
		}
		b.Run(name, func(b *testing.B) {
			SetLogMute(true)
			db, err := NewDB(walTestConfig(b.TempDir()))
			if err != nil {
				b.Fatalf("NewDB: %v", err)
			}
			defer db.Close()
			schema := "CREATE TABLE t (id INT PRIMARY KEY, age INT, name VARCHAR(32))"
			if withIndex {
				schema = "CREATE TABLE t (id INT PRIMARY KEY, age INT, name VARCHAR(32), KEY idx_age (age))"
			}
			if _, _, _, _, err := db.runSQL(schema); err != nil {
				b.Fatal(err)
			}
			const n = 20000
			for i := 1; i <= n; i++ {
				if _, _, _, _, err := db.runSQL(fmt.Sprintf(
					"INSERT INTO t (id, age, name) VALUES (%d, %d, 'nm')", i, i%1000)); err != nil {
					b.Fatal(err)
				}
			}
			// 窄范围查询：走二级索引范围下推（无索引时退化为全表扫描）
			q := "SELECT id FROM t WHERE age > 995"
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, _, _, err := db.runSQL(q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCoveringVsLookup 对比同一索引范围查询的两种执行计划：
//   - covering: 投影只有主键，索引条目即可回答，不回表
//   - lookup:   投影含非索引列，必须回表取行
func BenchmarkCoveringVsLookup(b *testing.B) {
	for _, tc := range []struct{ name, q string }{
		{"covering", "SELECT id FROM t WHERE age > 995"},
		{"lookup", "SELECT id, name FROM t WHERE age > 995"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			SetLogMute(true)
			db, err := NewDB(walTestConfig(b.TempDir()))
			if err != nil {
				b.Fatalf("NewDB: %v", err)
			}
			defer db.Close()
			if _, _, _, _, err := db.runSQL(
				"CREATE TABLE t (id INT PRIMARY KEY, age INT, name VARCHAR(32), KEY idx_age (age))"); err != nil {
				b.Fatal(err)
			}
			const n = 20000
			for i := 1; i <= n; i++ {
				if _, _, _, _, err := db.runSQL(fmt.Sprintf(
					"INSERT INTO t (id, age, name) VALUES (%d, %d, 'nm')", i, i%1000)); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, _, _, err := db.runSQL(tc.q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
