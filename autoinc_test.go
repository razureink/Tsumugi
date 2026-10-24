package main

import (
	"fmt"
	"sync"
	"testing"
)

// 自增主键：INSERT 未给主键（或给 0/NULL）时由引擎分配，
// 解决多客户端各自 "MAX(pk)+1" 播种导致的主键碰撞（实测 37% 插入失败）。
func TestAutoIncrementPrimaryKey(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE TABLE ai (id INT PRIMARY KEY, name VARCHAR(20))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, _, err := db.runSQL("INSERT INTO ai (name) VALUES ('a')"); err != nil {
		t.Fatalf("insert without pk: %v", err)
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM ai")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	first, _ := rows[0][0].(int64)
	if first <= 0 {
		t.Fatalf("auto assigned id = %d, want > 0", first)
	}
	// 主键列给 0 → 同样分配；显式主键不受影响
	if _, _, _, _, err := db.runSQL("INSERT INTO ai (id, name) VALUES (0, 'b')"); err != nil {
		t.Fatalf("insert with pk=0: %v", err)
	}
	if _, _, _, _, err := db.runSQL("INSERT INTO ai (id, name) VALUES (100, 'c')"); err != nil {
		t.Fatalf("insert with explicit pk: %v", err)
	}
	_, rows, _, _, _ = db.runSQL("SELECT id FROM ai ORDER BY id")
	seen := map[int64]bool{}
	for _, r := range rows {
		pk, _ := r[0].(int64)
		if seen[pk] {
			t.Fatalf("duplicate id %d", pk)
		}
		seen[pk] = true
	}
	if !seen[100] {
		t.Fatalf("explicit pk 100 missing")
	}
	// 显式插入的 100 也要抬高计数器：下一次自增必须 > 100
	if _, _, _, _, err := db.runSQL("INSERT INTO ai (name) VALUES ('d')"); err != nil {
		t.Fatalf("insert after explicit: %v", err)
	}
	_, rows, _, _, _ = db.runSQL("SELECT id FROM ai ORDER BY id DESC LIMIT 1")
	last, _ := rows[0][0].(int64)
	if last <= 100 {
		t.Fatalf("auto id after explicit 100 = %d, want > 100", last)
	}
	db.Close()

	// 重启后从 max(pk)+1 继续，不复用历史主键
	db2, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if _, _, _, _, err := db2.runSQL("INSERT INTO ai (name) VALUES ('e')"); err != nil {
		t.Fatalf("insert after reopen: %v", err)
	}
	_, rows, _, _, _ = db2.runSQL("SELECT id FROM ai ORDER BY id DESC LIMIT 1")
	after, _ := rows[0][0].(int64)
	if after <= last {
		t.Fatalf("id after reopen = %d, want > %d", after, last)
	}
}

// 并发自增：多协程同时插入（不指定主键）不得撞主键。
func TestAutoIncrementConcurrent(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	if _, _, _, _, err := db.runSQL("CREATE TABLE ai2 (id INT PRIMARY KEY, name VARCHAR(20))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	const workers, per = 8, 25
	var wg sync.WaitGroup
	errCh := make(chan error, workers*per)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, _, _, _, err := db.runSQL(fmt.Sprintf("INSERT INTO ai2 (name) VALUES ('w%d-%d')", w, i)); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent insert: %v", err)
	}
	_, rows, _, _, err := db.runSQL("SELECT id FROM ai2")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != workers*per {
		t.Fatalf("rows = %d, want %d", len(rows), workers*per)
	}
	seen := map[int64]bool{}
	for _, r := range rows {
		pk, _ := r[0].(int64)
		if seen[pk] {
			t.Fatalf("duplicate id %d", pk)
		}
		seen[pk] = true
	}
}

// 事务冲突策略：默认 error（乐观并发，冲突报错回滚）；
// last_write_wins 时后写者直接覆盖，提交不报错（与 MySQL autocommit 一致）。
// 同一个 DB 内切换策略，避免测试进程里出现两个 DB 实例（进程级 WAL 通道是单例）。
func TestTxnConflictPolicy(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	if _, _, _, _, err := db.runSQL("CREATE TABLE tc (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, _, err := db.runSQL("INSERT INTO tc (id, v) VALUES (1, 1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	race := func() (error, error) {
		t1 := db.BeginTxn()
		t2 := db.BeginTxn()
		tbl := db.getTable("tc")
		if err := db.txnApply(t1, tbl, 1, func() error {
			return tbl.Update(1, map[string]interface{}{"id": int64(1), "v": int64(10)}, 0, t1, false)
		}); err != nil {
			t.Fatalf("txn1 update: %v", err)
		}
		if err := db.txnApply(t2, tbl, 1, func() error {
			return tbl.Update(1, map[string]interface{}{"id": int64(1), "v": int64(20)}, 0, t2, false)
		}); err != nil {
			t.Fatalf("txn2 update: %v", err)
		}
		e1 := db.CommitTxn(t1)
		e2 := db.CommitTxn(t2)
		return e1, e2
	}
	// 默认策略：两个并发写同一行的事务，恰好一个提交报冲突
	// （先写的那方在提交时发现行已被后写方覆盖 → conflict）
	if e1, e2 := race(); (e1 == nil) == (e2 == nil) {
		t.Fatalf("exactly one commit should conflict under default policy, got %v / %v", e1, e2)
	}
	// last_write_wins：两个提交都成功（MySQL autocommit 语义）
	db.cfgMu.Lock()
	db.config.TxnConflict = "last_write_wins"
	db.cfgMu.Unlock()
	if e1, e2 := race(); e1 != nil || e2 != nil {
		t.Fatalf("last_write_wins: commits should both succeed, got %v / %v", e1, e2)
	}
}

// 数据目录独占锁：同一 data 目录不允许两个实例并存（否则 WAL 互相覆盖）。
func TestDataDirExclusiveLock(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, err := NewDB(walTestConfig(dir)); err == nil {
		t.Fatalf("second instance on same data dir must be rejected")
	}
	db.Close()
	db2, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	db2.Close()
}
