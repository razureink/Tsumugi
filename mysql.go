package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
)

// ==================== MySQL 协议兼容（文本/二进制协议子集） ====================
// 仅使用标准库，实现：
//   - 握手 V10 + native password 认证（mysql_native_password）与 caching_sha2_password 协商
//   - COM_QUERY / COM_PING / COM_QUIT / COM_INIT_DB / COM_FIELD_LIST
//   - COM_STMT_PREPARE / COM_STMT_EXECUTE / COM_STMT_CLOSE / COM_STMT_RESET / COM_STMT_SEND_LONG_DATA
//   - 结果集（列定义 + EOF + 文本行）与 OK/ERR 包
// 支持语句由 runSQL 解析器决定（SELECT/SHOW/DESCRIBE/INSERT/UPDATE/DELETE/CREATE/DROP + 事务）。

const (
	protocolVersion   = 10
	defaultCollation  = 45 // utf8mb4_general_ci
	statusAutocommit  = 0x0002
	longFlagOpened    = 0x0080

	comQuit         = 0x01
	comInitDB       = 0x02
	comQuery        = 0x03
	comFieldList    = 0x04
	comPing         = 0x0e
	comStmtPrepare  = 0x16
	comStmtExecute  = 0x17
	comStmtSendLong = 0x18
	comStmtClose    = 0x19
	comStmtReset    = 0x1a
	comSetOption    = 0x1b
	comStmtFetch    = 0x1c
)

const serverCapabilities = (1 << 3) | // CLIENT_PROTOCOL_41
	(1 << 5) | // CLIENT_CONNECT_WITH_DB
	(1 << 8) | // CLIENT_SECURE_CONNECTION
	(1 << 12) | // CLIENT_PLUGIN_AUTH
	(1 << 15) // CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA

var errBadPacket = fmt.Errorf("bad packet")
var errAccessDenied = fmt.Errorf("access denied")

func startMySQLServer(db *DB, port int) {
	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logf(LOG_ERR, "mysql listen %s: %v", addr, err)
		return
	}
	defer ln.Close()
	logf(LOG_VERB, "mysql server listening on %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			logf(LOG_ERR, "mysql accept: %v", err)
			return
		}
		go (&mysqlConn{db: db, conn: conn}).handle()
	}
}

type mysqlConn struct {
	db       *DB
	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	seq      uint8
	user     string
	scramble []byte
	nextStmt uint32           // 下一个可用的 prepared 语句 ID
	prepared map[uint32]*mysqlPreparedStmt
	longData map[uint32][]byte // COM_STMT_SEND_LONG_DATA 累积缓冲
	inTxn    bool
	txnID    uint64
}

// mysqlPreparedStmt 记录一条已解析的 prepared statement。
type mysqlPreparedStmt struct {
	id         uint32
	sql        string
	paramCount int
	toks       []sqlToken // 缓存分词结果，EXECUTE 复用避免重复分词
}

func (mc *mysqlConn) handle() {
	defer mc.conn.Close()
	defer func() {
		if r := recover(); r != nil {
			logf(LOG_ERR, "mysql conn panic: %v", r)
		}
	}()
	mc.br = bufio.NewReaderSize(mc.conn, 32*1024)
	mc.bw = bufio.NewWriterSize(mc.conn, 64*1024)
	mc.conn.SetDeadline(time.Now().Add(120 * time.Second))
	if err := mc.sendHandshake(); err != nil {
		return
	}
	if err := mc.readAuth(); err != nil {
		return
	}
	mc.conn.SetDeadline(time.Now().Add(24 * time.Hour))
	for {
		cmd, payload, err := mc.readPacket()
		if err != nil {
			return
		}
		switch cmd {
		case comQuit:
			return
		case comPing:
			mc.writeOK()
		case comInitDB:
			mc.writeOK()
		case comQuery:
			mc.handleQuery(string(payload))
		case comStmtPrepare:
			mc.handlePrepare(string(payload))
		case comStmtExecute:
			mc.handleExecute(payload)
		case comStmtClose:
			mc.handleStmtClose(payload)
		case comStmtReset:
			mc.handleStmtReset(payload)
		case comStmtSendLong:
			mc.handleSendLong(payload)
		default:
			mc.sendError(1047, fmt.Sprintf("unknown command %d", cmd))
		}
		// 每个命令处理后一次性 flush：多条结果包（列定义+行）合成更大的 TCP 写块，
		// 大幅减少系统调用，同时保持包边界（读侧按头解析）。
		if err := mc.bw.Flush(); err != nil {
			return
		}
	}
}

// ---- 包收发 ----

func (mc *mysqlConn) writePacket(payload []byte) error {
	return writeMysqlPacket(mc.bw, &mc.seq, payload)
}

// writeMysqlPacket 发送一个（可能多分片的）MySQL 包。用栈上 4 字节头部并分开写，
// 避免每包一次的堆分配与整包拷贝（append(hdr, chunk...)）。
func writeMysqlPacket(w io.Writer, seq *uint8, payload []byte) error {
	var hdr [4]byte
	for len(payload) > 0 {
		chunk := payload
		if len(chunk) > 0xffffff {
			chunk = chunk[:0xffffff]
			payload = payload[0xffffff:]
		} else {
			payload = payload[:0]
		}
		hdr[0] = byte(len(chunk))
		hdr[1] = byte(len(chunk) >> 8)
		hdr[2] = byte(len(chunk) >> 16)
		hdr[3] = *seq
		*seq++
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := w.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (mc *mysqlConn) readPacket() (byte, []byte, error) {
	_, payload, err := mc.readRawPacket()
	if err != nil {
		return 0, nil, err
	}
	if len(payload) == 0 {
		return 0, nil, nil
	}
	return payload[0], payload[1:], nil
}

// readRawPacket 返回整包 payload（不带命令字节裁剪），用于握手响应等无命令字节的包。
func (mc *mysqlConn) readRawPacket() (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(mc.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	length := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	mc.seq = hdr[3] + 1
	payload := make([]byte, length)
	if _, err := io.ReadFull(mc.br, payload); err != nil {
		return 0, nil, err
	}
	// 多分片拼接
	for length == 0xffffff {
		if _, err := io.ReadFull(mc.br, hdr[:]); err != nil {
			return 0, nil, err
		}
		length = int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
		mc.seq = hdr[3] + 1
		chunk := make([]byte, length)
		if _, err := io.ReadFull(mc.br, chunk); err != nil {
			return 0, nil, err
		}
		payload = append(payload, chunk...)
	}
	return 0, payload, nil
}

// ---- 长度编码 ----

func appendLenEncInt(buf []byte, v uint64) []byte {
	switch {
	case v < 251:
		return append(buf, byte(v))
	case v < 1<<16:
		return append(buf, 0xfc, byte(v), byte(v>>8))
	case v < 1<<24:
		return append(buf, 0xfd, byte(v), byte(v>>8), byte(v>>16))
	default:
		return append(buf, 0xfe, byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
			byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
	}
}

func appendLenEncStr(buf []byte, s string) []byte {
	buf = appendLenEncInt(buf, uint64(len(s)))
	return append(buf, s...)
}

func appendLenEncBytes(buf, b []byte) []byte {
	buf = appendLenEncInt(buf, uint64(len(b)))
	return append(buf, b...)
}

func appendU16LE(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}
func appendU32LE(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func readNulString(b []byte, pos *int) string {
	start := *pos
	for *pos < len(b) && b[*pos] != 0 {
		*pos++
	}
	s := string(b[start:*pos])
	if *pos < len(b) {
		*pos++
	}
	return s
}

// ---- 握手 ----

func (mc *mysqlConn) sendHandshake() error {
	authData := make([]byte, 20)
	if _, err := rand.Read(authData); err != nil {
		// 极低概率失败：退回确定性 seed 保证功能可用
		copy(authData, fixedSeed())
	}
	// 首字节不能为 0（会被误判为长度编码结束符），确保非零
	if authData[0] == 0 {
		authData[0] = 1
	}
	mc.scramble = authData
	plugin := "mysql_native_password"

	payload := make([]byte, 0, 64)
	payload = append(payload, protocolVersion)
	payload = append(payload, []byte("8.0.36-Tsumugi")...)
	payload = append(payload, 0)
	payload = appendU32LE(payload, randomThreadID()) // thread id
	payload = append(payload, authData[:8]...)
	payload = append(payload, 0)
	payload = appendU16LE(payload, uint16(serverCapabilities&0xffff))
	payload = append(payload, defaultCollation)
	payload = appendU16LE(payload, statusAutocommit)
	payload = appendU16LE(payload, uint16(serverCapabilities>>16))
	payload = append(payload, 21) // auth plugin data len (1 byte)
	payload = append(payload, make([]byte, 10)...)
	payload = append(payload, authData[8:]...)
	payload = append(payload, 0)
	payload = append(payload, []byte(plugin)...)
	payload = append(payload, 0)
	if err := mc.writePacket(payload); err != nil {
		return err
	}
	// 客户端等待握手包后才发送认证响应：必须先落盘再阻塞读
	return mc.bw.Flush()
}

func fixedSeed() []byte {
	seed := make([]byte, 20)
	for i := range seed {
		seed[i] = byte(i*7 + 3)
	}
	return seed
}

func randomThreadID() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	}
	return uint32(time.Now().UnixNano())
}

func (mc *mysqlConn) readAuth() error {
	_, payload, err := mc.readRawPacket()
	if err != nil {
		return err
	}
	pos := 0
	if pos+32 > len(payload) {
		mc.sendError(1045, "bad auth packet")
		mc.bw.Flush()
		return errBadPacket
	}
	pos += 4 + 4 + 1 + 23 // capabilities + maxpacket + charset + reserved
	user := readNulString(payload, &pos)
	mc.user = user
	// 认证响应：长度编码（CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA）
	authLen := 0
	if pos < len(payload) && payload[pos] < 251 {
		authLen = int(payload[pos])
		pos++
		if pos+authLen > len(payload) {
			mc.sendError(1045, "bad auth response length")
			mc.bw.Flush()
			return errBadPacket
		}
	}
	authResp := payload[pos : pos+authLen]
	pos += authLen
	// 数据库名（如声明 CLIENT_CONNECT_WITH_DB，位于插件名之前）——仅消费不识别；
	// 插件名（CLIENT_PLUGIN_AUTH）：尾部 null 结尾，仅当匹配已知插件才采用。
	plugin := "mysql_native_password"
	for pos < len(payload) {
		p := readNulString(payload, &pos)
		switch p {
		case "mysql_native_password", "caching_sha2_password", "mysql_clear_password", "sha256_password":
			plugin = p
		}
	}
	cfg := mc.db.config
	if user != cfg.User {
		mc.sendError(1045, fmt.Sprintf("access denied for user '%s'", user))
		mc.bw.Flush()
		return errAccessDenied
	}
	// 客户端选择 native → 直接校验；否则走 auth switch 回退到 native。
	if plugin == "mysql_native_password" {
		if !checkNativePassword(cfg.Password, authResp, mc.scramble) {
			mc.sendError(0, "access denied")
			mc.bw.Flush()
			return errAccessDenied
		}
	} else {
		// AuthSwitchRequest: 0xfe + plugin 名 + seed + 0。客户端据插件重算并回发。
		if err := mc.writeAuthSwitch(); err != nil {
			return err
		}
		// 必须立即 flush，否则客户端等 switch、服务端等响应而互锁
		if err := mc.bw.Flush(); err != nil {
			return err
		}
		// 等待客户端回发 native 响应（可能带 command 0xfe 前缀）
		seq, resp, err := mc.readRawPacket()
		if err != nil {
			return err
		}
		_ = seq
		if len(resp) >= 1 && resp[0] == 0xfe {
			resp = resp[1:]
		}
		if !checkNativePassword(cfg.Password, resp, mc.scramble) {
			mc.sendError(0, "access denied")
			mc.bw.Flush()
			return errAccessDenied
		}
	}
	logf(LOG_VERB, "mysql auth OK user=%q plugin=%s authRespLen=%d", user, plugin, len(authResp))
	if err := mc.writePacket([]byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}); err != nil {
		logf(LOG_ERR, "mysql auth OK write error: %v", err)
	}
	return mc.bw.Flush()
}

// writeAuthSwitch 发送 AuthSwitchRequest，要求客户端改用 mysql_native_password。
func (mc *mysqlConn) writeAuthSwitch() error {
	var buf []byte
	buf = append(buf, 0xfe)
	buf = append(buf, []byte("mysql_native_password")...)
	buf = append(buf, 0)
	buf = append(buf, mc.scramble...)
	buf = append(buf, 0)
	return mc.writePacket(buf)
}

// checkNativePassword 校验 mysql_native_password：
// authResp = SHA1(password) XOR SHA1(scramble + SHA1(SHA1(password)))
func checkNativePassword(password string, authResp, scramble []byte) bool {
	if len(authResp) == 0 {
		return password == ""
	}
	sha1p := sha1.Sum([]byte(password))
	sha1p2 := sha1.Sum(sha1p[:])
	h := sha1.New()
	h.Write(scramble)
	h.Write(sha1p2[:])
	expected := h.Sum(nil)
	if len(authResp) != len(expected) {
		return false
	}
	for i := 0; i < len(expected); i++ {
		if authResp[i] != sha1p[i]^expected[i] {
			return false
		}
	}
	return true
}

// ---- 命令处理 ----

// handleQuery 响应 COM_QUERY（事务/SET 由连接级处理，其余进入引擎）。
func (mc *mysqlConn) handleQuery(sql string) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		mc.sendError(1065, "query was empty")
		return
	}
	// 事务与 SET 语句：维护连接级事务状态，不进入数据引擎。
	upper := strings.ToUpper(sql)
	if kw := firstWordUpper(upper); kw == "BEGIN" || kw == "START" || kw == "COMMIT" || kw == "ROLLBACK" ||
		kw == "SET" || kw == "SETS" {
		mc.handleTxCommand(upper, sql)
		return
	}
	var txnID uint64
	if mc.inTxn {
		txnID = mc.txnID
	}
	columns, rows, affected, _, err := mc.db.runSQLTx(sql, nil, txnID)
	if err != nil {
		// 仅在出错时判断是否带表达式的 select（无 from），避免成功路径整串 ToLower 拷贝。
		lower := strings.ToLower(sql)
		if strings.HasPrefix(lower, "select ") && !strings.Contains(lower, " from ") {
			mc.sendExprResult(sql)
			return
		}
		mc.sendError(1064, "you have an error in your SQL syntax near '"+firstWord(sql)+"': "+err.Error())
		return
	}
	if len(columns) == 0 {
		mc.writeOKAffected(affected)
		return
	}
	mc.sendResultSet(columns, rows)
}

// firstWordUpper 返回 SQL 首个单词（大写形式）。
func firstWordUpper(upper string) string {
	upper = strings.TrimSpace(upper)
	if i := strings.IndexByte(upper, ' '); i >= 0 {
		return upper[:i]
	}
	return upper
}

// handleTxCommand 处理 BEGIN/COMMIT/ROLLBACK/SET 等连接级事务与会话语句。
func (mc *mysqlConn) handleTxCommand(upper, sql string) {
	switch {
	case strings.HasPrefix(upper, "BEGIN"), strings.HasPrefix(upper, "START"):
		if mc.inTxn {
			mc.sendError(1568, "transaction already active")
			return
		}
		mc.txnID = mc.db.BeginTxn()
		mc.inTxn = true
		mc.writeOK()
	case strings.HasPrefix(upper, "COMMIT"):
		if !mc.inTxn {
			mc.sendError(1614, "no transaction active")
			return
		}
		if err := mc.db.CommitTxn(mc.txnID); err != nil {
			mc.inTxn = false
			mc.txnID = 0
			mc.sendError(1213, err.Error())
			return
		}
		mc.inTxn = false
		mc.txnID = 0
		mc.writeOK()
	case strings.HasPrefix(upper, "ROLLBACK"):
		if mc.inTxn {
			_ = mc.db.RollbackTxn(mc.txnID)
			mc.inTxn = false
			mc.txnID = 0
		}
		mc.writeOK()
	case strings.HasPrefix(upper, "SET"): // 会话变量：忽略，仅返回 OK
		mc.writeOK()
	}
}

// ---- Prepared Statement (COM_STMT_*) ----

// handlePrepare 响应 COM_STMT_PREPARE：解析 SQL 结构，返回参数元数据。
// 列元数据不在 PREPARE 阶段执行（避免副作用），EXECUTE 时发送完整结果集列定义。
func (mc *mysqlConn) handlePrepare(sql string) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		mc.sendError(1065, "query was empty")
		return
	}
	// 预分词并缓存，EXECUTE 复用，避免每条语句每次执行都重新分词。
	toks, terr := tokenizeSQL(sql)
	if terr != nil {
		mc.sendError(1065, "query was empty")
		return
	}
	n := countSQLTokens(toks)
	mc.nextStmt++
	id := mc.nextStmt
	if mc.prepared == nil {
		mc.prepared = make(map[uint32]*mysqlPreparedStmt)
	}
	mc.prepared[id] = &mysqlPreparedStmt{id: id, sql: sql, paramCount: n, toks: toks}

	var buf []byte
	buf = append(buf, 0x00)                  // status OK
	buf = appendU32LE(buf, id)               // statement id
	buf = appendU16LE(buf, 0)                // column count（EXECUTE 时下发真实列定义）
	buf = appendU16LE(buf, uint16(n))        // parameter count
	buf = append(buf, 0x00)                  // filler
	buf = appendU16LE(buf, 0)                // warning count
	mc.writePacket(buf)

	// 参数定义包（每参数一个，之后 EOF）
	for i := 0; i < n; i++ {
		buf = buf[:0]
		buf = mc.appendColumnDef(buf, "def", mc.schemaName(), "", "", "?", "?", 0x0f)
		mc.writePacket(buf)
	}
	if n > 0 {
		buf = buf[:0]
		buf = append(buf, 0xfe, 0x00, 0x00, 0x02, 0x00)
		mc.writePacket(buf)
	}
}

// appendColumnDef 构建一个列定义包 payload。
func (mc *mysqlConn) appendColumnDef(buf []byte, catalog, schema, table, orgTable, name, orgName string, colType byte) []byte {
	buf = appendLenEncStr(buf, catalog)
	buf = appendLenEncStr(buf, schema)
	buf = appendLenEncStr(buf, table)
	buf = appendLenEncStr(buf, orgTable)
	buf = appendLenEncStr(buf, name)
	buf = appendLenEncStr(buf, orgName)
	buf = appendLenEncInt(buf, 0x0c)         // fixed length field count
	buf = appendU16LE(buf, defaultCollation) // charset
	buf = appendU32LE(buf, mysqlColLen(colType))
	buf = append(buf, colType)
	buf = appendU16LE(buf, 0) // flags
	buf = append(buf, 0)      // decimals
	buf = appendU16LE(buf, 0) // filler
	return buf
}

func (mc *mysqlConn) schemaName() string {
	if mc.db != nil {
		if cur := mc.db.getCurDB(); cur != "" {
			return cur
		}
	}
	return "tsumugi"
}

// handleExecute 响应 COM_STMT_EXECUTE：解析二进制协议参数并执行。
func (mc *mysqlConn) handleExecute(payload []byte) {
	if len(payload) < 10 {
		mc.sendError(1243, "malformed COM_STMT_EXECUTE")
		return
	}
	stmtID := uint32(payload[0]) | uint32(payload[1])<<8 | uint32(payload[2])<<16 | uint32(payload[3])<<24
	st := mc.prepared[stmtID]
	if st == nil {
		mc.sendError(1243, "statement not found")
		return
	}
	flags := uint8(payload[4])
	iterCount := uint32(payload[5]) | uint32(payload[6])<<8 | uint32(payload[7])<<16 | uint32(payload[8])<<24
	// COM_STMT_EXECUTE 体：4B id + 1B flags + 4B iterCount = 9B，随后是 null bitmap、new-bound、类型、值
	pos := 9
	params := make([]interface{}, st.paramCount)
	if st.paramCount > 0 {
		// null bitmap: (n+7)/8 字节
		nbitmap := (st.paramCount + 7) / 8
		if pos+nbitmap > len(payload) {
			mc.sendError(1243, "malformed null bitmap")
			return
		}
		var nullBitmap []byte
		nullBitmap = payload[pos : pos+nbitmap]
		pos += nbitmap
		newBound := byte(1)
		if pos < len(payload) {
			newBound = payload[pos]
			pos++
		}
		if newBound == 0 {
			// 复用上次类型——本实现始终要求携带类型
			mc.sendError(1243, "parameter type reuse not supported")
			return
		}
		// 每个参数 2 字节类型（type + unsigned flag）
		types := make([]byte, st.paramCount*2)
		if pos+len(types) > len(payload) {
			mc.sendError(1243, "malformed parameter types")
			return
		}
		copy(types, payload[pos:pos+len(types)])
		pos += len(types)
		// 按类型解析二进制值
		for i := 0; i < st.paramCount; i++ {
			if nullBitmap[i/8]&(1<<uint(i%8)) != 0 {
				params[i] = nil
				continue
			}
			v, err := parseBinaryParam(types[i*2], payload, &pos)
			if err != nil {
				mc.sendError(1243, err.Error())
				return
			}
			params[i] = v
		}
	}
	_ = flags
	_ = iterCount
	var txnID uint64
	if mc.inTxn {
		txnID = mc.txnID
	}
	columns, rows, affected, _, err := mc.db.runSQLTokens(st.toks, st.sql, params, txnID)
	if err != nil {
		mc.sendError(1064, "you have an error in your SQL syntax near '"+firstWord(st.sql)+"': "+err.Error())
		return
	}
	if len(columns) == 0 {
		mc.writeOKAffected(affected)
		return
	}
	mc.sendResultSet(columns, rows)
}

// parseBinaryParam 解析二进制协议的一个参数值，返回 Go 值。
func parseBinaryParam(pType byte, payload []byte, pos *int) (interface{}, error) {
	if *pos >= len(payload) {
		return nil, fmt.Errorf("missing param value")
	}
	switch pType {
	case 0x00: // NULL
		return nil, nil
	case 0x01: // TINY
		v := int64(int8(payload[*pos]))
		*pos++
		return v, nil
	case 0x02: // SHORT
		v := int64(int16(uint16(payload[*pos]) | uint16(payload[*pos+1])<<8))
		*pos += 2
		return v, nil
	case 0x03: // LONG
		v := int64(uint32(payload[*pos]) | uint32(payload[*pos+1])<<8 | uint32(payload[*pos+2])<<16 | uint32(payload[*pos+3])<<24)
		*pos += 4
		return v, nil
	case 0x08: // LONGLONG
		v := uint64(payload[*pos]) | uint64(payload[*pos+1])<<8 | uint64(payload[*pos+2])<<16 | uint64(payload[*pos+3])<<24 |
			uint64(payload[*pos+4])<<32 | uint64(payload[*pos+5])<<40 | uint64(payload[*pos+6])<<48 | uint64(payload[*pos+7])<<56
		*pos += 8
		return int64(v), nil
	case 0x04: // FLOAT
		v := math.Float32frombits(uint32(payload[*pos]) | uint32(payload[*pos+1])<<8 | uint32(payload[*pos+2])<<16 | uint32(payload[*pos+3])<<24)
		*pos += 4
		return v, nil
	case 0x05: // DOUBLE
		v := math.Float64frombits(uint64(payload[*pos]) | uint64(payload[*pos+1])<<8 | uint64(payload[*pos+2])<<16 | uint64(payload[*pos+3])<<24 |
			uint64(payload[*pos+4])<<32 | uint64(payload[*pos+5])<<40 | uint64(payload[*pos+6])<<48 | uint64(payload[*pos+7])<<56)
		*pos += 8
		return v, nil
	case 0x0f, 0x10, 0xfd, 0xfe, 0xfc: // VARCHAR / VAR_STRING / STRING / BLOB / lenenc
		return readLenEncBytes(payload, pos)
	case 0x07: // TIMESTAMP / DATETIME
		return readDateTimeParam(payload, pos)
	case 0x0a: // DATE
		return readDateParam(payload, pos)
	case 0x0b: // TIME
		return readTimeParam(payload, pos)
	case 0xf6: // NEWDATE
		return readDateParam(payload, pos)
	}
	return nil, fmt.Errorf("unsupported param type 0x%02x", pType)
}

// readLenEncBytes 读取一个长度编码字符串/字节串（用于字符串参数）。
func readLenEncBytes(payload []byte, pos *int) (interface{}, error) {
	b, err := readLenEncByteSlice(payload, pos)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func readLenEncByteSlice(payload []byte, pos *int) ([]byte, error) {
	if *pos >= len(payload) {
		return nil, fmt.Errorf("truncated lenenc")
	}
	first := payload[*pos]
	var length uint64
	switch {
	case first < 251:
		length = uint64(first)
		*pos++
	case first == 0xfc:
		if *pos+2 >= len(payload) {
			return nil, fmt.Errorf("truncated lenenc")
		}
		length = uint64(payload[*pos+1]) | uint64(payload[*pos+2])<<8
		*pos += 3
	case first == 0xfd:
		if *pos+3 >= len(payload) {
			return nil, fmt.Errorf("truncated lenenc")
		}
		length = uint64(payload[*pos+1]) | uint64(payload[*pos+2])<<8 | uint64(payload[*pos+3])<<16
		*pos += 4
	case first == 0xfe:
		if *pos+8 >= len(payload) {
			return nil, fmt.Errorf("truncated lenenc")
		}
		length = 0
		for i := 0; i < 8; i++ {
			length |= uint64(payload[*pos+1+i]) << (8 * uint(i))
		}
		*pos += 9
	default:
		return nil, fmt.Errorf("bad lenenc marker 0x%02x", first)
	}
	if uint64(len(payload)-*pos) < length {
		return nil, fmt.Errorf("truncated lenenc value")
	}
	b := payload[*pos : *pos+int(length)]
	*pos += int(length)
	return b, nil
}

// readDateTimeParam / readDateParam / readTimeParam 解析 MySQL 日期时间类型参数。
func readDateTimeParam(payload []byte, pos *int) (interface{}, error) {
	if *pos >= len(payload) {
		return nil, fmt.Errorf("truncated datetime")
	}
	length := int(payload[*pos])
	*pos++
	if length == 0 {
		return "0000-00-00 00:00:00", nil
	}
	if *pos+length > len(payload) {
		return nil, fmt.Errorf("truncated datetime")
	}
	b := payload[*pos : *pos+length]
	*pos += length
	y := int(b[0])<<8 | int(b[1])
	mo := int(b[2])
	d := int(b[3])
	var h, mi, s int
	if length >= 7 {
		h = int(b[4])
		mi = int(b[5])
		s = int(b[6])
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", y, mo, d, h, mi, s), nil
}

func readDateParam(payload []byte, pos *int) (interface{}, error) {
	if *pos >= len(payload) {
		return nil, fmt.Errorf("truncated date")
	}
	length := int(payload[*pos])
	*pos++
	if length == 0 {
		return "0000-00-00", nil
	}
	if *pos+length > len(payload) {
		return nil, fmt.Errorf("truncated date")
	}
	b := payload[*pos : *pos+length]
	*pos += length
	y := int(b[0])<<8 | int(b[1])
	mo := int(b[2])
	d := int(b[3])
	return fmt.Sprintf("%04d-%02d-%02d", y, mo, d), nil
}

func readTimeParam(payload []byte, pos *int) (interface{}, error) {
	if *pos >= len(payload) {
		return nil, fmt.Errorf("truncated time")
	}
	length := int(payload[*pos])
	*pos++
	if length == 0 {
		return "00:00:00", nil
	}
	if *pos+length > len(payload) {
		return nil, fmt.Errorf("truncated time")
	}
	b := payload[*pos : *pos+length]
	*pos += length
	neg := b[0] != 0
	d := int(b[1])<<24 | int(b[2])<<16 | int(b[3])<<8 | int(b[4])
	var h, mi, s int
	if length >= 9 {
		h = int(b[5])
		mi = int(b[6])
		s = int(b[7])
	}
	h += d * 24
	if neg {
		return fmt.Sprintf("-%02d:%02d:%02d", h, mi, s), nil
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, mi, s), nil
}

func (mc *mysqlConn) handleStmtClose(payload []byte) {
	if len(payload) < 4 {
		return
	}
	stmtID := uint32(payload[0]) | uint32(payload[1])<<8 | uint32(payload[2])<<16 | uint32(payload[3])<<24
	if st := mc.prepared[stmtID]; st != nil && st.toks != nil {
		sqlTokenPool.Put(st.toks[:0])
	}
	delete(mc.prepared, stmtID)
}

func (mc *mysqlConn) handleStmtReset(payload []byte) {
	if len(payload) < 4 {
		mc.sendError(1243, "malformed COM_STMT_RESET")
		return
	}
	stmtID := uint32(payload[0]) | uint32(payload[1])<<8 | uint32(payload[2])<<16 | uint32(payload[3])<<24
	if _, ok := mc.prepared[stmtID]; !ok {
		mc.sendError(1243, "statement not found")
		return
	}
	delete(mc.longData, stmtID)
	mc.writeOK()
}

// handleSendLong 累积长数据到参数缓冲区（COM_STMT_SEND_LONG_DATA）。
func (mc *mysqlConn) handleSendLong(payload []byte) {
	if len(payload) < 6 {
		return
	}
	stmtID := uint32(payload[0]) | uint32(payload[1])<<8 | uint32(payload[2])<<16 | uint32(payload[3])<<24
	if mc.longData == nil {
		mc.longData = make(map[uint32][]byte)
	}
	chunk := payload[6:]
	mc.longData[stmtID] = append(mc.longData[stmtID], chunk...)
}

// sendResultSet 发送结果集。列类型按首行实际值推断：
// bool → TINY(0x01)、int64 → LONGLONG(0x08)、float64 → DOUBLE(0x05)、其余 → VARCHAR(0x0f)。
func (mc *mysqlConn) sendResultSet(columns []string, rows [][]interface{}) {
	types := make([]byte, len(columns))
	for i := range types {
		types[i] = 0x0f // VARCHAR fallback
	}
	if len(rows) > 0 {
		for i := range columns {
			if i >= len(rows[0]) {
				continue
			}
			types[i] = mysqlValueType(rows[0][i])
		}
	}
	schema := "tsumugi"
	if mc.db != nil {
		if cur := mc.db.getCurDB(); cur != "" {
			schema = cur
		}
	}
	// 列数包（单独一个包）
	var buf []byte
	buf = appendLenEncInt(buf, uint64(len(columns)))
	mc.writePacket(buf)
	// 每个列定义一个独立包（客户端按列数读取）
	for i := range columns {
		buf = buf[:0]
		buf = appendLenEncStr(buf, "def")          // catalog
		buf = appendLenEncStr(buf, schema)         // schema
		buf = appendLenEncStr(buf, "")             // table
		buf = appendLenEncStr(buf, "")             // org_table
		buf = appendLenEncStr(buf, columns[i])     // name
		buf = appendLenEncStr(buf, columns[i])     // org_name
		buf = appendLenEncInt(buf, 0x0c)           // fixed length field count
		buf = appendU16LE(buf, defaultCollation)   // charset/transformation
		buf = appendU32LE(buf, mysqlColLen(types[i]))
		buf = append(buf, types[i])
		buf = appendU16LE(buf, 0)                  // flags
		buf = append(buf, 0)                       // decimals
		buf = appendU16LE(buf, 0)                  // filler
		mc.writePacket(buf)
	}
	buf = buf[:0]
	buf = append(buf, 0xfe, 0x00, 0x00, 0x02, 0x00) // EOF（列定义结束）
	mc.writePacket(buf)
	for _, r := range rows {
		buf = buf[:0]
		for _, v := range r {
			buf = appendCell(buf, v)
		}
		mc.writePacket(buf)
	}
	buf = buf[:0]
	buf = append(buf, 0xfe, 0x00, 0x00, 0x02, 0x00) // EOF（行结束）
	mc.writePacket(buf)
}

// mysqlColLen 返回列定义中的最大显示宽度（按类型）。
func mysqlColLen(t byte) uint32 {
	switch t {
	case 0x01: // TINY
		return 4
	case 0x08: // LONGLONG
		return 20
	case 0x05: // DOUBLE
		return 22
	case 0x0f: // VARCHAR / CHAR
		return 255
	}
	return 255
}

// appendCell 将单元格值按 MySQL 文本协议编码（lenenc string），避免 fmt.Sprintf 反射开销。
// 数值走 strconv.Append* 到栈上临时缓冲，再直接编码长度头，避免 string 中间分配。
func appendCell(buf []byte, v interface{}) []byte {
	var tmp [64]byte
	switch val := v.(type) {
	case nil:
		return appendLenEncInt(buf, 0xfb) // NULL
	case bool:
		if val {
			buf = appendLenEncInt(buf, 1)
			return append(buf, '1')
		}
		buf = appendLenEncInt(buf, 1)
		return append(buf, '0')
	case string:
		return appendLenEncStr(buf, val)
	case []byte:
		return appendLenEncBytes(buf, val)
	case int8:
		s := strconv.AppendInt(tmp[:0], int64(val), 10)
		return appendLenEncBytes(buf, s)
	case int16:
		s := strconv.AppendInt(tmp[:0], int64(val), 10)
		return appendLenEncBytes(buf, s)
	case int32:
		s := strconv.AppendInt(tmp[:0], int64(val), 10)
		return appendLenEncBytes(buf, s)
	case int64:
		s := strconv.AppendInt(tmp[:0], val, 10)
		return appendLenEncBytes(buf, s)
	case int:
		s := strconv.AppendInt(tmp[:0], int64(val), 10)
		return appendLenEncBytes(buf, s)
	case uint8:
		s := strconv.AppendUint(tmp[:0], uint64(val), 10)
		return appendLenEncBytes(buf, s)
	case uint16:
		s := strconv.AppendUint(tmp[:0], uint64(val), 10)
		return appendLenEncBytes(buf, s)
	case uint32:
		s := strconv.AppendUint(tmp[:0], uint64(val), 10)
		return appendLenEncBytes(buf, s)
	case uint64:
		s := strconv.AppendUint(tmp[:0], val, 10)
		return appendLenEncBytes(buf, s)
	case uint:
		s := strconv.AppendUint(tmp[:0], uint64(val), 10)
		return appendLenEncBytes(buf, s)
	case float32:
		s := strconv.AppendFloat(tmp[:0], float64(val), 'g', -1, 32)
		return appendLenEncBytes(buf, s)
	case float64:
		s := strconv.AppendFloat(tmp[:0], val, 'g', -1, 64)
		return appendLenEncBytes(buf, s)
	default:
		return appendLenEncStr(buf, fmt.Sprintf("%v", val))
	}
}

// mysqlValueType 按 Go 值推断 MySQL 列类型字节。
func mysqlValueType(v interface{}) byte {
	switch v.(type) {
	case bool:
		return 0x01 // TINY
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return 0x08 // LONGLONG
	case float32, float64:
		return 0x05 // DOUBLE
	}
	return 0x0f // VARCHAR
}

func (mc *mysqlConn) sendExprResult(sql string) {
	body := sql[len("select"):]
	parts := splitTopLevel(body, ',')
	var cols []string
	var vals []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		cols = append(cols, p)
		vals = append(vals, evalExpr(p))
	}
	mc.sendResultSet(cols, toRows(vals))
}

func toRows(vals []string) [][]interface{} {
	rows := make([][]interface{}, 0, 1)
	row := make([]interface{}, 0, len(vals))
	for _, v := range vals {
		row = append(row, v)
	}
	rows = append(rows, row)
	return rows
}

func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth := 0
	start := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			inStr = !inStr
		}
		if !inStr {
			switch c {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					depth--
				}
			case sep:
				if depth == 0 {
					parts = append(parts, s[start:i])
					start = i + 1
				}
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func evalExpr(e string) string {
	e = strings.TrimSpace(e)
	up := strings.ToUpper(e)
	switch {
	case up == "NOW()" || up == "CURRENT_TIMESTAMP" || up == "CURRENT_TIMESTAMP()":
		return time.Now().Format("2006-01-02 15:04:05")
	case up == "CURDATE()":
		return time.Now().Format("2006-01-02")
	case up == "CURTIME()":
		return time.Now().Format("15:04:05")
	case up == "DATABASE()" || up == "SCHEMA()":
		return "tsumugi"
	case up == "VERSION()":
		return "8.0.36-Tsumugi"
	case up == "USER()" || up == "CURRENT_USER()":
		return "root@localhost"
	}
	v, ok := evalSimpleArithmetic(e)
	if ok {
		return v
	}
	if len(e) >= 2 && e[0] == '\'' && e[len(e)-1] == '\'' {
		return e[1 : len(e)-1]
	}
	return e
}

// evalSimpleArithmetic 简单整数表达式求值：+ - * / 和括号，失败返回 false。
func evalSimpleArithmetic(e string) (string, bool) {
	toks, err := tokenizeExpr(e)
	if err != nil || len(toks) == 0 {
		return "", false
	}
	p := &exprParser{toks: toks}
	val, err := p.parseExpr()
	if err != nil || p.pos != len(toks)-1 {
		return "", false
	}
	return strconv.FormatInt(val, 10), true
}

type exprTok struct {
	kind string // num, op, lparen, rparen
	val  string
}

type exprParser struct {
	toks []exprTok
	pos  int
}

func (p *exprParser) peek() exprTok {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return exprTok{"eof", ""}
}
func (p *exprParser) next() exprTok {
	t := p.peek()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

func tokenizeExpr(e string) ([]exprTok, error) {
	var toks []exprTok
	for i := 0; i < len(e); {
		c := e[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c >= '0' && c <= '9':
			j := i
			for j < len(e) && e[j] >= '0' && e[j] <= '9' {
				j++
			}
			toks = append(toks, exprTok{"num", e[i:j]})
			i = j
		case c == '+' || c == '-' || c == '*' || c == '/':
			toks = append(toks, exprTok{"op", string(c)})
			i++
		case c == '(':
			toks = append(toks, exprTok{"lparen", "("})
			i++
		case c == ')':
			toks = append(toks, exprTok{"rparen", ")"})
			i++
		default:
			return nil, fmt.Errorf("invalid char %c", c)
		}
	}
	toks = append(toks, exprTok{"eof", ""})
	return toks, nil
}

func (p *exprParser) parseExpr() (int64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind != "op" || (t.val != "+" && t.val != "-") {
			return left, nil
		}
		p.next()
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if t.val == "+" {
			left = left + right
		} else {
			left = left - right
		}
	}
}

func (p *exprParser) parseTerm() (int64, error) {
	left, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind != "op" || (t.val != "*" && t.val != "/") {
			return left, nil
		}
		p.next()
		right, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		if t.val == "*" {
			left = left * right
		} else {
			if right == 0 {
				return 0, fmt.Errorf("divide by zero")
			}
			left = left / right
		}
	}
}

func (p *exprParser) parseFactor() (int64, error) {
	t := p.peek()
	switch t.kind {
	case "num":
		p.next()
		return strconv.ParseInt(t.val, 10, 64)
	case "lparen":
		p.next()
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if p.peek().kind != "rparen" {
			return 0, fmt.Errorf("expected )")
		}
		p.next()
		return v, nil
	case "op":
		if t.val == "-" {
			p.next()
			v, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			return -v, nil
		}
		return 0, fmt.Errorf("unexpected operator %v", t)
	default:
		return 0, fmt.Errorf("unexpected token %v", t)
	}
}

func firstWord(sql string) string {
	sql = strings.TrimSpace(sql)
	if i := strings.IndexByte(sql, ' '); i >= 0 {
		return sql[:i]
	}
	return sql
}

func (mc *mysqlConn) writeOK() {
	mc.writeOKAffected(0)
}

// writeOKAffected 发送 OK 包，携带真实影响行数（客户端 RowsAffected 正确性）。
func (mc *mysqlConn) writeOKAffected(affected int64) {
	// 响应包 seq 使用 readPacket 已设好的 mc.seq（首包=1），勿重置为 0
	var buf []byte
	buf = append(buf, 0x00)
	buf = appendLenEncInt(buf, uint64(affected)) // affected rows
	buf = appendLenEncInt(buf, 0)                // last insert id
	buf = appendU16LE(buf, statusAutocommit)     // status flags
	buf = appendU16LE(buf, 0)                    // warnings
	mc.writePacket(buf)
}

func (mc *mysqlConn) sendError(code int, msg string) {
	var buf []byte
	buf = append(buf, 0xff)
	buf = appendU16LE(buf, uint16(code))
	buf = append(buf, '#')
	buf = append(buf, []byte("42000")...)
	buf = append(buf, []byte(msg)...)
	mc.writePacket(buf)
}
