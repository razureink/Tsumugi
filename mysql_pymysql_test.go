package main

// ==================== 真实第三方客户端：PyMySQL ====================
// 之前的协议测试全是我们自己写的客户端，容易"自己圆自己"：
// 双方对协议的理解一致，不代表第三方驱动也能连。
// 这里用真实安装的 PyMySQL 跑一遍完整链路：
//   握手 → mysql_native_password 认证 → 能力协商 → 查询/结果集 →
//   参数化插入 → 错误码 → 事务 BEGIN/COMMIT/ROLLBACK → UTF-8 往返。
// 环境没有 python/pymysql 时自动跳过（不阻塞 CI）。

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const pyMySQLScript = `
import sys
import pymysql

try:
    conn = pymysql.connect(
        host='127.0.0.1', port=__PORT__, user='root', password='password',
        charset='utf8mb4', autocommit=True, connect_timeout=5)
except Exception as e:
    print('CONNECT_FAIL', repr(e))
    sys.exit(1)

cur = conn.cursor()

# 1) 普通查询与结果集编码
cur.execute('SELECT id, name, n FROM py_t ORDER BY id')
rows = cur.fetchall()
if rows != ((1, 'alice', 10), (2, '\u4e2d\u6587', 20)):
    print('BAD_ROWS', rows)
    sys.exit(1)

# 2) 参数化插入（PyMySQL 客户端转义），并验证 UTF-8 往返
cur.execute('INSERT INTO py_t (id, name, n) VALUES (%s, %s, %s)', (3, 'bob', 30))
if cur.rowcount != 1:
    print('BAD_ROWCOUNT', cur.rowcount)
    sys.exit(1)
cur.execute('SELECT name FROM py_t WHERE id = %s', (3,))
if cur.fetchone()[0] != 'bob':
    print('BAD_PARAM_SELECT')
    sys.exit(1)
cur.execute('INSERT INTO py_t (id, name, n) VALUES (%s, %s, %s)', (4, '\u4e2d\u6587-\u00e9', 40))
cur.execute('SELECT name FROM py_t WHERE id = 4')
if cur.fetchone()[0] != '\u4e2d\u6587-\u00e9':
    print('BAD_UTF8')
    sys.exit(1)

# 3) 错误码映射（工具链依赖 errno，而不是错误文本）
try:
    cur.execute("INSERT INTO py_t (id, name, n) VALUES (1, 'dup', 0)")
    print('EXPECTED_DUP_ERROR')
    sys.exit(1)
except pymysql.err.IntegrityError as e:
    if e.args[0] != 1062:
        print('BAD_DUP_ERRNO', e.args)
        sys.exit(1)

try:
    cur.execute('SELECT * FROM no_such_table')
    print('EXPECTED_TABLE_ERROR')
    sys.exit(1)
except pymysql.err.ProgrammingError as e:
    if e.args[0] != 1146:
        print('BAD_TABLE_ERRNO', e.args)
        sys.exit(1)

# 4) 事务：ROLLBACK 丢弃、COMMIT 保留
conn.autocommit(False)
cur.execute("INSERT INTO py_t (id, name, n) VALUES (5, 'rollback', 50)")
conn.rollback()
chk = conn.cursor()
chk.execute('SELECT COUNT(*) FROM py_t WHERE id = 5')
if chk.fetchone()[0] != 0:
    print('ROLLBACK_FAILED')
    sys.exit(1)

cur.execute("INSERT INTO py_t (id, name, n) VALUES (6, 'commit', 60)")
conn.commit()
chk.execute('SELECT n FROM py_t WHERE id = 6')
if chk.fetchone()[0] != 60:
    print('COMMIT_FAILED')
    sys.exit(1)

# 5) 更新/删除的 affected rows
cur.execute('UPDATE py_t SET n = n + 1 WHERE id = 6')
if cur.rowcount != 1:
    print('BAD_UPDATE_ROWCOUNT', cur.rowcount)
    sys.exit(1)
cur.execute('DELETE FROM py_t WHERE id = 6')
if cur.rowcount != 1:
    print('BAD_DELETE_ROWCOUNT', cur.rowcount)
    sys.exit(1)

conn.close()
print('ALL_OK')
`

// TestPyMySQLClient 用真实驱动跑完整链路。
func TestPyMySQLClient(t *testing.T) {
	if testing.Short() {
		t.Skip("real-client test skipped in -short")
	}
	py, err := exec.LookPath("python")
	if err != nil {
		t.Skip("python not found")
	}
	if err := exec.Command(py, "-c", "import pymysql").Run(); err != nil {
		t.Skip("pymysql not installed")
	}

	db := newTestDB(t)
	defer db.Close()
	mustRun(t, db, "CREATE TABLE py_t (id INT PRIMARY KEY, name VARCHAR(64), n INT)")
	mustRun(t, db, "INSERT INTO py_t VALUES (1, 'alice', 10), (2, '\u4e2d\u6587', 20)")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go (&mysqlConn{db: db, conn: conn, autocommit: true}).handle()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	script := strings.ReplaceAll(pyMySQLScript, "__PORT__", strconv.Itoa(port))

	cmd := exec.Command(py, "-c", script)
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		t.Fatal("PyMySQL 测试超时（60s），可能有连接/协议挂起")
	}
	if runErr != nil {
		t.Fatalf("PyMySQL 客户端退出异常: %v\n输出:\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "ALL_OK") {
		t.Fatalf("PyMySQL 断言失败:\n%s", out)
	}
	if testing.Verbose() {
		fmt.Printf("PyMySQL: 认证/查询/参数化/错误码/事务/UTF-8 全部通过\n")
	}
}
