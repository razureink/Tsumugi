package main

// ==================== 语句级 MySQL 兼容 ====================
// TRUNCATE / RENAME TABLE / 事务控制语句 / 各类"无害"管理语句，
// 以及 SHOW 的补充输出。这些语句在 MySQL 客户端与 ORM 里出现频率很高，
// 返回 unknown statement 会直接表现为"驱动不兼容"。

import (
	"fmt"
	"hash/crc32"
)

// truncateTable 清空一张表的全部行（保留表结构与授权）。
func (db *DB) truncateTable(name string) (int64, error) {
	t := db.getTable(name)
	if t == nil {
		return 0, fmt.Errorf("table not found: %s", name)
	}
	var keys []int64
	t.pkTree.scanRangeStop(nil, nil, func(k int64, v []byte) bool {
		keys = append(keys, k)
		return true
	})
	var n int64
	for _, k := range keys {
		if err := t.Delete(k, 0, false); err == nil {
			n++
		}
	}
	return n, db.flushWAL()
}

// renameTable 表重命名（写 WAL rename 记录，重启后保持）。
func (db *DB) renameTable(oldName, newName string) error {
	db.mu.Lock()
	_, ok := db.catalog[oldName]
	if !ok {
		db.mu.Unlock()
		return fmt.Errorf("table not found: %s", oldName)
	}
	if _, exists := db.catalog[newName]; exists {
		db.mu.Unlock()
		return fmt.Errorf("table %s already exists", newName)
	}
	db.mu.Unlock()

	// 表 ID 必须在改名前后保持一致：WAL 记录与内存重放都用同一个 ID
	id := walRegRegister(oldName)
	buf := make([]byte, 0, 16+len(oldName)+len(newName))
	buf = append(buf, 100, 3) // catalog subop=3: RENAME
	buf = appendVarUint(buf, uint64(id))
	buf = appendVarLenStr(buf, newName)
	if walCRC.Load() {
		buf = appendU32(buf, crc32.ChecksumIEEE(buf))
	}
	if err := appendWAL(buf, false); err != nil {
		return err
	}
	db.applyCatalogRenameV2(id, newName)
	return db.flushWAL()
}

// parseTruncate 处理 TRUNCATE [TABLE] name。
func (db *DB) parseTruncate(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // TRUNCATE
	p.matchKeyword("TABLE")
	name, e := p.readQualifiedTable()
	if e != nil {
		err = e
		return
	}
	name = db.qualifyTableIn(name, p.curSchema(db))
	n, e := db.truncateTable(name)
	if e != nil {
		err = e
		return
	}
	affected = n
	rawMsg = "Query OK"
	return
}

// parseRenameTable 处理 RENAME TABLE a TO b [, c TO d]。
func (db *DB) parseRenameTable(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next() // RENAME
	if !p.matchKeyword("TABLE") {
		err = fmt.Errorf("expected TABLE")
		return
	}
	for {
		oldName, e := p.readQualifiedTable()
		if e != nil {
			err = e
			return
		}
		if !p.matchKeyword("TO") {
			err = fmt.Errorf("expected TO")
			return
		}
		newName, e := p.readQualifiedTable()
		if e != nil {
			err = e
			return
		}
		if e := db.renameTable(db.qualifyTableIn(oldName, p.curSchema(db)), db.qualifyTableIn(newName, p.curSchema(db))); e != nil {
			err = e
			return
		}
		affected++
		if !p.matchSym(",") {
			break
		}
	}
	if e := p.expectEnd(); e != nil {
		err = e
		return
	}
	rawMsg = "Query OK"
	return
}

// parseTxnControl 处理 BEGIN / COMMIT / ROLLBACK / SAVEPOINT / RELEASE。
// 语句级原子性由写语句自身的隐式事务保证；这里保持连接语义兼容（返回 OK）。
func (db *DB) parseTxnControl(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	p.next()
	p.matchKeyword("TRANSACTION")
	p.matchKeyword("WORK")
	// SAVEPOINT name / RELEASE [SAVEPOINT] name / START TRANSACTION READ WRITE 等尾巴
	for {
		t := p.peek()
		if t.kind == kindEof {
			break
		}
		if t.kind == kindSym && t.val == ";" {
			p.next()
			continue
		}
		p.next()
	}
	rawMsg = "Query OK"
	return
}

// parseNoOpStmt 处理"接受但不做事"的管理语句：FLUSH / ANALYZE / OPTIMIZE /// CHECK / REPAIR / LOCK TABLES / UNLOCK TABLES / KILL / DO / HELP / RESET。
// MySQL 客户端（含连接池与运维脚本）会周期发送这些语句，报错会造成兼容性问题。
func (db *DB) parseNoOpStmt(p *sqlParser) (columns []string, rows [][]interface{}, affected int64, rawMsg string, err error) {
	// 消费到语句结束（容忍任意参数）
	for {
		t := p.peek()
		if t.kind == kindEof {
			break
		}
		if t.kind == kindSym && t.val == ";" {
			p.next()
			continue
		}
		p.next()
	}
	rawMsg = "Query OK"
	return
}
