package main

// ==================== 流式扫描算子测试 ====================
// 两条主线：
//   1. 正确性：流式结果必须与现有执行器逐行一致（列名/列序/值/顺序）；
//   2. 内存：5 万行下，流式扫描的堆增量必须显著小于物化路径。

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// streamTestDB 建一张三列表并灌入 n 行，返回库与表。
// 插入用 replay=true：测试只关心内存形态，跳过 WAL 落盘（也避免后续
// 内存测量窗口里被 WAL 刷盘协程的分配干扰）。
func streamTestDB(t *testing.T, n int) (*DB, *Table) {
	t.Helper()
	db, closeDB := openIndexTestDB(t)
	t.Cleanup(closeDB)
	mustExec(t, db, "CREATE TABLE st (id INT PRIMARY KEY, age INT, name VARCHAR(16))")
	tbl := db.getTable("st")
	if tbl == nil {
		t.Fatal("table st not found after CREATE TABLE")
	}
	for i := 1; i <= n; i++ {
		vals := []interface{}{int64(i), int64(i % 50), fmt.Sprintf("n-%d", i)}
		if err := tbl.InsertOrdered(int64(i), vals, 0, 0, true); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	return db, tbl
}

// streamCollect 把流式结果收成 [][]interface{}（每行 copy，因为算子复用缓冲）。
func streamCollect(db *DB, query string) ([]string, [][]interface{}, error) {
	var rows [][]interface{}
	cols, _, err := db.StreamSelectSQL(query, func(row []interface{}) error {
		cp := make([]interface{}, len(row))
		copy(cp, row)
		rows = append(rows, cp)
		return nil
	})
	return cols, rows, err
}

// TestStreamSelectMatchesEngine 用现有引擎做 oracle：同一批简单查询，
// 流式路径的列头、行数、行内容、行顺序都必须完全一致。
func TestStreamSelectMatchesEngine(t *testing.T) {
	db, _ := streamTestDB(t, 1000)

	queries := []string{
		"SELECT * FROM st",
		"SELECT id, name FROM st",
		"SELECT name, id FROM st LIMIT 7",
		"SELECT * FROM st WHERE age = 5",
		"SELECT id, age FROM st WHERE age >= 40",
		"SELECT id FROM st WHERE age > 10 AND age < 20",
		"SELECT id FROM st WHERE id = 42",
		"SELECT id FROM st WHERE id >= 995",
		"select ID, NAME from st limit 3",
	}
	for _, q := range queries {
		wantCols, wantRows, _, _, err := db.runSQL(q)
		if err != nil {
			t.Fatalf("%s: engine error: %v", q, err)
		}
		gotCols, gotRows, err := streamCollect(db, q)
		if err != nil {
			t.Fatalf("%s: stream error: %v", q, err)
		}
		if !reflect.DeepEqual(gotCols, wantCols) {
			t.Fatalf("%s: columns = %v, want %v", q, gotCols, wantCols)
		}
		if !equalRows(gotRows, wantRows) {
			t.Fatalf("%s: rows mismatch\n got (%d): %v\nwant (%d): %v",
				q, len(gotRows), previewRows(gotRows), len(wantRows), previewRows(wantRows))
		}
	}
	// 表别名 + 限定列名：现有 SQL 快路径不支持 AS（会报语法错误），
	// 但通用解析器支持，流式入口沿用它的语义。这里单独校验。
	cols, rows, err := streamCollect(db, "SELECT t.id, t.age FROM st AS t LIMIT 4")
	if err != nil {
		t.Fatalf("aliased select: %v", err)
	}
	if want := []string{"t.id", "t.age"}; !reflect.DeepEqual(cols, want) {
		t.Fatalf("aliased columns = %v, want %v", cols, want)
	}
	if len(rows) != 4 {
		t.Fatalf("aliased rows = %d, want 4", len(rows))
	}
	for i, r := range rows {
		// id 1..4，age = i%50，前 4 行两者相等
		if len(r) != 2 || r[0] != r[1] {
			t.Fatalf("aliased row %d = %v, want id == age", i, r)
		}
	}
}

// equalRows 逐行比较；把 nil 与空切片视为相同（引擎在空结果时两种形状都有）。
func equalRows(a, b [][]interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func previewRows(rows [][]interface{}) [][]interface{} {
	if len(rows) > 3 {
		return rows[:3]
	}
	return rows
}

// TestStreamScanDirect 直接调用算子（不经 SQL 解析）：覆盖列投影、范围条件、
// 主键区间、LIMIT 与 SELECT * 的恒等投影。
func TestStreamScanDirect(t *testing.T) {
	_, tbl := streamTestDB(t, 100)

	// 1) 窄投影 + 范围过滤 + LIMIT：按主键升序取前 5 行
	var ids []int64
	err := streamScan(tbl, []string{"id"}, nil,
		[]RangeCond{{Field: "age", Op: "ge", Val: int64(0)}},
		nil, nil, 5, func(row []interface{}) error {
			if len(row) != 1 {
				t.Fatalf("row width = %d, want 1", len(row))
			}
			ids = append(ids, row[0].(int64))
			return nil
		})
	if err != nil {
		t.Fatalf("streamScan: %v", err)
	}
	if want := []int64{1, 2, 3, 4, 5}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}

	// 2) minKey/maxKey 显式主键区间
	lo, hi := int64(10), int64(19)
	var rangeIDs []int64
	if err := streamScan(tbl, []string{"id"}, nil, nil, &lo, &hi, -1, func(row []interface{}) error {
		rangeIDs = append(rangeIDs, row[0].(int64))
		return nil
	}); err != nil {
		t.Fatalf("range streamScan: %v", err)
	}
	if len(rangeIDs) != 10 || rangeIDs[0] != 10 || rangeIDs[9] != 19 {
		t.Fatalf("range ids = %v, want 10..19", rangeIDs)
	}

	// 3) cols 为空 = SELECT *（恒等投影，宽度 = 字段数）
	var n int
	if err := streamScan(tbl, nil, nil, nil, nil, nil, -1, func(row []interface{}) error {
		n++
		if len(row) != 3 {
			t.Fatalf("SELECT * row width = %d, want 3", len(row))
		}
		return nil
	}); err != nil {
		t.Fatalf("star streamScan: %v", err)
	}
	if n != 100 {
		t.Fatalf("star rows = %d, want 100", n)
	}

	// 4) LIMIT 0：一行都不产出
	n = 0
	if err := streamScan(tbl, nil, nil, nil, nil, nil, 0, func(row []interface{}) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("limit 0: %v", err)
	}
	if n != 0 {
		t.Fatalf("limit 0 produced %d rows", n)
	}

	// 5) 主键点查 + 附加过滤：点查命中但过滤不通过时必须返回空集
	//    （点查分支也必须走 matchRow，不能只看主键）。
	err = streamScan(tbl, []string{"id"}, map[string]interface{}{"id": int64(7)},
		[]RangeCond{{Field: "age", Op: "lt", Val: int64(0)}},
		nil, nil, -1, func(row []interface{}) error {
			t.Fatalf("point lookup ignored the residual filter: %v", row)
			return nil
		})
	if err != nil {
		t.Fatalf("point lookup with filter: %v", err)
	}
}

// TestStreamScanDuplicateProjection SELECT id, id：decodeRowProjected 每个源字段
// 只能填一个输出位置，算子要把重复列补拷到其余位置。
func TestStreamScanDuplicateProjection(t *testing.T) {
	_, tbl := streamTestDB(t, 3)
	var rows [][]interface{}
	if err := streamScan(tbl, []string{"id", "name", "id"}, nil, nil, nil, nil, -1, func(row []interface{}) error {
		cp := make([]interface{}, len(row))
		copy(cp, row)
		rows = append(rows, cp)
		return nil
	}); err != nil {
		t.Fatalf("streamScan: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, r := range rows {
		if r[0] != r[2] {
			t.Fatalf("row %d: id columns differ: %v vs %v", i, r[0], r[2])
		}
		if _, ok := r[1].(string); !ok {
			t.Fatalf("row %d: middle column is not a string: %T", i, r[1])
		}
	}
}

// TestStreamScanSinkError sink 返回错误必须立刻中止并原样返回。
func TestStreamScanSinkError(t *testing.T) {
	_, tbl := streamTestDB(t, 50)
	stop := errors.New("downstream closed")
	visited := 0
	err := streamScan(tbl, []string{"id"}, nil, nil, nil, nil, -1, func(row []interface{}) error {
		visited++
		if visited == 5 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
	if visited != 5 {
		t.Fatalf("visited = %d, want 5 (scan must stop at the first sink error)", visited)
	}
}

// TestStreamScanRejectsUnknownColumn 未知列必须报错，而不是静默返回空集。
func TestStreamScanRejectsUnknownColumn(t *testing.T) {
	db, tbl := streamTestDB(t, 3)
	if err := streamScan(tbl, []string{"nope"}, nil, nil, nil, nil, -1, func(row []interface{}) error {
		return nil
	}); err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("err = %v, want unknown column error", err)
	}
	if _, _, err := db.StreamSelectSQL("SELECT id FROM st WHERE nope = 1", func(row []interface{}) error {
		return nil
	}); err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("WHERE unknown column err = %v, want unknown column error", err)
	}
}

// TestStreamSelectRejectsComplex 超出能力范围时必须返回 ErrNotSimpleSelect
// （调用方据此回落），而不是给出错误结果。
func TestStreamSelectRejectsComplex(t *testing.T) {
	db, _ := streamTestDB(t, 10)
	queries := []string{
		"SELECT DISTINCT id FROM st",
		"SELECT id FROM st ORDER BY id",
		"SELECT id FROM st LIMIT 1 OFFSET 2",
		"SELECT age + 1 FROM st",
		"SELECT COUNT(*) FROM st",
		"SELECT id FROM st WHERE age > 1 OR age < 3",
		"SELECT id FROM st WHERE age BETWEEN 1 AND 3",
		"SELECT id FROM st WHERE id IN (1, 2)",
		"SELECT 1",
		"INSERT INTO st (id, age, name) VALUES (999, 1, 'x')",
	}
	for _, q := range queries {
		_, _, err := db.StreamSelectSQL(q, func(row []interface{}) error { return nil })
		if !errors.Is(err, ErrNotSimpleSelect) {
			t.Fatalf("%s: err = %v, want ErrNotSimpleSelect", q, err)
		}
	}

	// 缺表是真错误，不是"走不了快路"，否则回落会把错误信息吞掉。
	if _, _, err := db.StreamSelectSQL("SELECT id FROM no_such_table", func(row []interface{}) error {
		return nil
	}); err == nil || errors.Is(err, ErrNotSimpleSelect) || !strings.Contains(err.Error(), "table not found") {
		t.Fatalf("missing table err = %v, want table not found", err)
	}

	// 虚拟表没有 pkTree：必须回落，而不是报 not found。
	if _, _, err := db.StreamSelectSQL("SELECT * FROM information_schema.tables", func(row []interface{}) error {
		return nil
	}); !errors.Is(err, ErrNotSimpleSelect) {
		t.Fatalf("virtual table err = %v, want ErrNotSimpleSelect", err)
	}
}

// TestStreamScanSkipsExpired 过期行不产出，且扫描结束后被清理（回调持读锁，
// 删除只能发生在锁外）。
func TestStreamScanSkipsExpired(t *testing.T) {
	db, closeDB := openIndexTestDB(t)
	t.Cleanup(closeDB)
	mustExec(t, db, "CREATE TABLE ex (id INT PRIMARY KEY, v INT)")
	tbl := db.getTable("ex")
	past := time.Now().Add(-time.Hour).UnixNano()
	for pk := int64(1); pk <= 3; pk++ {
		if err := tbl.InsertOrdered(pk, []interface{}{pk, pk * 10}, past, 0, true); err != nil {
			t.Fatalf("insert expired %d: %v", pk, err)
		}
	}
	if err := tbl.InsertOrdered(4, []interface{}{int64(4), int64(40)}, 0, 0, true); err != nil {
		t.Fatalf("insert live: %v", err)
	}
	var ids []int64
	if err := streamScan(tbl, []string{"id"}, nil, nil, nil, nil, -1, func(row []interface{}) error {
		ids = append(ids, row[0].(int64))
		return nil
	}); err != nil {
		t.Fatalf("streamScan: %v", err)
	}
	if want := []int64{4}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for pk := int64(1); pk <= 3; pk++ {
		if _, ok := tbl.pkTree.Get(pk); ok {
			t.Fatalf("expired pk %d was not deleted after scan", pk)
		}
	}
}

// ==================== 内存对比 ====================
// 关掉 GC 后执行 fn，HeapAlloc 只增不减，读到的增量就是"未回收的分配总量"，
// 即峰值上界（不会被后台回收掩盖）。TotalAlloc 同时返回做交叉验证。
//
// 为什么不用采样：ReadMemStats 每次都停世界，采样点太密会严重拖慢扫描、
// 反而改变内存行为；关 GC 的窗口足够短（一次扫描），数字稳定可复现。
func measureHeapAlloc(fn func()) (heapDelta, totalDelta uint64) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	prev := debug.SetGCPercent(-1)
	fn()
	runtime.ReadMemStats(&after)
	debug.SetGCPercent(prev)
	runtime.GC()
	if after.HeapAlloc > before.HeapAlloc {
		heapDelta = after.HeapAlloc - before.HeapAlloc
	}
	totalDelta = after.TotalAlloc - before.TotalAlloc
	return
}

// projectionFor 生成 selectRowsProjected 需要的 fieldToOut。
func projectionFor(meta *TableMeta, cols ...string) []int {
	fto := make([]int, len(meta.Fields))
	for i := range fto {
		fto[i] = -1
	}
	for out, c := range cols {
		if fi := meta.fieldIndexOf(c); fi >= 0 {
			fto[fi] = out
		}
	}
	return fto
}

// TestStreamScanHeapBounded 5 万行下，流式扫描的堆增量必须小于物化路径的 1/5。
//
// 物化侧用 selectRowsProjected（现有 SQL 快路径同款内核）：它把每行都 append
// 进 [][]interface{} 并持有 arena，直到函数返回；流式侧只保留一块行缓冲。
// 投影只取两个 INT 列，避免 varchar 装箱噪声主导比例。
func TestStreamScanHeapBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 50k-row heap comparison in -short mode")
	}
	const rows = 50000
	_, tbl := streamTestDB(t, rows)
	meta := tbl.Meta()
	fieldToOut := projectionFor(meta, "id", "age")

	// 物化路径
	matCount := 0
	matHeap, matTotal := measureHeapAlloc(func() {
		res := tbl.selectRowsProjected(nil, nil, -1, fieldToOut, 2)
		matCount = len(res)
	})
	if matCount != rows {
		t.Fatalf("materialized rows = %d, want %d", matCount, rows)
	}

	// 流式路径
	streamCount := 0
	var streamErr error
	streamHeap, streamTotal := measureHeapAlloc(func() {
		streamErr = streamScan(tbl, []string{"id", "age"}, nil, nil, nil, nil, -1,
			func(row []interface{}) error {
				streamCount++
				return nil
			})
	})
	if streamErr != nil {
		t.Fatalf("streamScan: %v", streamErr)
	}
	if streamCount != rows {
		t.Fatalf("streamed rows = %d, want %d", streamCount, rows)
	}

	t.Logf("materialized: HeapAlloc +%d B (%.2f MiB), TotalAlloc +%d B",
		matHeap, float64(matHeap)/(1<<20), matTotal)
	t.Logf("streaming:    HeapAlloc +%d B (%.2f KiB), TotalAlloc +%d B",
		streamHeap, float64(streamHeap)/1024, streamTotal)

	if matHeap < 1<<20 {
		t.Fatalf("materialized path should allocate >= 1 MiB for %d rows, got %d B", rows, matHeap)
	}
	if streamHeap*5 >= matHeap {
		t.Fatalf("streaming heap delta %d B is not < 1/5 of materialized %d B (ratio %.1fx)",
			streamHeap, matHeap, float64(matHeap)/float64(streamHeap))
	}
	if streamTotal*5 >= matTotal {
		t.Fatalf("streaming total alloc %d B is not < 1/5 of materialized %d B", streamTotal, matTotal)
	}
}

// ==================== 基准 ====================

func benchStreamTable(b *testing.B) *Table {
	b.Helper()
	SetLogMute(true)
	db, err := NewDB(walTestConfig(b.TempDir()))
	if err != nil {
		b.Fatalf("NewDB: %v", err)
	}
	b.Cleanup(func() { db.Close() })
	walMustCreate(b, db, "bench")
	tbl := db.getTable("bench")
	const n = 100000
	for i := 1; i <= n; i++ {
		if err := tbl.InsertOrdered(int64(i), []interface{}{int64(i), fmt.Sprintf("name-%d", i)}, 0, 0, true); err != nil {
			b.Fatalf("insert %d: %v", i, err)
		}
	}
	return tbl
}

// BenchmarkStreamScan 10 万行全表流式扫描（投影 id+name）。
// 关注 allocs/op：应与行数无关（每行只有值装箱，没有结果集累积）。
func BenchmarkStreamScan(b *testing.B) {
	tbl := benchStreamTable(b)
	var count int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count = 0
		if err := streamScan(tbl, []string{"id", "name"}, nil, nil, nil, nil, -1,
			func(row []interface{}) error {
				count++
				return nil
			}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if count != 100000 {
		b.Fatalf("scanned %d rows, want 100000", count)
	}
}

// BenchmarkStreamScanMaterialized 作为对照：同样的扫描 + 投影，但把结果
// 物化成 [][]interface{}（现有路径的行为）。
func BenchmarkStreamScanMaterialized(b *testing.B) {
	tbl := benchStreamTable(b)
	fieldToOut := projectionFor(tbl.Meta(), "id", "name")
	var count int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := tbl.selectRowsProjected(nil, nil, -1, fieldToOut, 2)
		count = len(res)
	}
	b.StopTimer()
	if count != 100000 {
		b.Fatalf("scanned %d rows, want 100000", count)
	}
}
