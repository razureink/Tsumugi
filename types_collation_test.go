package main

// ==================== A5 新类型 + A6 字符集折叠 测试 ====================
// 覆盖要求：
//   - DATE / DATETIME / DECIMAL / TEXT / BLOB 的插入、查询、索引、范围比较；
//   - VARCHAR 大小写不敏感等值 + 索引命中（折叠键）；
//   - ORDER BY 与比较语义一致；
//   - 边界：DECIMAL 溢出/非法日期写入必须报错而不是静默回绕。

import (
	"strings"
	"testing"
	"time"
)

func utcNanos(y int, mo time.Month, d, h, mi, s int) int64 {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC).UnixNano()
}

func idsOf(rows [][]interface{}) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		if n, ok := r[0].(int64); ok {
			out = append(out, n)
		}
	}
	return out
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNewColumnTypesRoundTrip 每种新类型的插入/查询/索引/范围比较。
func TestNewColumnTypesRoundTrip(t *testing.T) {
	db, closeDB := openIndexTestDB(t)
	defer closeDB()

	mustExec(t, db, "CREATE TABLE t ("+
		"id INT PRIMARY KEY, "+
		"d DATE, "+
		"dt DATETIME, "+
		"amount DECIMAL(12,2), "+
		"body TEXT, "+
		"raw BLOB, "+
		"KEY idx_d (d), KEY idx_amount (amount), KEY idx_body (body))")

	big := strings.Repeat("A", 70000) // > VARCHAR 上限 65534，验证 4 字节长度前缀
	mustExec(t, db, "INSERT INTO t (id,d,dt,amount,body,raw) VALUES "+
		"(1,'2024-01-02','2024-01-02 03:04:05',12.34,'Hello TEXT','blob-1'),"+
		"(2,'2024-02-03','2024-02-03 04:05:06',-7.50,'hello text','blob-2'),"+
		"(3,'2023-12-31','2023-12-31 23:59:59',100,'"+big+"','blob-3'),"+
		"(4,'2024-01-02 15:30:00','2024-01-02 00:00:00',0.00,'plain','blob-4')")

	// 1) 存储编码：DATE 截断到 UTC 零点，DATETIME 保留时刻，DECIMAL 定点放大 100 倍
	rows := mustExec(t, db, "SELECT id,d,dt,amount FROM t WHERE id = 1")
	if len(rows) != 1 {
		t.Fatalf("point row = %v", rows)
	}
	if got, want := rows[0][1], utcNanos(2024, 1, 2, 0, 0, 0); got != want {
		t.Errorf("DATE store = %v want %v", got, want)
	}
	if got, want := rows[0][2], utcNanos(2024, 1, 2, 3, 4, 5); got != want {
		t.Errorf("DATETIME store = %v want %v", got, want)
	}
	if got, want := rows[0][3], int64(1234); got != want {
		t.Errorf("DECIMAL(12,2) 12.34 store = %v want %v", got, want)
	}
	// DATE 写入带时刻的文本必须截断到当天
	rows = mustExec(t, db, "SELECT d FROM t WHERE id = 4")
	if got, want := rows[0][0], utcNanos(2024, 1, 2, 0, 0, 0); got != want {
		t.Errorf("DATE truncation = %v want %v", got, want)
	}

	// 2) 范围比较（走 idx_d）
	rows = mustExec(t, db, "SELECT id FROM t WHERE d >= '2024-01-01' AND d < '2024-02-01'")
	if got, want := idsOf(rows), []int64{1, 4}; !equalIDs(got, want) {
		t.Errorf("DATE range ids = %v want %v", got, want)
	}
	// OR 路径也做同样的类型折算（该路径用 Go 值比较，不走 scanConds）
	rows = mustExec(t, db, "SELECT id FROM t WHERE d = '2024-02-03' OR id = 1")
	if got, want := idsOf(rows), []int64{1, 2}; !equalIDs(got, want) {
		t.Errorf("DATE OR ids = %v want %v", got, want)
	}

	// 3) DECIMAL 等值/范围 + 负数（验证符号位翻转的索引键保序）
	rows = mustExec(t, db, "SELECT id FROM t WHERE amount = 12.34")
	if got, want := idsOf(rows), []int64{1}; !equalIDs(got, want) {
		t.Errorf("DECIMAL eq ids = %v want %v", got, want)
	}
	rows = mustExec(t, db, "SELECT id FROM t WHERE amount = -7.50")
	if got, want := idsOf(rows), []int64{2}; !equalIDs(got, want) {
		t.Errorf("DECIMAL negative eq ids = %v want %v", got, want)
	}
	rows = mustExec(t, db, "SELECT id FROM t WHERE amount BETWEEN -10 AND 20")
	if got, want := idsOf(rows), []int64{1, 2, 4}; !equalIDs(got, want) {
		t.Errorf("DECIMAL range ids = %v want %v", got, want)
	}

	// 4) TEXT 大小写不敏感等值（走 idx_body 折叠键），且结果仍是原始大小写
	rows = mustExec(t, db, "SELECT id FROM t WHERE body = 'HELLO TEXT'")
	if got, want := idsOf(rows), []int64{1, 2}; !equalIDs(got, want) {
		t.Errorf("TEXT CI index eq ids = %v want %v", got, want)
	}
	rows = mustExec(t, db, "SELECT body FROM t WHERE body = 'HELLO TEXT' ORDER BY id")
	if len(rows) != 2 || rows[0][0] != "Hello TEXT" || rows[1][0] != "hello text" {
		t.Errorf("TEXT projection = %v (must keep original case)", rows)
	}

	// 5) 超过 VARCHAR 上限的长文本能完整存回
	rows = mustExec(t, db, "SELECT body FROM t WHERE id = 3")
	if s, ok := rows[0][0].(string); !ok || len(s) != len(big) || s != big {
		t.Errorf("TEXT long value length = %d, want %d", len(rows[0][0].(string)), len(big))
	}

	// 6) BLOB 按字节原样存取
	rows = mustExec(t, db, "SELECT raw FROM t WHERE raw = 'blob-3'")
	if len(rows) != 1 || rows[0][0] != "blob-3" {
		t.Errorf("BLOB eq = %v", rows)
	}

	// 7) UPDATE 的表达式路径同样要做十进制折算
	if _, _, affected, _, err := db.runSQL("UPDATE t SET raw = 'blob-2b' WHERE amount = -7.50"); err != nil || affected != 1 {
		t.Errorf("UPDATE decimal affected=%d err=%v, want 1 row", affected, err)
	}

	// 8) 索引键直接校验：折叠 + 定点编码确实写进了索引树
	tbl := db.getTable("t")
	if tbl == nil {
		t.Fatal("table missing")
	}
	trees := tbl.indexes()
	if tree, ok := trees["idx_body"]; ok {
		pks, found := tree.Get("hello text")
		if !found || len(pks) != 2 {
			t.Errorf("idx_body key 'hello text' = %v/%v, want pks 1,2 (folded key)", pks, found)
		}
	} else {
		t.Error("idx_body missing")
	}
	if tree, ok := trees["idx_amount"]; ok {
		// DECIMAL(12,2) 的 12.34 → 1234，索引键是符号位翻转的大端 int64
		pks, found := tree.Get(tbl.encodeIndexValue("amount", float64(12.34)))
		if !found || len(pks) != 1 || pks[0] != 1 {
			t.Errorf("idx_amount 12.34 = %v/%v, want [1]", pks, found)
		}
	} else {
		t.Error("idx_amount missing")
	}
}

// TestNewColumnTypesRejectBadValues 溢出/非法值必须报错（不静默写 0）。
func TestNewColumnTypesRejectBadValues(t *testing.T) {
	db, closeDB := openIndexTestDB(t)
	defer closeDB()
	mustExec(t, db, "CREATE TABLE b (id INT PRIMARY KEY, amount DECIMAL(12,2), d DATE)")

	bad := []string{
		"INSERT INTO b VALUES (1, '999999999999999999999', NULL)", // 超出 int64 定点上限
		"INSERT INTO b VALUES (2, '12.34', 'not-a-date')",         // 非法日期文本
	}
	for _, q := range bad {
		if _, _, _, _, err := db.runSQL(q); err == nil {
			t.Errorf("%s: expected error", q)
		}
	}
	if _, _, _, _, err := db.runSQL("INSERT INTO b VALUES (3, '12.34', '2024-01-02')"); err != nil {
		t.Fatalf("valid insert failed: %v", err)
	}
}

// TestVarcharCaseInsensitiveCollation 大小写不敏感等值/范围/ORDER BY + 索引命中。
func TestVarcharCaseInsensitiveCollation(t *testing.T) {
	db, closeDB := openIndexTestDB(t)
	defer closeDB()
	mustExec(t, db, "CREATE TABLE ci (id INT PRIMARY KEY, name VARCHAR, KEY idx_name (name))")
	mustExec(t, db, "INSERT INTO ci VALUES (1,'Alice'),(2,'bob'),(3,'ALICE'),(4,'carol')")

	// 等值：大小写不敏感
	for _, lit := range []string{"alice", "ALICE", "Alice"} {
		rows := mustExec(t, db, "SELECT id FROM ci WHERE name = '"+lit+"'")
		if got, want := idsOf(rows), []int64{1, 3}; !equalIDs(got, want) {
			t.Errorf("WHERE name = %q ids = %v want %v", lit, got, want)
		}
	}

	// 走索引的覆盖路径：只投影主键，覆盖索引仍可用（折叠键不泄漏到输出）
	rows := mustExec(t, db, "SELECT id FROM ci WHERE name = 'alice'")
	if got, want := idsOf(rows), []int64{1, 3}; !equalIDs(got, want) {
		t.Errorf("covering id ids = %v want %v", got, want)
	}
	// 投影含字符串列：必须回表，返回原始大小写
	rows = mustExec(t, db, "SELECT name FROM ci WHERE name = 'alice' ORDER BY id")
	if len(rows) != 2 || rows[0][0] != "Alice" || rows[1][0] != "ALICE" {
		t.Errorf("projected names = %v, want original case", rows)
	}

	// 范围：与折叠比较一致（carol 的折叠形 'carol' >= 'c'，不在区间内）
	rows = mustExec(t, db, "SELECT id FROM ci WHERE name >= 'A' AND name < 'c'")
	if got, want := idsOf(rows), []int64{1, 2, 3}; !equalIDs(got, want) {
		t.Errorf("range ids = %v want %v", got, want)
	}

	// ORDER BY 与比较语义一致：alice(alice) < bob < carol
	rows = mustExec(t, db, "SELECT id FROM ci ORDER BY name ASC")
	if got, want := idsOf(rows), []int64{1, 3, 2, 4}; !equalIDs(got, want) {
		t.Errorf("ORDER BY name ASC ids = %v want %v", got, want)
	}
	rows = mustExec(t, db, "SELECT id FROM ci ORDER BY name DESC")
	if got, want := idsOf(rows), []int64{4, 2, 1, 3}; !equalIDs(got, want) {
		t.Errorf("ORDER BY name DESC ids = %v want %v", got, want)
	}

	// 索引树里的键必须是折叠形：只存小写规范形，大小写变体本身不应存在
	tbl := db.getTable("ci")
	tree := tbl.indexes()["idx_name"]
	if tree == nil {
		t.Fatal("idx_name missing")
	}
	pks, found := tree.Get("alice")
	if !found || len(pks) != 2 || pks[0] != 1 || pks[1] != 3 {
		t.Errorf("idx_name folded key 'alice' = %v/%v, want pks [1 3]", pks, found)
	}
	for _, raw := range []string{"ALICE", "Alice"} {
		if _, found := tree.Get(raw); found {
			t.Errorf("idx_name contains unfolded key %q; folding not applied", raw)
		}
	}
}
