package main

import (
	"fmt"
	"testing"
)

// ==================== C2：视图测试 ====================

func viewDB(t *testing.T) (*DB, string, func()) {
	t.Helper()
	SetLogMute(true)
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE emp (id INT PRIMARY KEY, name VARCHAR(32), dept INT, KEY idx_dept (dept))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 20; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf(
			"INSERT INTO emp VALUES (%d,'n-%d',%d)", i, i, i%3)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	closed := false
	return db, dir, func() {
		if !closed {
			closed = true
			db.Close()
		}
	}
}

// TestViewBasic 建视图 → 查视图 → 结果与直接查基表一致。
func TestViewBasic(t *testing.T) {
	db, _, closeDB := viewDB(t)
	defer closeDB()

	if _, _, _, _, err := db.runSQL("CREATE VIEW v_dept1 AS SELECT id, name FROM emp WHERE dept = 1"); err != nil {
		t.Fatalf("create view: %v", err)
	}
	_, rows, _, _, err := db.runSQL("SELECT id, name FROM v_dept1")
	if err != nil {
		t.Fatalf("select view: %v", err)
	}
	// 与直接查基表对比
	_, baseRows, _, _, err := db.runSQL("SELECT id, name FROM emp WHERE dept = 1")
	if err != nil {
		t.Fatalf("select base: %v", err)
	}
	if len(rows) != len(baseRows) || len(rows) == 0 {
		t.Fatalf("视图行数=%d 基表行数=%d", len(rows), len(baseRows))
	}
	for i := range rows {
		if rows[i][0] != baseRows[i][0] || rows[i][1] != baseRows[i][1] {
			t.Fatalf("第 %d 行不一致: 视图=%v 基表=%v", i, rows[i], baseRows[i])
		}
	}
}

// TestViewSeesBaseTableChanges 视图是"查询时物化"，基表变了视图必须跟着变
//（这是与物化视图最关键的区别，也是不维护副本的好处）。
func TestViewSeesBaseTableChanges(t *testing.T) {
	db, _, closeDB := viewDB(t)
	defer closeDB()
	if _, _, _, _, err := db.runSQL("CREATE VIEW v_all AS SELECT id FROM emp"); err != nil {
		t.Fatalf("create view: %v", err)
	}
	_, before, _, _, _ := db.runSQL("SELECT id FROM v_all")
	if _, _, _, _, err := db.runSQL("INSERT INTO emp VALUES (100,'new',0)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, after, _, _, _ := db.runSQL("SELECT id FROM v_all")
	if len(after) != len(before)+1 {
		t.Fatalf("基表插入后视图行数=%d，插入前=%d（视图未跟随基表）", len(after), len(before))
	}
}

// TestViewPersistsAcrossRestart 视图定义必须持久化，重开后仍可用。
func TestViewPersistsAcrossRestart(t *testing.T) {
	SetLogMute(true)
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, _, err := db.runSQL("INSERT INTO t VALUES (7)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE VIEW v_p AS SELECT id FROM t"); err != nil {
		t.Fatalf("create view: %v", err)
	}
	db.Close()

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	_, rows, _, _, err := db2.runSQL("SELECT id FROM v_p")
	if err != nil {
		t.Fatalf("重开后查视图失败: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != int64(7) {
		t.Fatalf("重开后视图结果=%v want [[7]]", rows)
	}
}

// TestViewErrors 各类非法操作必须明确报错，不能假装成功。
func TestViewErrors(t *testing.T) {
	db, _, closeDB := viewDB(t)
	defer closeDB()

	cases := []struct {
		sql string
		why string
	}{
		{"CREATE VIEW v1 AS INSERT INTO emp VALUES (1,'x',0)", "视图定义必须是 SELECT"},
		{"CREATE VIEW v2 AS SELECT nope FROM emp", "引用不存在的列"},
		{"CREATE VIEW v3 AS SELECT * FROM no_such_table", "引用不存在的表"},
		{"DROP VIEW no_such_view", "删除不存在的视图"},
	}
	for _, c := range cases {
		if _, _, _, _, err := db.runSQL(c.sql); err == nil {
			t.Fatalf("%q 应报错（%s）", c.sql, c.why)
		}
	}

	// 视图名不能与表冲突
	if _, _, _, _, err := db.runSQL("CREATE VIEW emp AS SELECT id FROM emp"); err == nil {
		t.Fatal("视图名与已有表冲突时应报错")
	}
	// 重复建同名视图：必须显式 OR REPLACE
	if _, _, _, _, err := db.runSQL("CREATE VIEW v4 AS SELECT id FROM emp"); err != nil {
		t.Fatalf("首次建视图失败: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE VIEW v4 AS SELECT id FROM emp"); err == nil {
		t.Fatal("重复建同名视图应报错")
	}
	if _, _, _, _, err := db.runSQL("CREATE OR REPLACE VIEW v4 AS SELECT id FROM emp WHERE id > 5"); err != nil {
		t.Fatalf("OR REPLACE 应成功: %v", err)
	}
	// IF EXISTS 删除不存在的视图应静默成功
	if _, _, _, _, err := db.runSQL("DROP VIEW IF EXISTS never_existed"); err != nil {
		t.Fatalf("DROP VIEW IF EXISTS 应成功: %v", err)
	}
}

// TestViewNested 视图可以引用视图（物化链路）。
func TestViewNested(t *testing.T) {
	db, _, closeDB := viewDB(t)
	defer closeDB()
	if _, _, _, _, err := db.runSQL("CREATE VIEW v_a AS SELECT id, dept FROM emp WHERE dept = 0"); err != nil {
		t.Fatalf("create v_a: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE VIEW v_b AS SELECT id FROM v_a WHERE id > 3"); err != nil {
		t.Fatalf("create v_b: %v", err)
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM v_b")
	if err != nil {
		t.Fatalf("select nested view: %v", err)
	}
	// dept=0 命中 id=3,6,9,12,15,18 共 6 行，再 id>3 应为 5 行。
	// 注意：这里**不用** `dept=0 AND id>3` 作对照——那条语句当前返回 6 行，
	// 是一个已发现的既有缺陷（索引等值 + 主键范围组合下主键范围被忽略），
	// 与视图功能无关，已单独记录待跟进。
	if len(rows) != 5 {
		t.Fatalf("嵌套视图行数=%d want 5", len(rows))
	}
}

// TestViewWithQuotedLiterals 视图定义里含引号/空格的字面量必须原样保存。
func TestViewWithQuotedLiterals(t *testing.T) {
	db, _, closeDB := viewDB(t)
	defer closeDB()
	if _, _, _, _, err := db.runSQL(`CREATE VIEW v_q AS SELECT id FROM emp WHERE name = 'n-1'`); err != nil {
		t.Fatalf("create view with literal: %v", err)
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM v_q")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != int64(1) {
		t.Fatalf("含字面量的视图结果=%v want [[1]]", rows)
	}
}
