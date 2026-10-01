package main

// ==================== 压力测试（走真实用户协议） ====================
// 压测流量与外部用户完全同路径，且可选择协议：
//   - native：TCP 二进制协议（CmdAuth 认证 → CmdInsert/CmdSelect）
//   - mysql：MySQL 线缆协议（握手 + mysql_native_password 认证 → COM_QUERY 文本协议）
// 每个命令都经过真实的连接建立、认证、协议解析与服务端处理，命令计数为
// 真实的 INSERT/SELECT（此前直接调引擎并使用 STRESS_* 专用命令名，QPS 不可比）。
//
// 凭据：native 模式每次压测创建仅存于内存的临时用户（随机口令，不写 users.json）
// 并内存授予压测表权限（不写 privileges.json）；mysql 模式使用配置用户 +
// password_sha1 摘要完成认证数学（服务端本就只存该摘要，无需明文口令）。

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const stressTableName = "stress_test"

// stressOpTimeout 单次协议往返的超时：卡死连接及时报错而不是拖到压测结束
const stressOpTimeout = 10 * time.Second

// stressConn 压测 worker 的协议连接抽象：native 与 mysql 各自实现。
type stressConn interface {
	insert(key int64) (bool, error)
	selectPoint(key int64) (bool, error)
	selectRange(key int64) (bool, error)
	createTable() error
	dropTable() error
	close()
}

// ioReadFull / ioCopyN 小包装，热循环里少写样板
func ioReadFull(r io.Reader, b []byte) (int, error) { return io.ReadFull(r, b) }

func ioCopyN(r io.Reader, n uint32) (int64, error) {
	return io.CopyN(io.Discard, r, int64(n))
}

// ---------------- native：二进制协议客户端 ----------------

type nativeStressClient struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	user string
	pass string
}

func dialStressNative(addr, user, pass string) (*nativeStressClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	c := &nativeStressClient{conn: conn, br: bufio.NewReaderSize(conn, 16*1024), bw: bufio.NewWriterSize(conn, 16*1024), user: user, pass: pass}
	if err := c.auth(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *nativeStressClient) close() { c.conn.Close() }

// resp 读取一个响应帧。
func (c *nativeStressClient) resp() (bool, error) {
	c.bw.Flush()
	var code [1]byte
	if _, err := io.ReadFull(c.br, code[:]); err != nil {
		return false, err
	}
	switch code[0] {
	case RespOK:
		return true, nil
	case RespErr:
		n, err := rU32(c.br)
		if err != nil {
			return false, err
		}
		if _, err := ioCopyN(c.br, n); err != nil {
			return false, err
		}
		return false, nil
	case RespRows:
		rows, err := rU32(c.br)
		if err != nil {
			return false, err
		}
		for i := uint32(0); i < rows; i++ {
			ln, err := rU32(c.br)
			if err != nil {
				return false, err
			}
			if _, err := ioCopyN(c.br, ln); err != nil {
				return false, err
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("unexpected response %d", code[0])
	}
}

func (c *nativeStressClient) auth() error {
	c.conn.SetDeadline(time.Now().Add(stressOpTimeout))
	c.bw.WriteByte(CmdAuth)
	wLenBytes(c.bw, []byte(c.user))
	wLenBytes(c.bw, []byte(c.pass))
	ok, err := c.resp()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("auth failed")
	}
	c.conn.SetDeadline(time.Time{})
	return nil
}

func (c *nativeStressClient) createTable() error {
	c.conn.SetDeadline(time.Now().Add(stressOpTimeout))
	c.bw.WriteByte(CmdCreateTable)
	wLenBytes(c.bw, []byte(stressTableName))
	wU16(c.bw, 2)
	wLenBytes(c.bw, []byte("id"))
	c.bw.WriteByte(byte(TypeInt))
	wU32(c.bw, 0)
	wLenBytes(c.bw, []byte("val"))
	c.bw.WriteByte(byte(TypeVarchar))
	wU32(c.bw, 100)
	wLenBytes(c.bw, []byte("id")) // pk
	wU16(c.bw, 0)                 // 无二级索引
	ok, err := c.resp()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("create table failed")
	}
	c.conn.SetDeadline(time.Time{})
	return nil
}

func (c *nativeStressClient) dropTable() error {
	c.conn.SetDeadline(time.Now().Add(stressOpTimeout))
	c.bw.WriteByte(CmdDropTable)
	wLenBytes(c.bw, []byte(stressTableName))
	ok, err := c.resp()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("drop table failed")
	}
	c.conn.SetDeadline(time.Time{})
	return nil
}

func (c *nativeStressClient) insert(key int64) (bool, error) {
	c.bw.WriteByte(CmdInsert)
	wLenBytes(c.bw, []byte(stressTableName))
	wI64(c.bw, key)
	wI64(c.bw, 0) // ttl
	wU16(c.bw, 2)
	wLenBytes(c.bw, []byte("id"))
	c.bw.WriteByte(byte(TypeInt))
	wI64(c.bw, key)
	wLenBytes(c.bw, []byte("val"))
	c.bw.WriteByte(byte(TypeVarchar))
	wLenBytes(c.bw, []byte(strconv.FormatInt(key, 10)))
	return c.resp()
}

func (c *nativeStressClient) selectPoint(key int64) (bool, error) {
	c.bw.WriteByte(CmdSelect)
	wLenBytes(c.bw, []byte(stressTableName))
	wU16(c.bw, 1)
	wLenBytes(c.bw, []byte("id"))
	c.bw.WriteByte(byte(TypeInt))
	wI64(c.bw, key)
	c.bw.WriteByte(0) // hasMin
	c.bw.WriteByte(0) // hasMax
	return c.resp()
}

func (c *nativeStressClient) selectRange(key int64) (bool, error) {
	c.bw.WriteByte(CmdSelect)
	wLenBytes(c.bw, []byte(stressTableName))
	wU16(c.bw, 0) // 无等值条件
	c.bw.WriteByte(1)
	wI64(c.bw, key)
	c.bw.WriteByte(1)
	wI64(c.bw, key+999)
	return c.resp()
}

// ---------------- mysql：线缆协议客户端（COM_QUERY 文本协议） ----------------

type mysqlStressClient struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	seq  byte
}

// mysqlAuthToken 由 SHA1(password) 摘要计算 native 认证响应：
// token = SHA1(pwd) XOR SHA1(scramble + SHA1(SHA1(pwd)))
func mysqlAuthToken(sha1pwd, scramble []byte) []byte {
	s2 := sha1.Sum(sha1pwd)
	h := sha1.New()
	h.Write(scramble)
	h.Write(s2[:])
	t := h.Sum(nil)
	for i := range t {
		t[i] ^= sha1pwd[i]
	}
	return t
}

// dialStressMySQL 完成 MySQL 握手与 mysql_native_password 认证。
// 认证只需 SHA1(password) 摘要（与 mysql.go verifyNativePassword 的数学一致）。
func dialStressMySQL(addr, user string, sha1pwd []byte) (*mysqlStressClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	c := &mysqlStressClient{conn: conn, br: bufio.NewReaderSize(conn, 16*1024), bw: bufio.NewWriterSize(conn, 16*1024)}
	payload, err := c.readPacket()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if len(payload) == 0 || payload[0] != 10 {
		conn.Close()
		return nil, fmt.Errorf("bad handshake protocol")
	}
	// 解析 scramble：proto(1) + version\0 + thread(4) + seed1(8) + filler(1)
	// + caps(2) + charset(1) + status(2) + caps2(2) + authLen(1) + reserved(10) + seed2(12)\0
	pos := 1
	for pos < len(payload) && payload[pos] != 0 {
		pos++
	}
	pos++
	pos += 4 // thread id
	seed1 := payload[pos : pos+8]
	pos += 8 + 1 + 2 + 1 + 2 + 2 + 1 + 10 // seed1 + filler + caps + charset + status + caps2 + authLen + reserved
	seed2 := payload[pos : pos+12]        // 第 13 字节是结尾 0
	scramble := append(append([]byte{}, seed1...), seed2...)

	// 握手响应：PROTOCOL_41 | SECURE_CONNECTION | PLUGIN_AUTH
	caps := uint32(1<<3 | 1<<8 | 1<<12)
	resp := make([]byte, 0, 128)
	resp = appendU32LE(resp, caps)
	resp = appendU32LE(resp, 1<<24) // max packet 16MB
	resp = append(resp, 45)         // utf8mb4_general_ci
	resp = append(resp, make([]byte, 23)...)
	resp = append(resp, []byte(user)...)
	resp = append(resp, 0)
	token := mysqlAuthToken(sha1pwd, scramble)
	resp = append(resp, byte(len(token)))
	resp = append(resp, token...)
	resp = append(resp, []byte("mysql_native_password")...)
	resp = append(resp, 0)
	if err := c.writePacket(resp); err != nil {
		conn.Close()
		return nil, err
	}
	// 认证结果（可能 AuthSwitchRequest：native 场景 scramble 不变，重算重发）
	for {
		payload, err = c.readPacket()
		if err != nil {
			conn.Close()
			return nil, err
		}
		if len(payload) == 0 {
			conn.Close()
			return nil, fmt.Errorf("empty auth response")
		}
		switch payload[0] {
		case 0x00: // OK
			return c, nil
		case 0xff:
			conn.Close()
			return nil, fmt.Errorf("auth denied")
		case 0xfe: // AuthSwitchRequest：同插件同 scramble，重发 token
			if err := c.writePacket(mysqlAuthToken(sha1pwd, scramble)); err != nil {
				conn.Close()
				return nil, err
			}
		default:
			conn.Close()
			return nil, fmt.Errorf("unexpected auth packet")
		}
	}
}

func (c *mysqlStressClient) close() { c.conn.Close() }

func (c *mysqlStressClient) readPacket() ([]byte, error) {
	var hdr [4]byte
	if _, err := ioReadFull(c.br, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	c.seq = hdr[3] + 1
	buf := make([]byte, n)
	if _, err := ioReadFull(c.br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (c *mysqlStressClient) writePacket(payload []byte) error {
	hdr := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), c.seq}
	if _, err := c.bw.Write(hdr); err != nil {
		return err
	}
	if _, err := c.bw.Write(payload); err != nil {
		return err
	}
	c.seq++
	return c.bw.Flush()
}

// query 发送 COM_QUERY 并读完整个响应（列定义/行/EOF/OK/ERR）。
func (c *mysqlStressClient) query(q string) (bool, error) {
	c.conn.SetDeadline(time.Now().Add(stressOpTimeout))
	defer c.conn.SetDeadline(time.Time{})
	if err := c.writePacket(append([]byte{0x03}, q...)); err != nil {
		return false, err
	}
	for {
		payload, err := c.readPacket()
		if err != nil {
			return false, err
		}
		if len(payload) == 0 {
			return false, fmt.Errorf("empty packet")
		}
		switch payload[0] {
		case 0x00: // OK
			return true, nil
		case 0xff: // ERR
			return false, nil
		case 0xfe:
			if len(payload) < 9 { // EOF（列定义/行结束）
				return true, nil
			}
			// lenenc 0xfe 开头的数据包（极长行）：继续读
		}
		// 列定义包 / 行包：继续
	}
}

func (c *mysqlStressClient) createTable() error {
	ok, err := c.query("CREATE TABLE " + stressTableName + " (id INT, val VARCHAR(100), PRIMARY KEY(id))")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("create table failed")
	}
	return nil
}

func (c *mysqlStressClient) dropTable() error {
	ok, err := c.query("DROP TABLE IF EXISTS " + stressTableName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("drop table failed")
	}
	return nil
}

func (c *mysqlStressClient) insert(key int64) (bool, error) {
	return c.query("INSERT INTO " + stressTableName + " VALUES (" + strconv.FormatInt(key, 10) + ", '" + strconv.FormatInt(key, 10) + "')")
}

func (c *mysqlStressClient) selectPoint(key int64) (bool, error) {
	return c.query("SELECT id FROM " + stressTableName + " WHERE id = " + strconv.FormatInt(key, 10))
}

func (c *mysqlStressClient) selectRange(key int64) (bool, error) {
	return c.query("SELECT id FROM " + stressTableName + " WHERE id BETWEEN " + strconv.FormatInt(key, 10) + " AND " + strconv.FormatInt(key+999, 10))
}

// ---------------- 压测编排 ----------------

// runStressTest 以真实协议流量压测。proto: "native"（默认）或 "mysql"。
func (db *DB) runStressTest(duration int, workers int, mode string, proto string) {
	nativeAddr := fmt.Sprintf("127.0.0.1:%d", db.config.Port)
	mysqlAddr := fmt.Sprintf("127.0.0.1:%d", db.config.MySQLPort)

	// 连接工厂：按协议建立（含认证），worker 断线后经此重连
	var dial func() (stressConn, error)
	switch proto {
	case "mysql":
		user := db.config.User
		var sha1pwd []byte
		if db.config.PasswordSHA1 != "" {
			if b, err := hex.DecodeString(db.config.PasswordSHA1); err == nil && len(b) == 20 {
				sha1pwd = b
			}
		}
		if sha1pwd == nil {
			s := sha1.Sum([]byte(db.config.Password))
			sha1pwd = s[:]
		}
		logf(LOG_VERB, "Stress test: %d workers, %d seconds, mode=%s, via %s (mysql protocol)", workers, duration, mode, mysqlAddr)
		dial = func() (stressConn, error) { return dialStressMySQL(mysqlAddr, user, sha1pwd) }
	default:
		proto = "native"
		// 临时用户（仅内存，随机口令）：AUTH 与表级权限全部走真实校验
		tempUser := "_stress_" + strconv.FormatInt(time.Now().UnixNano(), 36)
		tempPass := generatePassword()
		globalUsers.AddNoSave(&User{
			Username:  tempUser,
			Password:  hashPasswd(tempPass),
			CreatedAt: time.Now().Unix(),
		})
		db.privileges.GrantNoSave(tempUser, stressTableName, PermAll)
		defer func() {
			db.privileges.Revoke(tempUser, stressTableName, PermAll)
			globalUsers.Delete(tempUser)
		}()
		logf(LOG_VERB, "Stress test: %d workers, %d seconds, mode=%s, via %s (native protocol)", workers, duration, mode, nativeAddr)
		dial = func() (stressConn, error) { return dialStressNative(nativeAddr, tempUser, tempPass) }
	}

	// 建表（真实协议；残留旧表先删）
	boot, err := dial()
	if err != nil {
		logf(LOG_ERR, "stress: dial: %v", err)
		return
	}
	_ = boot.dropTable()
	if err := boot.createTable(); err != nil {
		logf(LOG_ERR, "stress: create table: %v", err)
		boot.close()
		return
	}
	boot.close()

	// workers
	stop := make(chan struct{})
	time.AfterFunc(time.Duration(duration)*time.Second, func() { close(stop) })
	var wg sync.WaitGroup
	var conns atomic.Int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var c stressConn
			defer func() {
				if c != nil {
					c.close()
				}
			}()
			ensureConn := func() bool {
				if c != nil {
					return true
				}
				nc, err := dial()
				if err != nil {
					return false
				}
				c = nc
				conns.Add(1)
				return true
			}
			if !ensureConn() {
				return
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				key := int64(atomic.AddUint64(&stressSeq, 1))
				var ok bool
				switch stressMode(mode) {
				case stressWrite:
					ok, _ = c.insert(key)
				case stressPoint:
					ok, _ = c.selectPoint(key)
				case stressRange:
					ok, _ = c.selectRange(key)
				case stressRead:
					if key%2 == 0 {
						ok, _ = c.selectPoint(key)
					} else {
						ok, _ = c.selectRange(key)
					}
				default: // rw：7:3 读写混合
					if key%10 < 7 {
						ok, _ = c.selectPoint(key)
					} else {
						ok, _ = c.insert(key)
					}
				}
				if !ok {
					// 连接级/协议级故障：断开重连重认证后继续
					if c != nil {
						c.close()
						c = nil
					}
					if !ensureConn() {
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()

	// 清理：走协议删表（临时用户/权限在 defer 中撤销）
	if cleanup, err := dial(); err == nil {
		if err := cleanup.dropTable(); err != nil {
			logf(LOG_VERB, "stress: cleanup drop: %v", err)
		}
		cleanup.close()
	}
	logf(LOG_VERB, "Stress test completed (proto=%s, mode=%s, connections=%d)", proto, mode, conns.Load())
}
