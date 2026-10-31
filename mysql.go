package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	protocolVersion  = 10
	defaultCollation = 45 // utf8mb4_general_ci
	statusAutocommit = 0x0002
	longFlagOpened   = 0x0080

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

// MySQL 能力位（数值必须与协议一致：旧实现把 PROTOCOL_41 写成 1<<3、
// SECURE_CONNECTION 写成 1<<8，导致真实驱动直接拒绝握手）。
const (
	clientLongPassword               = 1 << 0
	clientLongFlag                   = 1 << 2
	clientConnectWithDB              = 1 << 3
	clientProtocol41                 = 1 << 9
	clientTransactions               = 1 << 13
	clientSecureConnection           = 1 << 15
	clientMultiResults               = 1 << 17
	clientPSMultiResults             = 1 << 18
	clientPluginAuth                 = 1 << 19
	clientPluginAuthLenEncClientData = 1 << 21
)

const serverCapabilities = clientLongPassword | clientLongFlag | clientConnectWithDB |
	clientProtocol41 | clientTransactions | clientSecureConnection |
	clientMultiResults | clientPSMultiResults |
	clientPluginAuth | clientPluginAuthLenEncClientData

var errBadPacket = fmt.Errorf("bad packet")
var errAccessDenied = fmt.Errorf("access denied")

// maxMySQLPacket 单个请求包的最大字节数（与握手广告的 max_allowed_packet 一致）。
// 超限直接断连，防止 16MB 分片无限续包把内存打爆。
const maxMySQLPacket = 64 << 20

// maxPreparedStmts 单连接打开的 prepared statement 上限。
const maxPreparedStmts = 1024

func startMySQLServer(db *DB, port int) {
	cfg := db.configSnapshot()
	addr := cfg.listenAddr(port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logf(LOG_ERR, "mysql listen %s: %v", addr, err)
		return
	}
	registerServiceCloser("mysql", ln)
	defer ln.Close()
	logf(LOG_OK, "mysql protocol listening on %s", addr)
	maxConns := cfg.MaxConns
	if maxConns <= 0 {
		maxConns = 512
	}
	var active atomic.Int64
	var conns sync.Map
	// 优雅关闭：进程退出时释放监听器并断开活动连接，避免 goroutine 泄漏
	go func() {
		<-db.stopCh
		ln.Close()
		conns.Range(func(k, v interface{}) bool {
			if c, ok := k.(net.Conn); ok {
				c.Close()
			}
			return true
		})
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-db.stopCh:
				return
			default:
			}
			// 临时性 accept 错误（如 fd 暂时耗尽）不应终止整个监听
			logf(LOG_ERR, "mysql accept: %v", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if active.Add(1) > int64(maxConns) {
			active.Add(-1)
			writeMysqlPacket(conn, new(uint8), []byte{0xff, 0x40, 0x10, '#', '0', '8', 'S', '0', '1'}) // ER_CON_COUNT_ERROR 1040
			conn.Close()
			continue
		}
		conns.Store(conn, struct{}{})
		go func() {
			defer active.Add(-1)
			defer conns.Delete(conn)
			(&mysqlConn{db: db, conn: conn, autocommit: true}).handle()
		}()
	}
}

type mysqlConn struct {
	db       *DB
	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	seq      uint8
	user     string
	schema   string // 连接级默认数据库（USE / COM_INIT_DB / 连接串 db 名）
	scramble []byte
	authed   bool   // 认证前限制单包大小，避免未认证连接按包头索要巨额内存
	nextStmt uint32 // 下一个可用的 prepared 语句 ID
	prepared map[uint32]*mysqlPreparedStmt
	// longData COM_STMT_SEND_LONG_DATA 累积缓冲，key = stmtID<<16 | paramID
	longData    map[uint32]map[uint16][]byte
	inTxn       bool
	txnID       uint64
	autocommit  bool // SET autocommit=0 后写语句进入隐式事务
	implicitTxn uint64
	// isoLevel 事务隔离级别。引擎当前只提供 READ COMMITTED 语义，
	// 客户端显式设置其它级别时必须报错，而不是静默按 RC 运行——
	// 否则应用以为拿到了 REPEATABLE READ 的读快照，实际会出现不可重复读。
	isoLevel string
}

// mysqlPreparedStmt 记录一条已解析的 prepared statement。
type mysqlPreparedStmt struct {
	id         uint32
	sql        string
	paramCount int
	toks       []sqlToken // 缓存分词结果，EXECUTE 复用避免重复分词
	paramTypes []byte     // 上次 EXECUTE 的类型数组；new_params_bound_flag=0 时复用
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
	// 握手阶段：读用短超时，写单独设不限（写超时会在每命令前续期）。
	mc.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	if err := mc.sendHandshake(); err != nil {
		return
	}
	if err := mc.readAuth(); err != nil {
		return
	}
	mc.authed = true
	// 读/写超时分开维护（等待超时取广告的 wait_timeout）。
	// 旧实现握手时用 SetDeadline 设了"读+写"的固定 120s，主循环只续读，
	// 于是连接存活 2 分钟后任何写入都会 i/o timeout 被断开（连接池必现）。
	// 这里与二进制协议一致：只在窗口消耗过半时才真正下发 setsockopt，
	// 避免每条命令两次系统调用（热路径上两次 SetDeadline 约占 1µs）。
	waitTimeout := mysqlWaitTimeout()
	// 基准时间取"过去"，保证进循环后的第一次刷新必然下发 deadline
	// （否则前半个 wait_timeout 窗口内连接没有任何超时保护）
	readAt := time.Now().Add(-waitTimeout)
	writeAt := time.Now().Add(-waitTimeout)
	refreshDeadline := func() {
		if now := time.Now(); now.Sub(readAt) > waitTimeout/2 {
			readAt = now
			mc.conn.SetReadDeadline(now.Add(waitTimeout))
		}
		if now := time.Now(); now.Sub(writeAt) > waitTimeout/2 {
			writeAt = now
			mc.conn.SetWriteDeadline(now.Add(waitTimeout))
		}
	}
	for {
		refreshDeadline()
		cmd, payload, err := mc.readPacket()
		if err != nil {
			return
		}
		switch cmd {
		case comQuit:
			return
		case comPing:
			mc.db.stats.IncCmd("PING")
			mc.writeOK()
		case comInitDB:
			mc.handleInitDB(string(payload))
		case comQuery:
			mc.handleQuery(string(payload))
		case comFieldList:
			mc.db.stats.IncCmd("FIELD_LIST")
			mc.handleFieldList(payload)
		case comStmtPrepare:
			mc.db.stats.IncCmd("PREPARE")
			mc.handlePrepare(string(payload))
		case comStmtExecute:
			mc.db.stats.IncCmd("EXECUTE")
			mc.handleExecute(payload)
		case comStmtFetch:
			// 未声明 CLIENT_PS_MULTI_RESULTS / 游标支持：明确报错而不是静默丢弃
			mc.sendError(1235, "cursor fetch is not supported")
		case comStmtClose:
			mc.handleStmtClose(payload)
		case comStmtReset:
			mc.handleStmtReset(payload)
		case comStmtSendLong:
			mc.handleSendLong(payload)
		case comSetOption:
			// COM_SET_OPTION：无多语句支持，回 EOF 表示"保持现状"
			var eof []byte
			eof = append(eof, 0xfe, 0x00, 0x00, 0x02, 0x00)
			mc.writePacket(eof)
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

// mysqlWaitTimeout 从广告的变量里取 wait_timeout（秒），异常时退回 8 小时。
func mysqlWaitTimeout() time.Duration {
	if v := mysqlCfg.get().Variables["wait_timeout"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 8 * time.Hour
}

// handleInitDB 响应 COM_INIT_DB：真正切换连接级默认数据库（此前是静默 no-op）。
func (mc *mysqlConn) handleInitDB(name string) {
	if !mysqlCfg.hasDatabase(name) {
		mc.sendError(1049, fmt.Sprintf("unknown database '%s'", name)) // ER_BAD_DB_ERROR
		return
	}
	mc.schema = name
	mc.writeOK()
}

// ---- 包收发 ----

func (mc *mysqlConn) writePacket(payload []byte) error {
	return writeMysqlPacket(mc.bw, &mc.seq, payload)
}

// writeMysqlPacket 发送一个（可能多分片的）MySQL 包。用栈上 4 字节头部并分开写，
// 避免每包一次的堆分配与整包拷贝（append(hdr, chunk...)）。
func writeMysqlPacket(w io.Writer, seq *uint8, payload []byte) error {
	var hdr [4]byte
	writeChunk := func(chunk []byte) error {
		hdr[0] = byte(len(chunk))
		hdr[1] = byte(len(chunk) >> 8)
		hdr[2] = byte(len(chunk) >> 16)
		hdr[3] = *seq
		*seq++
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if len(chunk) == 0 {
			return nil
		}
		_, err := w.Write(chunk)
		return err
	}
	for {
		chunk := payload
		if len(chunk) > 0xffffff {
			chunk = chunk[:0xffffff]
			payload = payload[0xffffff:]
		} else {
			payload = nil
		}
		if err := writeChunk(chunk); err != nil {
			return err
		}
		if len(chunk) < 0xffffff {
			return nil
		}
		// 长度恰为 0xffffff 的整数倍时，必须以一个空包收尾，
		// 否则客户端会一直等下一个分片（大结果集挂起）。
		if payload == nil {
			return writeChunk(nil)
		}
	}
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
// 总长度受 maxMySQLPacket 钳制：多分片续包不设上限会被恶意客户端无限撑大内存。
func (mc *mysqlConn) readRawPacket() (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(mc.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	length := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	mc.seq = hdr[3] + 1
	// 认证前把单包限制到 64KB：旧实现按包头声明长度直接 make，
	// 未认证连接即可索要 16MB × N 分片（配合 100 连接上限是 GB 级放大）。
	limit := maxMySQLPacket
	if !mc.authed {
		limit = 64 << 10
	}
	if length > limit {
		return 0, nil, fmt.Errorf("packet exceeds limit before authentication")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(mc.br, payload); err != nil {
		return 0, nil, err
	}
	// 多分片拼接
	for length == 0xffffff {
		if len(payload)+0xffffff > limit {
			return 0, nil, fmt.Errorf("packet exceeds max_allowed_packet")
		}
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
		// 确定性 seed 会让 scramble 可预测（重放认证成为可能），直接拒绝服务
		return fmt.Errorf("crypto/rand unavailable: %w", err)
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
	// 认证响应：长度编码（CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA 或 1 字节 / 0xfc 两字节形式）
	authLen := 0
	if pos < len(payload) {
		switch first := payload[pos]; {
		case first < 251:
			authLen = int(first)
			pos++
		case first == 0xfc:
			if pos+3 > len(payload) {
				mc.sendError(1045, "bad auth response length")
				mc.bw.Flush()
				return errBadPacket
			}
			authLen = int(payload[pos+1]) | int(payload[pos+2])<<8
			pos += 3
		default:
			mc.sendError(1045, "unsupported auth response encoding")
			mc.bw.Flush()
			return errBadPacket
		}
		if pos+authLen > len(payload) {
			mc.sendError(1045, "bad auth response length")
			mc.bw.Flush()
			return errBadPacket
		}
	}
	authResp := payload[pos : pos+authLen]
	pos += authLen
	// 数据库名（CLIENT_CONNECT_WITH_DB 时位于插件名之前）；插件名（CLIENT_PLUGIN_AUTH）。
	plugin := "mysql_native_password"
	var clientDB string
	sawStr := false
	for pos < len(payload) {
		p := readNulString(payload, &pos)
		switch p {
		case "mysql_native_password", "caching_sha2_password", "mysql_clear_password", "sha256_password":
			plugin = p
			sawStr = true
		default:
			if !sawStr {
				clientDB = p
			}
		}
	}
	if clientDB != "" && mysqlCfg.hasDatabase(clientDB) {
		mc.schema = clientDB
	}
	cfg := mc.db.configSnapshot()
	storedUser := cfg.User
	storedSHA1 := cfg.PasswordSHA1
	// 口令爆破防护（与 Web / 二进制入口共用）
	ip := remoteIP(mc.conn)
	gkey := loginKey(ip, user)
	if blocked, left := loginLimit.Blocked(gkey); blocked {
		logf(LOG_WARN, "mysql auth blocked: ip=%s user=%q retry_after=%s", ip, user, left)
		mc.sendError(1045, "too many failed attempts")
		mc.bw.Flush()
		return errAccessDenied
	}
	if user != storedUser {
		loginLimit.Fail(gkey)
		mc.sendError(1045, fmt.Sprintf("access denied for user '%s'", user)) // ER_ACCESS_DENIED_ERROR
		mc.bw.Flush()
		return errAccessDenied
	}
	// 客户端选择 native → 直接校验；否则走 auth switch 回退到 native。
	if plugin == "mysql_native_password" {
		if !verifyNativePassword(storedSHA1, authResp, mc.scramble) {
			loginLimit.Fail(gkey)
			mc.sendError(1045, "access denied")
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
		// 等待客户端回发 native 响应。个别客户端会多包一层 0xfe 前缀
		//（此时总长 21）；原生 20 字节响应本身以 0xfe 开头属正常数据，不能误剥。
		_, resp, err := mc.readRawPacket()
		if err != nil {
			return err
		}
		if len(resp) == 21 && resp[0] == 0xfe {
			resp = resp[1:]
		}
		if !verifyNativePassword(storedSHA1, resp, mc.scramble) {
			loginLimit.Fail(gkey)
			mc.sendError(1045, "access denied")
			mc.bw.Flush()
			return errAccessDenied
		}
	}
	loginLimit.Reset(gkey)
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

// verifyNativePassword 校验 mysql_native_password：
// authResp = SHA1(password) XOR SHA1(scramble + SHA1(SHA1(password)))。
// 只接受配置中持久化的 SHA1(password) 摘要；摘要缺失时**拒绝**，
// 不再回退到内存明文（否则默认口令 `password` 会让 MySQL 端口裸奔）。
func verifyNativePassword(sha1Hex string, authResp, scramble []byte) bool {
	var sha1p [20]byte
	if sha1Hex == "" {
		return false
	}
	b, err := hex.DecodeString(sha1Hex)
	if err != nil || len(b) != 20 {
		return false
	}
	copy(sha1p[:], b)
	if len(authResp) == 0 {
		// 空口令：存储摘要应恰为 SHA1("")；非空口令拒绝空响应
		empty := sha1.Sum([]byte(""))
		return bytes.Equal(sha1p[:], empty[:])
	}
	sha1p2 := sha1.Sum(sha1p[:])
	h := sha1.New()
	h.Write(scramble)
	h.Write(sha1p2[:])
	expected := h.Sum(nil)
	if len(authResp) != len(expected) {
		return false
	}
	out := make([]byte, len(expected))
	for i := 0; i < len(expected); i++ {
		out[i] = authResp[i] ^ expected[i]
	}
	return subtle.ConstantTimeCompare(out, sha1p[:]) == 1
}

// ---- 命令处理 ----

// handleQuery 响应 COM_QUERY（事务/SET/USE 由连接级处理，其余进入引擎）。
func (mc *mysqlConn) handleQuery(sql string) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		mc.sendError(1065, "query was empty")
		return
	}
	// 事务与 SET 语句：维护连接级事务状态，不进入数据引擎。
	// 命令分派只依赖首词：EqualFold 直接比较（零分配），不再做任何 ToUpper
	// 拷贝——旧实现先整串、后首词大写，每条查询至少多一次堆分配。
	fw := firstWordRaw(sql)
	// 命令计数：按语句类型计入与二进制协议一致的命令名（此前 MySQL 路径
	// 完全没有计数，QPS/TPS 对 MySQL 流量恒为 0）。
	cmd := "QUERY"
	for _, w := range mysqlCmdWords {
		if strings.EqualFold(fw, w) {
			cmd = w
			break
		}
	}
	mc.db.stats.IncCmd(cmd)
	// 信息函数（DATABASE()/USER() 等）需要连接上下文
	setSessionContext(mc.schema, mc.user)
	if matchAnyFold(fw, "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "SET", "SETS") {
		mc.handleTxCommand(sql)
		return
	}
	// USE：切换本连接的默认数据库（连接级状态，不再污染全局 curDB）
	if strings.EqualFold(fw, "USE") {
		mc.handleUse(sql)
		return
	}
	var txnID uint64
	if mc.inTxn {
		txnID = mc.txnID
	} else if !mc.autocommit {
		// autocommit=0：写语句开启隐式事务，直到 COMMIT/ROLLBACK
		if isWriteStatement(sql) {
			txnID = mc.db.BeginTxn()
			mc.implicitTxn = txnID
		}
	}
	var out sqlOut
	columns, rows, affected, _, err := mc.db.runSQLSchemaAsOut(mc.user, sql, nil, txnID, mc.schema, &out)
	if err != nil {
		// 仅在出错时判断是否带表达式的 select（无 from），避免成功路径整串 ToLower 拷贝。
		lower := strings.ToLower(sql)
		if strings.HasPrefix(lower, "select ") && !strings.Contains(lower, " from ") {
			mc.sendExprResult(sql)
			return
		}
		mc.sendSQLError(err)
		return
	}
	if len(columns) == 0 {
		mc.writeOKAffectedID(affected, out.LastInsertID)
		return
	}
	mc.sendResultSet(columns, rows)
}

// handleUse 处理 USE <db>：按分词解析库名（支持反引号/引号），校验后切换连接级 schema。
func (mc *mysqlConn) handleUse(sql string) {
	toks, err := tokenizeSQL(sql)
	if err != nil {
		mc.sendError(1064, "parse error in USE")
		return
	}
	defer putSQLTokens(toks)
	// toks[0]=USE
	if len(toks) < 2 || (toks[1].kind != kindIdent && toks[1].kind != kindStr) || (len(toks) > 3 && toks[2].kind != kindEof) {
		mc.sendError(1064, "invalid USE statement")
		return
	}
	name := toks[1].val
	if !mysqlCfg.hasDatabase(name) {
		mc.sendError(1049, fmt.Sprintf("unknown database '%s'", name)) // ER_BAD_DB_ERROR
		return
	}
	mc.schema = name
	mc.writeOK()
}

// firstWordRaw 返回 SQL 首个单词（大小写保持原样，按任意空白或 '(' 截断）。
// 与 firstWord 的区别：不剥离前导注释、不转换大小写——调用方以
// EqualFold/matchAnyFold 比较，避免每条查询一次整串大写拷贝。
func firstWordRaw(sql string) string {
	sql = strings.TrimSpace(sql)
	end := len(sql)
	for i := 0; i < len(sql); i++ {
		if sql[i] == ' ' || sql[i] == '\t' || sql[i] == '\n' || sql[i] == '\r' || sql[i] == '(' {
			end = i
			break
		}
	}
	return sql[:end]
}

// mysqlCmdWords 统计用命令词表（与二进制协议一致的命令名）。
var mysqlCmdWords = [...]string{"INSERT", "UPDATE", "DELETE", "SELECT", "CREATE", "DROP", "ALTER", "SHOW", "DESCRIBE", "DESC"}

// handleTxCommand 处理 BEGIN/COMMIT/ROLLBACK/SET 等连接级事务与会话语句。
// 前缀判断走大小写不敏感比较：不再整串 ToUpper（SET 常携带很长的变量清单）。
func (mc *mysqlConn) handleTxCommand(sql string) {
	switch {
	case hasPrefixFold(sql, "BEGIN"), hasPrefixFold(sql, "START"):
		if mc.inTxn {
			mc.sendError(1568, "transaction already active")
			return
		}
		// 已处于 autocommit=0 的隐式事务：BEGIN 只是语义确认，沿用该事务
		if mc.implicitTxn != 0 {
			mc.txnID = mc.implicitTxn
			mc.implicitTxn = 0
			mc.inTxn = true
			mc.writeOK()
			return
		}
		mc.txnID = mc.db.BeginTxn()
		mc.inTxn = true
		mc.writeOK()
	case hasPrefixFold(sql, "COMMIT"):
		txn := mc.txnID
		if txn == 0 {
			txn = mc.implicitTxn
		}
		if txn == 0 {
			mc.writeOK() // autocommit 下 COMMIT 是空操作
			return
		}
		if err := mc.db.CommitTxn(txn); err != nil {
			mc.inTxn = false
			mc.txnID = 0
			mc.implicitTxn = 0
			mc.sendError(1213, err.Error())
			return
		}
		mc.inTxn = false
		mc.txnID = 0
		mc.implicitTxn = 0
		mc.writeOK()
	case hasPrefixFold(sql, "ROLLBACK"):
		// ROLLBACK TO [SAVEPOINT] name：部分回滚，事务保持活跃
		if isRollbackToSavepoint(sql) {
			if !mc.inTxn || mc.txnID == 0 {
				mc.sendError(1305, "SAVEPOINT does not exist")
				return
			}
			if err := mc.db.RollbackToSavepoint(mc.txnID, savepointName(sql)); err != nil {
				mc.sendSQLError(err)
				return
			}
			mc.writeOK()
			return
		}
		if txn := mc.txnID; txn != 0 {
			_ = mc.db.RollbackTxn(txn)
		}
		if mc.implicitTxn != 0 {
			_ = mc.db.RollbackTxn(mc.implicitTxn)
		}
		mc.inTxn = false
		mc.txnID = 0
		mc.implicitTxn = 0
		mc.writeOK()
	case hasPrefixFold(sql, "SAVEPOINT"):
		if !mc.inTxn || mc.txnID == 0 {
			mc.sendError(1305, "SAVEPOINT does not exist (no active transaction)")
			return
		}
		if err := mc.db.Savepoint(mc.txnID, savepointName(sql)); err != nil {
			mc.sendSQLError(err)
			return
		}
		mc.writeOK()
	case hasPrefixFold(sql, "RELEASE"):
		if !mc.inTxn || mc.txnID == 0 {
			mc.sendError(1305, "SAVEPOINT does not exist (no active transaction)")
			return
		}
		if err := mc.db.ReleaseSavepoint(mc.txnID, savepointName(sql)); err != nil {
			mc.sendSQLError(err)
			return
		}
		mc.writeOK()
	case hasPrefixFold(sql, "SET"), hasPrefixFold(sql, "SETS"):
		mc.handleSetVar(sql)
	}
}

// handleSetVar 处理 SET 语句：目前只识别 autocommit（影响事务行为），
// 其余（NAMES / sql_mode / 会话变量）一律返回 OK。
func (mc *mysqlConn) handleSetVar(sql string) {
	lower := strings.ToLower(sql)
	// 事务隔离级别：SET [SESSION|GLOBAL] TRANSACTION ISOLATION LEVEL <level>
	if strings.Contains(lower, "isolation level") || strings.Contains(lower, "transaction isolation") {
		mc.handleSetIsolation(lower)
		return
	}
	if strings.Contains(lower, "autocommit") {
		if strings.Contains(lower, "autocommit=0") || strings.Contains(lower, "autocommit = 0") ||
			strings.Contains(lower, "autocommit='0'") || strings.Contains(lower, "autocommit = '0'") {
			mc.autocommit = false
		} else if strings.Contains(lower, "autocommit=1") || strings.Contains(lower, "autocommit = 1") ||
			strings.Contains(lower, "autocommit='1'") || strings.Contains(lower, "autocommit = '1'") {
			// 打开 autocommit 前先把挂起的隐式事务提交掉
			if mc.implicitTxn != 0 {
				_ = mc.db.CommitTxn(mc.implicitTxn)
				mc.implicitTxn = 0
			}
			mc.autocommit = true
		}
	}
	mc.writeOK()
}

// handleSetIsolation 处理 SET TRANSACTION ISOLATION LEVEL。
// 引擎只实现 READ COMMITTED（写冲突在提交时按行版本检测）。
// 其它级别明确报错 1235，避免应用误以为拥有 REPEATABLE READ 的读快照。
func (mc *mysqlConn) handleSetIsolation(lower string) {
	switch {
	case strings.Contains(lower, "read committed"):
		mc.isoLevel = "READ COMMITTED"
		mc.writeOK()
	case strings.Contains(lower, "read uncommitted"):
		mc.sendError(1235, "isolation level 'READ UNCOMMITTED' is not supported; use READ COMMITTED")
	case strings.Contains(lower, "repeatable read"):
		mc.sendError(1235, "isolation level 'REPEATABLE READ' is not supported yet; use READ COMMITTED")
	case strings.Contains(lower, "serializable"):
		mc.sendError(1235, "isolation level 'SERIALIZABLE' is not supported; use READ COMMITTED")
	default:
		mc.sendError(1235, "unknown isolation level in SET TRANSACTION")
	}
}

// isWriteStatement 判断语句是否写库（autocommit=0 时用于开启隐式事务）。
func isWriteStatement(sql string) bool {
	switch strings.ToUpper(firstWordRaw(sql)) {
	case "INSERT", "REPLACE", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER",
		"TRUNCATE", "RENAME", "GRANT", "REVOKE":
		return true
	}
	return false
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
	// 参数计数写入响应包时是 uint16：超限会被静默截断导致协议错乱
	if n > 65535 {
		putSQLTokens(toks)
		mc.sendError(1391, "too many parameters")
		return
	}
	if len(mc.prepared) >= maxPreparedStmts {
		putSQLTokens(toks)
		mc.sendError(1461, "max prepared statement count reached")
		return
	}
	mc.nextStmt++
	id := mc.nextStmt
	if mc.prepared == nil {
		mc.prepared = make(map[uint32]*mysqlPreparedStmt)
	}
	mc.prepared[id] = &mysqlPreparedStmt{id: id, sql: sql, paramCount: n, toks: toks}

	var buf []byte
	buf = append(buf, 0x00)           // status OK
	buf = appendU32LE(buf, id)        // statement id
	buf = appendU16LE(buf, 0)         // column count（EXECUTE 时下发真实列定义）
	buf = appendU16LE(buf, uint16(n)) // parameter count
	buf = append(buf, 0x00)           // filler
	buf = appendU16LE(buf, 0)         // warning count
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
	if mc.schema != "" {
		return mc.schema
	}
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
	// 信息函数（DATABASE()/USER() 等）需要连接上下文
	setSessionContext(mc.schema, mc.user)
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
		// newParameterBoundFlag=0：复用上次的类型数组。libmysqlclient 等客户端
		// 第二次执行同一语句时不再重发类型，旧实现直接报 1243 导致预处理语句
		// 只能成功执行一次。
		types := st.paramTypes
		if newBound != 0 {
			types = make([]byte, st.paramCount*2)
			if pos+len(types) > len(payload) {
				mc.sendError(1243, "malformed parameter types")
				return
			}
			copy(types, payload[pos:pos+len(types)])
			pos += len(types)
			st.paramTypes = types
		} else if len(types) != st.paramCount*2 {
			mc.sendError(1243, "no cached parameter types to reuse")
			return
		}
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
			// COM_STMT_SEND_LONG_DATA 送来的大字段覆盖包内空值
			if ld, ok := mc.takeLongData(stmtID, uint16(i)); ok {
				v = string(ld)
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
	var out sqlOut
	columns, rows, affected, _, err := mc.db.runSQLTokensAsOut(mc.user, st.toks, st.sql, params, txnID, mc.schema, &out)
	if err != nil {
		mc.sendSQLError(err)
		return
	}
	if len(columns) == 0 {
		mc.writeOKAffectedID(affected, out.LastInsertID)
		return
	}
	// 预处理语句的结果集必须走二进制行协议：客户端（go-sql-driver 等）
	// 此时按 binaryRows 解析，发文本行会直接解析错乱。
	mc.sendBinaryResultSet(columns, rows)
}

// parseBinaryParam 解析二进制协议的一个参数值，返回 Go 值。
// 所有读取都做边界检查：畸形包返回错误（清洁的 1243 错误）而不是 panic。
func parseBinaryParam(pType byte, payload []byte, pos *int) (interface{}, error) {
	need := func(n int) error {
		if *pos+n > len(payload) {
			return fmt.Errorf("truncated param value")
		}
		return nil
	}
	switch pType {
	case 0x00: // NULL
		return nil, nil
	case 0x01: // TINY
		if err := need(1); err != nil {
			return nil, err
		}
		v := int64(int8(payload[*pos]))
		*pos++
		return v, nil
	case 0x02: // SHORT
		if err := need(2); err != nil {
			return nil, err
		}
		v := int64(int16(uint16(payload[*pos]) | uint16(payload[*pos+1])<<8))
		*pos += 2
		return v, nil
	case 0x03: // LONG
		if err := need(4); err != nil {
			return nil, err
		}
		v := int64(uint32(payload[*pos]) | uint32(payload[*pos+1])<<8 | uint32(payload[*pos+2])<<16 | uint32(payload[*pos+3])<<24)
		*pos += 4
		return v, nil
	case 0x08: // LONGLONG
		if err := need(8); err != nil {
			return nil, err
		}
		v := uint64(payload[*pos]) | uint64(payload[*pos+1])<<8 | uint64(payload[*pos+2])<<16 | uint64(payload[*pos+3])<<24 |
			uint64(payload[*pos+4])<<32 | uint64(payload[*pos+5])<<40 | uint64(payload[*pos+6])<<48 | uint64(payload[*pos+7])<<56
		*pos += 8
		return int64(v), nil
	case 0x04: // FLOAT
		if err := need(4); err != nil {
			return nil, err
		}
		v := math.Float32frombits(uint32(payload[*pos]) | uint32(payload[*pos+1])<<8 | uint32(payload[*pos+2])<<16 | uint32(payload[*pos+3])<<24)
		*pos += 4
		return v, nil
	case 0x05: // DOUBLE
		if err := need(8); err != nil {
			return nil, err
		}
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
// 按长度决定可用分量（MySQL 语义），长度不足的分量取零值而非越界读取。
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
	var y, mo, d, h, mi, s int
	if len(b) >= 4 {
		y = int(b[0])<<8 | int(b[1])
		mo = int(b[2])
		d = int(b[3])
	}
	if len(b) >= 7 {
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
	var y, mo, d int
	if len(b) >= 4 {
		y = int(b[0])<<8 | int(b[1])
		mo = int(b[2])
		d = int(b[3])
	}
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
	if len(b) < 8 {
		return nil, fmt.Errorf("truncated time value")
	}
	neg := b[0] != 0
	d := int(b[1])<<24 | int(b[2])<<16 | int(b[3])<<8 | int(b[4])
	h, mi, s := 0, 0, 0
	if len(b) >= 8 {
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
		putSQLTokens(st.toks)
	}
	delete(mc.prepared, stmtID)
	// long data 缓冲一并清理（此前只删 prepared，longData 泄漏到断连）
	delete(mc.longData, stmtID)
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
// 校验语句存在性并钳制总量（此前任意 stmtID 无上限累积，构成内存耗尽向量）。
func (mc *mysqlConn) handleSendLong(payload []byte) {
	if len(payload) < 6 {
		return
	}
	stmtID := uint32(payload[0]) | uint32(payload[1])<<8 | uint32(payload[2])<<16 | uint32(payload[3])<<24
	if _, ok := mc.prepared[stmtID]; !ok {
		return // 未知语句：静默丢弃（与 MySQL 行为一致）
	}
	paramID := uint16(payload[4]) | uint16(payload[5])<<8
	chunk := payload[6:]
	if mc.longData == nil {
		mc.longData = make(map[uint32]map[uint16][]byte)
	}
	m := mc.longData[stmtID]
	if m == nil {
		m = make(map[uint16][]byte)
		mc.longData[stmtID] = m
	}
	if len(m[paramID])+len(chunk) > maxMySQLPacket {
		return // 超过 max_allowed_packet：忽略多余数据
	}
	m[paramID] = append(m[paramID], chunk...)
}

// takeLongData 取出并清除某参数的 long data（一次性，与 MySQL 语义一致）。
func (mc *mysqlConn) takeLongData(stmtID uint32, paramID uint16) ([]byte, bool) {
	m := mc.longData[stmtID]
	if m == nil {
		return nil, false
	}
	b, ok := m[paramID]
	if ok {
		delete(m, paramID)
		if len(m) == 0 {
			delete(mc.longData, stmtID)
		}
	}
	return b, ok
}

// sendResultSet 发送结果集。列类型按首行实际值推断：
// bool → TINY(0x01)、int64 → LONGLONG(0x08)、float64 → DOUBLE(0x05)、其余 → VARCHAR(0x0f)。
func (mc *mysqlConn) sendResultSet(columns []string, rows [][]interface{}) {
	types := columnTypesFor(columns, rows)
	schema := "tsumugi"
	if mc.schema != "" {
		schema = mc.schema
	} else if mc.db != nil {
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
		buf = appendLenEncStr(buf, "def")        // catalog
		buf = appendLenEncStr(buf, schema)       // schema
		buf = appendLenEncStr(buf, "")           // table
		buf = appendLenEncStr(buf, "")           // org_table
		buf = appendLenEncStr(buf, columns[i])   // name
		buf = appendLenEncStr(buf, columns[i])   // org_name
		buf = appendLenEncInt(buf, 0x0c)         // fixed length field count
		buf = appendU16LE(buf, defaultCollation) // charset/transformation
		buf = appendU32LE(buf, mysqlColLen(types[i]))
		buf = append(buf, types[i])
		buf = appendU16LE(buf, 0) // flags
		buf = append(buf, 0)      // decimals
		buf = appendU16LE(buf, 0) // filler
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

// columnTypesFor 推断结果集列类型：取首行实际值；首行为 NULL 时向后找第一个非空值，
// 避免整列被当成 VARCHAR 而把数字当字符串返回。
func columnTypesFor(columns []string, rows [][]interface{}) []byte {
	types := make([]byte, len(columns))
	for i := range types {
		types[i] = 0x0f
	}
	for i := range columns {
		for _, r := range rows {
			if i < len(r) && r[i] != nil {
				types[i] = mysqlValueType(r[i])
				break
			}
		}
	}
	return types
}

// sendBinaryResultSet 发送二进制协议结果集（COM_STMT_EXECUTE 专用）。
func (mc *mysqlConn) sendBinaryResultSet(columns []string, rows [][]interface{}) {
	types := columnTypesFor(columns, rows)
	schema := mc.schemaName()
	var buf []byte
	buf = appendLenEncInt(buf, uint64(len(columns)))
	mc.writePacket(buf)
	for i := range columns {
		buf = buf[:0]
		buf = mc.appendColumnDef(buf, "def", schema, "", "", columns[i], columns[i], types[i])
		mc.writePacket(buf)
	}
	buf = buf[:0]
	buf = append(buf, 0xfe, 0x00, 0x00, 0x02, 0x00) // EOF: 列定义结束
	mc.writePacket(buf)

	nullLen := (len(columns) + 7 + 2) / 8
	var nulls [64]byte
	for _, r := range rows {
		buf = buf[:0]
		buf = append(buf, 0x00) // binary row 头
		if nullLen <= len(nulls) {
			for i := range nulls[:nullLen] {
				nulls[i] = 0
			}
			for i := 0; i < len(columns); i++ {
				if i >= len(r) || r[i] == nil {
					nulls[(i+2)>>3] |= 1 << uint((i+2)&7)
				}
			}
			buf = append(buf, nulls[:nullLen]...)
		} else {
			bitmap := make([]byte, nullLen)
			for i := 0; i < len(columns); i++ {
				if i >= len(r) || r[i] == nil {
					bitmap[(i+2)>>3] |= 1 << uint((i+2)&7)
				}
			}
			buf = append(buf, bitmap...)
		}
		for i := 0; i < len(columns) && i < len(r); i++ {
			if r[i] == nil {
				continue
			}
			buf = appendBinaryCell(buf, r[i], types[i])
		}
		mc.writePacket(buf)
	}
	buf = buf[:0]
	buf = append(buf, 0xfe, 0x00, 0x00, 0x02, 0x00) // EOF: 行结束
	mc.writePacket(buf)
}

// appendBinaryCell 按列类型编码一个二进制行值（NULL 由 null bitmap 表示，不在此处理）。
func appendBinaryCell(buf []byte, v interface{}, colType byte) []byte {
	switch colType {
	case 0x01: // TINY
		if b, ok := v.(bool); ok {
			if b {
				return append(buf, 1)
			}
			return append(buf, 0)
		}
		n, _ := toIntCell(v)
		return append(buf, byte(n))
	case 0x08: // LONGLONG
		n, _ := toIntCell(v)
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(n))
		return append(buf, b[:]...)
	case 0x05: // DOUBLE
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case float32:
			f = float64(x)
		default:
			if n, ok := toIntCell(v); ok {
				f = float64(n)
			}
		}
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(f))
		return append(buf, b[:]...)
	default: // VARCHAR / VAR_STRING：长度编码字符串
		return appendCell(buf, v)
	}
}

// handleFieldList 响应 COM_FIELD_LIST（客户端/驱动枚举表字段）。
func (mc *mysqlConn) handleFieldList(payload []byte) {
	pos := 0
	table := readNulString(payload, &pos)
	name := mc.db.qualifyTableIn(table, mc.schema)
	t := mc.db.getTable(name)
	if t == nil {
		mc.sendError(1146, fmt.Sprintf("table '%s' doesn't exist", table))
		return
	}
	if !mc.db.CheckPrivilege(mc.user, name, PermSelect) {
		mc.sendError(1142, "SELECT command denied")
		return
	}
	meta := t.Meta()
	for _, f := range meta.Fields {
		colType := byte(0x0f)
		switch f.Type {
		case TypeInt:
			colType = 0x08
		case TypeBool:
			colType = 0x01
		}
		var buf []byte
		buf = mc.appendColumnDef(buf, "def", mc.schemaName(), meta.Name, meta.Name, f.Name, f.Name, colType)
		mc.writePacket(buf)
	}
	mc.writePacket([]byte{0xfe, 0x00, 0x00, 0x02, 0x00})
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
		// NULL 在文本协议里必须是单字节 0xfb；appendLenEncInt(0xfb) 会输出
		// 0xfc 0xfb 0x00（长度编码），客户端据此解析出 3 字节垃圾。
		return append(buf, 0xfb)
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
	for len(sql) > 0 {
		if strings.HasPrefix(sql, "--") {
			if i := strings.IndexByte(sql, '\n'); i >= 0 {
				sql = strings.TrimSpace(sql[i+1:])
			} else {
				return ""
			}
		} else if strings.HasPrefix(sql, "/*") {
			if i := strings.Index(sql, "*/"); i >= 0 {
				sql = strings.TrimSpace(sql[i+2:])
			} else {
				return ""
			}
		} else if strings.HasPrefix(sql, "#") {
			if i := strings.IndexByte(sql, '\n'); i >= 0 {
				sql = strings.TrimSpace(sql[i+1:])
			} else {
				return ""
			}
		} else {
			break
		}
	}
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
	mc.writeOKAffectedID(affected, 0)
}

// writeOKAffectedID 写 OK 包：last insert id 带回服务端分配的自增主键
// （GORM 等 ORM 依赖 res.LastInsertId() 取回新建行的主键）。
func (mc *mysqlConn) writeOKAffectedID(affected int64, lastInsertID int64) {
	// 响应包 seq 使用 readPacket 已设好的 mc.seq（首包=1），勿重置为 0
	var buf []byte
	buf = append(buf, 0x00)
	buf = appendLenEncInt(buf, uint64(affected)) // affected rows
	buf = appendLenEncInt(buf, uint64(lastInsertID))
	buf = appendU16LE(buf, statusAutocommit) // status flags
	buf = appendU16LE(buf, 0)                // warnings
	mc.writePacket(buf)
}

func (mc *mysqlConn) sendError(code int, msg string) {
	mc.sendErrorState(code, sqlStateFor(code), msg)
}

// sendErrorState 允许指定 SQLSTATE（不同 errno 对应不同状态类）。
func (mc *mysqlConn) sendErrorState(code int, sqlState, msg string) {
	if len(sqlState) != 5 {
		sqlState = "HY000"
	}
	var buf []byte
	buf = append(buf, 0xff)
	buf = appendU16LE(buf, uint16(code))
	buf = append(buf, '#')
	buf = append(buf, []byte(sqlState)...)
	buf = append(buf, []byte(msg)...)
	mc.writePacket(buf)
}

// sendSQLError 按错误内容挑选 MySQL errno 与 SQLSTATE 后上报。
// ORM 依赖这些码做分支（1062 重复键、1213 死锁重试、1146 缺表迁移…），
// 一律返回 1064 会让客户端行为错误。
func (mc *mysqlConn) sendSQLError(err error) {
	code := mysqlErrno(err)
	mc.sendErrorState(code, sqlStateFor(code), mysqlErrText(err))
}
