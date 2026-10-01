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

// BenchmarkTxnCommitParallel 并行事务提交：观察 CommitTxn 全局锁 + 组提交等待的竞争
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
