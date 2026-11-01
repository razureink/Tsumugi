package main

import (
	"fmt"
	"testing"
)

// ==================== DDL 必须跟随连接级 schema ====================
// 回归：CREATE/DROP/ALTER TABLE 曾用全局 curDB 限定表名，忽略了连接级 schema
// （USE / COM_INIT_DB 之后的值）。多连接各自 USE 不同库时，DDL 会落到错误的库——
// 这类错误在生产里极难发现，因为"建表成功"但表不在你以为的地方。

// TestDDLUsesConnectionSchema 用带 schema 的执行入口验证建表落在指定库。
func TestDDLUsesConnectionSchema(t *testing.T) {
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	// 建两个库
	if _, _, _, _, err := db.runSQL("CREATE DATABASE db_a"); err != nil {
		t.Fatalf("create db_a: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE DATABASE db_b"); err != nil {
		t.Fatalf("create db_b: %v", err)
	}

	// 在 db_a 的 schema 下建表
	if _, _, _, _, err := db.runSQLSchemaAs("root", "CREATE TABLE t1 (id INT PRIMARY KEY)", nil, 0, "db_a"); err != nil {
		t.Fatalf("create in db_a: %v", err)
	}
	if tbl := db.getTable("db_a.t1"); tbl == nil {
		t.Fatal("表没有建在 db_a（DDL 仍用全局 curDB？）")
	}
	if tbl := db.getTable("t1"); tbl != nil {
		t.Fatal("表被错误地建在默认库")
	}

	// 在 db_b 的 schema 下建同名表：必须是两个独立的表
	if _, _, _, _, err := db.runSQLSchemaAs("root", "CREATE TABLE t1 (id INT PRIMARY KEY, v INT)", nil, 0, "db_b"); err != nil {
		t.Fatalf("create in db_b: %v", err)
	}
	a, b := db.getTable("db_a.t1"), db.getTable("db_b.t1")
	if a == nil || b == nil {
		t.Fatalf("两个库的 t1 应同时存在: a=%v b=%v", a != nil, b != nil)
	}
	if len(a.Meta().Fields) == len(b.Meta().Fields) {
		t.Fatalf("两表结构应不同（db_b 多一列），实际都是 %d 列", len(a.Meta().Fields))
	}

	// DROP 也必须跟随 schema：只删 db_b 的
	if _, _, _, _, err := db.runSQLSchemaAs("root", "DROP TABLE t1", nil, 0, "db_b"); err != nil {
		t.Fatalf("drop in db_b: %v", err)
	}
	if db.getTable("db_b.t1") != nil {
		t.Fatal("db_b.t1 未被删除")
	}
	if db.getTable("db_a.t1") == nil {
		t.Fatal("db_a.t1 被误删（DROP 用了错误的 schema）")
	}

	// ALTER 同理
	if _, _, _, _, err := db.runSQLSchemaAs("root", "ALTER TABLE t1 ADD COLUMN extra INT", nil, 0, "db_a"); err != nil {
		t.Fatalf("alter in db_a: %v", err)
	}
	// 建表时只有 id 一列，ADD COLUMN 后应为 2 列
	if got := len(db.getTable("db_a.t1").Meta().Fields); got != 2 {
		t.Fatalf("db_a.t1 列数=%d want 2（ALTER 未生效在正确库）", got)
	}
}

// TestDMLFollowsConnectionSchema DML 也必须跟随连接级 schema（与 DDL 一致）。
func TestDMLFollowsConnectionSchema(t *testing.T) {
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	if _, _, _, _, err := db.runSQL("CREATE DATABASE s1"); err != nil {
		t.Fatalf("create db: %v", err)
	}
	if _, _, _, _, err := db.runSQLSchemaAs("root", "CREATE TABLE m (id INT PRIMARY KEY)", nil, 0, "s1"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, _, _, _, err := db.runSQLSchemaAs("root",
			fmt.Sprintf("INSERT INTO m VALUES (%d)", i), nil, 0, "s1"); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	tbl := db.getTable("s1.m")
	if tbl == nil || tbl.pkTree.Size() != 3 {
		sfx := int64(-1)
		if tbl != nil {
			sfx = tbl.pkTree.Size()
		}
		t.Fatalf("s1.m 行数=%d want 3", sfx)
	}
	// 默认库不应存在同名表
	if db.getTable("m") != nil {
		t.Fatal("DML 落到了默认库")
	}
}
