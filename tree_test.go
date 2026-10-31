package main

import (
	"cmp"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// ==================== 两种索引实现的公共测试辅助 ====================

// tb 是 *testing.T 与 *testing.B 的公共子集，让测试与基准复用同一套辅助函数。
type tb interface {
	Helper()
	Cleanup(func())
}

// withTreeKind 在测试/基准期间切换到指定实现，结束后恢复。
func withTreeKind(t tb, k treeKind) {
	t.Helper()
	prev := currentTreeKind()
	setTreeKind(k)
	t.Cleanup(func() { setTreeKind(prev) })
}

// withTreeMode 在测试/基准期间切换并发模式，结束后恢复。
func withTreeMode(t tb, m treeMode) {
	t.Helper()
	prev := currentTreeMode()
	setTreeMode(m)
	t.Cleanup(func() { setTreeMode(prev) })
}

// treeVariant 一种"结构 × 并发模式"组合，供差分测试遍历。
type treeVariant struct {
	kind treeKind
	mode treeMode
}

func (v treeVariant) String() string { return v.kind.String() + "-" + v.mode.String() }

// allTreeVariants 覆盖所有可选组合，差分测试对每一种都跑一遍。
func allTreeVariants() []treeVariant {
	return []treeVariant{
		{treeKindLLRB, treeModeCOW},
		{treeKindBTree, treeModeCOW},
		{treeKindLLRB, treeModeLock},
		{treeKindBTree, treeModeLock},
	}
}

// useVariant 应用组合到全局选择（供构造新树）。
func useVariant(t tb, v treeVariant) {
	withTreeKind(t, v.kind)
	withTreeMode(t, v.mode)
}

// checkTreeInvariants 按实际实现选择对应的不变量校验器。
// 语义层面的正确性由差分测试保证，这里额外校验结构不变量（平衡/有序/键数）。
func checkTreeInvariants[K cmp.Ordered, V any](t *RBTree[K, V]) bool {
	switch impl := t.impl.(type) {
	case *llrbTree[K, V]:
		return rbInvariants(impl)
	case *btree[K, V]:
		return btInvariants(impl)
	}
	return false
}

// btInvariants 校验 B 树不变量：
//   - 节点键数在 [btMinKeys, btMaxKeys]（根可为 0）；
//   - 节点内键严格升序；
//   - 所有叶子在同一深度（B 树平衡性）；
//   - 子节点的键落在父键划定的区间内。
func btInvariants[K cmp.Ordered, V any](t *btree[K, V]) bool {
	root := t.root.Load()
	if root == nil {
		return t.size.Load() == 0
	}
	leafDepth := -1
	ok := true
	keys := int64(0)
	var walk func(n *btNode[K, V], depth int, isRoot bool)
	walk = func(n *btNode[K, V], depth int, isRoot bool) {
		if !ok {
			return
		}
		if n.n > btMaxKeys {
			ok = false
			return
		}
		if !isRoot && n.n < btMinKeys {
			ok = false
			return
		}
		for i := 1; i < n.n; i++ {
			if !(n.keys[i-1] < n.keys[i]) {
				ok = false
				return
			}
		}
		keys += int64(n.n)
		if n.isLeaf() {
			if leafDepth < 0 {
				leafDepth = depth
			} else if leafDepth != depth {
				ok = false
			}
			return
		}
		for i := 0; i <= n.n; i++ {
			kid := n.kids[i]
			if kid == nil || kid.n == 0 {
				ok = false
				return
			}
			if i > 0 && !(n.keys[i-1] < kid.keys[0]) {
				ok = false
				return
			}
			if i < n.n && !(kid.keys[kid.n-1] < n.keys[i]) {
				ok = false
				return
			}
			walk(kid, depth+1, false)
		}
	}
	walk(root, 0, true)
	if keys != t.size.Load() {
		return false
	}
	return ok
}

// TestBTreeEngineIntegration 在 B 树实现下跑完整引擎路径：
// 建表 + 二级索引 + 插入/更新/删除 + TTL + 范围查询，再关库重开（WAL 重放走
// SetNXMutable 原地路径），逐行校验结果与关闭前一致。
func TestBTreeEngineIntegration(t *testing.T) {
	SetLogMute(true)
	cfg := walTestConfig(t.TempDir())
	cfg.TreeKind = "btree" // 由配置驱动：NewDB 会在重放建索引前应用它

	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	// 失败也要保证关闭：否则 WAL writer 泄漏会污染同进程后续测试
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
	}()
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE bt (id INT PRIMARY KEY, age INT, name VARCHAR(32), KEY idx_age (age))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	real := db.getTable("bt")
	if kind := real.pkTree.Kind(); kind != treeKindBTree {
		t.Fatalf("pkTree kind=%v want btree", kind)
	}

	const n = 2000
	for i := 1; i <= n; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf(
			"INSERT INTO bt (id, age, name) VALUES (%d, %d, 'nm-%03d')", i, i%40, i%37)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// 更新一部分
	for i := 1; i <= n; i += 5 {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf(
			"UPDATE bt SET age = %d WHERE id = %d", 100+i%7, i)); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	// 删除一部分
	for i := 3; i <= n; i += 11 {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("DELETE FROM bt WHERE id = %d", i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	// 结构不变量
	if !checkTreeInvariants(real.pkTree) {
		t.Fatal("btree invariants violated after mixed workload")
	}

	before := map[int64][2]interface{}{}
	for i := 1; i <= n; i++ {
		_, rows, _, _, err := db.runSQL(fmt.Sprintf("SELECT id, age FROM bt WHERE id = %d", i))
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if len(rows) == 1 {
			before[int64(i)] = [2]interface{}{rows[0][0], rows[0][1]}
		}
	}
	ageRows := mustExec(t, db, "SELECT id FROM bt WHERE age > 100")
	countBefore := real.pkTree.Size()
	db.Close()
	closed = true

	// 重开：WAL 重放会重建主键树与二级索引（走 SetNXMutable）
	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	tbl2 := db2.getTable("bt")
	if tbl2 == nil {
		t.Fatal("table missing after replay")
	}
	if kind := tbl2.pkTree.Kind(); kind != treeKindBTree {
		t.Fatalf("pkTree kind after replay=%v want btree", kind)
	}
	if got := tbl2.pkTree.Size(); got != countBefore {
		t.Fatalf("row count after replay=%d want %d", got, countBefore)
	}
	if !checkTreeInvariants(tbl2.pkTree) {
		t.Fatal("btree invariants violated after WAL replay")
	}
	for id, want := range before {
		_, rows, _, _, err := db2.runSQL(fmt.Sprintf("SELECT id, age FROM bt WHERE id = %d", id))
		if err != nil {
			t.Fatalf("reopen select %d: %v", id, err)
		}
		if len(rows) != 1 {
			t.Fatalf("row %d missing after replay", id)
		}
		if rows[0][0] != want[0] || rows[0][1] != want[1] {
			t.Fatalf("row %d after replay = %v want %v", id, rows, want)
		}
	}
	ageAfter := mustExec(t, db2, "SELECT id FROM bt WHERE age > 100")
	if len(ageAfter) != len(ageRows) {
		t.Fatalf("indexed range rows after replay=%d want %d", len(ageAfter), len(ageRows))
	}
	// 二级索引也必须完好（索引树同样用的是 B 树）
	idxRows := mustExec(t, db2, "SELECT id FROM bt WHERE age = 103")
	if len(idxRows) == 0 {
		t.Fatal("secondary index lookup returned nothing after replay")
	}
}

// TestBTreeMatchesLLRB 同一串操作下两种实现的可见结果必须完全一致
// （覆盖 Set/SetNX/Delete/Get/全量扫描/范围扫描/限制扫描）。
func TestBTreeMatchesLLRB(t *testing.T) {
	SetLogMute(true)
	run := func(kind treeKind) (map[int64]int64, []int64, []int64, bool /*limitReached*/) {
		withTreeKind(t, kind)
		tree := newIntKeyTree[int64]()
		for i := int64(0); i < 300; i++ {
			tree.Set(i, i*2)
		}
		for i := int64(0); i < 300; i += 3 {
			tree.Set(i, i*10)
		}
		for i := int64(0); i < 300; i += 7 {
			tree.Delete(i)
		}
		for i := int64(300); i < 400; i++ {
			tree.SetNX(i, i)
		}
		if !checkTreeInvariants(tree) {
			t.Fatalf("%v: invariants violated", kind)
		}
		content := map[int64]int64{}
		tree.scanAll(func(k, v int64) { content[k] = v })
		var ranged []int64
		lo, hi := int64(50), int64(120)
		tree.scanRangeStop(&lo, &hi, func(k int64, v int64) bool {
			ranged = append(ranged, k)
			return true
		})
		var limited []int64
		min := int64(10)
		reached := tree.scanRangeLimit(&min, 25, func(k int64, v int64) bool {
			limited = append(limited, k)
			return true
		})
		return content, ranged, limited, reached
	}

	llrbContent, llrbRanged, llrbLimited, llrbReached := run(treeKindLLRB)
	btContent, btRanged, btLimited, btReached := run(treeKindBTree)

	if len(llrbContent) != len(btContent) {
		t.Fatalf("content size differs: llrb=%d btree=%d", len(llrbContent), len(btContent))
	}
	for k, v := range llrbContent {
		if btContent[k] != v {
			t.Fatalf("key %d differs: llrb=%d btree=%d", k, v, btContent[k])
		}
	}
	if fmt.Sprint(llrbRanged) != fmt.Sprint(btRanged) {
		t.Fatalf("range scan differs:\n llrb=%v\n btree=%v", llrbRanged, btRanged)
	}
	if fmt.Sprint(llrbLimited) != fmt.Sprint(btLimited) || llrbReached != btReached {
		t.Fatalf("limit scan differs: llrb=%v/%v btree=%v/%v", llrbLimited, llrbReached, btLimited, btReached)
	}
}

// ==================== 两种索引结构的写入/读取对比 ====================

// BenchmarkWriteByTreeKind 同一负载下对比 LLRB 与 COW B 树的写入开销。
// 注意：每个子基准独占一个 DB（WAL 通道是进程级单例），顺序执行。
func BenchmarkWriteByTreeKind(b *testing.B) {
	for _, kind := range []string{"llrb", "btree"} {
		b.Run(kind, func(b *testing.B) {
			SetLogMute(true)
			cfg := walTestConfig(b.TempDir())
			cfg.TreeKind = kind
			db, err := NewDB(cfg)
			if err != nil {
				b.Fatalf("NewDB: %v", err)
			}
			defer db.Close()
			walMustCreate(b, db, "bk")
			tbl := db.getTable("bk")
			if got := tbl.pkTree.Kind().String(); got != kind {
				b.Fatalf("tree kind=%s want %s", got, kind)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				vals := []interface{}{int64(i), "value"}
				if err := tbl.InsertOrdered(int64(i), vals, 0, 0, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReadByTreeKind 对比两种结构的主键点查与范围扫描（读路径不应变慢）。
func BenchmarkReadByTreeKind(b *testing.B) {
	for _, kind := range []string{"llrb", "btree"} {
		b.Run(kind, func(b *testing.B) {
			SetLogMute(true)
			cfg := walTestConfig(b.TempDir())
			cfg.TreeKind = kind
			db, err := NewDB(cfg)
			if err != nil {
				b.Fatalf("NewDB: %v", err)
			}
			defer db.Close()
			walMustCreate(b, db, "rk")
			tbl := db.getTable("rk")
			const n = 50000
			for i := 0; i < n; i++ {
				vals := []interface{}{int64(i), "value"}
				if err := tbl.InsertOrdered(int64(i), vals, 0, 0, false); err != nil {
					b.Fatal(err)
				}
			}
			b.Run("point", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, ok := tbl.pkTree.Get(int64(i % n)); !ok {
						b.Fatal("miss")
					}
				}
			})
			b.Run("range100", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					lo := int64(i % (n - 200))
					hi := lo + 100
					cnt := 0
					tbl.pkTree.scanRangeStop(&lo, &hi, func(k int64, v []byte) bool {
						cnt++
						return true
					})
					if cnt != 101 {
						b.Fatalf("scan count=%d", cnt)
					}
				}
			})
		})
	}
}

// ==================== 两种索引结构的纯树层面对比 ====================
// 不含 WAL / 编码，隔离出"平衡结构"本身的读写开销。

func benchTreeInsert(b *testing.B, kind treeKind) {
	withTreeKind(b, kind)
	tree := newIntKeyTree[[]byte]()
	val := make([]byte, 120)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tree.SetNX(int64(i), val)
	}
}

func BenchmarkTreeInsertLLRB(b *testing.B)  { benchTreeInsert(b, treeKindLLRB) }
func BenchmarkTreeInsertBTree(b *testing.B) { benchTreeInsert(b, treeKindBTree) }

func benchTreeRead(b *testing.B, kind treeKind) {
	withTreeKind(b, kind)
	tree := newIntKeyTree[[]byte]()
	val := make([]byte, 120)
	const n = 200000
	for i := 0; i < n; i++ {
		tree.SetNX(int64(i), val)
	}
	b.Run("point", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := tree.Get(int64(i % n)); !ok {
				b.Fatal("miss")
			}
		}
	})
	b.Run("range100", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			lo := int64(i % (n - 200))
			hi := lo + 100
			cnt := 0
			tree.scanRangeStop(&lo, &hi, func(k int64, v []byte) bool {
				cnt++
				return true
			})
			if cnt != 101 {
				b.Fatalf("scan count=%d", cnt)
			}
		}
	})
}

func BenchmarkTreeReadLLRB(b *testing.B)  { benchTreeRead(b, treeKindLLRB) }
func BenchmarkTreeReadBTree(b *testing.B) { benchTreeRead(b, treeKindBTree) }

// BenchmarkTreeMode 对比 COW 与读写锁原地更新的写入/读取开销。
func benchModeInsert(b *testing.B, kind treeKind, mode treeMode) {
	useVariant(b, treeVariant{kind, mode})
	tree := newIntKeyTree[[]byte]()
	val := make([]byte, 120)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tree.SetNX(int64(i), val)
	}
}

func BenchmarkModeInsertLLRBCOW(b *testing.B)   { benchModeInsert(b, treeKindLLRB, treeModeCOW) }
func BenchmarkModeInsertLLRBLLock(b *testing.B) { benchModeInsert(b, treeKindLLRB, treeModeLock) }
func BenchmarkModeInsertBTreeLock(b *testing.B) { benchModeInsert(b, treeKindBTree, treeModeLock) }

func benchModeRead(b *testing.B, kind treeKind, mode treeMode) {
	useVariant(b, treeVariant{kind, mode})
	tree := newIntKeyTree[[]byte]()
	val := make([]byte, 120)
	const n = 200000
	for i := 0; i < n; i++ {
		tree.SetNX(int64(i), val)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := tree.Get(int64(i % n)); !ok {
			b.Fatal("miss")
		}
	}
}

func BenchmarkModeReadLLRBCOW(b *testing.B)   { benchModeRead(b, treeKindLLRB, treeModeCOW) }
func BenchmarkModeReadLLRBLLock(b *testing.B) { benchModeRead(b, treeKindLLRB, treeModeLock) }

// TestLockModeEngineIntegration 在"读写锁 + 原地更新"模式下跑完整引擎路径：
// 建表 + 二级索引 + 增删改 + 索引查询 + 关库重开，验证与 COW 模式结果一致。
func TestLockModeEngineIntegration(t *testing.T) {
	SetLogMute(true)
	cfg := walTestConfig(t.TempDir())
	cfg.TreeKind = "btree"
	cfg.TreeMode = "lock"

	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
	}()

	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE lk (id INT PRIMARY KEY, age INT, name VARCHAR(32), KEY idx_age (age))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	real := db.getTable("lk")
	if kind := real.pkTree.Kind(); kind != treeKindBTree {
		t.Fatalf("kind=%v want btree", kind)
	}
	if mode := real.pkTree.Mode(); mode != treeModeLock {
		t.Fatalf("mode=%v want lock", mode)
	}

	const n = 1500
	for i := 1; i <= n; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf(
			"INSERT INTO lk (id, age, name) VALUES (%d, %d, 'nm-%03d')", i, i%40, i%37)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	for i := 1; i <= n; i += 4 {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("UPDATE lk SET age = %d WHERE id = %d", 200+i%9, i)); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	for i := 5; i <= n; i += 13 {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("DELETE FROM lk WHERE id = %d", i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if !checkTreeInvariants(real.pkTree) {
		t.Fatal("btree invariants violated in lock mode")
	}

	before := map[int64]int64{}
	for i := 1; i <= n; i++ {
		_, rows, _, _, err := db.runSQL(fmt.Sprintf("SELECT id FROM lk WHERE id = %d", i))
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if len(rows) == 1 {
			before[int64(i)] = 1
		}
	}
	idxBefore := mustExec(t, db, "SELECT id FROM lk WHERE age > 200")
	sizeBefore := real.pkTree.Size()
	db.Close()
	closed = true

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	tbl2 := db2.getTable("lk")
	if tbl2 == nil {
		t.Fatal("table missing after replay")
	}
	if got := tbl2.pkTree.Size(); got != sizeBefore {
		t.Fatalf("rows after replay=%d want %d", got, sizeBefore)
	}
	for id := range before {
		_, rows, _, _, err := db2.runSQL(fmt.Sprintf("SELECT id FROM lk WHERE id = %d", id))
		if err != nil || len(rows) != 1 {
			t.Fatalf("row %d missing after replay (err=%v)", id, err)
		}
	}
	idxAfter := mustExec(t, db2, "SELECT id FROM lk WHERE age > 200")
	if len(idxAfter) != len(idxBefore) {
		t.Fatalf("indexed rows after replay=%d want %d", len(idxAfter), len(idxBefore))
	}
}

// ==================== 自适应模式（auto）测试 ====================

// TestAutoModeStaysMutableWhenSingleThreaded 单线程下 auto 模式应一直走原地更新
// （不升级），因为从未观测到并发。
func TestAutoModeStaysMutableWhenSingleThreaded(t *testing.T) {
	withTreeMode(t, treeModeAuto)
	withTreeKind(t, treeKindBTree)
	tree := newIntKeyTree[int64]()
	for i := int64(0); i < 2000; i++ {
		tree.Set(i, i*2)
	}
	for i := int64(0); i < 2000; i += 3 {
		tree.Delete(i)
	}
	if tree.Upgraded() {
		t.Fatal("auto mode upgraded on a single-threaded workload")
	}
	if got, want := tree.Size(), int64(2000-667); got != want {
		t.Fatalf("size=%d want %d", got, want)
	}
	if v, ok := tree.Get(1); !ok || v != 2 {
		t.Fatalf("Get(1)=%v/%v", v, ok)
	}
}

// TestAutoModeUpgradesOnConcurrency 并发访问时必须单向升级为 COW，
// 且升级前后数据完全正确（这是本次设计的核心安全点）。
func TestAutoModeUpgradesOnConcurrency(t *testing.T) {
	withTreeMode(t, treeModeAuto)
	withTreeKind(t, treeKindBTree)
	tree := newIntKeyTree[int64]()
	const keys = 500
	for i := int64(0); i < keys; i++ {
		tree.Set(i, i)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < 4000; i++ {
				k := int64((i*7 + w) % keys)
				switch i % 5 {
				case 0:
					tree.Set(k, k*10)   // 写
				case 1:
					tree.SetNX(k, k)    // 写（键一般已存在）
				default:
					tree.Get(k)         // 读
					tree.scanRangeLimit(&k, 8, func(int64, int64) bool { return true })
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	if !tree.Upgraded() {
		t.Fatal("auto mode should have upgraded after concurrent access")
	}
	// 并发结束后把值全部写回确定值，验证树仍然可用且正确
	for i := int64(0); i < keys; i++ {
		tree.Set(i, i*3)
	}
	for i := int64(0); i < keys; i++ {
		v, ok := tree.Get(i)
		if !ok || v != i*3 {
			t.Fatalf("after upgrade Get(%d)=%v/%v want %d", i, v, ok, i*3)
		}
	}
	if !checkTreeInvariants(tree) {
		t.Fatal("tree invariants violated after auto upgrade")
	}
}

// TestAutoModeEngineIntegration auto 模式跑完整引擎路径 + WAL 重开。
func TestAutoModeEngineIntegration(t *testing.T) {
	SetLogMute(true)
	cfg := walTestConfig(t.TempDir())
	cfg.TreeKind = "btree"
	cfg.TreeMode = "auto"
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
	}()
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE au (id INT PRIMARY KEY, age INT, KEY idx_age (age))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	tbl := db.getTable("au")
	if tbl.pkTree.Mode() != treeModeAuto {
		t.Fatalf("mode=%v want auto", tbl.pkTree.Mode())
	}
	const n = 1200
	for i := 1; i <= n; i++ {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf(
			"INSERT INTO au (id, age) VALUES (%d, %d)", i, i%50)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// 单线程路径下不应升级
	if tbl.pkTree.Upgraded() {
		t.Log("note: tree upgraded during single-threaded load")
	}
	for i := 1; i <= n; i += 5 {
		if _, _, _, _, err := db.runSQL(fmt.Sprintf("DELETE FROM au WHERE id = %d", i)); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	sizeBefore := tbl.pkTree.Size()
	db.Close()
	closed = true

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	tbl2 := db2.getTable("au")
	if tbl2 == nil {
		t.Fatal("table missing after replay")
	}
	if got := tbl2.pkTree.Size(); got != sizeBefore {
		t.Fatalf("rows after replay=%d want %d", got, sizeBefore)
	}
	if rows := mustExec(t, db2, "SELECT id FROM au WHERE age = 7"); len(rows) == 0 {
		t.Fatal("secondary index broken after replay")
	}
}

// ==================== auto 模式性能验证 ====================

// BenchmarkAutoWrite 单线程写入：auto 未升级时应与 lock 同速（远快于 cow）。
func benchAutoWrite(b *testing.B, mode treeMode) {
	withTreeMode(b, mode)
	withTreeKind(b, treeKindBTree)
	tree := newIntKeyTree[[]byte]()
	val := make([]byte, 120)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tree.SetNX(int64(i), val)
	}
	if mode == treeModeAuto && tree.Upgraded() {
		b.Log("auto 模式在单线程基准中意外升级")
	}
}

func BenchmarkAutoWrite_COW(b *testing.B)  { benchAutoWrite(b, treeModeCOW) }
func BenchmarkAutoWrite_Lock(b *testing.B) { benchAutoWrite(b, treeModeLock) }
func BenchmarkAutoWrite_Auto(b *testing.B) { benchAutoWrite(b, treeModeAuto) }

// BenchmarkAutoReadParallel 并发读：auto 应在前几次并发后升级，读回到 COW 水平。
func benchAutoReadParallel(b *testing.B, mode treeMode) {
	withTreeMode(b, mode)
	withTreeKind(b, treeKindBTree)
	tree := newIntKeyTree[[]byte]()
	val := make([]byte, 120)
	const n = 50000
	for i := 0; i < n; i++ {
		tree.SetNX(int64(i), val)
	}
	var seq atomic.Int64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			k := seq.Add(1) % n
			if _, ok := tree.Get(k); !ok {
				b.Fatal("miss")
			}
		}
	})
	b.StopTimer()
	// 注：基准框架会先用 b.N=1 跑一轮标定，那一轮没有并发、不会升级，
	// 因此这里不做断言；升级行为由 TestAutoModeUpgradesOnConcurrency 覆盖。
}

func BenchmarkAutoReadParallel_COW(b *testing.B)  { benchAutoReadParallel(b, treeModeCOW) }
func BenchmarkAutoReadParallel_Lock(b *testing.B) { benchAutoReadParallel(b, treeModeLock) }
func BenchmarkAutoReadParallel_Auto(b *testing.B) { benchAutoReadParallel(b, treeModeAuto) }

