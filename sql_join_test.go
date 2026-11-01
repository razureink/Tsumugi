package main

// ==================== JOIN：哈希等值连接 ====================
// joinRows 原本是纯嵌套循环（O(N*M)）。等值连接改为哈希后，
// 用同一个查询分别走"哈希"和"强制嵌套循环"两条路径对拍，
// 保证优化没有改变任何语义（外连接、多条件 ON、非等值 ON、字符串 CI）。

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

func setupJoinDB(t *testing.T) *DB {
	t.Helper()
	SetLogMute(true)
	db := newTestDB(t)
	mustRun(t, db, "CREATE TABLE j_users (id INT PRIMARY KEY, name VARCHAR(32), code VARCHAR(16))")
	mustRun(t, db, "CREATE TABLE j_orders (id INT PRIMARY KEY, user_id INT, amount INT, code VARCHAR(16))")
	mustRun(t, db, "CREATE TABLE j_items (id INT PRIMARY KEY, order_id INT, label VARCHAR(16))")
	mustRun(t, db, "INSERT INTO j_users VALUES (1,'alice','AA'),(2,'bob','BB'),(3,'carol','CC')")
	mustRun(t, db, "INSERT INTO j_orders VALUES (10,1,100,'aa'),(11,1,200,'aa'),(12,2,300,'bb')")
	mustRun(t, db, "INSERT INTO j_items VALUES (100,10,'x'),(101,11,'y'),(102,12,'z')")
	return db
}

func sortedRowsOf(t *testing.T, db *DB, q string) [][]interface{} {
	t.Helper()
	rows := rowsOf(t, db, q)
	// 不依赖 ORDER BY 的 NULL 排序差异，按字符串表示排序后对比
	sort.Slice(rows, func(i, j int) bool { return fmt.Sprint(rows[i]) < fmt.Sprint(rows[j]) })
	return rows
}

// TestHashJoinMatchesNestedLoop 同一批查询在两条路径下必须逐行一致。
func TestHashJoinMatchesNestedLoop(t *testing.T) {
	db := setupJoinDB(t)
	defer db.Close()

	type tc struct {
		q          string
		wantHash   bool
		wantRows   int
	}
	cases := []tc{
		// 等值 INNER：3 条订单
		{"SELECT u.id AS uid, o.amount AS amt FROM j_users u JOIN j_orders o ON u.id = o.user_id", true, 3},
		// 等值 LEFT：carol 没有订单，补 NULL
		{"SELECT u.id AS uid, o.amount AS amt FROM j_users u LEFT JOIN j_orders o ON u.id = o.user_id", true, 4},
		// 多条件 ON：哈希收窄后仍要完整求值残余条件
		{"SELECT u.id AS uid, o.amount AS amt FROM j_users u JOIN j_orders o ON u.id = o.user_id AND o.amount >= 200", true, 2},
		// 三表串联
		{"SELECT u.id AS uid, i.label AS lbl FROM j_users u JOIN j_orders o ON u.id = o.user_id JOIN j_items i ON o.id = i.order_id", true, 3},
		// 字符串等值（CI）：'AA' 应匹配 'aa'
		{"SELECT u.id AS uid, o.amount AS amt FROM j_users u JOIN j_orders o ON u.code = o.code", true, 3},
		// 非等值 ON：不能走哈希，但必须仍然正确
		{"SELECT u.id AS uid, o.amount AS amt FROM j_users u JOIN j_orders o ON u.id < o.user_id", false, 1},
		// CROSS JOIN：9 行
		{"SELECT u.id AS uid, o.id AS oid FROM j_users u CROSS JOIN j_orders o", false, 9},
	}

	for _, c := range cases {
		hashJoinDisabled.Store(false)
		hashJoinSteps.Store(0)
		gotHash := sortedRowsOf(t, db, c.q)
		used := hashJoinSteps.Load() > 0

		hashJoinDisabled.Store(true)
		gotLoop := sortedRowsOf(t, db, c.q)
		hashJoinDisabled.Store(false)

		if !reflect.DeepEqual(gotHash, gotLoop) {
			t.Fatalf("hash/nested mismatch for %q\n hash: %#v\n loop: %#v", c.q, gotHash, gotLoop)
		}
		if len(gotHash) != c.wantRows {
			t.Fatalf("%q returned %d rows, want %d: %#v", c.q, len(gotHash), c.wantRows, gotHash)
		}
		if used != c.wantHash {
			t.Fatalf("%q used hash=%v, want %v", c.q, used, c.wantHash)
		}
	}
}

// TestHashJoinLeftJoinNulls 外连接无匹配时右表列必须为 NULL（而不是 0）。
func TestHashJoinLeftJoinNulls(t *testing.T) {
	db := setupJoinDB(t)
	defer db.Close()
	rows := rowsOf(t, db,
		"SELECT u.id AS uid, o.amount AS amt FROM j_users u LEFT JOIN j_orders o ON u.id = o.user_id")
	var carol []interface{}
	found := false
	for _, r := range rows {
		if r[0] == int64(3) {
			carol = r
			found = true
		}
	}
	if !found {
		t.Fatalf("carol missing from LEFT JOIN result: %#v", rows)
	}
	if carol[1] != nil {
		t.Fatalf("unmatched LEFT JOIN must produce NULL, got %#v", carol[1])
	}
}

// TestHashJoinScales 大表等值连接：哈希路径应在秒级内完成。
// 3000×3000 的纯嵌套循环是 900 万次合并，通常要数十秒。
func TestHashJoinScales(t *testing.T) {
	SetLogMute(true)
	db := newTestDB(t)
	defer db.Close()
	mustRun(t, db, "CREATE TABLE big_u (id INT PRIMARY KEY, v INT)")
	mustRun(t, db, "CREATE TABLE big_o (id INT PRIMARY KEY, uid INT, amt INT)")
	const n = 3000
	for base := 1; base <= n; base += 200 {
		var su, so []byte
		su = append(su, "INSERT INTO big_u VALUES "...)
		so = append(so, "INSERT INTO big_o VALUES "...)
		for i := 0; i < 200 && base+i <= n; i++ {
			if i > 0 {
				su = append(su, ',')
				so = append(so, ',')
			}
			su = append(su, fmt.Sprintf("(%d,%d)", base+i, (base+i)*2)...)
			so = append(so, fmt.Sprintf("(%d,%d,%d)", base+i, base+i, (base+i)*10)...)
		}
		mustRun(t, db, string(su))
		mustRun(t, db, string(so))
	}

	hashJoinDisabled.Store(false)
	hashJoinSteps.Store(0)
	begin := time.Now()
	rows := rowsOf(t,
		db, "SELECT COUNT(*) FROM big_u u JOIN big_o o ON u.id = o.uid")
	dur := time.Since(begin)
	if len(rows) != 1 || rows[0][0] != int64(n) {
		t.Fatalf("join count=%v want %d", rows, n)
	}
	if hashJoinSteps.Load() == 0 {
		t.Fatal("large equi-join did not use the hash path")
	}
	if dur > 5*time.Second {
		t.Fatalf("3000x3000 join took %v; hash join is not effective", dur)
	}
	t.Logf("3000x3000 equi-join: %v", dur)
}
