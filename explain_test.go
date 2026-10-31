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

// TestSlowQueryLog 慢查询日志到 SHOW SLOWQUERIES 的链路必须通。
// 这里直接调用 recordSlowQuery 而不是"跑一条真的慢语句"：
// 真实耗时会随机器抖动（1ns 阈值偶尔测到 0ns），把测试变成 flaky。
// 阈值门控的语义由 TestSlowQueryDisabled 覆盖。
func TestSlowQueryLog(t *testing.T) {
	resetSlowQueries()
	defer resetSlowQueries()

	db, closeDB := explainDB(t)
	defer closeDB()

	recordSlowQuery("dave", 42*time.Millisecond, "SELECT * FROM big WHERE x = 1", 7, false)

	cols, rows, _, _, err := db.runSQL("SHOW SLOWQUERIES")
	if err != nil {
		t.Fatalf("SHOW SLOWQUERIES: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("慢查询日志为空")
	}
	idx := map[string]int{}
	for i, c := range cols {
		idx[c] = i
	}
	var hit []interface{}
	for _, r := range rows {
		if s, _ := r[idx["sql_text"]].(string); strings.Contains(s, "big") {
			hit = r
			break
		}
	}
	if hit == nil {
		t.Fatalf("SHOW SLOWQUERIES 里找不到刚记录的语句: %v", rows)
	}
	if ms, ok := hit[idx["query_time_ms"]].(float64); !ok || ms < 41 || ms > 43 {
		t.Fatalf("query_time_ms=%v want ~42", hit[idx["query_time_ms"]])
	}
	if got := hit[idx["user"]]; got != "dave" {
		t.Fatalf("user=%v", got)
	}
	if got := hit[idx["rows_sent"]]; got != int64(7) {
		t.Fatalf("rows_sent=%v", got)
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
