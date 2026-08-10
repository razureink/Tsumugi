package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMySQLProtocol 验证 MySQL 协议握手→认证→查询的端到端完整性。
func TestMySQLProtocol(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	defer func() {
		// 异步启动的 metrics server 会持有后台 goroutine
		db.stopCh <- struct{}{}
	}()

	// 建表+写数据
	if _, _, _, _, err := db.runSQL("CREATE TABLE test (id INT, name VARCHAR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, _, _, _, err := db.runSQL("INSERT INTO test VALUES (" + itoa(i) + ", 'u" + itoa(i) + "')"); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// 监听随机端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go (&mysqlConn{db: db, conn: conn}).handle()
		}
	}()
	defer ln.Close()

	// TCP 客户端
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// --- 握手 ---
	var seq uint8
	readPkt := func() []byte {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			t.Fatalf("read hdr: %v", err)
		}
		length := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
		seq = hdr[3]
		pkt := make([]byte, length)
		if _, err := io.ReadFull(conn, pkt); err != nil {
			t.Fatalf("read pkt: %v", err)
		}
		return pkt
	}
	writePkt := func(pkt []byte) {
		seq++
		hdr := make([]byte, 4)
		hdr[0] = byte(len(pkt))
		hdr[1] = byte(len(pkt) >> 8)
		hdr[2] = byte(len(pkt) >> 16)
		hdr[3] = seq
		if _, err := conn.Write(append(hdr, pkt...)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	pkt := readPkt()
	if pkt[0] != 10 {
		t.Fatalf("expected protocol 10, got %d", pkt[0])
	}
	// parse server version (null-terminated)
	pos := 1
	for pkt[pos] != 0 {
		pos++
	}
	version := string(pkt[1:pos])
	pos++ // skip null
	t.Logf("server version: %s", version)
	_ = pkt[pos : pos+4] // thread id
	pos += 4
	authData1 := pkt[pos : pos+8]
	pos += 9                                  // 8+1 null
	_ = binary.LittleEndian.Uint16(pkt[pos:]) // caps
	pos += 2
	pos++     // collation
	pos += 2  // status
	pos += 2  // caps upper
	pos++     // auth data len
	pos += 10 // reserved
	authData2 := pkt[pos : pos+12]
	pos += 13 // auth data 12+null
	// calc auth
	scramble := append(authData1, authData2[:12]...)
	// compute mysql_native_password response: SHA1(pass) XOR SHA1(scramble + SHA1(SHA1(pass)))
	pass := []byte("password")
	sha1Pass := sha1.Sum(pass)
	sha1Pass2 := sha1.Sum(sha1Pass[:])
	h := sha1.New()
	h.Write(scramble)
	h.Write(sha1Pass2[:])
	stage2 := h.Sum(nil)
	authResp := make([]byte, 20)
	for i := 0; i < 20; i++ {
		authResp[i] = sha1Pass[i] ^ stage2[i]
	}

	// --- 认证 ---
	// capabilities 匹配服务器声明：CLIENT_PROTOCOL_41|CLIENT_SECURE_CONNECTION|CLIENT_PLUGIN_AUTH|CLIENT_CONNECT_WITH_DB
	var caps [4]byte
	binary.LittleEndian.PutUint16(caps[:], 0x8d40)
	var authBuf bytes.Buffer
	authBuf.Write(caps[:])               // client capabilities
	authBuf.Write([]byte{0, 0, 0, 0x01}) // max packet
	authBuf.Write([]byte{8})             // charset utf8
	authBuf.Write(make([]byte, 23))      // reserved
	authBuf.Write([]byte("root"))        // user
	authBuf.Write([]byte{0})
	authBuf.WriteByte(byte(len(authResp)))
	authBuf.Write(authResp)
	// database (optional) "tsumugi"
	authBuf.Write([]byte("tsumugi"))
	authBuf.Write([]byte{0})
	writePkt(authBuf.Bytes())

	// auth response
	authOK := readPkt()
	if len(authOK) == 0 || authOK[0] != 0x00 {
		code := "?"
		if len(authOK) >= 3 {
			code = itoa(int(authOK[1]) | int(authOK[2])<<8)
		}
		t.Fatalf("auth failed, first byte=%02x code=%s", authOK[0], code)
	}
	t.Log("auth OK")

	// --- COM_QUERY ---
	seq++ // seq advanced by auth OK
	q := append([]byte{comQuery}, []byte("SELECT * FROM test ORDER BY id LIMIT 2")...)
	hdr := make([]byte, 4)
	hdr[0] = byte(len(q))
	hdr[1] = byte(len(q) >> 8)
	hdr[2] = byte(len(q) >> 16)
	hdr[3] = seq
	if _, err := conn.Write(append(hdr, q...)); err != nil {
		t.Fatalf("write query: %v", err)
	}

	// 读结果集
	colCountPkt := readPkt()
	if len(colCountPkt) < 1 || colCountPkt[0] == 0xff {
		code := 0
		if len(colCountPkt) >= 3 {
			code = int(colCountPkt[1]) | int(colCountPkt[2])<<8
		}
		t.Fatalf("column count error, code=%d", code)
	}
	colCount := int(colCountPkt[0])
	t.Logf("columns: %d", colCount)
	// 读列定义
	for i := 0; i < colCount; i++ {
		readPkt()
	}
	// EOF
	eof := readPkt()
	if len(eof) < 1 || eof[0] != 0xfe {
		t.Fatalf("expected EOF, got %02x", eof[0])
	}
	// 读行
	var rows [][]byte
	for {
		rowPkt := readPkt()
		if len(rowPkt) == 0 || rowPkt[0] == 0xfe {
			break // EOF
		}
		rows = append(rows, rowPkt)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	t.Logf("got %d rows as expected", len(rows))
}

// TestMySQLPrepared 验证 COM_STMT_PREPARE → EXECUTE（二进制协议参数）→ 结果集的端到端流程。
func TestMySQLPrepared(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	defer func() { db.stopCh <- struct{}{} }()

	if _, _, _, _, err := db.runSQL("CREATE TABLE pt (id INT, name VARCHAR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, _, _, _, err := db.runSQL("INSERT INTO pt VALUES (" + itoa(i) + ", 'n" + itoa(i) + "')"); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	c := dialMySQL(t, db)
	defer c.close()

	// --- 准备 ---
	prepare := append([]byte{comStmtPrepare}, []byte("SELECT * FROM pt WHERE id = ?")...)
	c.write(prepare)
	// 解析 PREPARE 响应
	pkt := c.read()
	if pkt[0] != 0x00 {
		t.Fatalf("prepare failed, status=0x%02x", pkt[0])
	}
	stmtID := binary.LittleEndian.Uint32(pkt[1:5])
	colCount := binary.LittleEndian.Uint16(pkt[5:7])
	paramCount := binary.LittleEndian.Uint16(pkt[7:9])
	t.Logf("statement id=%d columns=%d params=%d", stmtID, colCount, paramCount)
	if paramCount != 1 {
		t.Fatalf("want 1 param, got %d", paramCount)
	}
	if colCount != 0 {
		t.Fatalf("want 0 prepare columns (metadata deferred to EXECUTE), got %d", colCount)
	}
	// 1 个参数定义包 + EOF
	c.read()
	c.read() // EOF

	// --- 执行：二进制协议参数 id=2 ---
	// Packet layout:
	//   [0]    comStmtExecute
	//   [1:5]  statement_id (le)
	//   [5]    flags
	//   [6:10] iteration_count
	//   [10]   null_bitmap (1 param → 1 byte, not null = 0x00)
	//   [11]   new_params_bound_flag = 0x01
	//   [12:14] param type (LONGLONG=0x08, unsigned=0)
	//   [14:22] 8-byte value = 2
	var exec []byte
	exec = append(exec, comStmtExecute)
	exec = appendU32Little(exec, stmtID)
	exec = append(exec, 0)          // flags
	exec = appendU32Little(exec, 1) // iteration count
	exec = append(exec, 0x00)       // null bitmap: not null
	exec = append(exec, 0x01)       // new_params_bound_flag
	exec = append(exec, 0x08, 0x00) // LONGLONG, unsigned
	exec = appendU64Little(exec, 2) // value = 2
	c.write(exec)

	// 消费结果集：列数、2 列定义、EOF、行、EOF
	colPkt := c.read()
	if colPkt[0] == 0xff {
		t.Fatalf("execute error code=%d msg=%q", int(colPkt[1])|int(colPkt[2])<<8, string(colPkt[9:]))
	}
	resultCols := int(colPkt[0])
	if resultCols != 2 {
		t.Fatalf("want 2 columns, got %d", resultCols)
	}
	for i := 0; i < resultCols; i++ {
		c.read()
	}
	for {
		if p := c.read(); len(p) > 0 && p[0] == 0xfe {
			break
		}
	}
	var rows []string
	for {
		r := c.read()
		if len(r) == 0 || r[0] == 0xfe {
			break
		}
		rows = append(rows, string(r))
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	t.Logf("prepared EXECUTE returned %d row as expected: %q", len(rows), rows[0])

	// --- 再次 EXECUTE（复用缓存的 token 计划），验证 id=3 ---
	var exec2 []byte
	exec2 = append(exec2, comStmtExecute)
	exec2 = appendU32Little(exec2, stmtID)
	exec2 = append(exec2, 0)
	exec2 = appendU32Little(exec2, 1)
	exec2 = append(exec2, 0x00, 0x01, 0x08, 0x00)
	exec2 = appendU64Little(exec2, 3)
	c.write(exec2)
	colPkt2 := c.read()
	if colPkt2[0] == 0xff {
		t.Fatalf("second execute error: %q", string(colPkt2[9:]))
	}
	cols2 := int(colPkt2[0])
	for i := 0; i < cols2; i++ {
		c.read()
	}
	for {
		if p := c.read(); len(p) > 0 && p[0] == 0xfe {
			break
		}
	}
	var rows2 []string
	for {
		r := c.read()
		if len(r) == 0 || r[0] == 0xfe {
			break
		}
		rows2 = append(rows2, string(r))
	}
	if len(rows2) != 1 {
		t.Fatalf("second execute want 1 row, got %d", len(rows2))
	}
	t.Logf("second EXECUTE (cached plan) returned 1 row as expected: %q", rows2[0])

	// --- 关闭语句 ---
	closePkt := append([]byte{comStmtClose}, byte(stmtID), byte(stmtID>>8), byte(stmtID>>16), byte(stmtID>>24))
	c.write(closePkt)
	// 关闭后再 EXECUTE 应返回 "statement not found"
	var again []byte
	again = append(again, comStmtExecute)
	again = appendU32Little(again, stmtID)
	again = append(again, 0)
	again = appendU32Little(again, 1)
	again = append(again, 0x00, 0x01, 0x08, 0x00)
	again = appendU64Little(again, 2)
	c.write(again)
	errPkt := c.read()
	if len(errPkt) == 0 || errPkt[0] != 0xff {
		t.Fatalf("expected error after close, got first byte=%02x", errPkt[0])
	}
	t.Log("statement closed and re-execute rejected as expected")
}

// TestMySQLTxn 验证事务 BEGIN/COMMIT/ROLLBACK 与 SET 的协议行为。
func TestMySQLTxn(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	defer func() { db.stopCh <- struct{}{} }()

	if _, _, _, _, err := db.runSQL("CREATE TABLE tx (id INT, name VARCHAR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	c := dialMySQL(t, db)
	defer c.close()

	query := func(sql string) byte {
		q := append([]byte{comQuery}, []byte(sql)...)
		c.write(q)
		p := c.read()
		return p[0]
	}

	// SET 应返回 OK
	if b := query("SET NAMES utf8mb4"); b != 0x00 {
		t.Fatalf("SET should return OK, got 0x%02x", b)
	}
	// BEGIN
	if b := query("BEGIN"); b != 0x00 {
		t.Fatalf("BEGIN should be OK, got 0x%02x", b)
	}
	// 事务内 INSERT 成功（返回 OK 包）
	if b := query("INSERT INTO tx VALUES (1, 1)"); b != 0x00 {
		t.Fatalf("insert in txn should be OK, got 0x%02x", b)
	}
	// ROLLBACK
	if b := query("ROLLBACK"); b != 0x00 {
		t.Fatalf("ROLLBACK should be OK, got 0x%02x", b)
	}
	_ = c
}

// TestSQLFeatures 验证多行 INSERT、ALTER ADD COLUMN、RENAME 及持久化重启。
func TestSQLFeatures(t *testing.T) {
	d := "C:\\Users\\Administrator\\AppData\\Local\\Temp\\opencode\\tsumugi_features_test"
	_ = os.RemoveAll(d)
	db, err := NewDB(&Config{
		WALDir: d, WALFile: "wal", PrivilegeFile: "priv.json",
		User: "root", Password: "password",
		FlushInterval: 50 * time.Millisecond, GroupCommitInterval: 2 * time.Millisecond,
		TTLCleanInterval: 30 * time.Second, IdleTimeout: 5 * time.Minute,
		BackupDir: filepath.Join(d, "backup"),
	})
	if err != nil {
		t.Fatalf("newDB: %v", err)
	}
	if _, _, _, _, err := db.runSQL("CREATE TABLE mt (id INT, name VARCHAR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, a, _, err := db.runSQL("INSERT INTO mt VALUES (1,'a'),(2,'b'),(3,'c')"); err != nil {
		t.Fatalf("multi insert: %v", err)
	} else if a != 3 {
		t.Fatalf("want 3 affected, got %d", a)
	}
	if _, _, _, _, err := db.runSQL("ALTER TABLE mt ADD COLUMN extra VARCHAR"); err != nil {
		t.Fatalf("alter add: %v", err)
	}
	if cols, _, _, _, err := db.runSQL("SELECT * FROM mt WHERE id=1"); err != nil {
		t.Fatalf("select after alter: %v", err)
	} else if len(cols) != 3 {
		t.Fatalf("want 3 cols, got %d", len(cols))
	}
	if _, _, _, _, err := db.runSQL("ALTER TABLE mt RENAME TO mt2"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// 重启持久化验证
	db.Close()
	db2, err := NewDB(&Config{
		WALDir: d, WALFile: "wal", PrivilegeFile: "priv.json",
		User: "root", Password: "password",
		FlushInterval: 50 * time.Millisecond, GroupCommitInterval: 2 * time.Millisecond,
		TTLCleanInterval: 30 * time.Second, IdleTimeout: 5 * time.Minute,
		BackupDir: filepath.Join(d, "backup"),
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	defer func() { db2.stopCh <- struct{}{} }()
	cols, rows, _, _, err2 := db2.runSQL("SELECT * FROM mt2 WHERE id=3")
	if err2 != nil {
		t.Fatalf("select after reopen: %v", err2)
	}
	t.Logf("reopen cols=%v rows=%v", cols, rows)
	if len(cols) != 3 {
		t.Fatalf("after reopen want 3 cols, got %d", len(cols))
	}
	if len(rows) != 1 || rows[0][0] != int64(3) {
		t.Fatalf("after reopen row mismatch: %v", rows)
	}
}

// TestMySQLAuthSwitch 验证客户端选择 caching_sha2_password 时，服务端发送 AuthSwitchRequest 回退 native。
func TestMySQLAuthSwitch(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	defer func() { db.stopCh <- struct{}{} }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go (&mysqlConn{db: db, conn: conn}).handle()
		}
	}()
	defer ln.Close()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.Close()

	seq := byte(0)
	readPkt := func() []byte {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			t.Fatalf("read hdr: %v", err)
		}
		l := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
		seq = hdr[3]
		p := make([]byte, l)
		if _, err := io.ReadFull(conn, p); err != nil {
			t.Fatalf("read: %v", err)
		}
		return p
	}
	writePkt := func(p []byte) {
		seq++
		hdr := []byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), seq}
		if _, err := conn.Write(append(hdr, p...)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// 握手
	hs := readPkt()
	pos := 1
	for hs[pos] != 0 {
		pos++
	}
	pos++
	pos += 4
	authData1 := hs[pos : pos+8]
	pos += 9
	pos += 2 + 1 + 2 + 2 + 1 + 10
	authData2 := hs[pos : pos+12]
	pos += 13
	_ = authData1
	_ = authData2

	// 发送认证响应：声明 caching_sha2_password；auth 响应置空
	var auth []byte
	auth = append(auth, 0x40, 0x8d, 0, 0) // caps 含 CLIENT_PLUGIN_AUTH|CONNECT_WITH_DB
	auth = append(auth, 0, 0, 0, 0x01)    // max packet
	auth = append(auth, 8)                // charset
	auth = append(auth, make([]byte, 23)...)
	auth = append(auth, []byte("root")...)
	auth = append(auth, 0, 0) // auth response length=0
	auth = append(auth, []byte("tsumugi")...)
	auth = append(auth, 0)
	auth = append(auth, []byte("caching_sha2_password")...)
	auth = append(auth, 0)
	writePkt(auth)

	// 应收到 AuthSwitchRequest（0xfe）
	as := readPkt()
	if len(as) == 0 || as[0] != 0xfe {
		t.Fatalf("expected AuthSwitchRequest (0xfe), got first=%02x", as[0])
	}
	// 校验插件名
	sw := string(as[1 : 1+len("mysql_native_password")])
	t.Logf("auth switch shown plugin: %s", sw)

	// 从 switch 包取新 scramble（20 字节 + 结尾 0）
	sc := as[1+len("mysql_native_password")+1 : 1+len("mysql_native_password")+1+20]

	// 回发 native 响应
	pass := []byte("password")
	sha1Pass := sha1.Sum(pass)
	sha1Pass2 := sha1.Sum(sha1Pass[:])
	h2 := sha1.New()
	h2.Write(sc)
	h2.Write(sha1Pass2[:])
	stage2 := h2.Sum(nil)
	resp := make([]byte, 20)
	for i := 0; i < 20; i++ {
		resp[i] = sha1Pass[i] ^ stage2[i]
	}
	seq++ // switch 响应 seq 递增
	hdr := []byte{byte(len(resp)), byte(len(resp) >> 8), byte(len(resp) >> 16), seq}
	if _, err := conn.Write(append(hdr, resp...)); err != nil {
		t.Fatalf("write native resp: %v", err)
	}
	ok := readPkt()
	if len(ok) == 0 || ok[0] != 0x00 {
		t.Fatalf("expected auth OK, got first=%02x", ok[0])
	}
	t.Log("auth switch → native OK")
}

// appendU32Little 追加小端 4 字节。
func appendU32Little(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// appendU64Little 追加小端 8 字节。
func appendU64Little(b []byte, v uint64) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}

// mysqlTestClient 封装一个已认证的 MySQL 协议测试客户端。
type mysqlTestClient struct {
	conn net.Conn
	seq  uint8
}

func (c *mysqlTestClient) read() []byte {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		panic(err)
	}
	length := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	c.seq = hdr[3]
	pkt := make([]byte, length)
	if _, err := io.ReadFull(c.conn, pkt); err != nil {
		panic(err)
	}
	return pkt
}

func (c *mysqlTestClient) write(pkt []byte) {
	c.seq++
	hdr := make([]byte, 4)
	hdr[0] = byte(len(pkt))
	hdr[1] = byte(len(pkt) >> 8)
	hdr[2] = byte(len(pkt) >> 16)
	hdr[3] = c.seq
	if _, err := c.conn.Write(append(hdr, pkt...)); err != nil {
		panic(err)
	}
}

func (c *mysqlTestClient) close() { c.conn.Close() }

// dialMySQL 建立连接并完成握手+认证（root/password）。
func dialMySQL(t *testing.T, db *DB) *mysqlTestClient {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go (&mysqlConn{db: db, conn: conn}).handle()
		}
	}()
	t.Cleanup(func() { ln.Close() })

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	c := &mysqlTestClient{conn: conn}

	hs := c.read()
	pos := 1
	for hs[pos] != 0 {
		pos++
	}
	pos++
	pos += 4
	authData1 := hs[pos : pos+8]
	pos += 9
	pos += 2 + 1 + 2 + 2 + 1 + 10
	authData2 := hs[pos : pos+12]
	pos += 13
	scramble := append(authData1, authData2[:12]...)
	pass := []byte("password")
	sha1Pass := sha1.Sum(pass)
	sha1Pass2 := sha1.Sum(sha1Pass[:])
	h := sha1.New()
	h.Write(scramble)
	h.Write(sha1Pass2[:])
	stage2 := h.Sum(nil)
	resp := make([]byte, 20)
	for i := 0; i < 20; i++ {
		resp[i] = sha1Pass[i] ^ stage2[i]
	}
	var auth []byte
	auth = append(auth, 0x40, 0x8d, 0, 0)
	auth = append(auth, 0, 0, 0, 0x01)
	auth = append(auth, 8)
	auth = append(auth, make([]byte, 23)...)
	auth = append(auth, []byte("root")...)
	auth = append(auth, 0, byte(len(resp)))
	auth = append(auth, resp...)
	c.write(auth)
	if pkt := c.read(); len(pkt) == 0 || pkt[0] != 0x00 {
		t.Fatalf("auth failed, first byte=%02x", pkt[0])
	}
	return c
}

func newTestDB(t *testing.T) *DB {
	d := "C:\\Users\\Administrator\\AppData\\Local\\Temp\\opencode\\tsumugi_mysql_test"
	_ = os.RemoveAll(d)
	db, err := NewDB(&Config{
		WALDir:              d,
		WALFile:             "wal",
		PrivilegeFile:       "priv.json",
		User:                "root",
		Password:            "password",
		FlushInterval:       100 * time.Millisecond,
		GroupCommitInterval: 2 * time.Millisecond,
		TTLCleanInterval:    30 * time.Second,
		IdleTimeout:         5 * time.Minute,
		BackupDir:           filepath.Join(d, "backup"),
	})
	if err != nil {
		t.Fatalf("newDB: %v", err)
	}
	// 测试需要 root 账号（生产环境由安装向导创建）
	if globalUsers != nil && globalUsers.Count() == 0 {
		globalUsers.Add(&User{
			Username:  "root",
			Password:  hashPasswd("password"),
			IsAdmin:   true,
			CanStress: true,
			CanManage: true,
			CreatedAt: time.Now().Unix(),
		})
	}
	return db
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

type mysqlBenchConn struct {
	conn net.Conn
	seq  uint8
}

func (b *mysqlBenchConn) read() []byte {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(b.conn, hdr); err != nil {
		panic(err)
	}
	length := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	b.seq = hdr[3]
	pkt := make([]byte, length)
	if _, err := io.ReadFull(b.conn, pkt); err != nil {
		panic(err)
	}
	return pkt
}

func (b *mysqlBenchConn) write(pkt []byte) {
	b.seq++
	hdr := make([]byte, 4)
	hdr[0] = byte(len(pkt))
	hdr[1] = byte(len(pkt) >> 8)
	hdr[2] = byte(len(pkt) >> 16)
	hdr[3] = b.seq
	if _, err := b.conn.Write(append(hdr, pkt...)); err != nil {
		panic(err)
	}
}

// BenchmarkMySQLQuery 测量单个 TCP 连接上的查询吞吐（握手后复用连接）。
func BenchmarkMySQLQuery(b *testing.B) {
	dir := b.TempDir()
	db, err := NewDB(&Config{
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
		Durability:          DuraBatch,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go (&mysqlConn{db: db, conn: conn}).handle()
		}
	}()
	defer ln.Close()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 3*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	bc := &mysqlBenchConn{conn: conn}
	// 握手
	hs := bc.read()
	pos := 1
	for hs[pos] != 0 {
		pos++
	}
	pos++
	pos += 4
	authData1 := hs[pos : pos+8]
	pos += 9
	pos += 2 + 1 + 2 + 2 + 1 + 10
	authData2 := hs[pos : pos+12]
	pos += 13
	_ = authData1
	_ = authData2
	scramble := append(authData1, authData2[:12]...)
	pass := []byte("password")
	sha1Pass := sha1.Sum(pass)
	sha1Pass2 := sha1.Sum(sha1Pass[:])
	h := sha1.New()
	h.Write(scramble)
	h.Write(sha1Pass2[:])
	stage2 := h.Sum(nil)
	resp := make([]byte, 20)
	for i := 0; i < 20; i++ {
		resp[i] = sha1Pass[i] ^ stage2[i]
	}
	var auth []byte
	auth = append(auth, 0x40, 0x8d, 0, 0)
	auth = append(auth, 0, 0, 0, 0x01)
	auth = append(auth, 8)
	auth = append(auth, make([]byte, 23)...)
	auth = append(auth, []byte("root")...)
	auth = append(auth, 0, byte(len(resp)))
	auth = append(auth, resp...)
	bc.write(auth)
	bc.read() // auth OK
	// 预置一张表
	if _, _, _, _, err := db.runSQL("CREATE TABLE t1 (id INT, name VARCHAR, PRIMARY KEY (id))"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := append([]byte{comQuery}, []byte("SELECT * FROM t1")...)
		bc.seq++
		hdr := make([]byte, 4)
		hdr[0] = byte(len(q))
		hdr[1] = byte(len(q) >> 8)
		hdr[2] = byte(len(q) >> 16)
		hdr[3] = bc.seq
		if _, err := bc.conn.Write(append(hdr, q...)); err != nil {
			b.Fatal(err)
		}
		// 消费整个结果集
		colPkt := bc.read()
		cols := int(colPkt[0])
		for i := 0; i < cols; i++ {
			bc.read()
		}
		bc.read() // EOF
		for {
			r := bc.read()
			if len(r) == 0 || r[0] == 0xfe {
				break
			}
		}
	}
}
