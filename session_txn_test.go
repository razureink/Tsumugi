package main

import (
	"testing"
)

// ==================== 会话级事务状态测试 ====================
// 回归：SQL 文本路径上的 BEGIN/COMMIT/ROLLBACK 曾经是静默 no-op
// （parseTxnControl 只消费 token 就返回 OK），导致客户端以为在事务里，
// 实际每条语句都自动提交。这里用真实的 Session 验证语义已正确。

// sessionDB 建一个测试库，返回库与一个绑定到它的会话。
func sessionDB(t *testing.T) (*DB, *Session, func()) {
	t.Helper()
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	sess := &Session{db: db, user: "root", auth: true}
	closed := false
	return db, sess, func() {
		if !closed {
			closed = true
			db.Close()
		}
	}
}

func runSess(t *testing.T, db *DB, sess *Session, sql string) {
	t.Helper()
	var out sqlOut
	if _, _, _, _, err := db.runSQLSessionAsOut(sess, sql, &out); err != nil {
		t.Fatalf("%q: %v", sql, err)
	}
}

// TestSessionTxnRollback 事务内写入后 ROLLBACK，行必须消失。
func TestSessionTxnRollback(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,1)")

	runSess(t, db, sess, "BEGIN")
	if !sess.inTxn || sess.txnID == 0 {
		t.Fatal("BEGIN 未建立会话事务状态（仍是 no-op）")
	}
	runSess(t, db, sess, "INSERT INTO t VALUES (2,2)")
	runSess(t, db, sess, "ROLLBACK")
	if sess.inTxn {
		t.Fatal("ROLLBACK 后会话仍处于事务中")
	}
	// 只有事务外的第 1 行应保留
	tbl := db.getTable("t")
	if got := tbl.pkTree.Size(); got != 1 {
		t.Fatalf("回滚后行数=%d want 1（事务内写入未回滚）", got)
	}
	if _, ok := tbl.SelectByPK(1); !ok {
		t.Fatal("事务外的行被误删")
	}
}

// TestSessionTxnCommit 事务内写入后 COMMIT，行必须保留。
func TestSessionTxnCommit(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,1)")
	runSess(t, db, sess, "INSERT INTO t VALUES (2,2)")
	runSess(t, db, sess, "COMMIT")
	if sess.inTxn {
		t.Fatal("COMMIT 后会话仍处于事务中")
	}
	if got := db.getTable("t").pkTree.Size(); got != 2 {
		t.Fatalf("提交后行数=%d want 2", got)
	}
}

// TestSessionTxnReopen 提交的数据必须能被 WAL 重放恢复（会话事务并入 WAL 事务号）。
func TestSessionTxnReopen(t *testing.T) {
	SetLogMute(true)
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	sess := &Session{db: db, user: "root", auth: true}
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "INSERT INTO t VALUES (1,10)")
	runSess(t, db, sess, "INSERT INTO t VALUES (2,20)")
	runSess(t, db, sess, "COMMIT")
	db.Close()

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	tbl := db2.getTable("t")
	if tbl == nil {
		t.Fatal("table missing after replay")
	}
	if got := tbl.pkTree.Size(); got != 2 {
		t.Fatalf("重放后行数=%d want 2", got)
	}
	row, ok := tbl.SelectByPK(2)
	if !ok || row["v"] != int64(20) {
		t.Fatalf("row 2 = %v/%v", row, ok)
	}
}

// TestSessionTxnConnCloseRollback 连接断开时未提交事务必须回滚。
func TestSessionTxnConnCloseRollback(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()
	runSess(t, db, sess, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	runSess(t, db, sess, "BEGIN")
	runSess(t, db, sess, "INSERT INTO t VALUES (9,9)")
	// 模拟 handleConn 的清理逻辑
	if sess.inTxn {
		if err := db.RollbackTxn(sess.txnID); err != nil {
			t.Fatalf("rollback on close: %v", err)
		}
		sess.inTxn = false
		sess.txnID = 0
	}
	if got := db.getTable("t").pkTree.Size(); got != 0 {
		t.Fatalf("断连后行数=%d want 0（未提交事务未回滚）", got)
	}
}

// TestIsRollbackToSavepoint 解析辅助函数（为后续 SAVEPOINT 实现铺路）。
func TestIsRollbackToSavepoint(t *testing.T) {
	yes := []string{
		"ROLLBACK TO SAVEPOINT sp1",
		"rollback to sp1",
		"ROLLBACK WORK TO SAVEPOINT a",
	}
	no := []string{
		"ROLLBACK",
		"rollback work",
		"COMMIT",
		"SELECT 1",
	}
	for _, s := range yes {
		if !isRollbackToSavepoint(s) {
			t.Fatalf("%q 应识别为 rollback to savepoint", s)
		}
	}
	for _, s := range no {
		if isRollbackToSavepoint(s) {
			t.Fatalf("%q 不应识别为 rollback to savepoint", s)
		}
	}
}
