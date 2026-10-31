package main

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
)

// ==================== 端到端性能对比：cow / lock 两种并发模式 ====================
// 注意：WAL 通道是进程级单例，每个子基准必须独占一个 DB（顺序执行、及时关闭）。

// modeDB 按指定结构/模式建库并建表，返回库与表。
func modeDB(b *testing.B, kind, mode string, rows int) (*DB, *Table) {
	b.Helper()
	SetLogMute(true)
	cfg := walTestConfig(b.TempDir())
	cfg.TreeKind = kind
	cfg.TreeMode = mode
	db, err := NewDB(cfg)
	if err != nil {
		b.Fatalf("NewDB: %v", err)
	}
	// 用 b.Cleanup 而不是 defer：defer 会在计时区间内执行，把关库/WAL 落盘
	// 的开销算进 ns/op（样本很小时会严重失真）。
	b.Cleanup(db.Close)
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE m (id INT PRIMARY KEY, age INT, name VARCHAR(64), KEY idx_age (age))"); err != nil {
		db.Close()
		b.Fatalf("create: %v", err)
	}
	tbl := db.getTable("m")
	for i := 1; i <= rows; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf(
			"INSERT INTO m (id, age, name) VALUES (%d, %d, 'name-%d')", i, i%500, i)); err != nil {
			db.Close()
			b.Fatalf("seed %d: %v", i, err)
		}
	}
	return db, tbl
}

var modePairs = []struct{ kind, mode string }{
	{"llrb", "cow"},
	{"llrb", "lock"},
	{"btree", "lock"},
}

// BenchmarkE2E_EngineInsert 引擎层写入（含行编码 + WAL 入队）。
func BenchmarkE2E_EngineInsert(b *testing.B) {
	for _, mp := range modePairs {
		b.Run(mp.kind+"-"+mp.mode, func(b *testing.B) {
			_, tbl := modeDB(b, mp.kind, mp.mode, 0)
			if got := tbl.pkTree.Mode().String(); got != mp.mode {
				b.Fatalf("mode=%s want %s", got, mp.mode)
			}
			name := "value-0123456789"
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := tbl.InsertOrdered(int64(i+1), []interface{}{int64(i + 1), int64(i % 500), name}, 0, 0, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkE2E_SQLInsert SQL 全链路写入（解析 + 计划 + 执行 + WAL）。
func BenchmarkE2E_SQLInsert(b *testing.B) {
	for _, mp := range modePairs {
		b.Run(mp.kind+"-"+mp.mode, func(b *testing.B) {
			db, _ := modeDB(b, mp.kind, mp.mode, 0)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				q := "INSERT INTO m (id, age, name) VALUES (" + strconv.Itoa(i+1) + ", " +
					strconv.Itoa(i%500) + ", 'n')"
				if _, _, _, _, err := db.runSQL(q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkE2E_SQLPoint SQL 主键点查。
func BenchmarkE2E_SQLPoint(b *testing.B) {
	const rows = 20000
	for _, mp := range modePairs {
		b.Run(mp.kind+"-"+mp.mode, func(b *testing.B) {
			db, _ := modeDB(b, mp.kind, mp.mode, rows)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				q := "SELECT id, age, name FROM m WHERE id = " + strconv.Itoa(i%rows+1)
				if _, _, _, _, err := db.runSQL(q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkE2E_SQLRange SQL 主键范围扫描（LIMIT 100）。
func BenchmarkE2E_SQLRange(b *testing.B) {
	const rows = 20000
	for _, mp := range modePairs {
		b.Run(mp.kind+"-"+mp.mode, func(b *testing.B) {
			db, _ := modeDB(b, mp.kind, mp.mode, rows)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				q := "SELECT id FROM m WHERE id > " + strconv.Itoa(i%(rows-200)) + " LIMIT 100"
				if _, _, _, _, err := db.runSQL(q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkE2E_SQLIndexed SQL 二级索引范围查询（走索引下推）。
func BenchmarkE2E_SQLIndexed(b *testing.B) {
	const rows = 20000
	for _, mp := range modePairs {
		b.Run(mp.kind+"-"+mp.mode, func(b *testing.B) {
			db, _ := modeDB(b, mp.kind, mp.mode, rows)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				q := "SELECT id FROM m WHERE age > " + strconv.Itoa(490+i%9)
				if _, _, _, _, err := db.runSQL(q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkE2E_ParallelRead 纯并发读（8x 并行点查），检验读锁是否拖慢扩展性。
func BenchmarkE2E_ParallelRead(b *testing.B) {
	const rows = 20000
	for _, mp := range modePairs {
		b.Run(mp.kind+"-"+mp.mode, func(b *testing.B) {
			_, tbl := modeDB(b, mp.kind, mp.mode, rows)
			var seq atomic.Int64
			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					k := seq.Add(1)%rows + 1
					if _, ok := tbl.pkTree.Get(k); !ok {
						b.Fatal("miss")
					}
				}
			})
		})
	}
}

// BenchmarkE2E_ParallelMixed 并发混合负载：90% 读 + 10% 写（最接近真实业务）。
func BenchmarkE2E_ParallelMixed(b *testing.B) {
	const rows = 20000
	for _, mp := range modePairs {
		b.Run(mp.kind+"-"+mp.mode, func(b *testing.B) {
			_, tbl := modeDB(b, mp.kind, mp.mode, rows)
			var seq atomic.Int64
			val := []byte("row-payload-0123456789")
			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					n := seq.Add(1)
					if n%10 == 0 {
						tbl.pkTree.Set(n%rows+1, val)
					} else {
						if _, ok := tbl.pkTree.Get(n%rows + 1); !ok {
							b.Fatal("miss")
						}
					}
				}
			})
		})
	}
}
