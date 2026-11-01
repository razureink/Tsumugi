package main

// ==================== 并发压力测试（-race 的替代，非等效） ====================
// 本机无 gcc、CGO_ENABLED=0，跑不了 `go test -race`。
// 这组测试不能替代竞态检测器，但能抓"逻辑性竞态"：
//   - 丢更新（最终内容与期望不符）
//   - 计数漂移（Size()/注册表条数）
//   - panic / 死锁（挂住会超时）
// 覆盖的是我评估中风险最高的四处：auto 升级的树、SAVEPOINT、会话注册表、
// 批量插入与流式扫描的并发使用。

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStressAutoModeTree auto 模式在"先单线程、后并发"的切换点最容易出问题：
// 升级必须等到 inflight==0，绝不能让 COW 读者看到半改的节点。
func TestStressAutoModeTree(t *testing.T) {
	withTreeMode(t, treeModeAuto)
	withTreeKind(t, treeKindLLRB)

	tree := newIntKeyTree[int64]()
	const keys = 400

	// 阶段一：单线程写满（此时应为原地更新、未升级）
	for i := int64(0); i < keys; i++ {
		tree.Set(i, i*2)
	}
	if tree.Upgraded() {
		t.Fatal("单线程阶段不应升级")
	}

	// 阶段二：并发混合读写——会触发升级
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mismatches atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < 3000; i++ {
				k := int64((i*7 + w) % keys)
				switch i % 5 {
				case 0:
					tree.Set(k, k*10)
				case 1:
					tree.SetNX(k, k)
				case 2:
					tree.Delete(k)
				default:
					// 读到的值必须"属于该键的合法取值集合"，不能是别的键的值
					if v, ok := tree.Get(k); ok {
						if v != k*2 && v != k*10 && v != k {
							mismatches.Add(1)
						}
					}
					lim := k
					tree.scanRangeLimit(&lim, 4, func(kk int64, vv int64) bool { return true })
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	if !tree.Upgraded() {
		t.Fatal("并发阶段结束后应已升级为 COW")
	}
	if n := mismatches.Load(); n != 0 {
		t.Fatalf("读到了不属于该键的值 %d 次（撕裂读）", n)
	}

	// 阶段三：并发结束后写回确定值，逐键校验，并确认 Size 准确
	for i := int64(0); i < keys; i++ {
		tree.Set(i, i*3)
	}
	if got := tree.Size(); got != keys {
		t.Fatalf("Size=%d want %d（计数漂移）", got, keys)
	}
	for i := int64(0); i < keys; i++ {
		v, ok := tree.Get(i)
		if !ok || v != i*3 {
			t.Fatalf("key %d = %v/%v want %d", i, v, ok, i*3)
		}
	}
	if !checkTreeInvariants(tree) {
		t.Fatal("升级后结构不变量被破坏")
	}
}

// TestStressSavepointsConcurrent 每个 goroutine 用自己的表做 SAVEPOINT 回滚，
// 互不干扰；最终每张表都必须是确定的期望状态。
func TestStressSavepointsConcurrent(t *testing.T) {
	db, sess, closeDB := sessionDB(t)
	defer closeDB()

	const tables = 8
	for i := 0; i < tables; i++ {
		if _, _, _, _, err := db.runSQLSchemaAs(sess.user,
			fmt.Sprintf("CREATE TABLE sp%d (id INT PRIMARY KEY, v INT)", i), nil, 0, ""); err != nil {
			t.Fatalf("create sp%d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < tables; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// 每个 goroutine 独立会话（事务状态挂在 Session 上）
			s := &Session{db: db, user: sess.user, auth: true}
			run := func(sql string) {
				var out sqlOut
				if _, _, _, _, err := db.runSQLSessionAsOut(s, sql, &out); err != nil {
					t.Errorf("sp%d %q: %v", idx, sql, err)
				}
			}
			tbl := fmt.Sprintf("sp%d", idx)
			run("BEGIN")
			run(fmt.Sprintf("INSERT INTO %s VALUES (1,100)", tbl))
			run("SAVEPOINT keep")
			// 保存点之后做一堆会被回滚的写入
			for k := 2; k <= 20; k++ {
				run(fmt.Sprintf("INSERT INTO %s VALUES (%d,%d)", tbl, k, k))
			}
			run("ROLLBACK TO SAVEPOINT keep")
			run(fmt.Sprintf("UPDATE %s SET v = 7 WHERE id = 1", tbl))
			run("COMMIT")

			// 校验：只剩 id=1，且 v=7（更新在回滚之后，应保留）
			t2 := db.getTable(tbl)
			if t2 == nil {
				t.Errorf("sp%d 表丢失", idx)
				return
			}
			if got := t2.pkTree.Size(); got != 1 {
				t.Errorf("sp%d 行数=%d want 1（保存点回滚不干净）", idx, got)
			}
			row, ok := t2.SelectByPK(1)
			if !ok || row["v"] != int64(7) {
				t.Errorf("sp%d 行1=%v/%v want v=7", idx, row, ok)
			}
		}(i)
	}
	wg.Wait()
}

// TestStressSessionRegistry 并发登记/更新/注销后，条数必须回到基线（不能泄漏）。
func TestStressSessionRegistry(t *testing.T) {
	base := len(snapshotSessions())
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := registerSession("native", "u", "h")
			touchSession(id, func(sv *sessionView) {
				sv.Command = "Query"
				sv.LastSQL = "SELECT 1"
			})
			touchSession(id, func(sv *sessionView) { sv.Command = "Sleep" })
			unregisterSession(id)
		}(i)
	}
	wg.Wait()
	if got := len(snapshotSessions()); got != base {
		t.Fatalf("会话注册表条数=%d，基线=%d（注销泄漏）", got, base)
	}
	// 触发一次快照，确认并发写不会让 processlistRows panic
	_ = processlistRows()
}

// TestStressSlowLogBounded 并发记录慢查询后，环形缓冲必须仍然有界且内容可读。
func TestStressSlowLogBounded(t *testing.T) {
	resetSlowQueries()
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				recordSlowQuery("u", time.Millisecond, fmt.Sprintf("SELECT %d-%d", w, i), 1, false)
			}
		}(w)
	}
	wg.Wait()
	got := len(snapshotSlowQueries())
	if got != slowLogCapacity {
		t.Fatalf("慢查询日志 size=%d want %d（环形缓冲越界或丢记录）", got, slowLogCapacity)
	}
	resetSlowQueries()
}

// TestStressBatchInsertAndScan 并发批量插入（各自独立键段）+ 并发流式扫描：
// 全部写完后行数与内容必须精确，扫描期间不得 panic。
func TestStressBatchInsertAndScan(t *testing.T) {
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	if _, _, _, _, err := db.runSQL(
		"CREATE TABLE sb (id INT PRIMARY KEY, grp INT, KEY idx_grp (grp))"); err != nil {
		t.Fatalf("create: %v", err)
	}

	const writers = 8
	const perWriter = 200
	var wg sync.WaitGroup
	var scanErr atomic.Int64
	stop := make(chan struct{})

	// 后台扫描者：与写入并发，验证不会 panic / 不返回撕裂行
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			cols, op, err := db.PlanStreamSelect("SELECT id FROM sb")
			_ = cols
			if err != nil {
				continue // 表/列尚未就绪时不视为失败
			}
			bad := false
			if err := op.Run(func(row []interface{}) error {
				if id, ok := row[0].(int64); !ok || id <= 0 {
					bad = true
				}
				return nil
			}); err != nil && err != ErrNotSimpleSelect {
				scanErr.Add(1)
			}
			if bad {
				scanErr.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			base := int64(w * perWriter)
			for i := 0; i < perWriter; i += 20 {
				var sb []interface{}
				_ = sb
				var sql string
				sql = "INSERT INTO sb (id, grp) VALUES "
				for k := 0; k < 20 && i+k < perWriter; k++ {
					if k > 0 {
						sql += ","
					}
					id := base + int64(i+k) + 1
					sql += fmt.Sprintf("(%d,%d)", id, id%7)
				}
				if _, _, _, _, err := db.runSQL(sql); err != nil {
					t.Errorf("writer %d insert: %v", w, err)
					return
				}
			}
		}(w)
	}

	// 等写完后停扫描
	writersDone := make(chan struct{})
	go func() {
		for {
			if db.getTable("sb").pkTree.Size() >= int64(writers*perWriter) {
				close(writersDone)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	<-writersDone
	close(stop)
	wg.Wait()

	if n := scanErr.Load(); n != 0 {
		t.Fatalf("并发扫描出错 %d 次", n)
	}
	tbl := db.getTable("sb")
	if got, want := tbl.pkTree.Size(), int64(writers*perWriter); got != want {
		t.Fatalf("行数=%d want %d（批量插入丢更新）", got, want)
	}
	// 每个 id 都必须存在且 grp 自洽
	for w := 0; w < writers; w++ {
		for i := 1; i <= perWriter; i++ {
			id := int64(w*perWriter + i)
			row, ok := tbl.SelectByPK(id)
			if !ok {
				t.Fatalf("id %d 丢失", id)
			}
			if row["grp"] != id%7 {
				t.Fatalf("id %d grp=%v want %d", id, row["grp"], id%7)
			}
		}
	}
}
