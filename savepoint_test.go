package main

import "testing"

// ==================== A4：SAVEPOINT 测试 ====================

// TestSavepointRollbackInsert 保存点后插入的行，回滚到保存点后必须消失。
func TestSavepointRollbackInsert(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,1)")
	runSess(t, db, sess, "SAVEPOINT s1")
	runSess(t, db, sess, "INSERT INTO t VALUES (2,2)")
	runSess(t, db, sess, "ROLLBACK TO SAVEPOINT s1")
	runSess(t, db, sess, "COMMIT")

	tbl := db.getTable("t")
	if got := tbl.pkTree.Size(); got != 1 {
		t.Fatalf("行数=%d want 1（保存点后的插入未回滚）", got)
	}
	if _, ok := tbl.SelectByPK(2); ok {
		t.Fatal("保存点之后插入的行仍在")
	}
	if _, ok := tbl.SelectByPK(1); !ok {
		t.Fatal("保存点之前的行被误删")
	}
}

// TestSavepointRollbackUpdate 保存点后的更新必须还原成保存点时刻的值。
func TestSavepointRollbackUpdate(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,100)")
	runSess(t, db, sess, "SAVEPOINT s1")
	runSess(t, db, sess, "UPDATE t SET v = 999 WHERE id = 1")
	runSess(t, db, sess, "ROLLBACK TO s1") // 省略 SAVEPOINT 关键字也应支持
	runSess(t, db, sess, "COMMIT")

	row, ok := db.getTable("t").SelectByPK(1)
	if !ok {
		t.Fatal("行丢失")
	}
	if row["v"] != int64(100) {
		t.Fatalf("v=%v want 100（保存点后的更新未还原）", row["v"])
	}
}

// TestSavepointRollbackDelete 保存点后的删除必须被还原。
func TestSavepointRollbackDelete(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,7)")
	runSess(t, db, sess, "SAVEPOINT s1")
	runSess(t, db, sess, "DELETE FROM t WHERE id = 1")
	runSess(t, db, sess, "ROLLBACK TO SAVEPOINT s1")
	runSess(t, db, sess, "COMMIT")

	row, ok := db.getTable("t").SelectByPK(1)
	if !ok {
		t.Fatal("被回滚的删除没有还原")
	}
	if row["v"] != int64(7) {
		t.Fatalf("v=%v want 7", row["v"])
	}
}

// TestSavepointNested 嵌套保存点：回滚到外层点应丢弃内层点。
func TestSavepointNested(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,1)")
	runSess(t, db, sess, "SAVEPOINT a")
	runSess(t, db, sess, "INSERT INTO t VALUES (2,2)")
	runSess(t, db, sess, "SAVEPOINT b")
	runSess(t, db, sess, "INSERT INTO t VALUES (3,3)")
	// 回滚到 a：2、3 都应消失，且 b 不再存在
	runSess(t, db, sess, "ROLLBACK TO SAVEPOINT a")
	if got := db.getTable("t").pkTree.Size(); got != 1 {
		t.Fatalf("回滚到 a 后行数=%d want 1", got)
	}
	var out sqlOut
	if _, _, _, _, err := db.runSQLSessionAsOut(sess, "ROLLBACK TO SAVEPOINT b", &out); err == nil {
		t.Fatal("b 应已随 a 的回滚被丢弃，回滚到 b 应报错")
	}
	runSess(t, db, sess, "COMMIT")
	if got := db.getTable("t").pkTree.Size(); got != 1 {
		t.Fatalf("提交后行数=%d want 1", got)
	}
}

// TestSavepointRelease 释放后不能再回滚到该点。
func TestSavepointRelease(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "SAVEPOINT s1")
	runSess(t, db, sess, "RELEASE SAVEPOINT s1")
	var out sqlOut
	if _, _, _, _, err := db.runSQLSessionAsOut(sess, "ROLLBACK TO SAVEPOINT s1", &out); err == nil {
		t.Fatal("已释放的保存点不应能回滚到")
	}
	runSess(t, db, sess, "COMMIT")
}

// TestSavepointSameName 同名保存点：新建会替换旧的，回滚行为以新点为准。
func TestSavepointSameName(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "SAVEPOINT s")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,1)")
	runSess(t, db, sess, "SAVEPOINT s") // 同名重建
	runSess(t, db, sess, "INSERT INTO t VALUES (2,2)")
	runSess(t, db, sess, "ROLLBACK TO SAVEPOINT s")
	runSess(t, db, sess, "COMMIT")
	// 新保存点包含第 1 行，所以只剩第 1 行
	if got := db.getTable("t").pkTree.Size(); got != 1 {
		t.Fatalf("行数=%d want 1", got)
	}
	if _, ok := db.getTable("t").SelectByPK(1); !ok {
		t.Fatal("第 1 行应保留（同名保存点在它之后建立）")
	}
}

// TestSavepointWALReplay 崩溃安全关键用例：保存点回滚后再提交，
// 重启（WAL 重放）后数据必须与提交时一致——证明补偿记录写对了。
func TestSavepointWALReplay(t *testing.T) {
	SetLogMute(true)
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	sess := &Session{db: db, user: "root", auth: true}
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT, KEY idx_v (v))")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,10)")
	runSess(t, db, sess, "SAVEPOINT sp")
	runSess(t, db, sess, "INSERT INTO t VALUES (2,20)")
	runSess(t, db, sess, "UPDATE t SET v = 999 WHERE id = 1")
	runSess(t, db, sess, "DELETE FROM t WHERE id = 1")
	runSess(t, db, sess, "ROLLBACK TO SAVEPOINT sp")
	runSess(t, db, sess, "COMMIT")
	// 提交时的期望状态：只有 (1,10)
	wantRows := int(db.getTable("t").pkTree.Size())
	if wantRows != 1 {
		t.Fatalf("提交时行数=%d want 1", wantRows)
	}
	db.Close()

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	tbl := db2.getTable("t")
	if tbl == nil {
		t.Fatal("表丢失")
	}
	if got := tbl.pkTree.Size(); got != int64(wantRows) {
		t.Fatalf("重放后行数=%d want %d（回滚的写入复活了 = 补偿记录缺失）", got, wantRows)
	}
	row, ok := tbl.SelectByPK(1)
	if !ok {
		t.Fatal("重放后第 1 行丢失")
	}
	if row["v"] != int64(10) {
		t.Fatalf("重放后 v=%v want 10（保存点后的 UPDATE/DELETE 未正确回滚）", row["v"])
	}
	if _, ok := tbl.SelectByPK(2); ok {
		t.Fatal("重放后出现保存点之后插入的第 2 行")
	}
	// 索引也必须与行一致
	if rows := mustExec(t, db2, "SELECT id FROM t WHERE v = 10"); len(rows) != 1 {
		t.Fatalf("索引查询结果=%d want 1", len(rows))
	}
}
