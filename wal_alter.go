package main

// ==================== 原子化的表结构重建（ALTER） ====================
// 行字节没有版本标记，解码完全依赖当前 meta。因此"发布新 meta + 逐行重写"
// 的任何中间状态，一旦进程崩溃就会让旧字节被新 meta 解码，静默读到错位数据。
//
// 这里用一条 catalog 记录（subop=5）承载「新 meta + 全部行」：
// 重放时要么整条记录生效（表被整体替换为新结构），要么因尾部残缺被截断丢弃
// （表保持旧结构与旧行）。不存在"半新半旧"的中间态。

import (
	"fmt"
	"hash/crc32"
)

// alterRowBytes 重建后的一行（data 已按新 meta 编码）。
type alterRowBytes struct {
	pk   int64
	data []byte
}

// encodeAlterAllRows 用新 meta 重新编码全表行，返回行区（不含 meta 与行数）。
// 流式编码：内存里只有一份字节缓冲，不再为每行保留 map。
func encodeAlterAllRows(old *Table, newMeta *TableMeta, transform func(oldMeta *TableMeta, row map[string]interface{}) map[string]interface{}) (rowsBuf []byte, rows []alterRowBytes, err error) {
	oldMeta := old.Meta()
	var encErr error
	old.pkTree.scanRangeStop(nil, nil, func(pk int64, value []byte) bool {
		row, _, exp := decodeRow(oldMeta, value)
		nr := row
		if transform != nil {
			nr = transform(oldMeta, row)
		}
		if nr == nil {
			return true // 该行被丢弃
		}
		data := encodeRow(newMeta, nr, nextRowVersion(), exp)
		rowsBuf = appendI64(rowsBuf, pk)
		rowsBuf = appendVarUint(rowsBuf, uint64(len(data)))
		rowsBuf = append(rowsBuf, data...)
		rows = append(rows, alterRowBytes{pk: pk, data: data})
		if len(rowsBuf) > alterMaxBytes {
			encErr = fmt.Errorf("table is too large to rebuild in memory (limit %d MB)", alterMaxBytes>>20)
			return false
		}
		return true
	})
	if encErr != nil {
		return nil, nil, encErr
	}
	return rowsBuf, rows, nil
}

// alterMaxBytes 单次重建的字节上限（防止把整机内存吃光）。
var alterMaxBytes = 1 << 30 // 1 GB

// writeCatalogAlterAll 追加一条整表替换记录：
//
//	[100][5][varint id][varLenStr name][meta: pk/fields/indexes][varint rowCount][rows][CRC]
//
// 行区为 [i64 pk][varint len][data] 重复。
func (db *DB) writeCatalogAlterAll(meta *TableMeta, rowsBuf []byte, rowCount int) error {
	id := walRegRegister(meta.Name)
	buf := make([]byte, 0, 128+len(meta.Name)*3+len(rowsBuf))
	buf = append(buf, 100, 5)
	buf = appendVarUint(buf, uint64(id))
	buf = appendVarLenStr(buf, meta.Name)
	buf = appendVarLenStr(buf, meta.PK)
	buf = appendVarUint(buf, uint64(len(meta.Fields)))
	for _, f := range meta.Fields {
		buf = appendVarLenStr(buf, f.Name)
		buf = append(buf, byte(f.Type))
		buf = appendVarUint(buf, uint64(f.Len))
	}
	buf = appendVarUint(buf, uint64(len(meta.Indexes)))
	for idxName, fieldName := range meta.Indexes {
		buf = appendVarLenStr(buf, idxName)
		buf = appendVarLenStr(buf, fieldName)
	}
	buf = appendVarUint(buf, uint64(rowCount))
	buf = append(buf, rowsBuf...)
	if walCRC.Load() {
		buf = appendU32(buf, crc32.ChecksumIEEE(buf))
	}
	if err := appendWAL(buf); err != nil {
		return err
	}
	return db.flushWAL()
}

// applyCatalogAlterAllV2 整表替换：用新 meta 建新表对象，灌入行，原子换入。
func (db *DB) applyCatalogAlterAllV2(id uint32, meta *TableMeta, rows []alterRowBytes) {
	tbl := NewTable(meta, db.walFile)
	for _, r := range rows {
		tbl.pkTree.Set(r.pk, r.data)
	}
	tbl.rebuildIndexes()
	db.mu.Lock()
	_, existed := db.tables.Load(meta.Name)
	db.tables.Store(meta.Name, tbl)
	db.catalog[meta.Name] = meta
	if !existed {
		db.tablesCount.Add(1)
	}
	db.mu.Unlock()
	walRegRebind(id, meta.Name)
}

// alterTableAtomic 以单条 WAL 记录完成表结构重建（供 ALTER DROP/MODIFY 使用）。
func (db *DB) alterTableAtomic(name string, newMeta *TableMeta, transform func(oldMeta *TableMeta, row map[string]interface{}) map[string]interface{}) error {
	t := db.getTable(name)
	if t == nil {
		return fmt.Errorf("table not found: %s", name)
	}
	// 重建期间禁止对该表写入：否则扫描之后写入的行会从新表里消失。
	// 注意不要在这里持 db.mu：writeCatalogAlterAll 的 flushWAL 与
	// applyCatalogAlterAllV2 内部各自需要拿锁（RWMutex 不可重入）。
	t.migrating.Store(true)
	defer t.migrating.Store(false)

	rowsBuf, rows, err := encodeAlterAllRows(t, newMeta, transform)
	if err != nil {
		return err
	}
	if err := db.writeCatalogAlterAll(newMeta, rowsBuf, len(rows)); err != nil {
		return err
	}
	db.applyCatalogAlterAllV2(walRegRegister(name), newMeta, rows)
	logf(LOG_VERB, "table %s rebuilt: %d rows, %d columns", name, len(rows), len(newMeta.Fields))
	return nil
}
