package main

// 针对 review 中确认缺陷的回归测试：每条对应一个已修复的问题，
// 防止后续重构把它们带回来。

import (
	"strings"
	"testing"
)

func newFixTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// ---- 授权：ACL 不能再用字符串启发式绕过 ----

func TestACLLeadingSemicolonBypass(t *testing.T) {
	u := &User{Username: "bob", Databases: []string{"tsumugi"}, Tables: []string{"t"}}
	for _, sql := range []string{";DROP TABLE t", " ; DELETE FROM t WHERE id=1", ";ALTER TABLE t ADD COLUMN x INT"} {
		if ok, _ := u.canExecSQL(sql); ok {
			t.Fatalf("write statement must be denied for non-admin: %q", sql)
		}
	}
	if ok, _ := u.canExecSQL("SELECT * FROM t"); !ok {
		t.Fatal("read statement should be allowed")
	}
}

func TestACLStringLiteralTableName(t *testing.T) {
	u := &User{Username: "bob", Databases: []string{"tsumugi"}, Tables: []string{"allowed"}}
	if u.canExecTableSQL("SELECT * FROM 'secret'") {
		t.Fatal("quoted table name must not bypass table-level ACL")
	}
	if !u.canExecTableSQL("SELECT * FROM allowed") {
		t.Fatal("granted table should be readable")
	}
}

func TestAnalyzeSQL(t *testing.T) {
	cases := []struct {
		sql    string
		kind   sqlStmtKind
		tables []string
	}{
		{";DROP TABLE t", sqlDDL, []string{"t"}},
		{"/*x*/USE otherdb", sqlUse, nil},
		{"SELECT * FROM a, b WHERE a.id = 1", sqlRead, []string{"a", "b"}},
		{"SELECT * FROM 'q'", sqlRead, []string{"q"}},
		{"GRANT SELECT ON db1.t1 TO bob", sqlGrant, []string{"db1.t1"}},
		{"INSERT INTO t (a) VALUES (1)", sqlInsert, []string{"t"}},
		{"CREATE INDEX idx ON t (a)", sqlDDL, []string{"t"}},
	}
	for _, c := range cases {
		a, err := analyzeSQL(c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if a.Kind != c.kind {
			t.Fatalf("%q kind = %v, want %v", c.sql, a.Kind, c.kind)
		}
		if len(c.tables) == 0 {
			continue
		}
		if len(a.Tables) != len(c.tables) {
			t.Fatalf("%q tables = %v, want %v", c.sql, a.Tables, c.tables)
		}
		for i := range c.tables {
			if a.Tables[i] != c.tables[i] {
				t.Fatalf("%q tables = %v, want %v", c.sql, a.Tables, c.tables)
			}
		}
	}
}

func TestAuthorizeSQLSystemTableAdminOnly(t *testing.T) {
	db := newFixTestDB(t)
	if err := db.authorizeSQL("bob", sqlAccess{Kind: sqlRead, Tables: []string{"__tables"}}); err == nil {
		t.Fatal("system table must be admin-only")
	}
	if err := db.authorizeSQL(db.rootUser(), sqlAccess{Kind: sqlRead, Tables: []string{"__tables"}}); err != nil {
		t.Fatalf("root must read system tables: %v", err)
	}
}

// ---- 事务：写自己的行不再必然冲突；并发写能检出冲突 ----

func TestTxnUpdateCommitSucceeds(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	tbl := db.getTable("t")
	if err := tbl.Insert(1, map[string]interface{}{"id": int64(1), "v": int64(10)}, 0, 0, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	txn := db.BeginTxn()
	if _, _, _, _, err := db.runSQLTx("UPDATE t SET v = 20 WHERE id = 1", nil, txn); err != nil {
		t.Fatalf("txn update: %v", err)
	}
	if err := db.CommitTxn(txn); err != nil {
		t.Fatalf("commit after txn update must succeed, got %v", err)
	}
	got, ok := tbl.SelectByPK(1)
	if !ok || got["v"] != int64(20) {
		t.Fatalf("v = %v, want 20", got["v"])
	}
}

func TestTxnRollbackRestoresUpdate(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	tbl := db.getTable("t")
	if err := tbl.Insert(1, map[string]interface{}{"id": int64(1), "v": int64(10)}, 0, 0, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	txn := db.BeginTxn()
	if _, _, _, _, err := db.runSQLTx("UPDATE t SET v = 99 WHERE id = 1", nil, txn); err != nil {
		t.Fatalf("txn update: %v", err)
	}
	if err := db.RollbackTxn(txn); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, ok := tbl.SelectByPK(1)
	if !ok || got["v"] != int64(10) {
		t.Fatalf("after rollback v = %v, want 10", got["v"])
	}
}

func TestTxnConflictDetected(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	tbl := db.getTable("t")
	if err := tbl.Insert(1, map[string]interface{}{"id": int64(1), "v": int64(0)}, 0, 0, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t1 := db.BeginTxn()
	t2 := db.BeginTxn()
	if _, _, _, _, err := db.runSQLTx("UPDATE t SET v = 1 WHERE id = 1", nil, t1); err != nil {
		t.Fatalf("t1 update: %v", err)
	}
	if _, _, _, _, err := db.runSQLTx("UPDATE t SET v = 2 WHERE id = 1", nil, t2); err != nil {
		t.Fatalf("t2 update: %v", err)
	}
	if err := db.CommitTxn(t1); err == nil {
		t.Fatal("commit of overwritten txn must report conflict")
	}
	if err := db.CommitTxn(t2); err != nil {
		t.Fatalf("last writer should commit: %v", err)
	}
	got, _ := tbl.SelectByPK(1)
	if got["v"] != int64(2) {
		t.Fatalf("v = %v, want 2", got["v"])
	}
}

// ---- 查询语义 ----

func TestCountCaseInsensitiveAndOR(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE c (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, _, _, _, err := db.runSQL("INSERT INTO c VALUES (" + itoa(i) + ", " + itoa(i*10) + ")"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	cols, rows, _, _, err := db.runSQL("SELECT COUNT(*) FROM c WHERE ID = 1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(cols) != 1 || len(rows) != 1 || rows[0][0] != int64(1) {
		t.Fatalf("count result = %v, want 1", rows)
	}
	if _, _, _, _, err := db.runSQL("SELECT COUNT(*) FROM c WHERE id = 1 OR id = 2"); err != nil {
		t.Fatalf("COUNT with OR should be evaluated correctly: %v", err)
	}
	if _, rows, _, _, _ := db.runSQL("SELECT COUNT(*) FROM c WHERE id = 1 OR id = 2"); len(rows) != 1 || rows[0][0] != int64(2) {
		t.Fatalf("COUNT with OR = %v, want 2", rows)
	}
	if _, _, _, _, err := db.runSQL("SELECT COUNT(*) FROM c WHERE nope = 1"); err == nil {
		t.Fatal("COUNT with unknown column must be rejected")
	}
}

func TestPKRangePushdownTypeSafety(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE r (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 60; i++ {
		if _, _, _, _, err := db.runSQL("INSERT INTO r VALUES (" + itoa(i) + ")"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM r WHERE id > '50'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("id > '50' returned %d rows, want 10", len(rows))
	}
	_, rows, _, _, err = db.runSQL("SELECT id FROM r WHERE id > NULL")
	if err != nil {
		t.Fatalf("null compare: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("id > NULL returned %d rows, want 0", len(rows))
	}
	// <> 主键不再直接报错
	_, rows, _, _, err = db.runSQL("SELECT id FROM r WHERE id <> 1")
	if err != nil {
		t.Fatalf("<> on pk: %v", err)
	}
	if len(rows) != 59 {
		t.Fatalf("id <> 1 returned %d rows, want 59", len(rows))
	}
}

func TestNullComparisonReturnsEmpty(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE nc (id INT PRIMARY KEY, name VARCHAR)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, _, _, _, err := db.runSQL("INSERT INTO nc VALUES (" + itoa(i) + ", 'n" + itoa(i) + "')"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// 与 NULL 比较恒为 UNKNOWN：任何字段都不该退回"全表"
	for _, sql := range []string{
		"SELECT id FROM nc WHERE name = NULL",
		"SELECT id FROM nc WHERE name > NULL",
		"SELECT id FROM nc WHERE id > NULL",
		"SELECT id FROM nc WHERE name = NULL AND id > 0",
		"SELECT COUNT(*) FROM nc WHERE name = NULL",
	} {
		_, rows, _, _, err := db.runSQL(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		n := int64(0)
		if len(rows) == 1 {
			if c, ok := rows[0][0].(int64); ok && len(rows[0]) == 1 {
				n = c
			} else {
				n = int64(len(rows))
			}
		} else {
			n = int64(len(rows))
		}
		if n != 0 {
			t.Fatalf("%s returned %d rows, want 0", sql, n)
		}
	}
	// 真正判断 NULL 的谓词仍然可用
	if _, _, _, _, err := db.runSQL("INSERT INTO nc VALUES (4, NULL)"); err != nil {
		t.Fatalf("insert null: %v", err)
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM nc WHERE name IS NULL")
	if err != nil {
		t.Fatalf("is null: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("IS NULL returned %d rows, want 1", len(rows))
	}
}

func TestOrderByLargeInt(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE big (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	ids := []int64{1<<53 + 3, 1<<53 + 1, 1<<53 + 2}
	for _, id := range ids {
		if _, _, _, _, err := db.runSQL("INSERT INTO big VALUES (" + numToStr(id) + ")"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM big ORDER BY id")
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	for i, want := range []int64{1<<53 + 1, 1<<53 + 2, 1<<53 + 3} {
		if rows[i][0] != want {
			t.Fatalf("ORDER BY row %d = %v, want %d", i, rows[i][0], want)
		}
	}
}

// ---- 存储层 ----

func TestVarcharNullRoundTrip(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE n (id INT PRIMARY KEY, note VARCHAR)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, _, err := db.runSQL("INSERT INTO n VALUES (1, NULL)"); err != nil {
		t.Fatalf("insert null: %v", err)
	}
	if _, _, _, _, err := db.runSQL("INSERT INTO n VALUES (2, '')"); err != nil {
		t.Fatalf("insert empty: %v", err)
	}
	tbl := db.getTable("n")
	r1, _ := tbl.SelectByPK(1)
	if r1["note"] != nil {
		t.Fatalf("NULL should round-trip as nil, got %#v", r1["note"])
	}
	r2, _ := tbl.SelectByPK(2)
	if r2["note"] != "" {
		t.Fatalf("empty string should stay \"\", got %#v", r2["note"])
	}
}

func TestOversizedVarcharRejected(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE s (id INT PRIMARY KEY, v VARCHAR)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	big := strings.Repeat("x", maxVarcharLen+10)
	if _, _, _, _, err := db.runSQL("INSERT INTO s VALUES (1, '" + big + "')"); err == nil {
		t.Fatal("oversized varchar must be rejected instead of silently truncated")
	}
}

func TestTakeVarBytesRejectsHugeLength(t *testing.T) {
	// varint 表示 2^63 左右：int(n) 溢出成负数会让旧实现的切片越界 panic
	buf := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}
	pos := 0
	if _, ok := takeVarBytes(buf, &pos); ok {
		t.Fatal("huge length must be rejected")
	}
	if _, err := parseMetaV2([]byte{2, 'i', 'd', 0xff, 0xff, 0xff, 0x7f}, new(int), "t"); err == nil {
		t.Fatal("absurd field count must be rejected")
	}
}

func TestAdminRowsPaginationNoDuplicate(t *testing.T) {
	db := newFixTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE p (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if _, _, _, _, err := db.runSQL("INSERT INTO p VALUES (" + itoa(i) + ")"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	page1, err := db.adminRows("p", -1, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	nextPK := page1["next_pk"].(int64)
	page2, err := db.adminRows("p", nextPK, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	pks := page2["pks"].([]int64)
	if len(pks) == 0 || pks[0] <= nextPK {
		t.Fatalf("page2 first pk = %v, must be > %v (no duplicate row)", pks, nextPK)
	}
}

func numToStr(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
