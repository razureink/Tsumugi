package main

import "testing"

// 聚合在无匹配行时必须返回一行（COUNT=0），而不是 0 行。
// 这是 PyMySQL 真实客户端测试暴露出来的：`SELECT COUNT(*) ... WHERE id=5`
// 在行不存在时 fetchone() 返回 None，导致调用方按 SQL 语义写下标越界。
func TestCountNoMatchReturnsZero(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	mustRun(t, db, "CREATE TABLE cnt_t (id INT PRIMARY KEY, v INT)")
	mustRun(t, db, "INSERT INTO cnt_t VALUES (1, 10)")

	cases := []string{
		"SELECT COUNT(*) FROM cnt_t WHERE id = 999",
		"SELECT COUNT(*) FROM cnt_t WHERE id > 100",
		"SELECT COUNT(*) FROM cnt_t WHERE v = 999",
		"SELECT COUNT(*) FROM cnt_t",
	}
	for _, q := range cases {
		rows := rowsOf(t, db, q)
		if len(rows) != 1 {
			t.Fatalf("%q 返回 %d 行 want 1: %#v", q, len(rows), rows)
		}
		if rows[0][0] != int64(1) && rows[0][0] != int64(0) {
			t.Fatalf("%q 结果=%v", q, rows[0][0])
		}
	}
}
