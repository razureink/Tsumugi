package main

import (
	"testing"
)

// TestPhpMyAdminCompat 验证 phpMyAdmin 风格的连接期与浏览期查询都能成功执行。
func TestPhpMyAdminCompat(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if _, _, _, _, err := db.runSQL("CREATE TABLE users (id INT, name VARCHAR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, _, _, _, err := db.runSQL("INSERT INTO users VALUES (1, 'Alice')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 连接期标量查询
	for _, q := range []string{"SELECT VERSION()", "SELECT @@version_comment", "SELECT @@session.sql_mode", "SELECT NOW()", "SELECT DATABASE()"} {
		if _, _, _, _, err := db.runSQL(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// information_schema 虚拟表
	cases := []string{
		"SELECT * FROM information_schema.SCHEMATA",
		"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = 'tsumugi'",
		"SELECT * FROM information_schema.TABLES",
		"SELECT * FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'tsumugi'",
		"SELECT * FROM information_schema.COLUMNS",
		"SELECT * FROM information_schema.COLUMNS WHERE TABLE_NAME = 'users'",
		"SELECT * FROM information_schema.STATISTICS WHERE TABLE_NAME = 'users'",
		"SELECT * FROM information_schema.TABLE_CONSTRAINTS",
		"SELECT * FROM information_schema.KEY_COLUMN_USAGE",
		"SELECT * FROM information_schema.USER_PRIVILEGES",
		"SELECT * FROM information_schema.CHARACTER_SETS",
		"SELECT * FROM information_schema.COLLATIONS",
		"SELECT * FROM information_schema.ENGINES",
		"SELECT * FROM information_schema.PLUGINS",
		"SELECT * FROM information_schema.ROUTINES",
		"SELECT COUNT(*) FROM information_schema.COLUMNS",
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'tsumugi'",
	}
	for _, q := range cases {
		if _, _, _, _, err := db.runSQL(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// 检查 SCHEMATA 过滤生效（返回 tsumugi 一行）
	_, rows, _, _, err := db.runSQL("SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = 'tsumugi'")
	if err != nil {
		t.Fatalf("schemata filter: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != "tsumugi" {
		t.Fatalf("SCHEMATA filter rows=%v, want [[tsumugi]]", rows)
	}

	// SHOW 变体
	for _, q := range []string{
		"SHOW TABLE STATUS FROM tsumugi",
		"SHOW FULL COLUMNS FROM users",
		"SHOW COLUMNS FROM users",
		"SHOW INDEX FROM users",
		"SHOW CREATE TABLE users",
		"SHOW CREATE DATABASE tsumugi",
		"SHOW GRANTS",
		"SHOW ENGINES",
		"SHOW CHARACTER SET",
		"SHOW COLLATION",
		"SHOW STATUS",
		"SHOW PROCESSLIST",
	} {
		if _, _, _, _, err := db.runSQL(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// COUNT 物理表
	_, rows, _, _, err = db.runSQL("SELECT COUNT(*) FROM users")
	if err != nil {
		t.Fatalf("count physical: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("count physical rows=%d want 1", len(rows))
	}
	if got, _ := rows[0][0].(int64); got != 1 {
		t.Fatalf("count physical = %v, want 1", rows[0][0])
	}
}