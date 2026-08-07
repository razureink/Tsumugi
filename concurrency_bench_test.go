package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var benchSeq int64

func benchDB(t *testing.T, dir string, durability Durability) *DB {
	cfg := &Config{
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
		Durability:          durability,
	}
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	meta := &TableMeta{
		Name:    "test",
		PK:      "id",
		Fields:  []Field{{Name: "id", Type: TypeInt}, {Name: "val", Type: TypeVarchar, Len: 100}},
		Indexes: map[string]string{},
	}
	tbl := NewTable(meta, db.walFile)
	db.tables.Store("test", tbl)
	db.tablesCount.Add(1)
	db.catalog["test"] = meta
	if err := db.writeCatalog(meta); err != nil {
		t.Fatalf("writeCatalog: %v", err)
	}
	return db
}

func runConcurrent(db *DB, workers, tasks int, writePct int) (qps float64, p50, p99, p999, maxMs time.Duration, total int) {
	perWorker := make([][]time.Duration, workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			local := make([]time.Duration, 0, tasks)
			var tbl *Table
			v, _ := db.tables.Load("test")
			if v != nil {
				tbl = v.(*Table)
			}
			for i := 0; i < tasks; i++ {
				key := atomic.AddInt64(&benchSeq, 1)
				b := time.Now()
				if key%100 < int64(writePct) {
					row := map[string]interface{}{"id": key, "val": strconv.FormatInt(key, 10)}
					tbl.Insert(key, row, 0, 0, false)
				} else {
					tbl.SelectByPK(key)
				}
				local = append(local, time.Since(b))
			}
			perWorker[idx] = local
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	total = workers * tasks
	dur := make([]time.Duration, 0, total)
	for _, l := range perWorker {
		dur = append(dur, l...)
	}
	sort.Slice(dur, func(i, j int) bool { return dur[i] < dur[j] })
	pct := func(p float64) time.Duration {
		idx := int(float64(len(dur)) * p / 100)
		if idx >= len(dur) {
			idx = len(dur) - 1
		}
		return dur[idx]
	}
	return float64(len(dur)) / elapsed.Seconds(), pct(50), pct(99), pct(99.9), dur[len(dur)-1], len(dur)
}

func TestConcurrencyBench(t *testing.T) {
	for _, mode := range []Durability{DuraBatch, DuraFsync} {
		for _, wk := range []int{100, 1000} {
			dir := t.TempDir()
			db := benchDB(t, dir, mode)
			qps, p50, p99, p999, max, total := runConcurrent(db, wk, 100, 30)
			fmt.Printf("mode=%-5s workers=%-4d total=%-6d qps=%-10.0f p50=%-9v p99=%-9v p999=%-9v max=%-9v\n",
				mode, wk, total, qps, p50, p99, p999, max)
			db.Close()
		}
	}
	_ = os.Remove
}
