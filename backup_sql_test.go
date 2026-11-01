package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// ==================== 逻辑备份 / 恢复 / PITR ====================

// TestDumpRestoreRoundTrip 导出 → 关闭 → 新实例导入 → 逐值比对。
func TestDumpRestoreRoundTrip(t *testing.T) {
	SetLogMute(true)
	dirA := t.TempDir()
	db1, err := NewDB(walTestConfig(dirA))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}

	mustRun(t, db1, `CREATE TABLE dump_t (
		id INT PRIMARY KEY,
		n INT,
		s VARCHAR(64),
		b BOOL,
		d DATE,
		dt DATETIME,
		dec DECIMAL(10,2),
		tx TEXT,
		bl BLOB,
		KEY idx_s (s)
	)`)
	mustRun(t, db1, `INSERT INTO dump_t VALUES
		(1, -42, 'O''Brien', TRUE,  '2024-01-02', '2024-01-02 03:04:05', 12.34, 'plain', 'bin'),
		(2, 0, 'back\\slash', FALSE, '1970-01-01', '2024-06-07 08:09:10', -0.05, 'line1\nline2', 'x'),
		(3, 7, 'unicode-\u4e2d\u6587', TRUE, '2030-12-31', '2030-12-31 23:59:59', 999.99, '', '')`)
	mustRun(t, db1, `CREATE OR REPLACE VIEW dump_v AS SELECT id, n FROM dump_t WHERE n > 0`)

	wantRows := rowsOf(t, db1, `SELECT id, n, s, b, d, dt, dec, tx, bl FROM dump_t ORDER BY id`)
	wantIdx := rowsOf(t, db1, `SELECT id FROM dump_t WHERE s = 'O''Brien'`)
	wantView := rowsOf(t, db1, `SELECT id, n FROM dump_v ORDER BY id`)

	var dump bytes.Buffer
	if err := db1.DumpSQL(&dump); err != nil {
		t.Fatalf("DumpSQL: %v", err)
	}
	script := dump.String()
	if !strings.Contains(script, "CREATE TABLE dump_t") {
		t.Fatalf("dump 缺少建表语句:\n%s", script)
	}
	if !strings.Contains(script, "CREATE OR REPLACE VIEW dump_v") {
		t.Fatalf("dump 缺少视图语句:\n%s", script)
	}

	// 引擎同一进程只能有一个 DB 实例
	db1.Close()

	db2, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB db2: %v", err)
	}
	defer db2.Close()
	if err := db2.RestoreSQL(script); err != nil {
		t.Fatalf("RestoreSQL: %v\n--- dump ---\n%s", err, script)
	}

	gotRows := rowsOf(t, db2, `SELECT id, n, s, b, d, dt, dec, tx, bl FROM dump_t ORDER BY id`)
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("数据不一致\n got: %#v\nwant: %#v", gotRows, wantRows)
	}
	gotIdx := rowsOf(t, db2, `SELECT id FROM dump_t WHERE s = 'O''Brien'`)
	if !reflect.DeepEqual(gotIdx, wantIdx) {
		t.Fatalf("索引未正确重建: got %#v want %#v", gotIdx, wantIdx)
	}
	gotView := rowsOf(t, db2, `SELECT id, n FROM dump_v ORDER BY id`)
	if !reflect.DeepEqual(gotView, wantView) {
		t.Fatalf("视图不一致: got %#v want %#v", gotView, wantView)
	}
}

// TestDumpEscapingRoundTrip 专门打转义：引号、反斜杠、换行、NUL、Ctrl-Z。
func TestDumpEscapingRoundTrip(t *testing.T) {
	SetLogMute(true)
	db1, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	mustRun(t, db1, `CREATE TABLE esc (id INT PRIMARY KEY, s TEXT)`)
	special := "quote' back\\ nl\n cr\r tab\t ctrlz\x1a"
	mustRun(t, db1, `INSERT INTO esc VALUES (1, `+quoteSQLString(special)+`)`)
	want := rowsOf(t, db1, `SELECT s FROM esc`)

	var dump bytes.Buffer
	if err := db1.DumpSQL(&dump); err != nil {
		t.Fatalf("DumpSQL: %v", err)
	}
	db1.Close()
	db2, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db2.Close()
	if err := db2.RestoreSQL(dump.String()); err != nil {
		t.Fatalf("RestoreSQL: %v", err)
	}
	got := rowsOf(t, db2, `SELECT s FROM esc`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("转义往返失败: got %q want %q", got, want)
	}
}

// TestSplitSQLStatements 分号切分不能切到字符串或注释里。
func TestSplitSQLStatements(t *testing.T) {
	script := "INSERT INTO t VALUES (1, 'a;b');\n" +
		"INSERT INTO t VALUES (2, 'it''s; ok'); -- trailing ; comment\n" +
		"/* block ; comment */\n" +
		"SELECT \"x;y\";\n" +
		"SELECT `weird;col` FROM t"
	got := splitSQLStatements(script)
	if len(got) != 4 {
		t.Fatalf("切成 %d 条语句 want 4: %#v", len(got), got)
	}
	if !strings.Contains(got[1], "'it''s; ok'") {
		t.Fatalf("引号内分号被切开: %q", got[1])
	}
	if !strings.Contains(got[3], "`weird;col`") {
		t.Fatalf("反引号内分号被切开: %q", got[3])
	}
}

// TestRestorePointInTime 恢复到某个 WAL 偏移之前的状态。
func TestRestorePointInTime(t *testing.T) {
	SetLogMute(true)
	dirA := t.TempDir()
	cfg := walTestConfig(dirA)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	mustRun(t, db, `CREATE TABLE pitr_t (id INT PRIMARY KEY, v INT)`)
	mustRun(t, db, `INSERT INTO pitr_t VALUES (1, 10)`)
	mustRun(t, db, `INSERT INTO pitr_t VALUES (2, 20)`)

	pos, err := db.WALPosition()
	if err != nil {
		t.Fatalf("WALPosition: %v", err)
	}
	walPath := db.walFile.Name()

	mustRun(t, db, `INSERT INTO pitr_t VALUES (3, 30)`)
	db.Close()

	restored, err := RestorePointInTime(cfg, walPath, t.TempDir(), pos)
	if err != nil {
		t.Fatalf("RestorePointInTime: %v", err)
	}
	rows := rowsOf(t, restored, `SELECT id, v FROM pitr_t ORDER BY id`)
	want := [][]interface{}{{int64(1), int64(10)}, {int64(2), int64(20)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("PITR 结果=%#v want %#v（偏移之后的行不应存在）", rows, want)
	}
	// 引擎的 WAL 写协程是包级单例，必须先关掉这个实例再开下一个。
	restored.Close()

	// upTo<=0 表示全量恢复，应包含第 3 行
	full, err := RestorePointInTime(cfg, walPath, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("RestorePointInTime full: %v", err)
	}
	defer full.Close()
	if got := rowsOf(t, full, `SELECT COUNT(*) FROM pitr_t`); len(got) != 1 || got[0][0] != int64(3) {
		t.Fatalf("全量恢复行数=%v want 3", got)
	}
}

// TestRequireUnderDir 管理接口路径限制。
func TestRequireUnderDir(t *testing.T) {
	base := t.TempDir()
	if err := requireUnderDir(base, base); err != nil {
		t.Fatalf("base 本身应允许: %v", err)
	}
	if err := requireUnderDir(base, base+"/sub/dir"); err != nil {
		t.Fatalf("子目录应允许: %v", err)
	}
	if err := requireUnderDir(base, base+"/../escape"); err == nil {
		t.Fatal(".. 逃逸应被拒绝")
	}
	if err := requireUnderDir(base, t.TempDir()); err == nil {
		t.Fatal("完全不同的目录应被拒绝")
	}
}
