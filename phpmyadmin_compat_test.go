package main

import (
	"strings"
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

	// phpMyAdmin postConnect 读取 $row['@@version'] 等，列名必须带 @@ 前缀
	{
		cols, rows, _, _, e := db.runSQL("SELECT @@version, @@version_comment, @@sql_mode")
		if e != nil {
			t.Fatalf("postconnect version select: %v", e)
		}
		want := []string{"@@version", "@@version_comment", "@@sql_mode"}
		for i, w := range want {
			if !strings.EqualFold(cols[i], w) {
				t.Fatalf("postconnect cols[%d]=%q, want %q (had=%v)", i, cols[i], w, cols)
			}
		}
		if len(rows) != 1 || len(rows[0]) != 3 {
			t.Fatalf("postconnect rows=%v, want 1 row x 3", rows)
		}
	}

	// phpMyAdmin 登录时的多列标量查询（无 AS 别名，列名带 @@ 前缀）
	multiCol := `SELECT @@session.auto_increment_increment, @@character_set_client, @@character_set_connection,
@@character_set_results, @@character_set_server, @@collation_server, @@collation_connection,
@@collation_database, @@init_connect, @@interactive_timeout, @@license,
@@lower_case_table_names, @@max_allowed_packet, @@net_buffer_length, @@net_write_timeout,
@@performance_schema, @@query_cache_size, @@query_cache_type, @@sql_mode, @@system_time_zone,
@@time_zone, @@transaction_isolation, @@wait_timeout`
	{
		cols, rows, _, _, e := db.runSQL(multiCol)
		if e != nil {
			t.Fatalf("multi-column scalar: %v", e)
		}
		if len(cols) < 20 {
			t.Fatalf("multi-column scalar returned %d cols, want >=20", len(cols))
		}
		if len(rows) != 1 || len(rows[0]) != len(cols) {
			t.Fatalf("multi-column scalar rows=%v want 1 row with %d values", rows, len(cols))
		}
		for i, c := range []string{"character_set_client", "collation_server", "sql_mode", "wait_timeout"} {
			for _, col := range cols {
				if strings.EqualFold(strings.TrimPrefix(col, "@@"), c) {
					if rows[0][i] == nil || rows[0][i] == "" {
						t.Fatalf("multi-column scalar %s empty: %v", c, rows[0][i])
					}
				}
			}
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
		// AS 别名（phpMyAdmin 的 DatabaseList/Structure 查询常见）
		"SELECT SCHEMA_NAME AS SCHEMA_NAME FROM information_schema.SCHEMATA",
		"SELECT TABLE_SCHEMA AS DB, TABLE_NAME AS TB, TABLE_TYPE AS TYPE FROM information_schema.TABLES",
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'tsumugi'",
	}
	for _, q := range cases {
		if _, _, _, _, err := db.runSQL(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// AS 别名必须作为输出列名
	{
		cols, _, _, _, e := db.runSQL("SELECT TABLE_NAME AS TB FROM information_schema.TABLES LIMIT 1")
		if e != nil {
			t.Fatalf("alias select: %v", e)
		}
		if len(cols) != 1 || !strings.EqualFold(cols[0], "TB") {
			t.Fatalf("alias cols=%v, want [TB]", cols)
		}
	}
	{
		cols, rows, _, _, e := db.runSQL("SELECT TABLE_SCHEMA AS DB, TABLE_NAME AS TB FROM information_schema.TABLES LIMIT 1")
		if e != nil {
			t.Fatalf("multi alias select: %v", e)
		}
		want := []string{"DB", "TB"}
		for i, w := range want {
			if !strings.EqualFold(cols[i], w) {
				t.Fatalf("multi alias cols[%d]=%q, want %q (had=%v)", i, cols[i], w, cols)
			}
		}
		if len(rows) != 1 || len(rows[0]) != 2 {
			t.Fatalf("multi alias rows=%v, want 1 row x 2", rows)
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
