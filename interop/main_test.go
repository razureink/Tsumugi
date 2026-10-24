package interop

// 与真实 MySQL 驱动的互通测试：用 github.com/go-sql-driver/mysql 连接
// 本地启动的 tsumugi（MySQL 兼容协议），覆盖文本/二进制协议、预处理语句、
// 事务、NULL/大字段、多包分片与并发。
//
// 运行：go test ./... -v（需要本地有 go 工具链，测试会自行编译 ../ 的服务端）

import (
	"bytes"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

var (
	dsn      string
	proc     *exec.Cmd
	logs     bytes.Buffer
	mysqlPrt int
)

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func TestMain(m *testing.M) {
	code := runTests(m)
	os.Exit(code)
}

func runTests(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "tsumugi-interop-*")
	if err != nil {
		fmt.Println("mktemp:", err)
		return 1
	}
	defer os.RemoveAll(tmp)

	bin := filepath.Join(tmp, "tsumugi.exe")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = ".."
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("build failed:", err)
		return 1
	}

	protoPort, e1 := freePort()
	metricsPort, e2 := freePort()
	mysqlPrt, e3 := freePort()
	if e1 != nil || e2 != nil || e3 != nil {
		fmt.Println("free port:", e1, e2, e3)
		return 1
	}
	cfgDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		fmt.Println("mkdir:", err)
		return 1
	}
	pwd := "interop-secret"
	sum := sha1.Sum([]byte(pwd))
	cfg := map[string]interface{}{
		"port":            protoPort,
		"metrics_port":    metricsPort,
		"mysql_enabled":   true,
		"mysql_port":      mysqlPrt,
		"wal_dir":         "./data",
		"user":            "root",
		"password_sha1":   hex.EncodeToString(sum[:]),
		"durability":      "fsync",
		"enable_checksum": true,
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(cfgDir, "tsumugi.json"), raw, 0o600); err != nil {
		fmt.Println("write config:", err)
		return 1
	}

	proc = exec.Command(bin)
	proc.Dir = tmp
	proc.Stdout = &logs
	proc.Stderr = &logs
	if err := proc.Start(); err != nil {
		fmt.Println("start:", err)
		return 1
	}
	defer func() {
		if proc.Process != nil {
			proc.Process.Kill()
			proc.Wait()
		}
	}()

	// 等 MySQL 端口可连
	addr := fmt.Sprintf("127.0.0.1:%d", mysqlPrt)
	ok := false
	for i := 0; i < 100; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			ok = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ok {
		fmt.Println("server did not start:\n" + logs.String())
		return 1
	}

	dsn = fmt.Sprintf("root:%s@tcp(%s)/?timeout=5s&readTimeout=20s&writeTimeout=20s&charset=utf8mb4&interpolateParams=false", pwd, addr)
	return m.Run()
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxLifetime(time.Minute)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v\nserver logs:\n%s", err, logs.String())
	}
	return db
}

func TestPing(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("second ping: %v", err)
	}
}

func TestCRUDText(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS crud_t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE crud_t (id INT PRIMARY KEY, name VARCHAR, age INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := db.Exec("INSERT INTO crud_t VALUES (1, 'alice', 30)")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("rows affected = %d, want 1", n)
	}
	var name string
	var age int64
	if err := db.QueryRow("SELECT name, age FROM crud_t WHERE id = 1").Scan(&name, &age); err != nil {
		t.Fatalf("select: %v", err)
	}
	if name != "alice" || age != 30 {
		t.Fatalf("got %q/%d", name, age)
	}
	if _, err := db.Exec("UPDATE crud_t SET age = 31 WHERE id = 1"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := db.QueryRow("SELECT age FROM crud_t WHERE id = 1").Scan(&age); err != nil || age != 31 {
		t.Fatalf("after update age=%d err=%v", age, err)
	}
	if _, err := db.Exec("DELETE FROM crud_t WHERE id = 1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	err = db.QueryRow("SELECT age FROM crud_t WHERE id = 1").Scan(&age)
	if err != sql.ErrNoRows {
		t.Fatalf("want ErrNoRows, got %v", err)
	}
}

func TestPrepared(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS prep_t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE prep_t (id INT PRIMARY KEY, name VARCHAR, score INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	ins, err := db.Prepare("INSERT INTO prep_t VALUES (?, ?, ?)")
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	defer ins.Close()
	for i := 1; i <= 5; i++ {
		if _, err := ins.Exec(i, fmt.Sprintf("user-%d", i), i*10); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
	}
	// 复用同一 statement 再执行一次（验证类型缓存 / 语句生命周期）
	if _, err := ins.Exec(6, "user-6", 60); err != nil {
		t.Fatalf("reuse exec: %v", err)
	}

	sel, err := db.Prepare("SELECT id, name, score FROM prep_t WHERE id = ?")
	if err != nil {
		t.Fatalf("prepare select: %v", err)
	}
	defer sel.Close()
	for round := 0; round < 3; round++ { // 二进制结果集必须可重复读取
		var id, score int64
		var name string
		if err := sel.QueryRow(3).Scan(&id, &name, &score); err != nil {
			t.Fatalf("round %d query: %v", round, err)
		}
		if id != 3 || name != "user-3" || score != 30 {
			t.Fatalf("round %d got %d/%q/%d", round, id, name, score)
		}
	}
	// 多行二进制结果集
	rows, err := db.Query("SELECT id, name FROM prep_t ORDER BY id")
	if err != nil {
		t.Fatalf("query rows: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if n != 6 {
		t.Fatalf("rows = %d, want 6", n)
	}
}

func TestNullAndUnicode(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS null_t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE null_t (id INT PRIMARY KEY, note VARCHAR)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec("INSERT INTO null_t VALUES (1, NULL)"); err != nil {
		t.Fatalf("insert null: %v", err)
	}
	if _, err := db.Exec("INSERT INTO null_t VALUES (2, '纺线 · Tsumugi')"); err != nil {
		t.Fatalf("insert unicode: %v", err)
	}
	var note sql.NullString
	if err := db.QueryRow("SELECT note FROM null_t WHERE id = 1").Scan(&note); err != nil {
		t.Fatalf("scan null: %v", err)
	}
	if note.Valid {
		t.Fatalf("want NULL, got %q", note.String)
	}
	var s string
	if err := db.QueryRow("SELECT note FROM null_t WHERE id = 2").Scan(&s); err != nil {
		t.Fatalf("scan unicode: %v", err)
	}
	if s != "纺线 · Tsumugi" {
		t.Fatalf("unicode mismatch: %q", s)
	}
	// 二进制协议下的 NULL（预处理）
	var note2 sql.NullString
	if err := db.QueryRow("SELECT note FROM null_t WHERE id = ?", 1).Scan(&note2); err != nil {
		t.Fatalf("prepared scan null: %v", err)
	}
	if note2.Valid {
		t.Fatalf("prepared want NULL, got %q", note2.String)
	}
}

func TestTransactions(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS txn_t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE txn_t (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// COMMIT 路径
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec("INSERT INTO txn_t VALUES (1, 100)"); err != nil {
		tx.Rollback()
		t.Fatalf("tx insert: %v", err)
	}
	if _, err := tx.Exec("UPDATE txn_t SET v = 200 WHERE id = 1"); err != nil {
		tx.Rollback()
		t.Fatalf("tx update: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var v int64
	if err := db.QueryRow("SELECT v FROM txn_t WHERE id = 1").Scan(&v); err != nil || v != 200 {
		t.Fatalf("after commit v=%d err=%v", v, err)
	}
	// ROLLBACK 路径
	tx2, err := db.Begin()
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	if _, err := tx2.Exec("INSERT INTO txn_t VALUES (2, 5)"); err != nil {
		tx2.Rollback()
		t.Fatalf("tx2 insert: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var cnt int64
	if err := db.QueryRow("SELECT COUNT(*) FROM txn_t").Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("count = %d, want 1 (rollback must not persist)", cnt)
	}
}

func TestMetaQueries(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS meta_t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE meta_t (id INT PRIMARY KEY, tag VARCHAR)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	rows, err := db.Query("SHOW TABLES")
	if err != nil {
		t.Fatalf("show tables: %v", err)
	}
	rows.Close()
	rows, err = db.Query("DESCRIBE meta_t")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("describe columns: %v", err)
	}
	if len(cols) == 0 {
		t.Fatalf("describe returned no columns")
	}
	rows.Close()
	var v string
	if err := db.QueryRow("SELECT VERSION()").Scan(&v); err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(v, "Tsumugi") {
		t.Fatalf("version = %q", v)
	}
}

func TestConcurrent(t *testing.T) {
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS conc_t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE conc_t (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				id := w*100 + i + 1
				if _, err := db.Exec("INSERT INTO conc_t VALUES (?, ?)", id, id*2); err != nil {
					errCh <- fmt.Errorf("insert %d: %w", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent: %v", err)
	}
	var cnt int64
	if err := db.QueryRow("SELECT COUNT(*) FROM conc_t").Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 80 {
		t.Fatalf("count = %d, want 80", cnt)
	}
}

func TestLargePayload(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	db := openDB(t)
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS big_t"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE big_t (id INT PRIMARY KEY, blob_col VARCHAR)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 接近存储上限（VARCHAR 长度前缀 2 字节，上限 65534）
	big := strings.Repeat("abcdefgh", 60000/8)
	if _, err := db.Exec("INSERT INTO big_t VALUES (?, ?)", 1, big); err != nil {
		t.Fatalf("insert big: %v", err)
	}
	var got string
	if err := db.QueryRow("SELECT blob_col FROM big_t WHERE id = 1").Scan(&got); err != nil {
		t.Fatalf("select big: %v", err)
	}
	if len(got) != len(big) {
		t.Fatalf("big length = %d, want %d", len(got), len(big))
	}
	// 超过上限必须报错，而不是静默截断写坏数据
	tooBig := strings.Repeat("x", 70000)
	if _, err := db.Exec("INSERT INTO big_t VALUES (?, ?)", 2, tooBig); err == nil {
		t.Fatalf("expected error for oversized value")
	}
	// 超长 SQL 文本（>16MB）会触发请求包分片：必须返回错误而不是挂起/崩溃
	huge := "INSERT INTO big_t VALUES (3, '" + strings.Repeat("y", 17<<20) + "')"
	if _, err := db.Exec(huge); err == nil {
		t.Fatalf("expected error for oversized statement")
	}
	// 连接仍然可用
	if err := db.Ping(); err != nil {
		t.Fatalf("ping after oversized statement: %v", err)
	}
}
