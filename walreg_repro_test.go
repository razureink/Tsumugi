package main

import (
	"sync"
	"testing"
	"time"
)

// 回归：NewDB 在 loadWAL 之后曾调用 walRegReset() 清空注册表，导致重启后
// 新建表从 1 重新编号、与既有表 tableID 冲突，第三次重启时数据串表。
// 见 db.go NewDB 注释。修复方式：保留恢复期重建的注册表。
func TestWalRegNoIDCollisionAfterRestart(t *testing.T) {
	dir := t.TempDir()

	// 阶段1：建表 aaa / bbb，各插 1 行，正常关闭
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	walMustCreate(t, db, "aaa")
	walMustCreate(t, db, "bbb")
	if err := db.getTable("aaa").Insert(1, map[string]interface{}{"id": int64(1), "name": "A1"}, 0, 0, false); err != nil {
		t.Fatalf("insert aaa: %v", err)
	}
	if err := db.getTable("bbb").Insert(1, map[string]interface{}{"id": int64(1), "name": "B1"}, 0, 0, false); err != nil {
		t.Fatalf("insert bbb: %v", err)
	}
	db.Close()

	// 阶段2：重启。恢复出的 aaa.tableID=1、bbb.tableID=2；新建 ccc 必须拿到 id=3。
	db2, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB 2: %v", err)
	}
	walMustCreate(t, db2, "ccc")
	if id := db2.getTable("ccc").tableID; id != 3 {
		t.Fatalf("new table ccc tableID = %d, want 3 (registry must survive restart)", id)
	}
	if err := db2.getTable("ccc").Insert(1, map[string]interface{}{"id": int64(1), "name": "C1"}, 0, 0, false); err != nil {
		t.Fatalf("insert ccc: %v", err)
	}
	if err := db2.getTable("aaa").Insert(2, map[string]interface{}{"id": int64(2), "name": "A2"}, 0, 0, false); err != nil {
		t.Fatalf("insert aaa2: %v", err)
	}
	db2.Close()

	// 阶段3：第三次重启，各表行数与归属必须正确
	db3, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB 3: %v", err)
	}
	defer db3.Close()
	gotA, gotB, gotC := walRowCount(t, db3, "aaa"), walRowCount(t, db3, "bbb"), walRowCount(t, db3, "ccc")
	if gotA != 2 || gotB != 1 || gotC != 1 {
		t.Fatalf("row misattribution after restart: aaa=%d bbb=%d ccc=%d, want 2/1/1", gotA, gotB, gotC)
	}
}

// 回归：CREATE INDEX / ALTER TABLE 曾在共享 map 上边遍历边写（并发致命错误）。
// 现为写时复制：并发 DDL 与读写不应崩溃、不产生数据竞态（-race 下运行）。
func TestConcurrentDDLWithDML(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	walMustCreate(t, db, "ct")
	tbl := db.getTable("ct")
	for i := int64(0); i < 50; i++ {
		if err := tbl.Insert(i, map[string]interface{}{"id": i, "name": "x"}, 0, 0, false); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}

	var wg sync.WaitGroup
	stop := time.After(300 * time.Millisecond)
	done := make(chan struct{})
	// 写者：持续 DML
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := int64(1000)
		for {
			select {
			case <-done:
				return
			default:
			}
			tbl.Insert(i, map[string]interface{}{"id": i, "name": "y"}, 0, 0, true)
			tbl.Delete(i, 0, true)
			i++
		}
	}()
	// 读者：持续 SELECT
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			tbl.Select(map[string]interface{}{"id": int64(5)}, nil, nil)
		}
	}()
	// DDL：交替 ALTER 与 CREATE INDEX
	for i := 0; ; i++ {
		select {
		case <-stop:
			close(done)
			wg.Wait()
			return
		default:
		}
		col := "col_alt"
		if i%2 == 1 {
			col = "col_idx"
		}
		if err := db.AlterTableAddColumn("ct", col, TypeInt, 0); err != nil {
			// 已存在则忽略，继续下一轮
			continue
		}
		if i%2 == 1 {
			db.mu.Lock()
			newMeta := cloneMeta(tbl.Meta(), tbl.Meta().Name)
			newMeta.Indexes["ix_"+col] = col
			tbl.setMeta(newMeta)
			db.mu.Unlock()
		}
	}
}
