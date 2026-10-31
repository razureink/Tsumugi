package main

// ==================== A4：SAVEPOINT / ROLLBACK TO / RELEASE ====================
// MySQL 语义：
//   SAVEPOINT name            建立一个命名保存点
//   ROLLBACK TO [SAVEPOINT] n  回滚到该保存点（事务仍在，可继续写入）
//   RELEASE SAVEPOINT n        释放该保存点（连同其后建立的）
// 同名保存点：新建时删除旧的同名点及其之后的全部保存点。
//
// 实现要点（与"整事务回滚"的本质区别）：
//   txnChange 只保存"事务开始前"的行镜像，还原不了"保存点时刻的中间态"。
//   因此保存点必须额外快照**当时的行字节**（images）。
//
// 崩溃安全：整事务回滚靠"重放时丢弃该事务的全部记录"实现；保存点回滚是**部分**
// 回滚，事务仍会提交，所以必须把还原动作作为补偿记录写进 WAL，
// 否则重启后已回滚的写入会复活。

import "fmt"

// txnSavepoint 一个命名保存点的快照。
type txnSavepoint struct {
	name string
	// images 保存点时刻、本事务已改动过的每个 (表, 主键) 在表里的行字节。
	// 值不存在（键缺失）表示"当时该行不存在"。
	images map[string]map[int64][]byte
	// changes 保存点时刻 txn.Changes 的深拷贝（回滚后要恢复事务记账）
	changes map[string]map[int64]txnChange
}

// cloneChanges 深拷贝 Changes（保存点需要独立副本）。
func cloneChanges(src map[string]map[int64]txnChange) map[string]map[int64]txnChange {
	out := make(map[string]map[int64]txnChange, len(src))
	for name, m := range src {
		cp := make(map[int64]txnChange, len(m))
		for pk, ch := range m {
			cp[pk] = ch
		}
		out[name] = cp
	}
	return out
}

// Savepoint 建立保存点。
func (db *DB) Savepoint(id uint64, name string) error {
	if name == "" {
		return fmt.Errorf("savepoint name is required")
	}
	db.txnMu.Lock()
	txn, ok := db.txns[id]
	if !ok || !txn.Active {
		db.txnMu.Unlock()
		return fmt.Errorf("transaction not active")
	}
	// 快照：本事务改过的每一行的当前字节
	images := make(map[string]map[int64][]byte, len(txn.Changes))
	for tableName, changes := range txn.Changes {
		val, ok := db.tables.Load(tableName)
		if !ok {
			continue
		}
		tbl := val.(*Table)
		m := make(map[int64][]byte, len(changes))
		for pk := range changes {
			if row, exists := tbl.pkTree.Get(pk); exists {
				m[pk] = append([]byte(nil), row...)
			} else {
				m[pk] = nil
			}
		}
		images[tableName] = m
	}
	sp := &txnSavepoint{
		name:    name,
		images:  images,
		changes: cloneChanges(txn.Changes),
	}
	// 同名保存点：删除旧的及其之后（MySQL 语义）
	for i, old := range txn.Savepoints {
		if old.name == name {
			txn.Savepoints = txn.Savepoints[:i]
			break
		}
	}
	txn.Savepoints = append(txn.Savepoints, sp)
	db.txnMu.Unlock()
	return nil
}

// findSavepoint 返回保存点下标（同名取最后一个），未找到返回 -1。
func findSavepoint(txn *Transaction, name string) int {
	for i := len(txn.Savepoints) - 1; i >= 0; i-- {
		if txn.Savepoints[i].name == name {
			return i
		}
	}
	return -1
}

// RollbackToSavepoint 回滚到指定保存点：数据恢复到保存点时刻，事务保持活跃。
func (db *DB) RollbackToSavepoint(id uint64, name string) error {
	db.txnMu.Lock()
	txn, ok := db.txns[id]
	if !ok || !txn.Active {
		db.txnMu.Unlock()
		return fmt.Errorf("transaction not active")
	}
	idx := findSavepoint(txn, name)
	if idx < 0 {
		db.txnMu.Unlock()
		return fmt.Errorf("savepoint %s does not exist", name)
	}
	sp := txn.Savepoints[idx]
	// 当前变更集（要在锁内取出快照，避免与其它写入交错）
	current := cloneChanges(txn.Changes)
	txn.Savepoints = txn.Savepoints[:idx+1] // 保留到该保存点（含）
	db.txnMu.Unlock()

	// 数据还原 + WAL 补偿记录（补偿必须写入，否则重启后回滚失效）
	db.txnWriteMu.Lock()
	defer db.txnWriteMu.Unlock()
	for tableName, changes := range current {
		val, ok := db.tables.Load(tableName)
		if !ok {
			continue
		}
		tbl := val.(*Table)
		for pk := range changes {
			var image []byte
			if m, ok := sp.images[tableName]; ok {
				image = m[pk]
			}
			if image == nil {
				// 保存点时刻这行还不存在 → 删掉
				if _, exists := tbl.pkTree.Get(pk); exists {
					tbl.pkTree.Delete(pk)
					if err := tbl.writeLog(WALDelete, id, pk, nil); err != nil {
						return err
					}
				}
				continue
			}
			// 恢复到保存点时刻的字节。
			// WAL 补偿用 "DELETE + INSERT" 而不是 UPDATE：重放侧遇到不存在的行会
			// 跳过 UPDATE（replayUpdate 的前置条件），而这里的目标行可能刚被事务内
			// 的 DELETE 从 WAL 里删掉，UPDATE 会被静默跳过导致回滚失效。
			tbl.pkTree.Set(pk, image)
			if err := tbl.writeLog(WALDelete, id, pk, nil); err != nil {
				return err
			}
			if err := tbl.writeLog(WALInsert, id, pk, image); err != nil {
				return err
			}
		}
		if len(tbl.Meta().Indexes) > 0 {
			tbl.rebuildIndexes()
		}
	}
	// 恢复事务记账
	db.txnMu.Lock()
	txn.Changes = sp.changes
	db.txnMu.Unlock()
	return nil
}

// ReleaseSavepoint 释放保存点（连同其后建立的保存点）。
func (db *DB) ReleaseSavepoint(id uint64, name string) error {
	db.txnMu.Lock()
	defer db.txnMu.Unlock()
	txn, ok := db.txns[id]
	if !ok || !txn.Active {
		return fmt.Errorf("transaction not active")
	}
	idx := findSavepoint(txn, name)
	if idx < 0 {
		return fmt.Errorf("savepoint %s does not exist", name)
	}
	txn.Savepoints = txn.Savepoints[:idx]
	return nil
}
