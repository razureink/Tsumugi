package interop

// 用真实 go-sql-driver/mysql 验证 MySQL 功能面：JOIN / GROUP BY / UNION /
// INSERT ... ON DUPLICATE KEY UPDATE / 子查询式标量表达式 / 事务内混合语句 /
// 预处理语句复用同一批语法。

import (
	"database/sql"
	"fmt"
	"testing"
)

func setupFeatureTables(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		"DROP TABLE IF EXISTS f_users",
		"DROP TABLE IF EXISTS f_depts",
		"CREATE TABLE f_users (id INT PRIMARY KEY, name VARCHAR, dept INT, note VARCHAR)",
		"CREATE TABLE f_depts (id INT PRIMARY KEY, dname VARCHAR)",
		"INSERT INTO f_users VALUES (1,'alice',10,'a'),(2,'bob',20,NULL),(3,'carol',10,'c'),(4,'dave',0,'d')",
		"INSERT INTO f_depts VALUES (10,'eng'),(20,'ops')",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestJoinQueries(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	setupFeatureTables(t, db)

	rows, err := db.Query("SELECT u.name, d.dname FROM f_users u JOIN f_depts d ON u.dept = d.id ORDER BY u.id")
	if err != nil {
		t.Fatalf("inner join: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name, dname string
		if err := rows.Scan(&name, &dname); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name+"/"+dname)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{"alice/eng", "bob/ops", "carol/eng"}
	if len(got) != len(want) {
		t.Fatalf("inner join rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %s, want %s", i, got[i], want[i])
		}
	}

	// LEFT JOIN：右表无匹配时右侧列为 NULL
	rows2, err := db.Query("SELECT u.name, d.dname FROM f_users u LEFT JOIN f_depts d ON u.dept = d.id ORDER BY u.id")
	if err != nil {
		t.Fatalf("left join: %v", err)
	}
	defer rows2.Close()
	n := 0
	var nullSeen bool
	for rows2.Next() {
		var name string
		var dname sql.NullString
		if err := rows2.Scan(&name, &dname); err != nil {
			t.Fatalf("scan left: %v", err)
		}
		if !dname.Valid {
			nullSeen = true
		}
		n++
	}
	if n != 4 || !nullSeen {
		t.Fatalf("left join n=%d nullSeen=%v", n, nullSeen)
	}
}

func TestAggregateAndUnion(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	setupFeatureTables(t, db)

	rows, err := db.Query("SELECT dept, COUNT(*) AS n, SUM(id) AS s FROM f_users GROUP BY dept HAVING COUNT(*) > 1 ORDER BY dept")
	if err != nil {
		t.Fatalf("group by: %v", err)
	}
	defer rows.Close()
	type agg struct {
		dept int64
		n    int64
		s    float64
	}
	var out []agg
	for rows.Next() {
		var a agg
		if err := rows.Scan(&a.dept, &a.n, &a.s); err != nil {
			t.Fatalf("scan agg: %v", err)
		}
		out = append(out, a)
	}
	if len(out) != 1 || out[0].dept != 10 || out[0].n != 2 || out[0].s != 4 {
		t.Fatalf("agg = %+v", out)
	}

	// UNION ALL 与 UNION
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM (SELECT id FROM f_users WHERE id = 1 UNION ALL SELECT id FROM f_users WHERE id = 2) t").Scan(&count); err == nil {
		t.Fatalf("derived table should not be supported, got %d", count)
	}
	rowsU, err := db.Query("SELECT id FROM f_users WHERE id <= 2 UNION SELECT id FROM f_users WHERE id >= 1 ORDER BY id")
	if err != nil {
		t.Fatalf("union: %v", err)
	}
	defer rowsU.Close()
	var ids []int64
	for rowsU.Next() {
		var id int64
		if err := rowsU.Scan(&id); err != nil {
			t.Fatalf("scan union: %v", err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 4 {
		t.Fatalf("union ids = %v, want 4 distinct rows", ids)
	}
}

func TestInsertVariantsViaDriver(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	setupFeatureTables(t, db)

	// INSERT IGNORE：冲突行跳过
	if _, err := db.Exec("INSERT IGNORE INTO f_users VALUES (1,'dup',10,'x'),(9,'new',10,'y')"); err != nil {
		t.Fatalf("insert ignore: %v", err)
	}
	var name string
	if err := db.QueryRow("SELECT name FROM f_users WHERE id = 1").Scan(&name); err != nil || name != "alice" {
		t.Fatalf("ignore changed row: %q err=%v", name, err)
	}

	// REPLACE INTO：整行替换
	if _, err := db.Exec("REPLACE INTO f_users VALUES (1,'replaced',10,'r')"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := db.QueryRow("SELECT name FROM f_users WHERE id = 1").Scan(&name); err != nil || name != "replaced" {
		t.Fatalf("replace = %q err=%v", name, err)
	}

	// ON DUPLICATE KEY UPDATE + VALUES()
	if _, err := db.Exec("INSERT INTO f_users VALUES (1,'x',99,'z') ON DUPLICATE KEY UPDATE name = 'upd', dept = VALUES(dept)"); err != nil {
		t.Fatalf("on duplicate: %v", err)
	}
	var dept int64
	if err := db.QueryRow("SELECT name, dept FROM f_users WHERE id = 1").Scan(&name, &dept); err != nil {
		t.Fatalf("select after odku: %v", err)
	}
	if name != "upd" || dept != 99 {
		t.Fatalf("odku name=%q dept=%d", name, dept)
	}

	// 预处理语句 + ON DUPLICATE（类型复用路径）
	stmt, err := db.Prepare("INSERT INTO f_users VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE name = ?")
	if err != nil {
		t.Fatalf("prepare odku: %v", err)
	}
	defer stmt.Close()
	for i := 0; i < 3; i++ {
		if _, err := stmt.Exec(1, "x", 30+i, "z", fmt.Sprintf("p%d", i)); err != nil {
			t.Fatalf("exec odku %d: %v", i, err)
		}
	}
	if err := db.QueryRow("SELECT name FROM f_users WHERE id = 1").Scan(&name); err != nil || name != "p2" {
		t.Fatalf("prepared odku name=%q err=%v", name, err)
	}
}

func TestUpdateDeleteWithoutPKViaDriver(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	setupFeatureTables(t, db)

	// 表达式赋值 + 非主键条件
	res, err := db.Exec("UPDATE f_users SET dept = dept + 1 WHERE name = 'alice'")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("rows affected = %d", n)
	}
	var dept int64
	if err := db.QueryRow("SELECT dept FROM f_users WHERE name = 'alice'").Scan(&dept); err != nil || dept != 11 {
		t.Fatalf("dept=%d err=%v", dept, err)
	}

	// 预处理语句形式
	stmt, err := db.Prepare("UPDATE f_users SET note = ? WHERE dept = ?")
	if err != nil {
		t.Fatalf("prepare update: %v", err)
	}
	defer stmt.Close()
	if _, err := stmt.Exec("n10", 10); err != nil {
		t.Fatalf("exec update: %v", err)
	}
	var cnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM f_users WHERE note = 'n10'").Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("count(where note=n10) = %d, want 1", cnt)
	}

	// 非主键 DELETE
	res, err = db.Exec("DELETE FROM f_users WHERE dept = 0")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("delete affected = %d", n)
	}
	var left int
	if err := db.QueryRow("SELECT COUNT(*) FROM f_users").Scan(&left); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if left != 3 {
		t.Fatalf("rows left = %d, want 3", left)
	}
}

func TestDDLAndTxnViaDriver(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS f_ddl"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	// 列属性 / 表级索引 / 表选项全部被接受
	if _, err := db.Exec("CREATE TABLE f_ddl (id INT NOT NULL AUTO_INCREMENT, name VARCHAR(32) DEFAULT '', PRIMARY KEY (id), KEY idx_name (name)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"); err != nil {
		t.Fatalf("create with options: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE f_ddl ADD COLUMN age INT"); err != nil {
		t.Fatalf("add column: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE f_ddl MODIFY COLUMN name VARCHAR(64)"); err != nil {
		t.Fatalf("modify column: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE f_ddl RENAME COLUMN age TO years"); err != nil {
		t.Fatalf("rename column: %v", err)
	}
	if _, err := db.Exec("INSERT INTO f_ddl (id, name, years) VALUES (1,'a',30)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 事务：提交与回滚
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec("INSERT INTO f_ddl (id, name, years) VALUES (2,'b',40)"); err != nil {
		t.Fatalf("tx insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tx2, err := db.Begin()
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	if _, err := tx2.Exec("INSERT INTO f_ddl (id, name, years) VALUES (3,'c',50)"); err != nil {
		t.Fatalf("tx2 insert: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM f_ddl").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2 (rollback must not persist row 3)", n)
	}
	// TRUNCATE / RENAME TABLE
	if _, err := db.Exec("TRUNCATE TABLE f_ddl"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := db.Exec("RENAME TABLE f_ddl TO f_ddl2"); err != nil {
		t.Fatalf("rename table: %v", err)
	}
	if _, err := db.Exec("DROP TABLE f_ddl2"); err != nil {
		t.Fatalf("drop renamed: %v", err)
	}
}
