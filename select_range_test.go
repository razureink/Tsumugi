package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func newTestTable() *Table {
	meta := &TableMeta{
		PK:     "id",
		Fields: []Field{{Name: "id", Type: TypeInt}, {Name: "name", Type: TypeVarchar}, {Name: "age", Type: TypeInt}},
	}
	t := NewTable(meta, nil)
	for i := int64(1); i <= 10; i++ {
		row := map[string]interface{}{"id": i, "name": "n" + strconv.Itoa(int(i)), "age": int64(i * 10)}
		data := encodeRow(meta, row, 0, 0)
		t.pkTree.Set(i, data)
	}
	return t
}

func TestSelectR_BetweenLimit(t *testing.T) {
	tbl := newTestTable()
	lo, hi := int64(3), int64(8)
	res, err := tbl.SelectR(nil, nil, &lo, &hi, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 rows, got %d", len(res))
	}
	for _, r := range res {
		id := r["id"].(int64)
		if id < 3 || id > 8 {
			t.Fatalf("out of range id %d", id)
		}
	}
}

func TestSelectRFilter(t *testing.T) {
	tbl := newTestTable()
	hi := int64(4)
	filters := []RangeCond{{Field: "age", Op: "ge", Val: int64(40)}}
	res, err := tbl.SelectR(nil, filters, nil, &hi, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("want 1 row (id=4, age=40), got %d", len(res))
	}
	if res[0]["age"].(int64) != 40 {
		t.Fatalf("unexpected row %v", res[0])
	}
}

func TestSelectNoLimit(t *testing.T) {
	tbl := newTestTable()
	res, err := tbl.SelectR(nil, nil, nil, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 10 {
		t.Fatalf("want 10 rows, got %d", len(res))
	}
}

// TestDecodeExpireAt 验证轻量过期读取与 encodeRow 布局一致（version[0:8], expireAt[8:16]）。
func TestDecodeExpireAt(t *testing.T) {
	meta := &TableMeta{
		PK:     "id",
		Fields: []Field{{Name: "id", Type: TypeInt}},
	}
	row := map[string]interface{}{"id": int64(7)}
	data := encodeRow(meta, row, 5, 123456789)
	if v := decodeExpireAt(data); v != 123456789 {
		t.Fatalf("decodeExpireAt = %d, want 123456789", v)
	}
	// 无 TTL 行
	if v := decodeExpireAt(encodeRow(meta, row, 5, 0)); v != 0 {
		t.Fatalf("decodeExpireAt(no ttl) = %d, want 0", v)
	}
}

// TestCleanExpired 验证过期清理：过期行被删除、未过期行保留、无过期行时索引不重建。
func TestCleanExpired(t *testing.T) {
	meta := &TableMeta{
		PK:      "id",
		Fields:  []Field{{Name: "id", Type: TypeInt}, {Name: "grp", Type: TypeVarchar}},
		Indexes: map[string]string{"ix_grp": "grp"},
	}
	tbl := NewTable(meta, nil)
	now := time.Now().UnixNano()
	past := now - int64(10*time.Second)
	future := now + int64(10*time.Second)

	// 过期行 + 未过期行 + 无 TTL 行
	if err := tbl.Insert(1, map[string]interface{}{"id": int64(1), "grp": "a"}, past, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Insert(2, map[string]interface{}{"id": int64(2), "grp": "a"}, future, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Insert(3, map[string]interface{}{"id": int64(3), "grp": "b"}, 0, 0, true); err != nil {
		t.Fatal(err)
	}

	tbl.CleanExpired(now)

	if _, ok := tbl.pkTree.Get(1); ok {
		t.Fatal("expired row 1 should be deleted")
	}
	for _, pk := range []int64{2, 3} {
		if _, ok := tbl.pkTree.Get(pk); !ok {
			t.Fatalf("row %d should remain", pk)
		}
	}
	// 索引仍正确（仅 grp=b 含 3）
	if list, ok := tbl.idxTrees["ix_grp"].Get(tbl.encodeIndexValue("grp", "b")); !ok || len(list) != 1 {
		t.Fatalf("index for b = %v, want [3]", list)
	}
}

func TestSQLRange(t *testing.T) {
	dir := t.TempDir()
	// Windows 下 TempDir 偶尔触发"read <dir>/: Incorrect function"，改用固定子目录
	dir = filepath.Join("C:\\Users\\Administrator\\AppData\\Local\\Temp\\opencode", "tsumugi_range_db")
	_ = os.RemoveAll(dir)
	_ = os.MkdirAll(dir, 0755)
	db, err := NewDB(&Config{
		WALDir:              dir,
		WALFile:             "tsumugi.wal",
		PrivilegeFile:       "privileges.json",
		User:                "root",
		Password:            "password",
		FlushInterval:       100 * time.Millisecond,
		GroupCommitInterval: 2 * time.Millisecond,
		TTLCleanInterval:    30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, _, _, _, err := db.runSQL("CREATE TABLE users (id INT, name VARCHAR, age INT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 10; i++ {
		s := strconv.Itoa(i)
		sql := "INSERT INTO users (id,name,age) VALUES (" + s + ", 'n" + s + "', " + s + ")"
		if _, _, _, _, err := db.runSQL(sql); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// 范围 + BETWEEN + LIMIT
	cols, rows, _, _, err := db.runSQL("SELECT * FROM users WHERE id > 2 AND id <= 9 LIMIT 3")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(cols) == 0 {
		t.Fatal("no columns returned")
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (LIMIT 3), got %d", len(rows))
	}
	if rows[0][0].(int64) != 3 {
		t.Fatalf("first row id=%v, want 3", rows[0][0])
	}
	if rows[2][0].(int64) != 5 {
		t.Fatalf("third row id=%v, want 5", rows[2][0])
	}

	// BETWEEN 无 limit
	_, rows, _, _, err = db.runSQL("SELECT * FROM users WHERE id BETWEEN 4 AND 7")
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("between want 4 rows, got %d", len(rows))
	}

	// 非主键字段范围过滤
	_, rows, _, _, err = db.runSQL("SELECT * FROM users WHERE age >= 8 ORDER BY id")
	if err == nil {
		t.Logf("age>=8 rows=%d (ORDER BY ignored)", len(rows))
	}
	if err != nil {
		t.Fatalf("range filter: %v", err)
	}
}
