package main

import (
	"testing"
)

// TestDatabaseCommands 验证虚拟数据库管理命令的完整生命周期。
func TestDatabaseCommands(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// 默认在 tsumugi 库，建一个表
	if _, _, _, _, err := db.runSQL("CREATE TABLE base_t (id INT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("create base table: %v", err)
	}

	// SHOW DATABASES 应包含默认 5 个库
	_, rows, _, _, err := db.runSQL("SHOW DATABASES")
	if err != nil {
		t.Fatalf("show databases: %v", err)
	}
	if len(rows) < 5 {
		t.Fatalf("SHOW DATABASES rows=%d, want >=5", len(rows))
	}

	// CREATE DATABASE myapp
	_, _, _, msg, err := db.runSQL("CREATE DATABASE myapp")
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Logf("create db msg: %s", msg)

	// USE myapp
	if _, _, _, msg, err := db.runSQL("USE myapp"); err != nil {
		t.Fatalf("use: %v", err)
	} else {
		t.Logf("use msg: %s", msg)
	}

	// USE 后建表应归入 myapp
	if _, _, _, _, err := db.runSQL("CREATE TABLE users (id INT, name VARCHAR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("create table in myapp: %v", err)
	}

	// SHOW TABLES 当前库应显示 users（不带前缀）
	_, rows, _, _, err = db.runSQL("SHOW TABLES")
	if err != nil {
		t.Fatalf("show tables: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != "users" {
		t.Fatalf("SHOW TABLES in myapp = %v, want [users]", rows)
	}

	// 插入 + 查询
	if _, _, _, _, err := db.runSQL("INSERT INTO users VALUES (1, 'Alice')"); err != nil {
		t.Fatalf("insert in myapp: %v", err)
	}
	_, rows, _, _, err = db.runSQL("SELECT * FROM users")
	if err != nil {
		t.Fatalf("select in myapp: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("select rows=%d want 1", len(rows))
	}

	// 切回 tsumugi，SHOW TABLES 应只显示无前缀表 base_t
	if _, _, _, _, err := db.runSQL("USE tsumugi"); err != nil {
		t.Fatalf("use tsumugi: %v", err)
	}
	_, rows, _, _, err = db.runSQL("SHOW TABLES")
	if err != nil {
		t.Fatalf("show tables in tsumugi: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != "base_t" {
		t.Fatalf("SHOW TABLES in tsumugi = %v, want [base_t]", rows)
	}

	// 默认库仍能直接访问 users？不能（users 属于 myapp）—— 用限定名访问
	_, _, _, _, err = db.runSQL("SELECT * FROM myapp.users")
	if err != nil {
		t.Fatalf("qualified select myapp.users: %v", err)
	}

	// DROP DATABASE myapp 应删除 myapp.users
	_, _, _, msg, err = db.runSQL("DROP DATABASE myapp")
	if err != nil {
		t.Fatalf("drop database: %v", err)
	}
	t.Logf("drop msg: %s", msg)
	if _, _, _, _, err := db.runSQL("SELECT * FROM myapp.users"); err == nil {
		t.Fatal("myapp.users should be gone after DROP DATABASE")
	}

	// SHOW DATABASES 不应再含 myapp
	_, rows, _, _, err = db.runSQL("SHOW DATABASES")
	if err != nil {
		t.Fatalf("show databases after drop: %v", err)
	}
	for _, r := range rows {
		if r[0] == "myapp" {
			t.Fatal("myapp still in databases after drop")
		}
	}

	// DROP 不存在的库报错
	if _, _, _, _, err := db.runSQL("DROP DATABASE not_exist"); err == nil {
		t.Fatal("expected error dropping unknown database")
	}
}
