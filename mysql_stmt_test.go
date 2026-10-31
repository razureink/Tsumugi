package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ==================== MySQL 协议级测试（C3） ====================
// 覆盖：COM_STMT_CLOSE / RESET / SEND_LONG_DATA、握手能力位、deprecate EOF
// 两种语义、压缩协议、COM_PING / COM_INIT_DB / COM_QUIT。
// 与 mysql_test.go 的 writePkt/readPkt 思路一致，但这里需要按能力位协商，
// 所以封装了一个可配置能力位与压缩的测试客户端。

// protoBaseCaps 通用客户端能力位（与 serverCapabilities 求交集后使用）。
const protoBaseCaps = uint32(clientLongPassword | clientLongFlag | clientProtocol41 |
	clientTransactions | clientSecureConnection | clientPluginAuth |
	clientPluginAuthLenEncClientData)

type protoClient struct {
	t       *testing.T
	conn    net.Conn
	br      *bufio.Reader
	seq     uint8
	caps    uint32 // 协商后的能力位（客户端请求 & 服务端声明）
	srvCaps uint32 // 服务端握手包声明的能力位

	compress bool
	pending  []byte // 解压后尚未消费的字节（跨帧/跨包）

	compSeq     uint8 // 下一个发送帧的压缩序号
	lastCompSeq uint8 // 最近一个接收帧的压缩序号
	lastUlen    int   // 最近一个接收帧的压缩前长度（0 = 未压缩）
	lastSendUln int   // 最近一个发送帧的压缩前长度（0 = 未压缩）
}

// ---- 握手 ----

// parseServerHandshake 解析 Handshake V10，返回服务端能力位与 scramble。
func parseServerHandshake(t *testing.T, pkt []byte) (uint32, []byte) {
	t.Helper()
	if len(pkt) < 1 || pkt[0] != protocolVersion {
		t.Fatalf("bad handshake protocol version: %x", pkt)
	}
	pos := 1
	for pos < len(pkt) && pkt[pos] != 0 {
		pos++
	}
	pos++    // server version NUL
	pos += 4 // thread id
	if pos+8 > len(pkt) {
		t.Fatalf("truncated handshake: %x", pkt)
	}
	auth1 := pkt[pos : pos+8]
	pos += 8 + 1 // auth part 1 + filler
	lo := uint32(binary.LittleEndian.Uint16(pkt[pos : pos+2]))
	pos += 2
	pos += 1 + 2 // collation + status flags
	hi := uint32(binary.LittleEndian.Uint16(pkt[pos : pos+2]))
	pos += 2
	pos += 1 + 10 // auth plugin data len + reserved
	if pos+12 > len(pkt) {
		t.Fatalf("truncated handshake auth data: %x", pkt)
	}
	auth2 := pkt[pos : pos+12]
	scramble := append(append([]byte{}, auth1...), auth2...)
	return lo | hi<<16, scramble
}

// nativeAuthResp 计算 mysql_native_password 认证响应。
func nativeAuthResp(password string, scramble []byte) []byte {
	sha1Pass := sha1.Sum([]byte(password))
	sha1Pass2 := sha1.Sum(sha1Pass[:])
	h := sha1.New()
	h.Write(scramble)
	h.Write(sha1Pass2[:])
	stage2 := h.Sum(nil)
	resp := make([]byte, 20)
	for i := 0; i < 20; i++ {
		resp[i] = sha1Pass[i] ^ stage2[i]
	}
	return resp
}

// dialProto 建立连接并完成握手 + 认证。reqCaps 会被裁剪到服务端声明的能力位，
// 因此可以安全地请求服务端未支持的能力来验证"未声明就不使用"。
func dialProto(t *testing.T, db *DB, reqCaps uint32) *protoClient {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go (&mysqlConn{db: db, conn: conn, autocommit: true}).handle()
		}
	}()
	t.Cleanup(func() { ln.Close() })

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	c := &protoClient{t: t, conn: conn, br: bufio.NewReaderSize(conn, 64*1024)}
	t.Cleanup(func() { conn.Close() })

	hs := c.readRawPacket()
	caps, scramble := parseServerHandshake(t, hs)
	c.srvCaps = caps
	c.caps = (reqCaps | protoBaseCaps) & caps

	auth := nativeAuthResp("password", scramble)
	var resp []byte
	resp = protoU32(resp, c.caps)
	resp = protoU32(resp, 1<<24) // max packet size
	resp = append(resp, defaultCollation)
	resp = append(resp, make([]byte, 23)...)
	resp = append(resp, []byte("root")...)
	resp = append(resp, 0)
	if c.caps&clientPluginAuthLenEncClientData != 0 {
		resp = appendLenEncBytes(resp, auth)
	} else {
		resp = append(resp, byte(len(auth)))
		resp = append(resp, auth...)
	}
	if c.caps&clientPluginAuth != 0 {
		resp = append(resp, []byte("mysql_native_password")...)
		resp = append(resp, 0)
	}
	c.writePacket(resp)

	okPkt := c.readRawPacket()
	if len(okPkt) == 0 || okPkt[0] != 0x00 {
		t.Fatalf("auth failed: %x", okPkt)
	}
	// 认证 OK 必须是非压缩的；到这里才开始使用压缩协议
	if c.caps&clientCompress != 0 {
		c.compress = true
		c.compSeq = 0
		c.pending = nil
	}
	return c
}

// ---- 包收发 ----

func protoU32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func protoU64(b []byte, v uint64) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}

// readN 读 n 个 MySQL 字节（压缩模式下从解压缓冲里取，必要时继续拆帧）。
func (c *protoClient) readN(n int) []byte {
	if n == 0 {
		return nil
	}
	if !c.compress {
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			c.t.Fatalf("read %d bytes: %v", n, err)
		}
		return buf
	}
	out := make([]byte, 0, n)
	for len(out) < n {
		if len(c.pending) == 0 {
			c.readFrame()
		}
		take := n - len(out)
		if take > len(c.pending) {
			take = len(c.pending)
		}
		out = append(out, c.pending[:take]...)
		c.pending = c.pending[take:]
	}
	return out
}

func (c *protoClient) readRawPacket() []byte {
	hdr := c.readN(4)
	length := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	c.seq = hdr[3]
	return c.readN(length)
}

// readFrame 读一个压缩帧并解压到 pending。
func (c *protoClient) readFrame() {
	var hdr [7]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		c.t.Fatalf("read compressed header: %v", err)
	}
	clen := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	ulen := int(hdr[4]) | int(hdr[5])<<8 | int(hdr[6])<<16
	c.lastCompSeq = hdr[3]
	c.lastUlen = ulen
	c.compSeq = hdr[3] + 1
	body := make([]byte, clen)
	if _, err := io.ReadFull(c.br, body); err != nil {
		c.t.Fatalf("read compressed body: %v", err)
	}
	if ulen == 0 {
		c.pending = body
		return
	}
	zr, err := zlib.NewReader(bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("zlib header: %v", err)
	}
	out := make([]byte, ulen)
	if _, err := io.ReadFull(zr, out); err != nil {
		c.t.Fatalf("zlib body: %v", err)
	}
	zr.Close()
	c.pending = out
}

// frameFor 构造一个带包头序号的完整 MySQL 包字节（未加压缩帧）。
func (c *protoClient) frameFor(p []byte) []byte {
	c.seq++
	frame := make([]byte, 4+len(p))
	frame[0] = byte(len(p))
	frame[1] = byte(len(p) >> 8)
	frame[2] = byte(len(p) >> 16)
	frame[3] = c.seq
	copy(frame[4:], p)
	return frame
}

func (c *protoClient) writePacket(p []byte) {
	if !c.compress {
		if _, err := c.conn.Write(c.frameFor(p)); err != nil {
			c.t.Fatalf("write packet: %v", err)
		}
		return
	}
	// 每条命令的压缩序号都从 0 重新开始（与真实客户端一致）
	c.compSeq = 0
	c.writeFrame(c.frameFor(p))
}

// writeFrame 把一段 MySQL 字节（可以只是半个包）包进压缩帧。
// 与真实客户端一致：达到阈值才压缩，否则以 ulen=0 的未压缩帧发送。
func (c *protoClient) writeFrame(data []byte) {
	payload := data
	ulen := 0
	if len(data) >= minCompressLen {
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		if _, err := zw.Write(data); err != nil {
			c.t.Fatalf("zlib write: %v", err)
		}
		if err := zw.Close(); err != nil {
			c.t.Fatalf("zlib close: %v", err)
		}
		if zb.Len() < len(data) {
			payload = zb.Bytes()
			ulen = len(data)
		}
	}
	c.lastSendUln = ulen
	var hdr [7]byte
	hdr[0] = byte(len(payload))
	hdr[1] = byte(len(payload) >> 8)
	hdr[2] = byte(len(payload) >> 16)
	hdr[3] = c.compSeq
	hdr[4] = byte(ulen)
	hdr[5] = byte(ulen >> 8)
	hdr[6] = byte(ulen >> 16)
	c.compSeq++
	if _, err := c.conn.Write(hdr[:]); err != nil {
		c.t.Fatalf("write compressed header: %v", err)
	}
	if _, err := c.conn.Write(payload); err != nil {
		c.t.Fatalf("write compressed body: %v", err)
	}
}

// ---- 响应解析 ----

func errPacketText(p []byte) string {
	if len(p) < 3 {
		return "<short ERR>"
	}
	return string(p[3:])
}

// readOKPacket 断言收到 OK 包并返回原始 payload。
func (c *protoClient) readOKPacket() []byte {
	p := c.readRawPacket()
	if len(p) == 0 {
		c.t.Fatalf("empty packet, want OK")
	}
	if p[0] == 0xff {
		c.t.Fatalf("unexpected ERR %d: %s",
			int(binary.LittleEndian.Uint16(p[1:3])), errPacketText(p))
	}
	if p[0] != 0x00 {
		c.t.Fatalf("want OK packet, got first=0x%02x payload=%x", p[0], p)
	}
	return p
}

// readOK 断言 OK 并返回 affected rows / last insert id。
func (c *protoClient) readOK() (uint64, uint64) {
	p := c.readOKPacket()
	pos := 1
	aff := readLenEncAt(p, &pos)
	last := readLenEncAt(p, &pos)
	return aff, last
}

// readErr 断言 ERR 并返回 errno。
func (c *protoClient) readErr() int {
	p := c.readRawPacket()
	if len(p) < 3 || p[0] != 0xff {
		c.t.Fatalf("want ERR packet, got %x", p)
	}
	return int(binary.LittleEndian.Uint16(p[1:3]))
}

// readLenEncAt 解码一个 length-encoded integer。
func readLenEncAt(b []byte, pos *int) uint64 {
	if *pos >= len(b) {
		return 0
	}
	first := b[*pos]
	switch {
	case first < 251:
		*pos++
		return uint64(first)
	case first == 0xfc && *pos+2 < len(b):
		v := uint64(b[*pos+1]) | uint64(b[*pos+2])<<8
		*pos += 3
		return v
	case first == 0xfd && *pos+3 < len(b):
		v := uint64(b[*pos+1]) | uint64(b[*pos+2])<<8 | uint64(b[*pos+3])<<16
		*pos += 4
		return v
	case first == 0xfe && *pos+8 < len(b):
		var v uint64
		for i := 0; i < 8; i++ {
			v |= uint64(b[*pos+1+i]) << (8 * uint(i))
		}
		*pos += 9
		return v
	}
	return 0
}

func readLenEncStr(b []byte, pos *int) string {
	n := int(readLenEncAt(b, pos))
	if *pos+n > len(b) {
		return ""
	}
	s := string(b[*pos : *pos+n])
	*pos += n
	return s
}

func skipLenEncStr(b []byte, pos *int) {
	readLenEncStr(b, pos)
}

// parseTextRow 解析一行文本协议结果（每列是 lenenc 字符串，0xfb 为 NULL）。
func parseTextRow(pkt []byte) []string {
	var out []string
	pos := 0
	for pos < len(pkt) {
		if pkt[pos] == 0xfb {
			out = append(out, "NULL")
			pos++
			continue
		}
		start := pos
		out = append(out, readLenEncStr(pkt, &pos))
		if pos == start { // 非法标记：跳过 1 字节，避免死循环
			pos++
		}
	}
	return out
}

// query 发 COM_QUERY 并读取文本结果集。
func (c *protoClient) query(sql string) ([]string, [][]string) {
	c.writePacket(append([]byte{comQuery}, []byte(sql)...))
	return c.readTextResultSet()
}

func (c *protoClient) readTextResultSet() ([]string, [][]string) {
	p := c.readRawPacket()
	if len(p) == 0 {
		c.t.Fatalf("empty resultset packet")
	}
	if p[0] == 0xff {
		c.t.Fatalf("query failed %d: %s",
			int(binary.LittleEndian.Uint16(p[1:3])), errPacketText(p))
	}
	pos := 0
	n := int(readLenEncAt(p, &pos))
	if n <= 0 {
		c.t.Fatalf("want result set, got packet %x", p)
	}
	cols := make([]string, 0, n)
	for i := 0; i < n; i++ {
		def := c.readRawPacket()
		dp := 0
		for j := 0; j < 4; j++ { // catalog, schema, table, org_table
			skipLenEncStr(def, &dp)
		}
		cols = append(cols, readLenEncStr(def, &dp))
	}
	// 列定义终止包：deprecate EOF 下不存在
	if c.caps&clientDeprecateEOF == 0 {
		eof := c.readRawPacket()
		if len(eof) != 5 || eof[0] != 0xfe {
			c.t.Fatalf("want EOF after columns, got %x", eof)
		}
	}
	var rows [][]string
	for {
		r := c.readRawPacket()
		if len(r) > 0 && r[0] == 0xfe && (len(r) == 5 || len(r) == 7) {
			break // 行流终止包
		}
		rows = append(rows, parseTextRow(r))
	}
	return cols, rows
}

// prepare 发 COM_STMT_PREPARE 并消费参数定义块。
func (c *protoClient) prepare(sql string) (id uint32, params int) {
	c.writePacket(append([]byte{comStmtPrepare}, []byte(sql)...))
	p := c.readRawPacket()
	if len(p) < 12 || p[0] != 0x00 {
		c.t.Fatalf("prepare failed: %x", p)
	}
	id = binary.LittleEndian.Uint32(p[1:5])
	params = int(binary.LittleEndian.Uint16(p[7:9]))
	if n := int(binary.LittleEndian.Uint16(p[5:7])); n != 0 {
		c.t.Fatalf("server should defer column metadata to EXECUTE, got %d columns", n)
	}
	for i := 0; i < params; i++ {
		def := c.readRawPacket()
		if len(def) == 0 || def[0] == 0xfe || def[0] == 0xff {
			c.t.Fatalf("want param definition packet, got %x", def)
		}
	}
	if params > 0 && c.caps&clientDeprecateEOF == 0 {
		eof := c.readRawPacket()
		if len(eof) != 5 || eof[0] != 0xfe {
			c.t.Fatalf("want EOF after param definitions, got %x", eof)
		}
	}
	return id, params
}

// execHeader 构造 COM_STMT_EXECUTE 头部（含 null bitmap 与参数类型数组）。
func execHeader(stmtID uint32, params int, newBound bool, types []byte) []byte {
	buf := []byte{comStmtExecute}
	buf = protoU32(buf, stmtID)
	buf = append(buf, 0x00) // flags: CURSOR_TYPE_NO_CURSOR
	buf = protoU32(buf, 1)  // iteration count
	buf = append(buf, make([]byte, (params+7)/8)...)
	if params > 0 {
		if newBound {
			buf = append(buf, 0x01)
		} else {
			buf = append(buf, 0x00)
		}
		buf = append(buf, types...)
	}
	return buf
}

func resetPacket(stmtID uint32) []byte {
	return protoU32([]byte{comStmtReset}, stmtID)
}

func closePacket(stmtID uint32) []byte {
	return protoU32([]byte{comStmtClose}, stmtID)
}

func sendLongPacket(stmtID uint32, param uint16, data string) []byte {
	p := []byte{comStmtSendLong}
	p = protoU32(p, stmtID)
	p = append(p, byte(param), byte(param>>8))
	return append(p, data...)
}

func mustRunSQL(t *testing.T, db *DB, sql string) {
	t.Helper()
	if _, _, _, _, err := db.runSQL(sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// ==================== 测试 ====================

// TestMySQLHandshakeCapabilities 校验服务端声明的能力位与实现一致：
// 声明了的都必须真正可用（压缩、deprecate EOF），没实现的（SSL、多语句、
// 多结果集、session track、connect attrs）绝不能声明。
func TestMySQLHandshakeCapabilities(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

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

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	c := &protoClient{t: t, conn: conn, br: bufio.NewReaderSize(conn, 32*1024)}
	caps, _ := parseServerHandshake(t, c.readRawPacket())

	want := uint32(clientLongPassword | clientLongFlag | clientConnectWithDB |
		clientProtocol41 | clientTransactions | clientSecureConnection |
		clientPluginAuth | clientPluginAuthLenEncClientData |
		clientCompress | clientDeprecateEOF)
	if caps&want != want {
		t.Fatalf("server capabilities 0x%08x missing 0x%08x", caps, want&^caps)
	}
	for _, f := range []struct {
		bit  uint32
		name string
	}{
		{1 << 8, "CLIENT_INTERACTIVE"},
		{1 << 11, "CLIENT_SSL"},
		{1 << 16, "CLIENT_MULTI_STATEMENTS"},
		{clientMultiResults, "CLIENT_MULTI_RESULTS"},
		{clientPSMultiResults, "CLIENT_PS_MULTI_RESULTS"},
		{1 << 20, "CLIENT_CONNECT_ATTRS"},
		{1 << 23, "CLIENT_SESSION_TRACK"},
	} {
		if caps&f.bit != 0 {
			t.Fatalf("server advertises %s without implementing it (caps=0x%08x)", f.name, caps)
		}
	}
}

// TestMySQLPingInitDBQuit 校验三个轻量命令的响应语义。
func TestMySQLPingInitDBQuit(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	c := dialProto(t, db, protoBaseCaps)

	// COM_PING → OK，status flags 带 autocommit
	c.writePacket([]byte{comPing})
	ok := c.readOKPacket()
	if flags := binary.LittleEndian.Uint16(ok[3:5]); flags&statusAutocommit == 0 {
		t.Fatalf("PING OK status=0x%04x, want SERVER_STATUS_AUTOCOMMIT", flags)
	}

	// COM_INIT_DB 真正切换连接级默认库。
	// 用一个真实数据库（mysql 库是虚拟系统表，SELECT 会走 info_schema 的专用路径），
	// 先建库+表，再验证裸表名能按连接级 schema 解析。
	mustRunSQL(t, db, "CREATE DATABASE c3_initdb")
	mustRunSQL(t, db, "USE c3_initdb")
	mustRunSQL(t, db, "CREATE TABLE initdb_probe (id INT, PRIMARY KEY (id))")
	mustRunSQL(t, db, "INSERT INTO initdb_probe VALUES (7)")
	mustRunSQL(t, db, "USE tsumugi")
	defer func() { mustRunSQL(t, db, "DROP DATABASE c3_initdb") }()

	// 未切换库时，裸表名解析到 tsumugi，应该找不到
	c.writePacket(append([]byte{comQuery}, []byte("SELECT id FROM initdb_probe")...))
	if code := c.readErr(); code != 1146 {
		t.Fatalf("before COM_INIT_DB: want 1146, got %d", code)
	}

	c.writePacket(append([]byte{comInitDB}, []byte("c3_initdb")...))
	c.readOK()
	if _, rows := c.query("SELECT id FROM initdb_probe"); len(rows) != 1 || rows[0][0] != "7" {
		t.Fatalf("SELECT after COM_INIT_DB c3_initdb = %v, want [[7]]", rows)
	}
	_, rows := c.query("SHOW TABLES")
	found := false
	for _, r := range rows {
		if len(r) > 0 && r[0] == "initdb_probe" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SHOW TABLES after COM_INIT_DB c3_initdb missing initdb_probe: %v", rows)
	}

	// 未知库 → 1049，且连接仍可用
	c.writePacket(append([]byte{comInitDB}, []byte("no_such_db")...))
	if code := c.readErr(); code != 1049 {
		t.Fatalf("COM_INIT_DB unknown db: want 1049, got %d", code)
	}
	c.writePacket([]byte{comPing})
	c.readOK()

	// COM_QUIT → 无响应，连接关闭
	c.writePacket([]byte{comQuit})
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.br.ReadByte(); err == nil {
		t.Fatalf("COM_QUIT must not produce a response and must close the connection")
	}
}

// TestMySQLStmtCloseReset 覆盖 COM_STMT_RESET / COM_STMT_CLOSE 的完整语义。
func TestMySQLStmtCloseReset(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	mustRunSQL(t, db, "CREATE TABLE stmt_life (id INT, val VARCHAR, PRIMARY KEY (id))")

	// 用 deprecate EOF 连接，同时验证 prepare 参数块后没有多余终止包
	c := dialProto(t, db, protoBaseCaps|clientDeprecateEOF)

	id, params := c.prepare("INSERT INTO stmt_life (id, val) VALUES (?, ?)")
	if params != 2 {
		t.Fatalf("want 2 params, got %d", params)
	}
	// prepare 之后不应残留任何包：PING 必须直接拿到 OK
	c.writePacket([]byte{comPing})
	c.readOK()

	// 首次 EXECUTE：带参数类型 + 值
	exec := execHeader(id, 2, true, []byte{0x08, 0x00, 0xfe, 0x00})
	exec = protoU64(exec, 1)
	exec = appendLenEncBytes(exec, []byte("one"))
	c.writePacket(exec)
	if aff, _ := c.readOK(); aff != 1 {
		t.Fatalf("first execute affected=%d, want 1", aff)
	}

	// COM_STMT_RESET：语句保留，回 OK
	c.writePacket(resetPacket(id))
	c.readOK()

	// reset 清除了服务端缓存的参数类型：new_params_bound=0 必须报 1243
	// （libmysql 的 mysql_stmt_reset 会把 send_types_to_server 重新置 1）
	c.writePacket(execHeader(id, 2, false, nil))
	if code := c.readErr(); code != 1243 {
		t.Fatalf("execute after reset with cached types: want 1243, got %d", code)
	}

	// 重新绑定类型后语句仍可用
	exec = execHeader(id, 2, true, []byte{0x08, 0x00, 0xfe, 0x00})
	exec = protoU64(exec, 2)
	exec = appendLenEncBytes(exec, []byte("two"))
	c.writePacket(exec)
	if aff, _ := c.readOK(); aff != 1 {
		t.Fatalf("execute after reset affected=%d, want 1", aff)
	}

	// COM_STMT_CLOSE 无响应体：紧跟的 PING 必须是第一个响应
	c.writePacket(closePacket(id))
	c.writePacket([]byte{comPing})
	c.readOK()

	// 关闭后再 EXECUTE → 1243
	c.writePacket(execHeader(id, 0, false, nil))
	if code := c.readErr(); code != 1243 {
		t.Fatalf("execute after close: want 1243, got %d", code)
	}

	// 对未知语句 CLOSE 同样无响应（不能回 ERR，否则响应错位）
	c.writePacket(closePacket(0xdeadbeef))
	c.writePacket([]byte{comPing})
	c.readOK()

	// RESET 未知语句必须回 1243
	c.writePacket(resetPacket(0xdeadbeef))
	if code := c.readErr(); code != 1243 {
		t.Fatalf("reset unknown statement: want 1243, got %d", code)
	}

	// 0 参数语句：EXECUTE 最小体是 9 字节（不含命令字节），
	// 以前按 10 字节判定畸形，0 参数预处理语句根本无法执行。
	zid, zparams := c.prepare("SELECT COUNT(*) FROM stmt_life")
	if zparams != 0 {
		t.Fatalf("0-param statement reports %d params", zparams)
	}
	zexec := execHeader(zid, 0, false, nil)
	if len(zexec) != 10 {
		t.Fatalf("0-param EXECUTE packet len=%d, want 10 (incl. command byte)", len(zexec))
	}
	c.writePacket(zexec)
	if p := c.readRawPacket(); int(readLenEncAt(p, new(int))) != 1 {
		t.Fatalf("0-param EXECUTE want 1 column, got %x", p)
	}
	c.readRawPacket() // column def（deprecate EOF 下没有终止包）
	if row := c.readRawPacket(); len(row) == 0 || row[0] != 0x00 {
		t.Fatalf("0-param EXECUTE want binary row, got %x", row)
	}
	if end := c.readRawPacket(); len(end) != 7 || end[0] != 0xfe {
		t.Fatalf("0-param EXECUTE want 7-byte OK terminator, got %x", end)
	}

	// 数据只应包含两次成功执行
	_, rows := c.query("SELECT id FROM stmt_life ORDER BY id")
	if len(rows) != 2 || rows[0][0] != "1" || rows[1][0] != "2" {
		t.Fatalf("unexpected rows after close/reset: %v", rows)
	}
}

// TestMySQLStmtSendLongData 覆盖 COM_STMT_SEND_LONG_DATA 的累积、
// EXECUTE 时替代包内值、执行后消费、RESET 清空。
func TestMySQLStmtSendLongData(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	mustRunSQL(t, db, "CREATE TABLE long_t (id INT, val VARCHAR, PRIMARY KEY (id))")

	c := dialProto(t, db, protoBaseCaps|clientDeprecateEOF)
	id, params := c.prepare("INSERT INTO long_t (id, val) VALUES (?, ?)")
	if params != 2 {
		t.Fatalf("want 2 params, got %d", params)
	}

	// 分两段发送长数据（该命令没有任何响应）
	c.writePacket(sendLongPacket(id, 1, "hello"))
	c.writePacket(sendLongPacket(id, 1, " world"))

	// EXECUTE 只给 id 值，val 参数只有类型、没有值：
	// 服务端必须先判定长数据并跳过解析（旧实现会报 truncated param value）
	exec := execHeader(id, 2, true, []byte{0x08, 0x00, 0xfe, 0x00})
	exec = protoU64(exec, 1)
	c.writePacket(exec)
	if aff, _ := c.readOK(); aff != 1 {
		t.Fatalf("execute with long data affected=%d, want 1", aff)
	}
	if _, rows := c.query("SELECT val FROM long_t WHERE id = 1"); len(rows) != 1 || rows[0][0] != "hello world" {
		t.Fatalf("long data not used: %v", rows)
	}

	// 长数据在 EXECUTE 后已消费：普通参数值必须生效
	exec = execHeader(id, 2, true, []byte{0x08, 0x00, 0xfe, 0x00})
	exec = protoU64(exec, 2)
	exec = appendLenEncBytes(exec, []byte("plain"))
	c.writePacket(exec)
	c.readOK()
	if _, rows := c.query("SELECT val FROM long_t WHERE id = 2"); len(rows) != 1 || rows[0][0] != "plain" {
		t.Fatalf("stale long data leaked into next execute: %v", rows)
	}

	// RESET 清空长数据：随后 EXECUTE 的包内值必须优先生效
	c.writePacket(sendLongPacket(id, 1, "ghost"))
	c.writePacket(resetPacket(id))
	c.readOK()
	exec = execHeader(id, 2, true, []byte{0x08, 0x00, 0xfe, 0x00})
	exec = protoU64(exec, 3)
	exec = appendLenEncBytes(exec, []byte("after-reset"))
	c.writePacket(exec)
	c.readOK()
	if _, rows := c.query("SELECT val FROM long_t WHERE id = 3"); len(rows) != 1 || rows[0][0] != "after-reset" {
		t.Fatalf("long data not cleared by COM_STMT_RESET: %v", rows)
	}

	// 未知语句/越界参数的长数据静默丢弃，不能产生响应
	c.writePacket(sendLongPacket(0x9999, 0, "x"))
	c.writePacket(sendLongPacket(id, 9, "y"))
	c.writePacket([]byte{comPing})
	c.readOK()
}

// TestMySQLDeprecateEOF 校验两种 EOF 语义：未协商时列定义块后是 EOF(5B)，
// 行流结束是 EOF(5B)；协商后列定义块后不发终止包，行流结束是 OK(0xfe, 7B)。
func TestMySQLDeprecateEOF(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	mustRunSQL(t, db, "CREATE TABLE eof_t (id INT, name VARCHAR, PRIMARY KEY (id))")
	mustRunSQL(t, db, "INSERT INTO eof_t VALUES (1, 'a'), (2, 'b')")

	for _, deprecate := range []bool{false, true} {
		name := "eof"
		if deprecate {
			name = "deprecate-eof"
		}
		t.Run(name, func(t *testing.T) {
			caps := protoBaseCaps
			if deprecate {
				caps |= clientDeprecateEOF
			}
			c := dialProto(t, db, caps)
			if (c.caps&clientDeprecateEOF != 0) != deprecate {
				t.Fatalf("deprecate EOF negotiation mismatch: caps=0x%08x", c.caps)
			}

			// --- 文本结果集（2 行） ---
			c.writePacket(append([]byte{comQuery}, []byte("SELECT id, name FROM eof_t ORDER BY id")...))
			hdr := c.readRawPacket()
			if int(readLenEncAt(hdr, new(int))) != 2 {
				t.Fatalf("want 2 columns, got %x", hdr)
			}
			c.readRawPacket() // column 1
			c.readRawPacket() // column 2
			rows := 0
			if deprecate {
				// 列定义后必须直接是数据行
				p := c.readRawPacket()
				if len(p) > 0 && p[0] == 0xfe {
					t.Fatalf("deprecate EOF must omit the column-definition terminator, got %x", p)
				}
				rows++
				for {
					p = c.readRawPacket()
					if len(p) > 0 && p[0] == 0xfe {
						if len(p) != 7 {
							t.Fatalf("deprecate EOF terminator must be a 7-byte OK, got %x", p)
						}
						break
					}
					rows++
				}
			} else {
				if eof := c.readRawPacket(); len(eof) != 5 || eof[0] != 0xfe {
					t.Fatalf("want 5-byte EOF after columns, got %x", eof)
				}
				for {
					p := c.readRawPacket()
					if len(p) > 0 && p[0] == 0xfe {
						if len(p) != 5 {
							t.Fatalf("want 5-byte EOF terminator, got %x", p)
						}
						break
					}
					rows++
				}
			}
			if rows != 2 {
				t.Fatalf("want 2 rows, got %d", rows)
			}

			// --- 空结果集：列定义块后立刻是终止包 ---
			// 注意：非 deprecate EOF 下会产生两个 EOF——列定义块结束的 EOF
			// 之后马上是行流结束的 EOF（0 行时两者相邻）。
			c.writePacket(append([]byte{comQuery}, []byte("SELECT id FROM eof_t WHERE id = 999")...))
			c.readRawPacket() // column count
			c.readRawPacket() // column def
			if !deprecate {
				if eof := c.readRawPacket(); len(eof) != 5 || eof[0] != 0xfe {
					t.Fatalf("want 5-byte EOF after columns (empty set), got %x", eof)
				}
			}
			end := c.readRawPacket()
			if len(end) == 0 || end[0] != 0xfe {
				t.Fatalf("want terminator for empty result set, got %x", end)
			}
			if deprecate && len(end) != 7 {
				t.Fatalf("deprecate EOF empty-set terminator len=%d, want 7", len(end))
			}
			if !deprecate && len(end) != 5 {
				t.Fatalf("EOF empty-set terminator len=%d, want 5", len(end))
			}

			// --- PREPARE 参数定义块：deprecate EOF 下不发终止包 ---
			id, params := c.prepare("SELECT id FROM eof_t WHERE id = ?")
			if params != 1 {
				t.Fatalf("want 1 param, got %d", params)
			}
			// 若服务端多发了一个终止包，这里的 PING 会读到它
			c.writePacket([]byte{comPing})
			c.readOK()

			// --- 二进制结果集 ---
			exec := execHeader(id, 1, true, []byte{0x08, 0x00})
			exec = protoU64(exec, 1)
			c.writePacket(exec)
			if p := c.readRawPacket(); int(readLenEncAt(p, new(int))) != 1 {
				t.Fatalf("EXECUTE want 1 column, got %x", p)
			}
			c.readRawPacket() // column def
			if !deprecate {
				if eof := c.readRawPacket(); len(eof) != 5 || eof[0] != 0xfe {
					t.Fatalf("want 5-byte EOF after binary columns, got %x", eof)
				}
			}
			if row := c.readRawPacket(); len(row) == 0 || row[0] != 0x00 {
				t.Fatalf("want binary row packet, got %x", row)
			}
			end = c.readRawPacket()
			if len(end) == 0 || end[0] != 0xfe {
				t.Fatalf("want binary row terminator, got %x", end)
			}
			if deprecate && len(end) != 7 {
				t.Fatalf("deprecate EOF binary terminator len=%d, want 7", len(end))
			}
			if !deprecate && len(end) != 5 {
				t.Fatalf("EOF binary terminator len=%d, want 5", len(end))
			}
		})
	}
}

// TestMySQLCompressedProtocol 校验 CLIENT_COMPRESS：握手/认证不压缩，
// 从第一条命令开始按 [3B 压缩长度][1B 序号][3B 原长][zlib] 收发。
func TestMySQLCompressedProtocol(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	mustRunSQL(t, db, "CREATE TABLE comp_t (id INT, val VARCHAR, PRIMARY KEY (id))")

	// 认证 OK 必须是非压缩包（dialProto 在开启压缩前已经按普通包头读完了它）
	c := dialProto(t, db, protoBaseCaps|clientCompress|clientDeprecateEOF)
	if !c.compress {
		t.Fatalf("compression not negotiated: caps=0x%08x", c.caps)
	}

	// 小命令：合法地使用 ulen=0 的未压缩帧；响应压缩序号从 1 开始
	c.writePacket([]byte{comPing})
	if p := c.readRawPacket(); len(p) == 0 || p[0] != 0x00 {
		t.Fatalf("compressed PING failed: %x", p)
	}
	if c.lastCompSeq != 1 {
		t.Fatalf("response compressed seq=%d, want 1", c.lastCompSeq)
	}
	if c.lastUlen != 0 {
		t.Fatalf("tiny response should be sent uncompressed, ulen=%d", c.lastUlen)
	}

	// 大请求：真实 zlib 压缩后发送，服务端必须解压成功
	big := strings.Repeat("x", 5000)
	insert := "INSERT INTO comp_t (id, val) VALUES (1, '" + big + "')"
	c.writePacket(append([]byte{comQuery}, []byte(insert)...))
	if c.lastSendUln == 0 {
		t.Fatalf("test client did not compress the large request")
	}
	if aff, _ := c.readOK(); aff != 1 {
		t.Fatalf("compressed INSERT affected=%d, want 1", aff)
	}
	if _, rows := c.query("SELECT val FROM comp_t WHERE id = 1"); len(rows) != 1 || rows[0][0] != big {
		t.Fatalf("compressed roundtrip mismatch: got %d rows", len(rows))
	}
	if c.lastUlen == 0 {
		t.Fatalf("large result set should be sent compressed, ulen=%d", c.lastUlen)
	}

	// 一个 MySQL 包头被拆到两个压缩帧：服务端必须按字节流拼接
	frame := c.frameFor([]byte{comPing})
	c.compSeq = 0 // 新命令，压缩序号从 0 开始
	c.writeFrame(frame[:2])
	c.writeFrame(frame[2:])
	if p := c.readRawPacket(); len(p) == 0 || p[0] != 0x00 {
		t.Fatalf("split-packet PING failed: %x", p)
	}
	if c.lastCompSeq != 2 {
		t.Fatalf("response compressed seq=%d, want 2 (after 2 client frames)", c.lastCompSeq)
	}

	// 一个压缩帧里放两条完整命令：第二条从解压缓冲继续读
	c.compSeq = 0
	c.writeFrame(append([]byte{1, 0, 0, 0, comPing}, []byte{1, 0, 0, 0, comPing}...))
	if p := c.readRawPacket(); len(p) == 0 || p[0] != 0x00 {
		t.Fatalf("first pipelined PING failed: %x", p)
	}
	if p := c.readRawPacket(); len(p) == 0 || p[0] != 0x00 {
		t.Fatalf("second pipelined PING failed: %x", p)
	}
}
