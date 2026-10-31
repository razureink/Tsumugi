package main

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// ==================== 引擎热路径基准 ====================
// 运行：go test -bench . -benchmem -run '^$'
// batch 模式（默认）下 WAL 为异步落盘，基准测的是引擎路径本身（编码 + 树操作 + 入队）。

func benchDB(b *testing.B) (*DB, *Table) {
	b.Helper()
	SetLogMute(true)
	db, err := NewDB(walTestConfig(b.TempDir()))
	if err != nil {
		b.Fatalf("NewDB: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	walMustCreate(b, db, "bench")
	tbl := db.getTable("bench")
	return db, tbl
}

func benchRow(i int64) map[string]interface{} {
	return map[string]interface{}{"id": i, "name": fmt.Sprintf("name-%d", i)}
}

// BenchmarkInsert 单协程顺序插入：编码 + SetNX + 索引维护 + WAL 入队
func BenchmarkInsert(b *testing.B) {
	_, tbl := benchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tbl.Insert(int64(i), benchRow(int64(i)), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInsertParallel 多协程并行插入（不同主键段）：树写互斥 + 计数器竞争
func BenchmarkInsertParallel(b *testing.B) {
	_, tbl := benchDB(b)
	var seq atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := seq.Add(1)
			if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkPointSelect 主键点查：Get + 解码
func BenchmarkPointSelect(b *testing.B) {
	_, tbl := benchDB(b)
	const n = 100000
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := tbl.SelectByPK(int64(i % n)); !ok {
			b.Fatal("row not found")
		}
	}
}

// BenchmarkPointSelectParallel 并行点查：COW 树读路径应完全无锁
func BenchmarkPointSelectParallel(b *testing.B) {
	_, tbl := benchDB(b)
	const n = 100000
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
	var seq atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := seq.Add(1)
			if _, ok := tbl.SelectByPK(i % n); !ok {
				b.Fatal("row not found")
			}
		}
	})
}

// BenchmarkRangeSelect 范围扫描（LIMIT 100）：树遍历 + 每行解码
func BenchmarkRangeSelect(b *testing.B) {
	_, tbl := benchDB(b)
	const n = 100000
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lo := int64(i % (n - 200))
		if _, err := tbl.SelectR(nil, nil, &lo, nil, 100); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUpdate 原地更新（版本号递增 + 全量重编码）
func BenchmarkUpdate(b *testing.B) {
	_, tbl := benchDB(b)
	const n = 10000
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tbl.Update(int64(i%n), benchRow(int64(i%n)), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTxnCommit 事务提交吞吐（串行）：BEGIN + 单行 INSERT + COMMIT
func BenchmarkTxnCommit(b *testing.B) {
	db, tbl := benchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := db.BeginTxn()
		if err := tbl.Insert(int64(i), benchRow(int64(i)), 0, id, false); err != nil {
			b.Fatal(err)
		}
		if err := db.CommitTxn(id); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTxnCommitParallel 并行事务提交：观察 batch 异步提交下 CommitTxn 的全局锁竞争
func BenchmarkTxnCommitParallel(b *testing.B) {
	db, tbl := benchDB(b)
	var seq atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := seq.Add(1)
			id := db.BeginTxn()
			if err := tbl.Insert(i, benchRow(i), 0, id, false); err != nil {
				b.Fatal(err)
			}
			if err := db.CommitTxn(id); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkStatsIncCmd 命令计数器开销（当前为分片互斥锁 + map）
func BenchmarkStatsIncCmd(b *testing.B) {
	s := NewStats()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.IncCmd("INSERT")
		}
	})
}

// BenchmarkSQLPointSelect 走完整 SQL 链路（分词 → 解析 → 权限 → 引擎）的点查，
// 量化 sql_parser 的分词/解析开销。
func BenchmarkSQLPointSelect(b *testing.B) {
	db, tbl := benchDB(b)
	const n = 100000
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, rows, _, _, err := db.runSQL(fmt.Sprintf("SELECT * FROM bench WHERE id = %d", i%n))
		if err != nil || len(rows) != 1 {
			b.Fatalf("rows=%v err=%v", rows, err)
		}
	}
}

// BenchmarkSQLRangeScan 走完整 SQL 链路的主键范围扫描（SELECT * ... LIMIT 100），
// 量化 selectRowsFlat 行结果的分配开销（rowArena 后备存储）。
func BenchmarkSQLRangeScan(b *testing.B) {
	db, tbl := benchDB(b)
	const n = 100000
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, rows, _, _, err := db.runSQL(fmt.Sprintf("SELECT * FROM bench WHERE id > %d LIMIT 100", i%(n-200)))
		if err != nil || len(rows) != 100 {
			b.Fatalf("rows=%d err=%v", len(rows), err)
		}
	}
}

// BenchmarkTokenizeSQL 分词器本身（含关键字内部化路径）
func BenchmarkTokenizeSQL(b *testing.B) {
	sql := "select id, name from bench where id = 12345 and name like 'name-1%' order by id desc limit 10"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		toks, err := tokenizeSQL(sql)
		if err != nil {
			b.Fatal(err)
		}
		putSQLTokens(toks)
	}
}

// BenchmarkFilteredScan 引擎级谓词过滤扫描：WHERE 非索引列 = 常量（1 万行命中 1 行）。
// 量化 SelectR 扫描路径的每行开销——flat 求值不应为被过滤行分配 map。
func BenchmarkFilteredScan(b *testing.B) {
	_, tbl := benchDB(b)
	const n = 10000
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := tbl.SelectR(map[string]interface{}{"name": fmt.Sprintf("name-%d", i%(n/2))}, nil, nil, nil, -1)
		if err != nil || len(rows) != 1 {
			b.Fatalf("rows=%d err=%v", len(rows), err)
		}
	}
}

// BenchmarkSQLFilteredScan 走完整 SQL 链路的非索引列等值过滤（全表扫描 + 投影）。
func BenchmarkSQLFilteredScan(b *testing.B) {
	db, tbl := benchDB(b)
	const n = 10000
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, rows, _, _, err := db.runSQL(fmt.Sprintf("SELECT * FROM bench WHERE name = 'name-%d'", i%(n/2)))
		if err != nil || len(rows) != 1 {
			b.Fatalf("rows=%d err=%v", len(rows), err)
		}
	}
}

// ==================== WAL 重放（启动加载）基准与正确性 ====================

// replayFixture 生成 n 行 WAL（compact 决定是否转成 v3 分帧格式），
// 返回配置供反复重开测量。调用方负责在后用完删除目录。
func replayFixture(b *testing.B, n int64, compact bool) (*Config, int64) {
	b.Helper()
	SetLogMute(true)
	dir := b.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		b.Fatalf("NewDB: %v", err)
	}
	walMustCreate(b, db, "bench")
	tbl := db.getTable("bench")
	for i := int64(0); i < n; i++ {
		if err := tbl.Insert(i, benchRow(i), 0, 0, false); err != nil {
			b.Fatalf("insert %d: %v", i, err)
		}
	}
	if compact {
		if err := db.Compact(); err != nil {
			b.Fatalf("compact: %v", err)
		}
	}
	db.Close()
	return cfg, n
}

// BenchmarkWALReplay 量化启动加载：重开数据库（重放整份 WAL）并校验行数。
func BenchmarkWALReplay(b *testing.B) {
	for _, tc := range []struct {
		name    string
		compact bool
	}{{"v2-unframed", false}, {"v3-framed", true}} {
		b.Run(tc.name, func(b *testing.B) {
			const n = 50000
			cfg, want := replayFixture(b, n, tc.compact)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				db, err := NewDB(cfg)
				if err != nil {
					b.Fatalf("reopen: %v", err)
				}
				tbl := db.getTable("bench")
				if tbl == nil {
					db.Close()
					b.Fatal("table missing after replay")
				}
				if got := tbl.pkTree.Size(); got != want {
					db.Close()
					b.Fatalf("rows after replay = %d, want %d", got, want)
				}
				db.Close()
			}
		})
	}
}

// TestWALReplayIntegrity 校验段式并行重放的数据完整性：
// 混合 insert/update/delete + 事务提交，重开后逐行比对最终值。
func TestWALReplayIntegrity(t *testing.T) {
	SetLogMute(true)
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE TABLE ri (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	const n = 4000
	// 主键从 1 开始：0 在主键插入里等价于"自动分配"，会与后续显式 1 撞键。
	for i := 1; i <= n; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("INSERT INTO ri (id, v) VALUES (%d, %d)", i, i*2)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// 覆盖写
	for i := 1; i <= n; i += 3 {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("UPDATE ri SET v = %d WHERE id = %d", i*2+1, i)); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	// 删除
	for i := 1; i <= n; i += 7 {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("DELETE FROM ri WHERE id = %d", i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	// 事务内写入
	if _, _, _, _, err := db.runSQL("BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := n + 1; i <= n+100; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("INSERT INTO ri (id, v) VALUES (%d, %d)", i, -i)); err != nil {
			t.Fatalf("txn insert %d: %v", i, err)
		}
	}
	if _, _, _, _, err := db.runSQL("COMMIT"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 期望值
	want := map[int64]int64{}
	for i := 1; i <= n; i++ {
		want[int64(i)] = int64(i * 2)
	}
	for i := 1; i <= n; i += 3 {
		want[int64(i)] = int64(i*2 + 1)
	}
	for i := 1; i <= n; i += 7 {
		delete(want, int64(i))
	}
	for i := n + 1; i <= n+100; i++ {
		want[int64(i)] = int64(-i)
	}
	db.Close()

	// 重开并逐行比对
	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	tbl := db2.getTable("ri")
	if tbl == nil {
		t.Fatal("table ri missing after replay")
	}
	if got, wantN := tbl.pkTree.Size(), int64(len(want)); got != wantN {
		t.Fatalf("row count after replay = %d, want %d", got, wantN)
	}
	for id, v := range want {
		row, ok := tbl.SelectByPK(id)
		if !ok {
			t.Fatalf("row %d missing after replay", id)
		}
		if got := row["v"]; got != v {
			t.Fatalf("row %d v = %v, want %v", id, got, v)
		}
	}
}
