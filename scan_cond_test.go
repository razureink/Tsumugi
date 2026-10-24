package main

import (
	"testing"
)

// TestScanCondsMatchesMapSemantics 差分验证：行字节上的类型化条件求值
// （scanConds.matchRow，SelectR 扫描/点查/索引路径使用）与 map 求值
// （matchConditions + matchRange，countSelect 等低频路径在用）在全值矩阵上结果一致。
// 字段名仅用精确大小写与"不存在的字段"——大小写不敏感解析是本次的有意变更，
// 单独在 TestSelectCaseInsensitiveWhere 中验证。
func TestScanCondsMatchesMapSemantics(t *testing.T) {
	meta := &TableMeta{
		Name: "t", PK: "id",
		Fields: []Field{
			{Name: "id", Type: TypeInt},
			{Name: "name", Type: TypeVarchar},
			{Name: "ok", Type: TypeBool},
		},
	}
	rows := []map[string]interface{}{
		{"id": int64(1), "name": "alpha", "ok": true},
		{"id": int64(300), "name": "beta", "ok": false},
		{"id": int64(-5), "name": "", "ok": true},
		{"id": int64(1 << 40), "name": "gamma", "ok": false},
	}
	encoded := make([][]byte, len(rows))
	decoded := make([]map[string]interface{}, len(rows))
	for i, r := range rows {
		encoded[i] = encodeRow(meta, r, 0, 0)
		d, _, _ := decodeRow(meta, encoded[i])
		decoded[i] = d
	}
	// 旧 schema 短行（ALTER ADD COLUMN 前写入）：只有 id/name 两个编码字段。
	// decodeRow 对缺失尾部字段零值填充（ok=false），matchRow 必须同样按零值
	// 求值——此前截断直接判 false，旧行会被条件查询静默排除。
	shortMeta := &TableMeta{Name: "t", PK: "id", Fields: meta.Fields[:2]}
	shortEncoded := encodeRow(shortMeta, rows[0], 0, 0)
	shortDecoded, _, _ := decodeRow(meta, shortEncoded)
	encoded = append(encoded, shortEncoded)
	decoded = append(decoded, shortDecoded)

	vals := []interface{}{
		nil, int64(1), int64(-5), int64(1 << 40), int(1), int(300), 1.0, 2.5, "", "alpha", "zzz", true, false, []byte("x"),
	}
	ops := []string{"gt", "ge", "lt", "le", "ne", "between"}

	for _, field := range []string{"id", "name", "ok", "nosuch"} {
		for vi, v := range vals {
			// 等值条件差分
			conds := map[string]interface{}{field: v}
			sc := newScanConds(meta, conds, nil)
			for i := range encoded {
				want := matchConditions(decoded[i], conds)
				if got := sc.matchRow(meta, encoded[i]); got != want {
					t.Errorf("eq mismatch: field=%s val=%#v row=%d got=%v want=%v", field, v, i, got, want)
				}
			}
			// 范围条件差分（between 用第二个值）
			v2 := vals[(vi+3)%len(vals)]
			for _, op := range ops {
				filters := []RangeCond{{Field: field, Op: op, Val: v, Val2: v2}}
				sc2 := newScanConds(meta, nil, filters)
				for i := range encoded {
					want := matchRange(decoded[i], filters)
					if got := sc2.matchRow(meta, encoded[i]); got != want {
						t.Errorf("range mismatch: field=%s op=%s val=%#v,%#v row=%d got=%v want=%v",
							field, op, v, v2, i, got, want)
					}
				}
			}
		}
	}
}

// TestSelectCaseInsensitiveWhere WHERE 中列名大小写与定义不一致时也应命中
// （此前条件 map 按原文匹配，WHERE ID = 1 会静默返回空结果）。
func TestSelectCaseInsensitiveWhere(t *testing.T) {
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		"CREATE TABLE ci (id INT, name STRING, PRIMARY KEY (id))",
		"INSERT INTO ci VALUES (1, 'one')",
		"INSERT INTO ci VALUES (2, 'two')",
	}
	for _, s := range stmts {
		if _, _, _, _, err := db.runSQL(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	cases := []struct {
		sql      string
		wantRows int
	}{
		{"SELECT * FROM ci WHERE id = 1", 1},
		{"SELECT * FROM ci WHERE ID = 1", 1},
		{"SELECT * FROM ci WHERE Name = 'two'", 1},
		{"SELECT * FROM ci WHERE NAME like 't%'", 1},
		{"SELECT * FROM ci WHERE id > 0", 2},
		{"SELECT * FROM ci WHERE id > 0 AND NAME = 'one'", 1},
	}
	for _, c := range cases {
		_, rows, _, _, err := db.runSQL(c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if len(rows) != c.wantRows {
			t.Errorf("%s: got %d rows, want %d", c.sql, len(rows), c.wantRows)
		}
	}
}

// TestSelectStarRowWidth 行宽回归：所有扫描路径（主键点查/条件/OR/无条件）
// 返回的每行值个数必须与列头一致且值正确。rowArena stride 曾误用 len(colIdx)，
// SELECT * 时为 0 被钳成 1 列，条件/OR 路径的命中行被截断成只剩首列
// （行数仍正确，只数行数的测试发现不了）。
func TestSelectStarRowWidth(t *testing.T) {
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		"CREATE TABLE rw (id INT, name STRING, ok BOOL, PRIMARY KEY (id))",
		"INSERT INTO rw VALUES (1, 'one', true)",
		"INSERT INTO rw VALUES (2, 'two', false)",
	}
	for _, s := range stmts {
		if _, _, _, _, err := db.runSQL(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	cases := []struct {
		sql  string
		want [][]interface{}
	}{
		// 条件路径（非主键等值）
		{"SELECT * FROM rw WHERE name = 'two'", [][]interface{}{{int64(2), "two", false}}},
		{"SELECT * FROM rw WHERE ok = false", [][]interface{}{{int64(2), "two", false}}},
		// 条件路径（非主键范围 → filters）
		{"SELECT * FROM rw WHERE name >= 'a'", [][]interface{}{{int64(1), "one", true}, {int64(2), "two", false}}},
		// OR 路径
		{"SELECT * FROM rw WHERE id=1 OR id=2", [][]interface{}{{int64(1), "one", true}, {int64(2), "two", false}}},
		// 主键点查路径
		{"SELECT * FROM rw WHERE id = 1", [][]interface{}{{int64(1), "one", true}}},
		// 命名列（含乱序投影）
		{"SELECT name, id FROM rw WHERE id = 2", [][]interface{}{{"two", int64(2)}}},
		{"SELECT name FROM rw WHERE ok = false", [][]interface{}{{"two"}}},
	}
	for _, c := range cases {
		columns, rows, _, _, err := db.runSQL(c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if len(rows) != len(c.want) {
			t.Errorf("%s: got %d rows, want %d", c.sql, len(rows), len(c.want))
			continue
		}
		for i, row := range rows {
			if len(row) != len(columns) {
				t.Errorf("%s: row %d has %d values, header has %d cols: %v", c.sql, i, len(row), len(columns), row)
				continue
			}
			for j := range row {
				if row[j] != c.want[i][j] {
					t.Errorf("%s: row %d col %s = %#v, want %#v", c.sql, i, columns[j], row[j], c.want[i][j])
				}
			}
		}
	}
}

// TestAsciiEqNoPunctuationFold 标点不得折叠进字母：'{' 折叠后等于 '['、
// '|' 等于 '\\'、'~' 等于 '^' 曾因折叠范围无上界而返回真。
func TestAsciiEqNoPunctuationFold(t *testing.T) {
	for _, pair := range [][2]string{{"{", "["}, {"|", "\\"}, {"~", "^"}, {"a{c", "a[c"}} {
		if asciiEq(pair[0], pair[1]) {
			t.Errorf("asciiEq(%q, %q) = true, punctuation must not fold", pair[0], pair[1])
		}
	}
	if !asciiEq("select", "SELECT") || !asciiEq("SeLeCt", "select") || asciiEq("selectx", "select") {
		t.Error("asciiEq letter folding broken")
	}
}
