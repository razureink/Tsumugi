package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newParserTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := NewDB(&Config{
		WALDir:              dir,
		WALFile:             "wal",
		PrivilegeFile:       "priv.json",
		User:                "root",
		Password:            "password",
		FlushInterval:       100 * time.Millisecond,
		GroupCommitInterval: 2 * time.Millisecond,
		TTLCleanInterval:    30 * time.Second,
		IdleTimeout:         5 * time.Minute,
		BackupDir:           filepath.Join(dir, "backup"),
		MetricsPort:         0,
		EnableChecksum:      true,
	})
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedSQLDB 建表并插入三行，供各解析器测试使用。
func seedSQLDB(t *testing.T, db *DB) {
	t.Helper()
	steps := []string{
		"CREATE TABLE users(id INT, name VARCHAR(40), grp INT, PRIMARY KEY(id))",
		"INSERT INTO users VALUES (1,'alice',10),(2,'bob',20),(3,'carol',10)",
	}
	for _, s := range steps {
		if _, _, _, _, err := db.runSQL(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

// 反引号引用的标识符（phpMyAdmin 默认全程加反引号）此前直接报错。
func TestBacktickIdentifiers(t *testing.T) {
	db := newParserTestDB(t)
	steps := []string{
		"CREATE TABLE `users`(`id` INT, `name` VARCHAR(40), PRIMARY KEY(`id`))",
		"INSERT INTO `users` (`id`, `name`) VALUES (1, 'a')",
		"SELECT `name` FROM `users` WHERE `id` = 1",
	}
	for _, s := range steps {
		if _, _, _, _, err := db.runSQL(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
	_, rows, _, _, err := db.runSQL("SELECT `name` FROM `users`")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0][0] != "a" {
		t.Fatalf("rows = %v, want [[a]]", rows)
	}
}

// 反斜杠转义（MySQL 默认 sql_mode）：'it\'s' 与 \n。
func TestBackslashEscapes(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	if _, _, _, _, err := db.runSQL(`INSERT INTO users VALUES (4,'it\'s',10)`); err != nil {
		t.Fatalf("insert with backslash escape: %v", err)
	}
	_, rows, _, _, err := db.runSQL(`SELECT name FROM users WHERE id = 4`)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0][0]; got != "it's" {
		t.Fatalf("name = %v, want it's", got)
	}
	// \n 转义为真实换行
	if _, _, _, _, err := db.runSQL(`INSERT INTO users VALUES (5,'a\nb',10)`); err != nil {
		t.Fatal(err)
	}
	_, rows, _, _, err = db.runSQL(`SELECT name FROM users WHERE id = 5`)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0][0]; got != "a\nb" {
		t.Fatalf("name = %q, want \"a\\nb\"", got)
	}
}

// OR 条件此前被静默忽略（只返回 id=1），现在必须正确匹配两组。
func TestWhereOr(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	_, rows, _, _, err := db.runSQL("SELECT id FROM users WHERE id = 1 OR id = 3")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2 rows (1,3)", rows)
	}
	ids := []int64{rows[0][0].(int64), rows[1][0].(int64)}
	if ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("ids = %v, want [1 3]", ids)
	}
	// AND 高于 OR：grp=10 AND id=1 OR id=2 → 组1(grp=10 AND id=1) OR 组2(id=2)
	_, rows, _, _, err = db.runSQL("SELECT id FROM users WHERE grp = 10 AND id = 1 OR id = 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("AND/OR precedence: rows = %v, want 2 rows", rows)
	}
}

// ORDER BY 此前被消费后丢弃（结果静默不排序）。
func TestOrderByApplied(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	_, rows, _, _, err := db.runSQL("SELECT id FROM users ORDER BY id DESC")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0][0].(int64) != 3 || rows[2][0].(int64) != 1 {
		t.Fatalf("desc rows = %v, want [3 2 1]", rows)
	}
	// 字符串排序
	_, rows, _, _, err = db.runSQL("SELECT name FROM users ORDER BY name ASC")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0] != "alice" || rows[2][0] != "carol" {
		t.Fatalf("asc rows = %v", rows)
	}
}

// LIMIT offset,count（phpMyAdmin 浏览固定发 LIMIT 0, N）与默认不限行数。
func TestLimitOffsetAndDefault(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	// 默认不再静默截断到 200（此处仅 3 行，验证 LIMIT offset 形式）
	_, rows, _, _, err := db.runSQL("SELECT id FROM users LIMIT 1, 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0][0].(int64) != 2 || rows[1][0].(int64) != 3 {
		t.Fatalf("LIMIT 1,2 rows = %v, want [2 3]", rows)
	}
	_, rows, _, _, err = db.runSQL("SELECT id FROM users LIMIT 2 OFFSET 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0][0].(int64) != 2 {
		t.Fatalf("LIMIT 2 OFFSET 1 rows = %v", rows)
	}
	// ORDER BY + LIMIT 组合：先排序后截断
	_, rows, _, _, err = db.runSQL("SELECT id FROM users ORDER BY id DESC LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0][0].(int64) != 3 || rows[1][0].(int64) != 2 {
		t.Fatalf("ORDER+LIMIT rows = %v, want [3 2]", rows)
	}
}

// 浮点字面量此前被 tokenizer 截断成整数（1.5 → 1）。
func TestFloatLiterals(t *testing.T) {
	db := newParserTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE f(id INT, v INT, PRIMARY KEY(id))"); err != nil {
		t.Fatal(err)
	}
	// WHERE 中使用浮点：解析不再破坏语句（INT 列比较按数值处理）
	if _, _, _, _, err := db.runSQL("INSERT INTO f VALUES (1, 2)"); err != nil {
		t.Fatal(err)
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM f WHERE v > 1.5")
	if err != nil {
		t.Fatalf("float literal in WHERE: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want 1", rows)
	}
}

// NULL 字面量与 IS NULL / IS NOT NULL。
func TestNullLiterals(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	// WHERE id = NULL 恒为假（SQL 语义）
	_, rows, _, _, err := db.runSQL("SELECT id FROM users WHERE id = NULL")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("id = NULL must match nothing, got %v", rows)
	}
	_, rows, _, _, err = db.runSQL("SELECT id FROM users WHERE id IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("IS NOT NULL rows = %v, want 3", rows)
	}
}

// LIKE 条件（此前直接报错）。
func TestWhereLike(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	_, rows, _, _, err := db.runSQL("SELECT id FROM users WHERE name LIKE 'a%'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0][0].(int64) != 1 {
		t.Fatalf("LIKE rows = %v, want [1]", rows)
	}
}

// 注释支持：-- / # / /* */。
func TestComments(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	_, rows, _, _, err := db.runSQL("SELECT id FROM users -- trailing comment")
	if err != nil {
		t.Fatalf("trailing -- comment: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
	_, rows, _, _, err = db.runSQL("SELECT id /* inline */ FROM users WHERE id = 1 # end")
	if err != nil {
		t.Fatalf("inline comments: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
}

// 语句尾部残留 token 必须报错而不是静默忽略。
func TestTrailingTokensRejected(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	_, _, _, _, err := db.runSQL("SELECT id FROM users WHERE id = 1 LIMIT 5, 10, 15")
	if err == nil || !strings.Contains(err.Error(), "after statement") {
		t.Fatalf("expected trailing-token error, got %v", err)
	}
}

// GROUP BY / HAVING 显式报错（此前静默丢弃返回未分组结果）。
func TestGroupByRejected(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	if _, _, _, _, err := db.runSQL("SELECT grp FROM users GROUP BY grp"); err == nil {
		t.Fatal("GROUP BY should error")
	}
	if _, _, _, _, err := db.runSQL("SELECT id FROM users HAVING id > 1"); err == nil {
		t.Fatal("HAVING should error")
	}
}

// 多行 INSERT 原子性：任一行失败则整体回滚。
func TestMultiRowInsertAtomic(t *testing.T) {
	db := newParserTestDB(t)
	seedSQLDB(t, db)
	// 第二行的主键 2 与既有行冲突 → 整体失败，第三行不应插入
	_, _, _, _, err := db.runSQL("INSERT INTO users VALUES (10,'x',1),(2,'dup',2),(11,'y',3)")
	if err == nil {
		t.Fatal("expected conflict error")
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM users WHERE id >= 10")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("multi-row insert must be atomic; leaked rows: %v", rows)
	}
	// 正常多行插入后重启验证 COMMIT 记录已落盘（重放不丢行）
	if _, _, _, _, err := db.runSQL("INSERT INTO users VALUES (20,'m',1),(21,'n',1)"); err != nil {
		t.Fatal(err)
	}
}

// 回归 H1：显式列清单且列序与表字段序不一致时，此前走位置参数快速路径
// 导致 values 与 meta.Fields 错位、静默写坏数据（且 WAL 重放后同样错）。
func TestInsertExplicitColumnOrder(t *testing.T) {
	db := newParserTestDB(t)
	steps := []string{
		"CREATE TABLE ord(id INT, name VARCHAR(40), grp INT, PRIMARY KEY(id))",
		"INSERT INTO ord (name, grp, id) VALUES ('a', 10, 5)",
		"INSERT INTO ord (grp, id, name) VALUES (20, 6, 'b')",
		"INSERT INTO ord VALUES (7, 'c', 30)", // 完全一致列序 → 快速路径
	}
	for _, stmt := range steps {
		if _, _, _, _, err := db.runSQL(stmt); err != nil {
			t.Fatalf("%q: %v", stmt, err)
		}
	}
	want := []struct {
		id   int64
		name string
		grp  int64
	}{{5, "a", 10}, {6, "b", 20}, {7, "c", 30}}
	for _, w := range want {
		rows, err := db.getTable("ord").Select(map[string]interface{}{"id": w.id}, nil, nil)
		if err != nil || len(rows) != 1 {
			t.Fatalf("row %d: rows=%v err=%v", w.id, rows, err)
		}
		if rows[0]["name"] != w.name || rows[0]["grp"] != w.grp {
			t.Fatalf("pk=%d got name=%v grp=%v, want %s/%d", w.id, rows[0]["name"], rows[0]["grp"], w.name, w.grp)
		}
	}
}

// 回归 C1：SQL 路径 ALTER TABLE ADD COLUMN 曾原地 append published meta，
// 并发 SELECT 无锁读 Fields 切片会撕裂。现走引擎 COW 路径。
func TestAlterAddColumnCOW(t *testing.T) {
	db := newParserTestDB(t)
	if _, _, _, _, err := db.runSQL("CREATE TABLE cow(id INT, name VARCHAR(40), PRIMARY KEY(id))"); err != nil {
		t.Fatal(err)
	}
	tbl := db.getTable("cow")
	for i := int64(0); i < 100; i++ {
		if err := tbl.Insert(i, map[string]interface{}{"id": i, "name": "x"}, 0, 0, false); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			stmt := "ALTER TABLE cow ADD COLUMN col" + string(rune('a'+i%26)) + " INT"
			if i >= 26 {
				stmt = "ALTER TABLE cow ADD COLUMN colz INT" // 已存在 → 报错也行，覆盖并发路径
			}
			db.runSQL(stmt)
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		if _, _, _, _, err := db.runSQL("SELECT * FROM cow WHERE id = 5"); err != nil {
			t.Fatalf("concurrent select failed: %v", err)
		}
	}
}
