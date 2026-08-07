package main

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// walTestConfig 构造一个隔离的测试配置（metrics 用临时端口避免 8080 冲突）。
func walTestConfig(dir string) *Config {
	return &Config{
		WALDir:              dir,
		WALFile:             "wal",
		PrivilegeFile:       "priv.json",
		User:                "root",
		Password:            "password",
		FlushInterval:       100 * time.Millisecond,
		GroupCommitInterval: 2 * time.Millisecond,
		TTLCleanInterval:    30 * time.Second,
		IdleTimeout:         5 * time.Minute,
		BackupDir:           filepath.Join(dir, "backup"),
		MetricsPort:         0,
		EnableChecksum:      true,
	}
}

func walTestMeta(name string) *TableMeta {
	return &TableMeta{
		Name:    name,
		PK:      "id",
		Fields:  []Field{{Name: "id", Type: TypeInt}, {Name: "name", Type: TypeVarchar, Len: 40}},
		Indexes: map[string]string{},
	}
}

func walMustCreate(t *testing.T, db *DB, name string) {
	t.Helper()
	meta := walTestMeta(name)
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.tables.Load(name); ok {
		return
	}
	tbl := NewTable(meta, db.walFile)
	db.tables.Store(name, tbl)
	db.tablesCount.Add(1)
	db.catalog[name] = meta
	if err := db.writeCatalog(meta); err != nil {
		t.Fatalf("writeCatalog: %v", err)
	}
}

func walFileVersion(t *testing.T, dir string) uint32 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if len(data) < len(WALMagic)+4 {
		t.Fatalf("wal too short")
	}
	return binary.BigEndian.Uint32(data[len(WALMagic):])
}

func walRowCount(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	tbl := db.getTable(name)
	if tbl == nil {
		return -1
	}
	return tbl.pkTree.Size()
}

// 写路径（INSERT/UPDATE/DELETE）均落 v1 紧凑编码（varint + 表 ID），版本号固定为 1。
func TestWALV1Header(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	walMustCreate(t, db, "users")
	tbl := db.getTable("users")
	if err := tbl.Insert(1, map[string]interface{}{"id": int64(1), "name": "Alice"}, 0, 0, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.flushWAL()
	db.Close()
	if v := walFileVersion(t, dir); v != WALVersion {
		t.Fatalf("WAL version = %d, want %d", v, WALVersion)
	}
}

// 完整往返：建表 → 插入 → 更新 → 删除 → 重建 → 重启恢复。
func TestWALV1RoundTrip(t *testing.T) {
	dir := t.TempDir()

	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	walMustCreate(t, db, "users")
	tbl := db.getTable("users")
	for i := int64(1); i <= 5; i++ {
		if err := tbl.Insert(i, map[string]interface{}{"id": i, "name": "u" + itoa(int(i))}, 0, 0, false); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tbl.Update(2, map[string]interface{}{"id": int64(2), "name": "UPDATED"}, 0, 0, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := tbl.Delete(3, 0, false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// 删除后重建同名校（验证注册表 ID 复用一致性）
	if err := db.dropTableByName("users"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	walMustCreate(t, db, "users")
	tbl2 := db.getTable("users")
	if err := tbl2.Insert(7, map[string]interface{}{"id": int64(7), "name": "new"}, 0, 0, false); err != nil {
		t.Fatalf("recreate insert: %v", err)
	}
	db.flushWAL()
	db.Close()

	// 重启恢复
	db2, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := walRowCount(t, db2, "users"); got != 1 {
		t.Fatalf("recovered row count = %d, want 1", got)
	}
	row, ok := db2.getTable("users").pkTree.Get(7)
	if !ok {
		t.Fatalf("row 7 missing after recovery")
	}
	dec, _, _ := decodeRow(walTestMeta("users"), row)
	if dec["name"] != "new" {
		t.Fatalf("row 7 name = %v, want new", dec["name"])
	}
}

// 事务：COMMIT 持久化，ROLLBACK 不持久化。
func TestWALV1Txn(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	walMustCreate(t, db, "t")
	tbl := db.getTable("t")

	tx1 := db.BeginTxn()
	db.txnMu.Lock()
	tx := db.txns[tx1]
	tx.Tables["t"] = true
	tx.Changes["t"] = make(map[int64][]byte)
	tx.ReadVersions["t"] = make(map[int64]int64)
	db.txnMu.Unlock()
	if err := tbl.Insert(1, map[string]interface{}{"id": int64(1), "name": "a"}, 0, tx1, false); err != nil {
		t.Fatalf("tx insert: %v", err)
	}
	if err := db.CommitTxn(tx1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tx2 := db.BeginTxn()
	db.txnMu.Lock()
	tx2r := db.txns[tx2]
	tx2r.Tables["t"] = true
	tx2r.Changes["t"] = make(map[int64][]byte)
	tx2r.ReadVersions["t"] = make(map[int64]int64)
	db.txnMu.Unlock()
	if err := tbl.Insert(2, map[string]interface{}{"id": int64(2), "name": "b"}, 0, tx2, false); err != nil {
		t.Fatalf("tx2 insert: %v", err)
	}
	if err := db.RollbackTxn(tx2); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	db.Close()

	db2, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := walRowCount(t, db2, "t"); got != 1 {
		t.Fatalf("recovered rows = %d, want 1 (rollback must not persist)", got)
	}
	if _, ok := db2.getTable("t").pkTree.Get(2); ok {
		t.Fatalf("rolled-back row 2 survived recovery")
	}
}

// Compact 后文件显著缩小，且重启后数据一致。
func TestCompactShrinks(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	walMustCreate(t, db, "big")
	tbl := db.getTable("big")
	// 大量写入制造膨胀（含重复覆盖）
	for i := int64(1); i <= 2000; i++ {
		if err := tbl.Insert(i, map[string]interface{}{"id": i, "name": "value-" + itoa(int(i))}, 0, 0, false); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// 覆盖一半、删除四分之一，制造大量死记录
	for i := int64(1); i <= 2000; i += 2 {
		if err := tbl.Update(i, map[string]interface{}{"id": i, "name": "updated-" + itoa(int(i))}, 0, 0, false); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	for i := int64(1); i <= 500; i++ {
		if err := tbl.Delete(i, 0, false); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	db.flushWAL()
	before := walFileSizeMB(db)
	if before == 0 {
		t.Fatalf("wal size 0")
	}
	if err := db.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	after := walFileSizeMB(db)
	if after >= before {
		t.Fatalf("compact did not shrink: before=%vMB after=%vMB", before, after)
	}
	// 压缩后仍可继续写入并重启恢复
	if err := tbl.Insert(9999, map[string]interface{}{"id": int64(9999), "name": "post-compact"}, 0, 0, false); err != nil {
		t.Fatalf("insert after compact: %v", err)
	}
	db.Close()

	db2, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("reopen after compact: %v", err)
	}
	defer db2.Close()
	// 期望：初始 2000 - 覆盖不增减 - 删除 500 = 1500，加 9999 = 1501
	if got := walRowCount(t, db2, "big"); got != 1501 {
		t.Fatalf("recovered rows = %d, want 1501", got)
	}
	row, ok := db2.getTable("big").pkTree.Get(9999)
	if !ok {
		t.Fatalf("post-compact row missing after recovery")
	}
	dec, _, _ := decodeRow(walTestMeta("big"), row)
	if dec["name"] != "post-compact" {
		t.Fatalf("post-compact name = %v", dec["name"])
	}
	// 已删除的主键不应复活
	if _, ok := db2.getTable("big").pkTree.Get(250); ok {
		t.Fatalf("deleted row 250 resurrected after compact+recovery")
	}
}

// 尾部残缺记录（崩溃残留）应被截断丢弃而非拒绝启动。
func TestWALV1TailTruncate(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	walMustCreate(t, db, "t")
	tbl := db.getTable("t")
	for i := int64(1); i <= 3; i++ {
		if err := tbl.Insert(i, map[string]interface{}{"id": i, "name": "x"}, 0, 0, false); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	db.flushWAL()
	db.Close()

	// 追加一条只写了前两字节的残缺 catalog 记录（模拟强杀）
	f, err := os.OpenFile(filepath.Join(dir, "wal"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	if _, err := f.Write([]byte{100, 1}); err != nil {
		t.Fatalf("append partial: %v", err)
	}
	f.Close()

	db2, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("reopen with tail truncation: %v", err)
	}
	defer db2.Close()
	if got := walRowCount(t, db2, "t"); got != 3 {
		t.Fatalf("recovered rows = %d, want 3", got)
	}
}

// 手工构造 v1 紧凑格式 WAL（版本号=1），验证字节级往返读取。
func TestWALV1Compat(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	meta := walTestMeta("legacy")
	const id = uint64(1)

	f, err := os.Create(walPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// header：magic + version=1 + reserved
	if _, err := f.Write([]byte(WALMagic)); err != nil {
		t.Fatalf("write magic: %v", err)
	}
	binary.Write(f, binary.BigEndian, uint32(1))
	binary.Write(f, binary.BigEndian, uint32(0))
	// catalog CREATE：[100][1][varint id][varlen name][varlen pk]...（尾部 CRC）
	cat := []byte{100, 1}
	cat = appendVarUint(cat, id)
	cat = appendVarLenStr(cat, meta.Name)
	cat = appendVarLenStr(cat, meta.PK)
	cat = appendVarUint(cat, uint64(len(meta.Fields)))
	for _, fld := range meta.Fields {
		cat = appendVarLenStr(cat, fld.Name)
		cat = append(cat, byte(fld.Type))
		cat = appendVarUint(cat, uint64(fld.Len))
	}
	cat = appendVarUint(cat, 0) // 无索引
	cat = appendU32(cat, crc32.ChecksumIEEE(cat))
	if _, err := f.Write(cat); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	// INSERT 记录：[2][varint id][varint txn][varint pk][varint len][data][crc]
	row := map[string]interface{}{"id": int64(42), "name": "legacy-row"}
	data := encodeRow(meta, row, 0, 0)
	for _, pk := range []int64{42, 43} {
		rec := []byte{WALInsert}
		rec = appendVarUint(rec, id)
		rec = appendVarUint(rec, 0) // 非事务
		rec = appendVarUint(rec, uint64(pk))
		rec = appendVarUint(rec, uint64(len(data)))
		rec = append(rec, data...)
		rec = appendU32(rec, crc32.ChecksumIEEE(rec))
		if _, err := f.Write(rec); err != nil {
			t.Fatalf("write insert: %v", err)
		}
	}
	f.Close()

	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB on v1 wal: %v", err)
	}
	defer db.Close()
	if got := walRowCount(t, db, "legacy"); got != 2 {
		t.Fatalf("v1 recovered rows = %d, want 2", got)
	}
	rowData, ok := db.getTable("legacy").pkTree.Get(42)
	if !ok {
		t.Fatalf("v1 row 42 missing")
	}
	dec, _, _ := decodeRow(meta, rowData)
	if dec["name"] != "legacy-row" {
		t.Fatalf("v1 row name = %v, want legacy-row", dec["name"])
	}
}
