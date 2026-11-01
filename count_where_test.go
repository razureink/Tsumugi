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

// TestIntColumnComparedWithFloat MySQL 允许 INT 与数值比较：
// `id = 2.0` 命中，`id = 2.5` 不命中，`id = '2'` 命中。
// 二进制协议把 JS 数字编码成 DOUBLE，这里不折算会让 `WHERE id = ?` 返回 0 行。
func TestIntColumnComparedWithFloat(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	mustRun(t, db, "CREATE TABLE num_t (id INT PRIMARY KEY, v INT)")
	mustRun(t, db, "INSERT INTO num_t VALUES (1, 10), (2, 20), (3, 30)")

	for _, q := range []string{
		"SELECT id FROM num_t WHERE id = 2.0",
		"SELECT id FROM num_t WHERE id = '2'",
		"SELECT id FROM num_t WHERE v = 30.0",
		"SELECT id FROM num_t WHERE v = '20'",
	} {
		rows := rowsOf(t, db, q)
		if len(rows) != 1 {
			t.Fatalf("%q 返回 %d 行 want 1: %#v", q, len(rows), rows)
		}
	}
	// 范围比较本来就走 cmpF64，确认 int/float 混比仍然正确
	if rows := rowsOf(t, db, "SELECT COUNT(*) FROM num_t WHERE id > 1.5"); rows[0][0] != int64(2) {
		t.Fatalf("id > 1.5 应命中 2 行: %#v", rows)
	}
	// 非整数浮点不应被截断后误命中
	if rows := rowsOf(t, db, "SELECT id FROM num_t WHERE id = 2.5"); len(rows) != 0 {
		t.Fatalf("id = 2.5 不应命中: %#v", rows)
	}
}

// TestPKFromValue 主键折算：二进制协议的数字参数是 float64，必须接受；
// 非整数浮点必须拒绝，不能被截断成相邻整数。
func TestPKFromValue(t *testing.T) {
	cases := []struct {
		in      interface{}
		want    int64
		auto    bool
		wantErr bool
	}{
		{int64(5), 5, false, false},
		{float64(5), 5, false, false},
		{float64(0), 0, true, false},
		{float64(2.5), 0, false, true},
		{nil, 0, true, false},
		{"7", 7, false, false},
		{"", 0, true, false},
		{"abc", 0, false, true},
		{true, 0, false, true},
	}
	for _, c := range cases {
		got, auto, err := pkFromValue(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("pkFromValue(%#v) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if err != nil {
			continue
		}
		if got != c.want || auto != c.auto {
			t.Fatalf("pkFromValue(%#v)=%d,auto=%v want %d,auto=%v", c.in, got, auto, c.want, c.auto)
		}
	}
}
