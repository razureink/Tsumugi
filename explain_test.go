package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ==================== C5：EXPLAIN / 慢查询日志 测试 ====================

func explainDB(t *testing.T) (*DB, func()) {
	t.Helper()
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE e (id INT PRIMARY KEY, age INT, name VARCHAR(32), KEY idx_age (age))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 100; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf(
			"INSERT INTO e VALUES (%d,%d,'nm-%d')", i, i%10, i)); err != nil {
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

// explainType 执行 EXPLAIN 并取出 type 列（访问路径）。
func explainType(t *testing.T, db *DB, sql string) (string, string, int64) {
	t.Helper()
	cols, rows, _, _, err := db.runSQL("EXPLAIN " + sql)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", sql, err)
	}
	if len(rows) != 1 {
		t.Fatalf("EXPLAIN %s 返回 %d 行，want 1", sql, len(rows))
	}
	idx := map[string]int{}
	for i, c := range cols {
		idx[c] = i
	}
	r := rows[0]
	accessType, _ := r[idx["type"]].(string)
	key, _ := r[idx["key"]].(string)
	est, _ := r[idx["rows"]].(int64)
	return accessType, key, est
}

// TestExplainAccessPaths EXPLAIN 报告的访问路径必须与执行器实际选择一致。
func TestExplainAccessPaths(t *testing.T) {
	db, closeDB := explainDB(t)
	defer closeDB()

	cases := []struct {
		sql      string
		wantType string
		wantKey  string
	}{
		{"SELECT * FROM e WHERE id = 5", "const", "PRIMARY"},
		{"SELECT id FROM e WHERE age = 3", "ref", "idx_age"},
		{"SELECT * FROM e WHERE age > 5", "range", "idx_age"},
		{"SELECT * FROM e", "ALL", ""},
	}
	for _, c := range cases {
		typ, key, _ := explainType(t, db, c.sql)
		if typ != c.wantType {
			t.Fatalf("%s: type=%q want %q", c.sql, typ, c.wantType)
		}
		if key != c.wantKey {
			t.Fatalf("%s: key=%q want %q", c.sql, key, c.wantKey)
		}
	}
}

// TestExplainShape 校验输出的列集合与 MySQL EXPLAIN 一致（客户端工具按列名取值）。
func TestExplainShape(t *testing.T) {
	db, closeDB := explainDB(t)
	defer closeDB()
	cols, rows, _, _, err := db.runSQL("EXPLAIN SELECT * FROM e WHERE id = 1")
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	want := []string{"id", "select_type", "table", "partitions", "type",
		"possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra"}
	if strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Fatalf("列不匹配:\n got=%v\nwant=%v", cols, want)
	}
	if len(rows) != 1 {
		t.Fatalf("EXPLAIN 应返回 1 行，实际 %d 行", len(rows))
	}
	if len(rows[0]) != len(want) {
		t.Fatalf("首行 %d 列，want %d 列", len(rows[0]), len(want))
	}
}

// TestExplainOnlySelect EXPLAIN 非 SELECT 必须明确报错，而不是给出假计划。
func TestExplainOnlySelect(t *testing.T) {
	db, closeDB := explainDB(t)
	defer closeDB()
	if _, _, _, _, err := db.runSQL("EXPLAIN DELETE FROM e WHERE id = 1"); err == nil {
		t.Fatal("EXPLAIN DELETE 应报错")
	}
}

// TestSlowQueryLog 超阈值语句必须进慢查询日志，且 SHOW SLOWQUERIES 能查出来。
func TestSlowQueryLog(t *testing.T) {
	resetSlowQueries()
	db, closeDB := explainDB(t)
	defer closeDB()
	// 必须在 NewDB **之后**设置：NewDB 会按 cfg.SlowQueryMS 覆盖全局阈值。
	SetSlowQueryThreshold(time.Nanosecond) // 所有语句都算慢
	defer SetSlowQueryThreshold(0)

	// 阈值生效之后必须真的执行一条语句，否则日志当然是空的
	//（SHOW SLOWQUERIES 本身在结果返回后才记账，不会包含自己）
	if _, _, _, _, err := db.runSQL("CREATE TABLE slow_probe (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("probe: %v", err)
	}

	cols, rows, _, _, err := db.runSQL("SHOW SLOWQUERIES")
	if err != nil {
		t.Fatalf("SHOW SLOWQUERIES: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("慢查询日志为空（阈值 1ns 下应有记录）")
	}
	idx := map[string]int{}
	for i, c := range cols {
		idx[c] = i
	}
	// 至少要能取到 SQL 文本与耗时
	found := false
	for _, r := range rows {
		sqlText, _ := r[idx["sql_text"]].(string)
		if strings.Contains(strings.ToUpper(sqlText), "SLOW_PROBE") {
			found = true
			if ms, ok := r[idx["query_time_ms"]].(float64); !ok || ms < 0 {
				t.Fatalf("query_time_ms 非法: %v", r[idx["query_time_ms"]])
			}
			break
		}
	}
	if !found {
		t.Fatal("慢查询日志里找不到刚执行的 CREATE TABLE")
	}
}

// TestSlowQueryDisabled 阈值为 0 时不得记录任何东西（正常路径零开销）。
func TestSlowQueryDisabled(t *testing.T) {
	resetSlowQueries()
	SetSlowQueryThreshold(0)
	db, closeDB := explainDB(t)
	defer closeDB()
	if _, _, _, _, err := db.runSQL("SELECT 1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got := len(snapshotSlowQueries()); got != 0 {
		t.Fatalf("关闭慢查询后仍记录了 %d 条", got)
	}
}

// TestSlowQueryRingBounded 环形缓冲必须有界，不能无限增长。
func TestSlowQueryRingBounded(t *testing.T) {
	resetSlowQueries()
	for i := 0; i < slowLogCapacity*3; i++ {
		recordSlowQuery("u", time.Millisecond, fmt.Sprintf("SELECT %d", i), 1, false)
	}
	got := len(snapshotSlowQueries())
	if got != slowLogCapacity {
		t.Fatalf("环形缓冲 size=%d want %d（必须有界）", got, slowLogCapacity)
	}
	// 最新一条必须是最后写入的
	snap := snapshotSlowQueries()
	last := snap[len(snap)-1].SQL
	if last != fmt.Sprintf("SELECT %d", slowLogCapacity*3-1) {
		t.Fatalf("环形缓冲未保留最新记录: %q", last)
	}
}
