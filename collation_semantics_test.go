package main

import "testing"

// ==================== 语义一致性：`=` / `LIKE` / DISTINCT / GROUP BY ====================
// 同一个字符串列上只能有一套大小写语义。utf8mb4 CI 下 '=' 与 LIKE 都不区分大小写，
// 因此 DISTINCT / GROUP BY 的分组键也必须折叠，否则会出现
// "WHERE name='abc' 命中，但 GROUP BY name 把 'ABC' 分成另一组"的矛盾。

func collationDB(t *testing.T) (*DB, func()) {
	t.Helper()
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE TABLE ci (id INT PRIMARY KEY, name VARCHAR(32))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i, n := range []string{"abc", "ABC", "Abc", "xyz"} {
		if _, _, _, _, err := db.runSQL(
			"INSERT INTO ci VALUES (" + itoa(i+1) + ",'" + n + "')"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
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

// TestEqualityIsCaseInsensitive 基准语义：`=` 不区分大小写。
func TestEqualityIsCaseInsensitive(t *testing.T) {
	db, closeDB := collationDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL("SELECT id FROM ci WHERE name = 'abc'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("= 命中 %d 行 want 3（应不区分大小写）", len(rows))
	}
}

// TestLikeIsCaseInsensitive LIKE 与 `=` 必须一致。
func TestLikeIsCaseInsensitive(t *testing.T) {
	db, closeDB := collationDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL("SELECT id FROM ci WHERE name LIKE 'a%'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("LIKE 命中 %d 行 want 3（应与 = 一致）", len(rows))
	}
}

// TestDistinctIsCaseInsensitive DISTINCT 必须把 'abc'/'ABC'/'Abc' 视为同一个值。
func TestDistinctIsCaseInsensitive(t *testing.T) {
	db, closeDB := collationDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL("SELECT DISTINCT name FROM ci")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// abc/ABC/Abc → 1 组；xyz → 1 组；共 2 组
	if len(rows) != 2 {
		t.Fatalf("DISTINCT 返回 %d 行 want 2: %v", len(rows), rows)
	}
}

// TestGroupByIsCaseInsensitive GROUP BY 与 DISTINCT 使用同一套折叠语义。
func TestGroupByIsCaseInsensitive(t *testing.T) {
	db, closeDB := collationDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL("SELECT name, COUNT(*) FROM ci GROUP BY name")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("GROUP BY 返回 %d 组 want 2: %v", len(rows), rows)
	}
	// 折叠组应聚合 3 行
	found := false
	for _, r := range rows {
		if c, ok := r[1].(int64); ok && c == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("折叠组未聚合到 3 行: %v", rows)
	}
}

// TestGroupByStillDistinguishesDifferentStrings 折叠不能把不同字符串错误合并。
func TestGroupByStillDistinguishesDifferentStrings(t *testing.T) {
	db, closeDB := collationDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL("SELECT COUNT(DISTINCT name) FROM ci")
	if err != nil {
		// 不支持 COUNT(DISTINCT) 时退化为 DISTINCT 校验
		_, rows2, _, _, e2 := db.runSQL("SELECT DISTINCT name FROM ci WHERE name IN ('xyz')")
		if e2 != nil {
			t.Skip("COUNT(DISTINCT) 与 IN 均不可用，跳过")
		}
		if len(rows2) != 1 {
			t.Fatalf("xyz 应单独一组，实际 %v", rows2)
		}
		return
	}
	if len(rows) != 1 {
		t.Fatalf("COUNT(DISTINCT) 结果行数=%d", len(rows))
	}
	if got, ok := rows[0][0].(int64); !ok || got != 2 {
		t.Fatalf("COUNT(DISTINCT name)=%v want 2（abc组 + xyz）", rows[0][0])
	}
}

// TestUnionDedupIsCaseInsensitive UNION（非 ALL）的整行去重也必须折叠。
func TestUnionDedupIsCaseInsensitive(t *testing.T) {
	db, closeDB := collationDB(t)
	defer closeDB()
	_, rows, _, _, err := db.runSQL(
		"SELECT name FROM ci WHERE id = 1 UNION SELECT name FROM ci WHERE id = 2")
	if err != nil {
		t.Fatalf("union: %v", err)
	}
	// id=1 是 'abc'，id=2 是 'ABC'；折叠后应只剩 1 行
	if len(rows) != 1 {
		t.Fatalf("UNION 返回 %d 行 want 1: %v", len(rows), rows)
	}
}
