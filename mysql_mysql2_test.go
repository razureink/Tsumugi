package main

// ==================== 真实第三方客户端：Node mysql2 ====================
// PyMySQL 走的是文本协议（COM_QUERY + 客户端转义），覆盖不到服务端预编译。
// mysql2 的 execute() 走 COM_STMT_PREPARE / COM_STMT_EXECUTE，结果集是
// **二进制行编码**，与文本协议完全是两套编解码。这里用真实驱动验证它：
//   文本查询 → 二进制预编译查询 → 预编译插入/更新/删除 → errno →
//   事务 → UTF-8。
// 环境没有 node/npm 或安装失败（离线）时自动跳过。

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const mysql2Script = `
const mysql = require('mysql2/promise');

function ok(cond, msg) {
  if (!cond) { console.error('ASSERT:', msg); process.exit(1); }
}

(async () => {
  const port = Number(process.argv[2]);
  const conn = await mysql.createConnection({
    host: '127.0.0.1', port, user: 'root', password: 'password', charset: 'utf8mb4'
  });

  // 1) 文本协议
  let [rows] = await conn.query('SELECT id, name, n FROM n_t ORDER BY id');
  ok(rows.length === 2, 'text rows ' + JSON.stringify(rows));
  ok(rows[0].id === 1 && rows[0].n === 10, 'text row0 ' + JSON.stringify(rows[0]));
  ok(rows[1].name === '\u4e2d\u6587', 'text utf8 ' + JSON.stringify(rows[1]));

  // 2) 二进制预编译协议（COM_STMT_PREPARE/EXECUTE）
  [rows] = await conn.execute('SELECT id, name, n FROM n_t WHERE id = ?', [2]);
  ok(rows.length === 1 && rows[0].n === 20 && rows[0].name === '\u4e2d\u6587',
     'binary row ' + JSON.stringify(rows));

  const [ins] = await conn.execute('INSERT INTO n_t (id, name, n) VALUES (?, ?, ?)', [3, 'bob', 30]);
  ok(ins.affectedRows === 1, 'insert affectedRows ' + ins.affectedRows);
  let [afterIns] = await conn.query('SELECT id, name, n FROM n_t WHERE id = 3');
  ok(afterIns.length === 1 && afterIns[0].name === 'bob', 'inserted row ' + JSON.stringify(afterIns));

  // 3) 错误码
  let dup = null;
  try {
    await conn.execute('INSERT INTO n_t (id, name, n) VALUES (?, ?, ?)', [3, 'dup', 0]);
  } catch (e) { dup = e; }
  let [afterDup] = await conn.query('SELECT id, name, n FROM n_t WHERE id = 3');
  ok(afterDup.length === 1 && afterDup[0].name === 'bob',
     'duplicate must not overwrite ' + JSON.stringify(afterDup));
  ok(dup && dup.errno === 1062,
     'dup errno ' + (dup && dup.errno) + ' code=' + (dup && dup.code) +
     ' msg=' + (dup && dup.message) + ' keys=' + (dup && Object.keys(dup).join(',')));

  let missing = null;
  try { await conn.query('SELECT * FROM n_no_such'); } catch (e) { missing = e; }
  ok(missing && missing.errno === 1146, 'table errno ' + (missing && missing.errno));

  // 4) 事务
  await conn.beginTransaction();
  await conn.execute('INSERT INTO n_t (id, name, n) VALUES (?, ?, ?)', [4, 'rollback', 40]);
  await conn.rollback();
  [rows] = await conn.execute('SELECT COUNT(*) AS c FROM n_t WHERE id = ?', [4]);
  ok(rows[0].c === 0, 'rollback count ' + JSON.stringify(rows));

  await conn.beginTransaction();
  await conn.execute('INSERT INTO n_t (id, name, n) VALUES (?, ?, ?)', [5, 'commit', 50]);
  await conn.commit();
  [rows] = await conn.execute('SELECT n FROM n_t WHERE id = ?', [5]);
  ok(rows[0].n === 50, 'commit ' + JSON.stringify(rows));

  const [u] = await conn.execute('UPDATE n_t SET n = n + 1 WHERE id = ?', [5]);
  ok(u.affectedRows === 1, 'update affectedRows');
  const [d] = await conn.execute('DELETE FROM n_t WHERE id = ?', [5]);
  ok(d.affectedRows === 1, 'delete affectedRows');

  await conn.end();
  console.log('ALL_OK');
})().catch(e => { console.error('FAIL:', e); process.exit(1); });
`

// TestNodeMySQL2Client 用 mysql2 跑文本 + 二进制两套协议。
func TestNodeMySQL2Client(t *testing.T) {
	if testing.Short() {
		t.Skip("real-client test skipped in -short")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	npm, err := exec.LookPath("npm.cmd")
	if err != nil {
		npm, err = exec.LookPath("npm")
		if err != nil {
			t.Skip("npm not found")
		}
	}

	db := newTestDB(t)
	defer db.Close()
	mustRun(t, db, "CREATE TABLE n_t (id INT PRIMARY KEY, name VARCHAR(64), n INT)")
	mustRun(t, db, "INSERT INTO n_t VALUES (1, 'alice', 10), (2, '\u4e2d\u6587', 20)")

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "package.json"),
		[]byte(`{"name":"tsumugi-client-test","private":true}`), 0600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	install := exec.Command(npm, "install", "mysql2", "--no-audit", "--no-fund", "--loglevel=error")
	install.Dir = work
	if out, err := install.CombinedOutput(); err != nil {
		t.Skipf("npm install mysql2 failed (offline?): %v\n%s", err, out)
	}
	scriptPath := filepath.Join(work, "client.js")
	if err := os.WriteFile(scriptPath, []byte(mysql2Script), 0600); err != nil {
		t.Fatalf("write client.js: %v", err)
	}

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

	cmd := exec.Command(node, scriptPath, strconv.Itoa(port))
	cmd.Dir = work
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		t.Fatal("mysql2 测试超时（90s）")
	}
	if runErr != nil {
		t.Fatalf("mysql2 客户端退出异常: %v\n输出:\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "ALL_OK") {
		t.Fatalf("mysql2 断言失败:\n%s", out)
	}
}
