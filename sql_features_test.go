package main

// MySQL 功能扩展的回归测试：表达式 / JOIN / GROUP BY / UNION / 函数 /
// INSERT 变体 / DDL 变体 / 语句级兼容。

import (
	"strings"
	"testing"
)

func newFeatDB(t *testing.T) *DB {
	t.Helper()
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustRun(t *testing.T, db *DB, sql string) {
	t.Helper()
	if _, _, _, _, err := db.runSQL(sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func rowsOf(t *testing.T, db *DB, sql string) [][]interface{} {
	t.Helper()
	_, rows, _, _, err := db.runSQL(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return rows
}

func seedFeatDB(t *testing.T) *DB {
	db := newFeatDB(t)
	// 注意：INT 列没有 NULL 语义（NULL 落 0），只有 VARCHAR 能存 NULL。
	mustRun(t, db, "CREATE TABLE users(id INT PRIMARY KEY, name VARCHAR, dept INT, note VARCHAR)")
	mustRun(t, db, "CREATE TABLE depts(id INT PRIMARY KEY, dname VARCHAR)")
	mustRun(t, db, "INSERT INTO users VALUES (1,'alice',10,'a'),(2,'bob',20,'b'),(3,'carol',10,NULL),(4,'dave',0,'d')")
	mustRun(t, db, "INSERT INTO depts VALUES (10,'eng'),(20,'ops')")
	return db
}

// ---- 表达式与函数 ----

func TestExprProjectionAndFunctions(t *testing.T) {
	db := seedFeatDB(t)
	rows := rowsOf(t, db, "SELECT id, name, id * 2 AS dbl, UPPER(name) AS up FROM users WHERE id = 1")
	if len(rows) != 1 || rows[0][0] != int64(1) || rows[0][1] != "alice" {
		t.Fatalf("row = %v", rows)
	}
	if f, _ := toFloat(rows[0][2]); f != 2 {
		t.Fatalf("id*2 = %v", rows[0][2])
	}
	if rows[0][3] != "ALICE" {
		t.Fatalf("upper = %v", rows[0][3])
	}
	// 字符串/数学/条件函数
	rows = rowsOf(t, db, "SELECT CONCAT(name, '-', id) AS c, LENGTH(name) AS l, ROUND(id / 3.0, 2) AS r, IFNULL(dept, -1) AS d FROM users ORDER BY id")
	if rows[0][0] != "alice-1" || rows[0][1] != int64(5) {
		t.Fatalf("concat/length = %v", rows[0])
	}
	if f, _ := toFloat(rows[0][2]); f < 0.32 || f > 0.34 {
		t.Fatalf("round = %v", rows[0][2])
	}
	if rows[3][3] != int64(0) {
		t.Fatalf("ifnull = %v (%T) (INT 列无 NULL，落 0)", rows[3][3], rows[3][3])
	}
	// CASE（note 是 VARCHAR，才有真正的 NULL 语义）
	rows = rowsOf(t, db, "SELECT CASE WHEN note IS NULL THEN 'none' WHEN dept = 10 THEN 'eng' ELSE 'other' END AS g FROM users ORDER BY id")
	if rows[0][0] != "eng" || rows[2][0] != "none" {
		t.Fatalf("case = %v", rows)
	}
	// 日期函数
	rows = rowsOf(t, db, "SELECT YEAR('2024-03-05') AS y, DATE_FORMAT('2024-03-05', '%Y/%m') AS d")
	if rows[0][0] != int64(2024) || rows[0][1] != "2024/03" {
		t.Fatalf("date funcs = %v", rows)
	}
	// 无 FROM 标量
	rows = rowsOf(t, db, "SELECT 1 + 2 AS s, ABS(-3) AS a, (NOW() IS NOT NULL) AS has_now")
	if v0, _ := toFloat(rows[0][0]); v0 != 3 {
		t.Fatalf("scalar = %v", rows)
	}
	if v1, _ := toFloat(rows[0][1]); v1 != 3 {
		t.Fatalf("abs = %v", rows)
	}
	if rows[0][2] != true {
		t.Fatalf("now = %v", rows)
	}
	// 未知列报错
	if _, _, _, _, err := db.runSQL("SELECT nope FROM users"); err == nil {
		t.Fatal("unknown column should error")
	}
}

func TestWhereEnhancements(t *testing.T) {
	db := seedFeatDB(t)
	// IN / NOT IN
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE id IN (1,3)")); n != 2 {
		t.Fatalf("IN rows = %d", n)
	}
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE id NOT IN (1,3)")); n != 2 {
		t.Fatalf("NOT IN rows = %d", n)
	}
	// NOT LIKE / LIKE
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE name LIKE 'a%'")); n != 1 {
		t.Fatalf("LIKE rows = %d", n)
	}
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE name NOT LIKE 'a%'")); n != 3 {
		t.Fatalf("NOT LIKE rows = %d", n)
	}
	// NOT BETWEEN / BETWEEN
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE id BETWEEN 2 AND 3")); n != 2 {
		t.Fatalf("BETWEEN rows = %d", n)
	}
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE id NOT BETWEEN 2 AND 3")); n != 2 {
		t.Fatalf("NOT BETWEEN rows = %d", n)
	}
	// 复杂布尔 + IS NULL
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE (dept = 10 OR dept = 20) AND name <> 'bob'")); n != 2 {
		t.Fatalf("bool rows = %d", n)
	}
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE note IS NULL")); n != 1 {
		t.Fatalf("IS NULL rows = %d", n)
	}
	// INT 列的 NULL 落 0：dept IS NULL 匹配不到任何行（文档已说明）
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE dept IS NULL")); n != 0 {
		t.Fatalf("INT NULL rows = %d, want 0", n)
	}
	// DISTINCT：dept 取值 0/10/20（INT 无 NULL）
	rows := rowsOf(t, db, "SELECT DISTINCT dept FROM users ORDER BY dept")
	if len(rows) != 3 {
		t.Fatalf("distinct = %v", rows)
	}
}

func TestJoin(t *testing.T) {
	db := seedFeatDB(t)
	// INNER JOIN + ON：dave(dept=0) 不匹配 depts，被 INNER JOIN 排除
	rows := rowsOf(t, db, "SELECT u.name, d.dname FROM users u JOIN depts d ON u.dept = d.id ORDER BY u.id")
	if len(rows) != 3 {
		t.Fatalf("inner join rows = %d (%v)", len(rows), rows)
	}
	if rows[0][0] != "alice" || rows[0][1] != "eng" {
		t.Fatalf("join row0 = %v", rows[0])
	}
	// LEFT JOIN：dept 为 NULL 的用户也要出现
	rows = rowsOf(t, db, "SELECT u.name, d.dname FROM users u LEFT JOIN depts d ON u.dept = d.id ORDER BY u.id")
	if len(rows) != 4 {
		t.Fatalf("left join rows = %d", len(rows))
	}
	if rows[3][1] != nil {
		t.Fatalf("left join null col = %v", rows[3])
	}
	// 三表 + WHERE
	rows = rowsOf(t, db, "SELECT u.name FROM users u JOIN depts d ON u.dept = d.id WHERE d.dname = 'ops'")
	if len(rows) != 1 || rows[0][0] != "bob" {
		t.Fatalf("join where = %v", rows)
	}
	// USING：要求两侧同名列（dept_map.dept 与 users.dept）
	mustRun(t, db, "CREATE TABLE dept_map(dept INT PRIMARY KEY, label VARCHAR)")
	mustRun(t, db, "INSERT INTO dept_map VALUES (10,'eng'),(20,'ops')")
	rows = rowsOf(t, db, "SELECT name, label FROM users JOIN dept_map USING (dept) ORDER BY name")
	if len(rows) != 3 {
		t.Fatalf("using rows = %d (%v)", len(rows), rows)
	}
	// 逗号交叉连接 + WHERE 等值
	rows = rowsOf(t, db, "SELECT u.name, d.dname FROM users u, depts d WHERE u.dept = d.id ORDER BY u.id")
	if len(rows) != 3 {
		t.Fatalf("comma join rows = %d", len(rows))
	}
	// 未知列报错
	if _, _, _, _, err := db.runSQL("SELECT u.nope FROM users u JOIN depts d ON u.dept = d.id"); err == nil {
		t.Fatal("join unknown column should error")
	}
}

func TestGroupByUnion(t *testing.T) {
	db := seedFeatDB(t)
	// 注意：INT 列不存 NULL（只有 VARCHAR 有 NULL 语义），dept=NULL 落入 0 组
	rows := rowsOf(t, db, "SELECT dept, COUNT(*) AS n, SUM(id) AS s, MIN(id) AS mn, MAX(id) AS mx FROM users GROUP BY dept ORDER BY dept")
	// 三组：dept = 0（dave，INT 无 NULL）、10、20
	if len(rows) != 3 {
		t.Fatalf("groups = %v", rows)
	}
	if rows[0][0] != int64(0) || rows[1][0] != int64(10) || rows[1][1] != int64(2) {
		t.Fatalf("groups = %v", rows)
	}
	rows = rowsOf(t, db, "SELECT dept, COUNT(*) AS n, SUM(id) AS s FROM users GROUP BY dept ORDER BY dept")
	if f, _ := toFloat(rows[1][2]); f != 4 { // alice(1)+carol(3)
		t.Fatalf("sum = %v", rows[1][2])
	}
	if f, _ := toFloat(rows[0][2]); f != 4 { // 1+3
		t.Fatalf("sum = %v", rows[0][2])
	}
	// COUNT(DISTINCT)：dept 有 0/10/20 三个不同值（INT 无 NULL）
	rows = rowsOf(t, db, "SELECT COUNT(DISTINCT dept) AS c FROM users")
	if rows[0][0] != int64(3) {
		t.Fatalf("count distinct = %v", rows)
	}
	// COUNT(DISTINCT) 忽略 NULL（VARCHAR 列）
	rows = rowsOf(t, db, "SELECT COUNT(DISTINCT note) AS c FROM users")
	if rows[0][0] != int64(3) {
		t.Fatalf("count distinct note = %v", rows)
	}
	// HAVING
	rows = rowsOf(t, db, "SELECT dept FROM users GROUP BY dept HAVING COUNT(*) = 2")
	if len(rows) != 1 || rows[0][0] != int64(10) {
		t.Fatalf("having = %v", rows)
	}
	// UNION ALL / UNION
	rows = rowsOf(t, db, "SELECT id FROM users WHERE id = 1 UNION ALL SELECT id FROM users WHERE id = 2")
	if len(rows) != 2 {
		t.Fatalf("union all = %v", rows)
	}
	rows = rowsOf(t, db, "SELECT id FROM users WHERE id <= 2 UNION SELECT id FROM users WHERE id >= 1")
	if len(rows) != 4 {
		t.Fatalf("union dedup = %v", rows)
	}
}

// ---- INSERT 变体 ----

func TestInsertVariants(t *testing.T) {
	db := seedFeatDB(t)
	// INSERT IGNORE：冲突行跳过，其它行照常
	if _, _, _, _, err := db.runSQL("INSERT IGNORE INTO users VALUES (1,'dup',10,'x'),(9,'new',10,'y')"); err != nil {
		t.Fatalf("insert ignore: %v", err)
	}
	if n := len(rowsOf(t, db, "SELECT id FROM users WHERE id = 9")); n != 1 {
		t.Fatalf("ignore did not insert row 9")
	}
	if v := rowsOf(t, db, "SELECT name FROM users WHERE id = 1")[0][0]; v != "alice" {
		t.Fatalf("ignore overwrote row: %v", v)
	}
	// REPLACE：冲突行被替换
	mustRun(t, db, "REPLACE INTO users VALUES (1,'replaced',10,'r')")
	if v := rowsOf(t, db, "SELECT name FROM users WHERE id = 1")[0][0]; v != "replaced" {
		t.Fatalf("replace = %v", v)
	}
	// ON DUPLICATE KEY UPDATE
	mustRun(t, db, "INSERT INTO users VALUES (1,'ignored',99,'n') ON DUPLICATE KEY UPDATE name = 'odku', dept = VALUES(dept)")
	rows := rowsOf(t, db, "SELECT name, dept FROM users WHERE id = 1")
	if rows[0][0] != "odku" {
		t.Fatalf("odku name = %v", rows[0][0])
	}
	if f, _ := toFloat(rows[0][1]); f != 99 { // VALUES(dept) 取待插入值
		t.Fatalf("odku values() = %v", rows[0][1])
	}
	// 普通 INSERT 冲突仍然报错
	if _, _, _, _, err := db.runSQL("INSERT INTO users VALUES (1,'x',1)"); err == nil {
		t.Fatal("plain insert conflict should error")
	}
	// 显式列清单 + ON DUPLICATE 自增
	mustRun(t, db, "INSERT INTO users (id, name) VALUES (2,'bob2') ON DUPLICATE KEY UPDATE name = CONCAT(name, '!')")
	if v := rowsOf(t, db, "SELECT name FROM users WHERE id = 2")[0][0]; v != "bob!" {
		t.Fatalf("odku concat = %v", v)
	}
}

// ---- DDL 变体 ----

func TestDDLVariants(t *testing.T) {
	db := newFeatDB(t)
	// 列属性 + 表级索引 + 表选项全部容忍
	mustRun(t, db, "CREATE TABLE t1 (id INT NOT NULL AUTO_INCREMENT, name VARCHAR(20) DEFAULT 'x' COMMENT 'c', PRIMARY KEY (id), KEY idx_name (name)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	mustRun(t, db, "INSERT INTO t1 VALUES (1,'a')")
	// IF NOT EXISTS 幂等
	mustRun(t, db, "CREATE TABLE IF NOT EXISTS t1 (id INT PRIMARY KEY)")
	// 追加列
	mustRun(t, db, "ALTER TABLE t1 ADD COLUMN age INT")
	if v := rowsOf(t, db, "SELECT age FROM t1 WHERE id = 1")[0][0]; v != int64(0) {
		t.Fatalf("added column = %v", v)
	}
	// 改列
	mustRun(t, db, "ALTER TABLE t1 MODIFY COLUMN name VARCHAR(64)")
	mustRun(t, db, "ALTER TABLE t1 CHANGE COLUMN age years INT")
	if v := rowsOf(t, db, "SELECT years FROM t1 WHERE id = 1")[0][0]; v != int64(0) {
		t.Fatalf("renamed column = %v", v)
	}
	// 索引增删
	mustRun(t, db, "ALTER TABLE t1 ADD INDEX idx_years (years)")
	mustRun(t, db, "ALTER TABLE t1 DROP INDEX idx_name")
	// 删列（重写行）
	mustRun(t, db, "ALTER TABLE t1 DROP COLUMN years")
	if v := rowsOf(t, db, "SELECT name FROM t1 WHERE id = 1")[0][0]; v != "a" {
		t.Fatalf("after drop column = %v", v)
	}
	if _, _, _, _, err := db.runSQL("SELECT years FROM t1 WHERE id = 1"); err == nil {
		t.Fatal("dropped column should not be selectable")
	}
	// 不能删主键列
	if _, _, _, _, err := db.runSQL("ALTER TABLE t1 DROP COLUMN id"); err == nil {
		t.Fatal("dropping PK should error")
	}
	// TRUNCATE
	mustRun(t, db, "INSERT INTO t1 VALUES (2,'b')")
	mustRun(t, db, "TRUNCATE TABLE t1")
	if n := len(rowsOf(t, db, "SELECT id FROM t1")); n != 0 {
		t.Fatalf("truncate left %d rows", n)
	}
	// RENAME TABLE
	mustRun(t, db, "INSERT INTO t1 VALUES (3,'c')")
	mustRun(t, db, "RENAME TABLE t1 TO t2")
	if v := rowsOf(t, db, "SELECT name FROM t2 WHERE id = 3")[0][0]; v != "c" {
		t.Fatalf("renamed table = %v", v)
	}
	if db.getTable("t1") != nil {
		t.Fatal("old table still exists")
	}
	// DROP IF EXISTS
	mustRun(t, db, "DROP TABLE IF EXISTS nope")
	mustRun(t, db, "DROP TABLE t2")
}

// ---- 语句级兼容 ----

func TestStatementCompat(t *testing.T) {
	db := seedFeatDB(t)
	for _, sql := range []string{
		"BEGIN",
		"START TRANSACTION",
		"COMMIT",
		"ROLLBACK",
		"SAVEPOINT sp1",
		"RELEASE SAVEPOINT sp1",
		"SET autocommit=1",
		"SET NAMES utf8mb4",
		"SET SESSION sql_mode='STRICT_TRANS_TABLES'",
		"FLUSH PRIVILEGES",
		"FLUSH TABLES",
		"ANALYZE TABLE users",
		"OPTIMIZE TABLE users",
		"CHECK TABLE users",
		"LOCK TABLES users READ",
		"UNLOCK TABLES",
		"KILL 42",
		"DO 1",
		"RESET QUERY CACHE",
	} {
		if _, _, _, _, err := db.runSQL(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// SHOW 扩展
	for _, sql := range []string{
		"SHOW MASTER STATUS",
		"SHOW SLAVE STATUS",
		"SHOW PRIVILEGES",
		"SHOW PROCEDURE STATUS",
		"SHOW FUNCTION STATUS",
		"SHOW OPEN TABLES",
		"SHOW BINARY LOGS",
		"SHOW ENGINES",
		"SHOW CHARACTER SET",
		"SHOW COLLATION",
		"SHOW VARIABLES LIKE 'version%'",
		"SHOW STATUS",
		"SHOW WARNINGS",
	} {
		if _, _, _, _, err := db.runSQL(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// 存储过程明确报错
	if _, _, _, _, err := db.runSQL("CALL foo()"); err == nil {
		t.Fatal("CALL should error")
	}
	// 信息函数
	setSessionContext("tsumugi", "root")
	rows := rowsOf(t, db, "SELECT DATABASE() AS db, USER() AS u, VERSION() AS v")
	if rows[0][0] != "tsumugi" || rows[0][1] != "root@localhost" {
		t.Fatalf("info funcs = %v", rows)
	}
	if !strings.Contains(rows[0][2].(string), "Tsumugi") {
		t.Fatalf("version = %v", rows[0][2])
	}
}

// ---- UPDATE / DELETE 通用谓词 ----

func TestUpdateDeleteWithoutPK(t *testing.T) {
	db := seedFeatDB(t)
	// 非主键条件 + 表达式赋值
	if _, _, n, _, err := db.runSQL("UPDATE users SET dept = dept + 1 WHERE name = 'alice'"); err != nil {
		t.Fatalf("update: %v", err)
	} else if n != 1 {
		t.Fatalf("update affected = %d, want 1", n)
	}
	if v := rowsOf(t, db, "SELECT dept FROM users WHERE name = 'alice'")[0][0]; v != int64(11) {
		t.Fatalf("dept = %v, want 11", v)
	}
	// 多行更新
	if _, _, n, _, err := db.runSQL("UPDATE users SET note = 'x' WHERE dept = 10"); err != nil {
		t.Fatalf("multi update: %v", err)
	} else if n != 1 { // 只有 carol 还是 dept=10
		t.Fatalf("multi update affected = %d", n)
	}
	// WHERE 复杂表达式 + LIMIT
	if _, _, n, _, err := db.runSQL("UPDATE users SET note = 'y' WHERE id > 0 LIMIT 1"); err != nil {
		t.Fatalf("limit update: %v", err)
	} else if n != 1 {
		t.Fatalf("limit update affected = %d", n)
	}
	// 主键点查路径仍然可用
	if _, _, n, _, err := db.runSQL("UPDATE users SET name = 'ALICE' WHERE id = 1"); err != nil {
		t.Fatalf("pk update: %v", err)
	} else if n != 1 {
		t.Fatalf("pk update affected = %d", n)
	}
	// 不存在的行：0 rows
	if _, _, n, _, err := db.runSQL("UPDATE users SET name = 'z' WHERE id = 999"); err != nil {
		t.Fatalf("noop update: %v", err)
	} else if n != 0 {
		t.Fatalf("noop update affected = %d", n)
	}
	// 未知列报错
	if _, _, _, _, err := db.runSQL("UPDATE users SET nope = 1 WHERE id = 1"); err == nil {
		t.Fatal("unknown column in SET should error")
	}

	// DELETE：非主键条件
	if _, _, n, _, err := db.runSQL("DELETE FROM users WHERE id = 4"); err != nil {
		t.Fatalf("pk delete: %v", err)
	} else if n != 1 {
		t.Fatalf("pk delete affected = %d", n)
	}
	if _, _, n, _, err := db.runSQL("DELETE FROM users WHERE name LIKE 'A%'"); err != nil {
		t.Fatalf("scan delete: %v", err)
	} else if n != 1 {
		t.Fatalf("scan delete affected = %d", n)
	}
	// 全表删除
	if _, _, n, _, err := db.runSQL("DELETE FROM users"); err != nil {
		t.Fatalf("delete all: %v", err)
	} else if n != 2 {
		t.Fatalf("delete all affected = %d", n)
	}
	if got := len(rowsOf(t, db, "SELECT id FROM users")); got != 0 {
		t.Fatalf("rows left = %d", got)
	}
	// 删空后仍可写
	mustRun(t, db, "INSERT INTO users VALUES (10,'x',1,'n')")
	if got := len(rowsOf(t, db, "SELECT id FROM users")); got != 1 {
		t.Fatalf("insert after delete = %d rows", got)
	}
}

// ---- review 发现的问题回归 ----

// 改主键必须迁移索引键，否则行变成"查不到"的孤儿。
func TestUpdatePrimaryKeyMigrates(t *testing.T) {
	db := newFeatDB(t)
	mustRun(t, db, "CREATE TABLE pk1(id INT PRIMARY KEY, v VARCHAR)")
	mustRun(t, db, "INSERT INTO pk1 VALUES (1,'a'),(9,'b')")
	if _, _, n, _, err := db.runSQL("UPDATE pk1 SET id = 5 WHERE id = 1"); err != nil || n != 1 {
		t.Fatalf("pk update: n=%d err=%v", n, err)
	}
	if rows := rowsOf(t, db, "SELECT id, v FROM pk1 WHERE id = 5"); len(rows) != 1 || rows[0][1] != "a" {
		t.Fatalf("new pk lookup = %v", rows)
	}
	if rows := rowsOf(t, db, "SELECT v FROM pk1 WHERE id = 1"); len(rows) != 0 {
		t.Fatalf("old pk still resolvable: %v", rows)
	}
	// 改成已存在的键：主键冲突必须报错且不破坏原行
	if _, _, _, _, err := db.runSQL("UPDATE pk1 SET id = 9 WHERE id = 5"); err == nil {
		t.Fatal("pk conflict should error")
	}
	if rows := rowsOf(t, db, "SELECT v FROM pk1 WHERE id = 5"); len(rows) != 1 {
		t.Fatalf("row lost after failed pk update: %v", rows)
	}
	// 非整数主键：报错，不能静默落 0
	if _, _, _, _, err := db.runSQL("UPDATE pk1 SET id = 'abc' WHERE id = 5"); err == nil {
		t.Fatal("non-integer pk should error")
	}
}

// LIMIT 0 必须一行都不改。
func TestLimitZeroAffectsNothing(t *testing.T) {
	db := newFeatDB(t)
	mustRun(t, db, "CREATE TABLE lz(id INT PRIMARY KEY)")
	mustRun(t, db, "INSERT INTO lz VALUES (1),(2),(3)")
	if _, _, n, _, err := db.runSQL("DELETE FROM lz LIMIT 0"); err != nil || n != 0 {
		t.Fatalf("delete limit 0: n=%d err=%v", n, err)
	}
	if _, _, n, _, err := db.runSQL("UPDATE lz SET id = id LIMIT 0"); err != nil || n != 0 {
		t.Fatalf("update limit 0: n=%d err=%v", n, err)
	}
	if c := len(rowsOf(t, db, "SELECT id FROM lz")); c != 3 {
		t.Fatalf("rows = %d, want 3", c)
	}
	if _, _, n, _, err := db.runSQL("DELETE FROM lz LIMIT 2"); err != nil || n != 2 {
		t.Fatalf("delete limit 2: n=%d err=%v", n, err)
	}
}

// ALTER 重建后行必须能正确解码，且重启后一致、不残留影子表、不丢授权。
func TestAlterRewritePersistsAndKeepsGrants(t *testing.T) {
	dir := t.TempDir()
	cfg := walTestConfig(dir)
	db, err := NewDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, db, "CREATE TABLE ar(id INT PRIMARY KEY, a VARCHAR, b INT)")
	mustRun(t, db, "INSERT INTO ar VALUES (1,'x',10),(2,'y',20),(3,NULL,30)")
	globalUsers.Add(&User{Username: "tester", Password: hashPasswd("p"), IsAdmin: false})
	if err := db.privileges.Grant("tester", "ar", PermSelect); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := db.runSQL("ALTER TABLE ar DROP COLUMN b"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	rows := rowsOf(t, db, "SELECT id, a FROM ar ORDER BY id")
	if len(rows) != 3 || rows[0][1] != "x" || rows[2][1] != nil {
		t.Fatalf("after drop column rows=%v", rows)
	}
	if !db.privileges.Check("tester", "ar", PermSelect) {
		t.Fatal("grant lost after ALTER")
	}
	// 不应残留影子表
	db.mu.RLock()
	for tbl := range db.catalog {
		if strings.Contains(tbl, "@alter") {
			db.mu.RUnlock()
			t.Fatalf("shadow table left behind: %s", tbl)
		}
	}
	db.mu.RUnlock()
	db.Close()
	globalUsers.Delete("tester")

	db2, err := NewDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	rows = rowsOf(t, db2, "SELECT id, a FROM ar ORDER BY id")
	if len(rows) != 3 || rows[0][1] != "x" || rows[2][1] != nil {
		t.Fatalf("after restart rows=%v", rows)
	}
	// 类型变更同样重建
	if _, _, _, _, err := db2.runSQL("ALTER TABLE ar MODIFY COLUMN a VARCHAR(64)"); err != nil {
		t.Fatalf("modify: %v", err)
	}
}

// 事务内 DELETE 必须可回滚。
func TestDeleteInTxnRollback(t *testing.T) {
	db := newFeatDB(t)
	mustRun(t, db, "CREATE TABLE dt(id INT PRIMARY KEY, v VARCHAR)")
	mustRun(t, db, "INSERT INTO dt VALUES (1,'a'),(2,'b')")
	tx := db.BeginTxn()
	if _, _, _, _, err := db.runSQLTx("DELETE FROM dt WHERE v = 'a'", nil, tx); err != nil {
		t.Fatalf("tx delete: %v", err)
	}
	if c := len(rowsOf(t, db, "SELECT id FROM dt")); c != 1 {
		t.Fatalf("inside txn rows=%d", c)
	}
	if err := db.RollbackTxn(tx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if c := len(rowsOf(t, db, "SELECT id FROM dt")); c != 2 {
		t.Fatalf("after rollback rows=%d", c)
	}
}

// 逗号连接后接 LEFT JOIN：连接类型不能错位。
func TestCommaJoinThenLeftJoin(t *testing.T) {
	db := newFeatDB(t)
	mustRun(t, db, "CREATE TABLE cj_x(id INT PRIMARY KEY, gid INT)")
	mustRun(t, db, "CREATE TABLE cj_y(id INT PRIMARY KEY, gid INT)")
	mustRun(t, db, "CREATE TABLE cj_z(gid INT PRIMARY KEY, label VARCHAR)")
	mustRun(t, db, "INSERT INTO cj_x VALUES (1,10)")
	mustRun(t, db, "INSERT INTO cj_y VALUES (1,99)")
	mustRun(t, db, "INSERT INTO cj_z VALUES (10,'ten')")
	rows := rowsOf(t, db, "SELECT cj_x.id, cj_y.id, cj_z.label FROM cj_x, cj_y LEFT JOIN cj_z ON cj_y.gid = cj_z.gid")
	if len(rows) != 1 || rows[0][2] != nil {
		t.Fatalf("comma+left rows=%v (want 1 row with NULL label)", rows)
	}
}

// 通用路径与快路径结果必须一致（防止两条路径语义漂移）。
func TestGeneralAndFastPathAgree(t *testing.T) {
	db := seedFeatDB(t)
	fast := rowsOf(t, db, "SELECT id, name FROM users WHERE id = 2")
	general := rowsOf(t, db, "SELECT id, name FROM users WHERE id = 2 AND name = 'bob'")
	if len(fast) != 1 || len(general) != 1 || fast[0][1] != general[0][1] {
		t.Fatalf("fast=%v general=%v", fast, general)
	}
	// 表达式版与快路径版结果一致
	a := rowsOf(t, db, "SELECT name FROM users WHERE dept = 10 ORDER BY id")
	b := rowsOf(t, db, "SELECT name FROM users WHERE dept = 10 AND id > 0 ORDER BY id")
	if len(a) != len(b) {
		t.Fatalf("a=%v b=%v", a, b)
	}
	for i := range a {
		if a[i][0] != b[i][0] {
			t.Fatalf("row %d differs: %v vs %v", i, a[i], b[i])
		}
	}
}
