package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// ==================== 内存池 ====================

// rowMapPool 复用 map[string]interface{}，用于行数据构建和传递。
var rowMapPool = sync.Pool{
	New: func() interface{} { return make(map[string]interface{}, 8) },
}

// bytesBufPool 复用 *bytes.Buffer，用于网络响应拼接和临时缓冲。
var bytesBufPool = sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// ==================== 命令常量 ====================

const (
	CmdAuth           = 10
	CmdPing           = 11
	CmdCreateTable   = 20
	CmdDropTable     = 21
	CmdDescribe       = 22
	CmdAlterTable    = 23
	CmdInsert         = 30
	CmdSelect         = 31
	CmdUpdate         = 32
	CmdDelete         = 33
	CmdQuerySQL      = 35
	CmdBegin          = 40
	CmdCommit         = 41
	CmdRollback       = 42
	CmdCreateIndex   = 50
	CmdBatch          = 55
	CmdBackup         = 56
	CmdStatus         = 57
	CmdCompact        = 58
	CmdSetDurability = 59
	CmdCreateProc    = 60
	CmdCallProc      = 61
	CmdCreateView    = 70
	CmdCreateTrigger = 80
	CmdGrant          = 90
	CmdRevoke         = 91
)

const (
	RespOK        = 1
	RespErr       = 2
	RespValue     = 3
	RespNotFound = 4
	RespRows      = 5
	RespTxnID    = 6
)

// ==================== Server ====================

type Server struct {
	ln     net.Listener
	db     *DB
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// serviceClosers 服务监听器注册表（name -> io.Closer）。
// restart 需要在拉起子进程前关闭所有监听端口，否则子进程 bind 失败即服务全下线。
var serviceClosers sync.Map

func registerServiceCloser(name string, c io.Closer) { serviceClosers.Store(name, c) }

func closeServiceListeners() {
	serviceClosers.Range(func(key, value interface{}) bool {
		if c, ok := value.(io.Closer); ok {
			c.Close()
		}
		return true
	})
}

func NewServer(addr string, db *DB) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	registerServiceCloser("binary", ln)
	return &Server{ln: ln, db: db, stopCh: make(chan struct{})}, nil
}

func (s *Server) Start() {
	logf(LOG_OK, "Tsumugi started on %s", s.ln.Addr())
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				continue
			}
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) Stop() {
	close(s.stopCh)
	s.ln.Close()
	s.wg.Wait()
}

// ==================== Session ====================

type Session struct {
	conn       net.Conn
	br         *bufio.Reader
	bw         *bufio.Writer
	user       string
	auth       bool
	inTxn      bool
	txnID      uint64
	db         *DB
}

// read 从会话缓冲读取指定字节数（优先用 bufio，减少 syscall）。
// readFull 等价 io.ReadFull(session.conn)。
func (s *Session) readFull(b []byte) (int, error) { return io.ReadFull(s.br, b) }
func (s *Session) writeByte(b byte) error         { return s.bw.WriteByte(b) }

// ---- 快速手动编解码（避免 binary.Read/Write 的反射开销） ----

func rU16(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return uint16(b[0])<<8 | uint16(b[1]), nil
}

func rU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}

func rI64(r io.Reader) (int64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return int64(b[0])<<56 | int64(b[1])<<48 | int64(b[2])<<40 | int64(b[3])<<32 |
		int64(b[4])<<24 | int64(b[5])<<16 | int64(b[6])<<8 | int64(b[7]), nil
}

func rByte(r io.Reader) (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// rLenBytes 读取 u16 长度前缀 + 字节内容。
func rLenBytes(r io.Reader) ([]byte, error) {
	n, err := rU16(r)
	if err != nil {
		return nil, err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func wU16(w io.Writer, v uint16) error {
	var b [2]byte
	b[0], b[1] = byte(v>>8), byte(v)
	_, err := w.Write(b[:])
	return err
}

func wU32(w io.Writer, v uint32) error {
	var b [4]byte
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	_, err := w.Write(b[:])
	return err
}

func wI64(w io.Writer, v int64) error {
	var b [8]byte
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
	_, err := w.Write(b[:])
	return err
}

func wLenBytes(w io.Writer, data []byte) error {
	if err := wU16(w, uint16(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// ==================== 主连接处理 ====================

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	// 修复: 添加 panic recover，防止单个连接 panic 影响整体
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			logf(LOG_ERR, "connection handler panic: %v\nstack: %s", r, buf[:n])
		}
	}()
	session := &Session{
		conn:       conn,
		br:         bufio.NewReaderSize(conn, 32*1024),
		bw:         bufio.NewWriterSize(conn, 32*1024),
		db:   s.db,
		auth: false,
	}
	defer session.bw.Flush()
	defer func() {
		if session.inTxn {
			s.db.RollbackTxn(session.txnID)
			session.inTxn = false
		}
	}()

	// 空闲超时按需刷新：每条命令一次 SetReadDeadline 是多余的系统调用，
	// 只在超时窗口消耗过半时续期（认证状态变化时立即生效）。
	deadline := s.db.config.IdleTimeout
	if !session.auth && deadline > 30*time.Second {
		deadline = 30 * time.Second
	}
	deadlineAt := time.Now().Add(deadline)
	conn.SetReadDeadline(deadlineAt)

	for {
		if now := time.Now(); now.After(deadlineAt.Add(-deadline / 2)) {
			d := deadline
			if !session.auth && d > 30*time.Second {
				d = 30 * time.Second
			}
			deadlineAt = now.Add(d)
			conn.SetReadDeadline(deadlineAt)
		}

		cmd, err := session.br.ReadByte()
		if err != nil {
			if err != io.EOF {
				logf(LOG_ERR, "read cmd: %v", err)
			}
			return
		}
		if !session.auth && cmd != CmdAuth {
			s.writeError(session, "authentication required")
			return
		}

		switch cmd {
		case CmdAuth:
			s.handleAuth(session)
		case CmdPing:
			s.handlePing(session)
		case CmdCreateTable:
			s.handleCreateTable(session)
		case CmdDropTable:
			s.handleDropTable(session)
		case CmdDescribe:
			s.handleDescribe(session)
		case CmdAlterTable:
			s.handleAlterTable(session)
		case CmdInsert:
			s.handleInsert(session)
		case CmdSelect:
			s.handleSelect(session)
		case CmdUpdate:
			s.handleUpdate(session)
		case CmdDelete:
			s.handleDelete(session)
		case CmdQuerySQL:
			s.handleQuerySQL(session)
		case CmdBegin:
			s.handleBegin(session)
		case CmdCommit:
			s.handleCommit(session)
		case CmdRollback:
			s.handleRollback(session)
		case CmdCreateIndex:
			s.handleCreateIndex(session)
		case CmdBatch:
			s.handleBatch(session)
		case CmdBackup:
			s.handleBackup(session)
		case CmdStatus:
			s.handleStatus(session)
		case CmdCompact:
			s.handleCompact(session)
		case CmdSetDurability:
			s.handleSetDurability(session)
		case CmdCreateProc:
			s.handleCreateProc(session)
		case CmdCallProc:
			s.handleCallProc(session)
		case CmdCreateView:
			s.handleCreateView(session)
		case CmdCreateTrigger:
			s.handleCreateTrigger(session)
		case CmdGrant:
			s.handleGrant(session)
		case CmdRevoke:
			s.handleRevoke(session)
		default:
			s.writeError(session, fmt.Sprintf("unknown command: %d", cmd))
			session.bw.Flush()
			return
		}
		session.bw.Flush()
	}
}

// ==================== 辅助函数 ====================

func (s *Server) writeError(session *Session, msg string) {
	session.bw.WriteByte(RespErr)
	wU32(session.bw, uint32(len(msg)))
	session.bw.WriteString(msg)
	s.db.stats.IncErr()
}

func (s *Server) writeOK(session *Session) {
	session.bw.WriteByte(RespOK)
}

// ==================== 命令处理 ====================

// ---- AUTH ----
func (s *Server) handleAuth(session *Session) {
	s.db.stats.IncCmd("AUTH")
	userLen, err := rU16(session.br)
	if err != nil {
		return
	}
	user := make([]byte, userLen)
	if _, err := io.ReadFull(session.br, user); err != nil {
		return
	}
	passLen, err := rU16(session.br)
	if err != nil {
		return
	}
	pass := make([]byte, passLen)
	if _, err := io.ReadFull(session.br, pass); err != nil {
		return
	}
	uname := string(user)
	passwd := string(pass)
	// 验证用户凭据
	u := globalUsers.Get(uname)
	matched := u != nil && verifyPasswd(u, passwd)
	if matched {
		session.auth = true
		session.user = uname
		s.writeOK(session)
		logf(LOG_VERB, "auth success: %s", session.user)
	} else {
		s.writeError(session, "auth failed")
	}
}

// ---- PING ----
func (s *Server) handlePing(session *Session) {
	s.db.stats.IncCmd("PING")
	s.writeOK(session)
}

// ---- CREATE TABLE ----
func (s *Server) handleCreateTable(session *Session) {
	s.db.stats.IncCmd("CREATE_TABLE")
	tableNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "invalid table name length")
		return
	}
	name := make([]byte, tableNameLen)
	if _, err := io.ReadFull(session.br, name); err != nil {
		s.writeError(session, "read table name failed")
		return
	}
	tableName := string(name)

	fieldCount, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read field count failed")
		return
	}
	fields := make([]Field, fieldCount)
	for i := 0; i < int(fieldCount); i++ {
		fNameLen, err := rU16(session.br)
		if err != nil {
			s.writeError(session, "read field name length failed")
			return
		}
		fName := make([]byte, fNameLen)
		if _, err := io.ReadFull(session.br, fName); err != nil {
			s.writeError(session, "read field name failed")
			return
		}
		fType, err := rByte(session.br)
		if err != nil {
			s.writeError(session, "read field type failed")
			return
		}
		fLen, err := rU32(session.br)
		if err != nil {
			s.writeError(session, "read field length failed")
			return
		}
		fields[i] = Field{Name: string(fName), Type: FieldType(fType), Len: int(fLen)}
	}

	pkLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read pk length failed")
		return
	}
	pk := make([]byte, pkLen)
	if _, err := io.ReadFull(session.br, pk); err != nil {
		s.writeError(session, "read pk name failed")
		return
	}

	idxCount, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read index count failed")
		return
	}
	indexes := make(map[string]string)
	for i := 0; i < int(idxCount); i++ {
		idxNameLen, err := rU16(session.br)
		if err != nil {
			s.writeError(session, "read index name length failed")
			return
		}
		idxName := make([]byte, idxNameLen)
		if _, err := io.ReadFull(session.br, idxName); err != nil {
			s.writeError(session, "read index name failed")
			return
		}
		idxFieldLen, err := rU16(session.br)
		if err != nil {
			s.writeError(session, "read index field length failed")
			return
		}
		idxField := make([]byte, idxFieldLen)
		if _, err := io.ReadFull(session.br, idxField); err != nil {
			s.writeError(session, "read index field name failed")
			return
		}
		indexes[string(idxName)] = string(idxField)
	}

	if !session.db.CheckPrivilege(session.user, tableName, PermDDL) && session.user != session.db.config.User {
		s.writeError(session, "permission denied")
		return
	}

	meta := &TableMeta{
		Name:    tableName,
		PK:      string(pk),
		Fields:  fields,
		Indexes: indexes,
	}

	db := session.db
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.tables.Load(tableName); ok {
		s.writeError(session, "table exists")
		return
	}
	table := NewTable(meta, db.walFile)
	db.tables.Store(tableName, table)
	db.tablesCount.Add(1)
	db.catalog[tableName] = meta
	if err := db.writeCatalog(meta); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
	logf(LOG_VERB, "table %s created", tableName)
}

// readTableName 从连接读取 u16 长度前缀的表名字符串。
func readTableName(r *bufio.Reader) (string, error) {
	n, err := rU16(r)
	if err != nil {
		return "", fmt.Errorf("read table name length: %w", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("read table name: %w", err)
	}
	return string(buf), nil
}

// ---- DROP TABLE ----
func (s *Server) handleDropTable(session *Session) {
	s.db.stats.IncCmd("DROP_TABLE")
	tableName, err := readTableName(session.br)
	if err != nil {
		s.writeError(session, err.Error())
		return
	}
	if err := validateTableName([]byte(tableName)); err != nil {
		s.writeError(session, err.Error())
		return
	}
	if !s.db.CheckPrivilege(session.user, tableName, PermDDL) && session.user != s.db.config.User {
		s.writeError(session, "permission denied")
		return
	}

	db := session.db
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.tables.Load(tableName); ok {
		db.tables.Delete(tableName)
		db.tablesCount.Add(-1)
		delete(db.catalog, tableName)
		// 修复: 写 catalog DROP 记录，确保重启后表不会复活
		if err := db.writeCatalogDrop(tableName); err != nil {
			s.writeError(session, err.Error())
			return
		}
		s.writeOK(session)
	} else {
		s.writeError(session, "table not found")
	}
}

// ---- DESCRIBE ----
func (s *Server) handleDescribe(session *Session) {
	s.db.stats.IncCmd("DESCRIBE")
	tableName, err := readTableName(session.br)
	if err != nil {
		s.writeError(session, err.Error())
		return
	}
	if err := validateTableName([]byte(tableName)); err != nil {
		s.writeError(session, err.Error())
		return
	}
	db := session.db
	db.mu.RLock()
	meta, ok := db.catalog[tableName]
	db.mu.RUnlock()
	if !ok {
		s.writeError(session, "table not found")
		return
	}
	s.writeOK(session)
	wU16(session.bw, uint16(len(meta.Fields)))
	wU16(session.bw, uint16(len(meta.Indexes)))
	for _, f := range meta.Fields {
		nameBytes := []byte(f.Name)
		wU16(session.bw, uint16(len(nameBytes)))
		session.bw.Write(nameBytes)
		session.bw.Write([]byte{byte(f.Type)})
		wU32(session.bw, uint32(f.Len))
	}
	for idxName, fieldName := range meta.Indexes {
		nameBytes := []byte(idxName)
		wU16(session.bw, uint16(len(nameBytes)))
		session.bw.Write(nameBytes)
		fieldBytes := []byte(fieldName)
		wU16(session.bw, uint16(len(fieldBytes)))
		session.bw.Write(fieldBytes)
	}
}

// ---- ALTER TABLE (ADD COLUMN) ----
func (s *Server) handleAlterTable(session *Session) {
	s.db.stats.IncCmd("ALTER_TABLE")
	tableName, err := readTableName(session.br)
	if err != nil {
		s.writeError(session, err.Error())
		return
	}
	if err := validateTableName([]byte(tableName)); err != nil {
		s.writeError(session, err.Error())
		return
	}
	fieldNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read field name length failed")
		return
	}
	fieldName := make([]byte, fieldNameLen)
	if _, err := io.ReadFull(session.br, fieldName); err != nil {
		s.writeError(session, "read field name failed")
		return
	}
	fType, err := rByte(session.br)
	if err != nil {
		s.writeError(session, "read field type failed")
		return
	}
	fLen, err := rU32(session.br)
	if err != nil {
		s.writeError(session, "read field length failed")
		return
	}
	if !session.db.CheckPrivilege(session.user, tableName, PermDDL) && session.user != session.db.config.User {
		s.writeError(session, "permission denied")
		return
	}
	if err := session.db.AlterTableAddColumn(tableName, string(fieldName), FieldType(fType), int(fLen)); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// ---- INSERT ----

// readFieldValue 从连接读取一个字段的键值对：[fNameLen u16][fName bytes][valType byte][value]。
// 返回字段名与解析后的值。失败时返回具体错误。
func readFieldValue(r io.Reader) (string, interface{}, error) {
	fNameLen, err := rU16(r)
	if err != nil {
		return "", nil, fmt.Errorf("read field name length failed")
	}
	fName := make([]byte, fNameLen)
	if _, err := io.ReadFull(r, fName); err != nil {
		return "", nil, fmt.Errorf("read field name failed")
	}
	valType, err := rByte(r)
	if err != nil {
		return "", nil, fmt.Errorf("read value type failed")
	}
	switch FieldType(valType) {
	case TypeInt:
		v, err := rI64(r)
		if err != nil {
			return "", nil, fmt.Errorf("read int value failed")
		}
		return string(fName), v, nil
	case TypeVarchar:
		vLen, err := rU16(r)
		if err != nil {
			return "", nil, fmt.Errorf("read varchar length failed")
		}
		v := make([]byte, vLen)
		if _, err := io.ReadFull(r, v); err != nil {
			return "", nil, fmt.Errorf("read varchar value failed")
		}
		return string(fName), string(v), nil
	case TypeBool:
		b, err := rByte(r)
		if err != nil {
			return "", nil, fmt.Errorf("read bool value failed")
		}
		return string(fName), b == 1, nil
	}
	return "", nil, fmt.Errorf("unsupported field type %d", valType)
}

func (s *Server) handleInsert(session *Session) {
	s.db.stats.IncCmd("INSERT")
	tableNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read table name length failed")
		return
	}
	tableName := make([]byte, tableNameLen)
	if _, err := io.ReadFull(session.br, tableName); err != nil {
		s.writeError(session, "read table name failed")
		return
	}
	pk, err := rI64(session.br)
	if err != nil {
		s.writeError(session, "read pk failed")
		return
	}
	ttl, err := rI64(session.br)
	if err != nil {
		s.writeError(session, "read ttl failed")
		return
	}
	fieldCount, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read field count failed")
		return
	}
	values := rowMapPool.Get().(map[string]interface{})
	defer func() {
		for k := range values {
			delete(values, k)
		}
		rowMapPool.Put(values)
	}()
	for i := 0; i < int(fieldCount); i++ {
		name, val, err := readFieldValue(session.br)
		if err != nil {
			s.writeError(session, err.Error())
			return
		}
		values[name] = val
	}
	db := session.db
	table, ok := s.db.lookupTable(string(tableName))
	if !ok {
		s.writeError(session, "table not found")
		return
	}
	if !db.CheckPrivilege(session.user, string(tableName), PermInsert) {
		s.writeError(session, "permission denied")
		return
	}
	var expireAt int64
	if ttl > 0 {
		expireAt = time.Now().UnixNano() + ttl*1e9
	}
	var txnID uint64
	if session.inTxn {
		txnID = session.txnID
		if oldData, ok := table.pkTree.Get(pk); ok {
			version := decodeVersion(oldData)
			db.recordReadVersion(txnID, string(tableName), pk, version)
		}
		db.txnMu.Lock()
		if txn, ok := db.txns[txnID]; ok {
			if txn.Changes[string(tableName)] == nil {
				txn.Changes[string(tableName)] = make(map[int64][]byte)
			}
			txn.Changes[string(tableName)][pk] = nil
			txn.Tables[string(tableName)] = true
		}
		db.txnMu.Unlock()
	}
	if err := table.Insert(pk, values, expireAt, txnID, false); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// ---- SELECT ----
func (s *Server) handleSelect(session *Session) {
	s.db.stats.IncCmd("SELECT")
	tableNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read table name length failed")
		return
	}
	tableName := make([]byte, tableNameLen)
	if _, err := io.ReadFull(session.br, tableName); err != nil {
		s.writeError(session, "read table name failed")
		return
	}
	name := string(tableName)

	// 系统表
	if len(name) >= 2 && name[0] == '_' && name[1] == '_' {
		rows, err := session.db.getSystemTable(name)
		if err != nil {
			s.writeError(session, err.Error())
			return
		}
		session.bw.WriteByte(RespRows)
		wU32(session.bw, uint32(len(rows)))
		buf := bytesBufPool.Get().(*bytes.Buffer)
		defer func() {
			buf.Reset()
			bytesBufPool.Put(buf)
		}()
		for _, row := range rows {
			jsonBytes, _ := json.Marshal(row)
			buf.Reset()
			wI64(buf, 0)
			wI64(buf, 0)
			wU16(buf, uint16(len(jsonBytes)))
			buf.Write(jsonBytes)
			wU32(session.bw, uint32(buf.Len()))
			session.bw.Write(buf.Bytes())
		}
		return
	}

	condCount, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read condition count failed")
		return
	}
	conditions := rowMapPool.Get().(map[string]interface{})
	defer func() {
		for k := range conditions {
			delete(conditions, k)
		}
		rowMapPool.Put(conditions)
	}()
	for i := 0; i < int(condCount); i++ {
		name, val, err := readFieldValue(session.br)
		if err != nil {
			s.writeError(session, err.Error())
			return
		}
		conditions[name] = val
	}

	hasMin, err := rByte(session.br)
	if err != nil {
		s.writeError(session, "read hasMin failed")
		return
	}
	var minKey *int64 = nil
	if hasMin == 1 {
		v, err := rI64(session.br)
		if err != nil {
			s.writeError(session, "read minKey failed")
			return
		}
		minKey = &v
	}
	hasMax, err := rByte(session.br)
	if err != nil {
		s.writeError(session, "read hasMax failed")
		return
	}
	var maxKey *int64 = nil
	if hasMax == 1 {
		v, err := rI64(session.br)
		if err != nil {
			s.writeError(session, "read maxKey failed")
			return
		}
		maxKey = &v
	}

	db := session.db
	table, ok := s.db.lookupTable(name)
	if !ok {
		s.writeError(session, "table not found")
		return
	}
	if !db.CheckPrivilege(session.user, name, PermSelect) {
		s.writeError(session, "permission denied")
		return
	}
	// 无条件快速路径：直接返回 RBTree 原始字节，跳过 decodeRow → encodeRow 双重编解码
	if condCount == 0 {
		rawRows := table.SelectRaw(minKey, maxKey, 100)
		session.bw.WriteByte(RespRows)
		wU32(session.bw, uint32(len(rawRows)))
		for _, data := range rawRows {
			wU32(session.bw, uint32(len(data)))
			session.bw.Write(data)
		}
		return
	}
	// 条件查询原始字节路径：与上面的无条件路径一致，直接返回树内行编码
	// 字节（含 version/expireAt 前缀），跳过 Select 的行 map 物化与逐行
	// encodeRow 重编码——命中行零分配。
	rawRows := table.SelectRBytesCond(conditions, minKey, maxKey, -1)
	session.bw.WriteByte(RespRows)
	wU32(session.bw, uint32(len(rawRows)))
	for _, data := range rawRows {
		wU32(session.bw, uint32(len(data)))
		session.bw.Write(data)
	}
}

// ---- 通用 SQL 执行（CLI 使用）----
func (s *Server) handleQuerySQL(session *Session) {
	s.db.stats.IncCmd("QUERY_SQL")
	sqlLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read sql length failed")
		return
	}
	sqlBytes := make([]byte, sqlLen)
	if _, err := io.ReadFull(session.br, sqlBytes); err != nil {
		s.writeError(session, "read sql failed")
		return
	}
	columns, rows, affected, rawMsg, err := s.db.runSQL(string(sqlBytes))
	if err != nil {
		s.writeError(session, err.Error())
		return
	}
	if len(columns) == 0 {
		// 非结果集语句：RespOK + affected + rawMsg
		session.bw.WriteByte(RespOK)
		binary.Write(session.bw, binary.BigEndian, uint64(affected))
		wU16(session.bw, uint16(len(rawMsg)))
		session.bw.WriteString(rawMsg)
		return
	}
	// 结果集：RespRows + 列数 + 列名列表 + 行数 + 行数据。
	// 先写入本地缓冲，一次 Write 发送，减少系统调用与 TCP 小包。
	buf := bytesBufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bytesBufPool.Put(buf)
	}()
	buf.Reset()
	buf.WriteByte(RespRows)
	wU32(buf, uint32(len(columns)))
	for _, c := range columns {
		wU16(buf, uint16(len(c)))
		buf.WriteString(c)
	}
	wU32(buf, uint32(len(rows)))
	for _, r := range rows {
		wU32(buf, uint32(len(r)))
		for _, v := range r {
			writeQueryCell(buf, v)
		}
	}
	session.bw.Write(buf.Bytes())
}

// writeQueryCell 按"u32 长度 + 文本"编码一个单元格值。
// 数值走 strconv 写入调用方的栈缓冲（零堆分配），字符串直写；
// 取代旧的 fmt.Sprintf("%v")——反射 + 每格一次字符串分配是
// CmdQuerySQL 结果集路径最大的分配源（对齐 mysql.go appendCell 的做法）。
func writeQueryCell(buf *bytes.Buffer, v interface{}) {
	var tmp [64]byte
	switch val := v.(type) {
	case nil:
		buf.WriteString("")
	case string:
		wU32(buf, uint32(len(val)))
		buf.WriteString(val)
		return
	case bool:
		if val {
			wU32(buf, 1)
			buf.WriteByte('1')
		} else {
			wU32(buf, 1)
			buf.WriteByte('0')
		}
		return
	case int64:
		b := strconv.AppendInt(tmp[:0], val, 10)
		wU32(buf, uint32(len(b)))
		buf.Write(b)
		return
	case int:
		b := strconv.AppendInt(tmp[:0], int64(val), 10)
		wU32(buf, uint32(len(b)))
		buf.Write(b)
		return
	case float64:
		b := strconv.AppendFloat(tmp[:0], val, 'g', -1, 64)
		wU32(buf, uint32(len(b)))
		buf.Write(b)
		return
	case []byte:
		wU32(buf, uint32(len(val)))
		buf.Write(val)
		return
	default:
		sv := fmt.Sprintf("%v", val)
		wU32(buf, uint32(len(sv)))
		buf.WriteString(sv)
	}
}

// ---- UPDATE ----
func (s *Server) handleUpdate(session *Session) {
	s.db.stats.IncCmd("UPDATE")
	tableNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read table name length failed")
		return
	}
	tableName := make([]byte, tableNameLen)
	if _, err := io.ReadFull(session.br, tableName); err != nil {
		s.writeError(session, "read table name failed")
		return
	}
	pk, err := rI64(session.br)
	if err != nil {
		s.writeError(session, "read pk failed")
		return
	}
	ttl, err := rI64(session.br)
	if err != nil {
		s.writeError(session, "read ttl failed")
		return
	}
	fieldCount, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read field count failed")
		return
	}
	values := rowMapPool.Get().(map[string]interface{})
	defer func() {
		for k := range values {
			delete(values, k)
		}
		rowMapPool.Put(values)
	}()
	for i := 0; i < int(fieldCount); i++ {
		name, val, err := readFieldValue(session.br)
		if err != nil {
			s.writeError(session, err.Error())
			return
		}
		values[name] = val
	}
	db := session.db
	table, ok := s.db.lookupTable(string(tableName))
	if !ok {
		s.writeError(session, "table not found")
		return
	}
	if !db.CheckPrivilege(session.user, string(tableName), PermUpdate) {
		s.writeError(session, "permission denied")
		return
	}
	var expireAt int64
	if ttl > 0 {
		expireAt = time.Now().UnixNano() + ttl*1e9
	}
	var txnID uint64
	if session.inTxn {
		txnID = session.txnID
		oldData, _ := table.pkTree.Get(pk)
		if oldData != nil {
			version := decodeVersion(oldData)
			db.recordReadVersion(txnID, string(tableName), pk, version)
		}
		db.txnMu.Lock()
		txn, _ := db.txns[txnID]
		if txn != nil {
			if txn.Changes[string(tableName)] == nil {
				txn.Changes[string(tableName)] = make(map[int64][]byte)
			}
			txn.Changes[string(tableName)][pk] = oldData
			txn.Tables[string(tableName)] = true
		}
		db.txnMu.Unlock()
	}
	if err := table.Update(pk, values, expireAt, txnID, false); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// ---- DELETE ----
func (s *Server) handleDelete(session *Session) {
	s.db.stats.IncCmd("DELETE")
	tableNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read table name length failed")
		return
	}
	tableName := make([]byte, tableNameLen)
	if _, err := io.ReadFull(session.br, tableName); err != nil {
		s.writeError(session, "read table name failed")
		return
	}
	pk, err := rI64(session.br)
	if err != nil {
		s.writeError(session, "read pk failed")
		return
	}
	db := session.db
	table, ok := s.db.lookupTable(string(tableName))
	if !ok {
		s.writeError(session, "table not found")
		return
	}
	if !db.CheckPrivilege(session.user, string(tableName), PermDelete) {
		s.writeError(session, "permission denied")
		return
	}
	var txnID uint64
	if session.inTxn {
		txnID = session.txnID
		oldData, _ := table.pkTree.Get(pk)
		if oldData != nil {
			version := decodeVersion(oldData)
			db.recordReadVersion(txnID, string(tableName), pk, version)
		}
		db.txnMu.Lock()
		txn, _ := db.txns[txnID]
		if txn != nil {
			if txn.Changes[string(tableName)] == nil {
				txn.Changes[string(tableName)] = make(map[int64][]byte)
			}
			txn.Changes[string(tableName)][pk] = oldData
			txn.Tables[string(tableName)] = true
		}
		db.txnMu.Unlock()
	}
	if err := table.Delete(pk, txnID, false); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// ---- BEGIN ----
func (s *Server) handleBegin(session *Session) {
	s.db.stats.IncCmd("BEGIN")
	// 重复 BEGIN 会让旧事务失去会话引用而永久泄漏在 db.txns
	if session.inTxn {
		s.writeError(session, fmt.Sprintf("transaction %d already active", session.txnID))
		return
	}
	txnID := session.db.BeginTxn()
	session.inTxn = true
	session.txnID = txnID
	session.bw.WriteByte(RespTxnID)
	binary.Write(session.bw, binary.BigEndian, txnID)
}

// ---- COMMIT ----
func (s *Server) handleCommit(session *Session) {
	s.db.stats.IncCmd("COMMIT")
	if !session.inTxn {
		s.writeError(session, "no active transaction")
		return
	}
	if err := session.db.CommitTxn(session.txnID); err != nil {
		s.writeError(session, err.Error())
		return
	}
	session.inTxn = false
	session.txnID = 0
	s.writeOK(session)
}

// ---- ROLLBACK ----
func (s *Server) handleRollback(session *Session) {
	s.db.stats.IncCmd("ROLLBACK")
	if !session.inTxn {
		s.writeError(session, "no active transaction")
		return
	}
	if err := session.db.RollbackTxn(session.txnID); err != nil {
		s.writeError(session, err.Error())
		return
	}
	session.inTxn = false
	session.txnID = 0
	s.writeOK(session)
}

// ---- CREATE INDEX ----
func (s *Server) handleCreateIndex(session *Session) {
	s.db.stats.IncCmd("CREATE_INDEX")
	tableNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read table name length failed")
		return
	}
	tableName := make([]byte, tableNameLen)
	if _, err := io.ReadFull(session.br, tableName); err != nil {
		s.writeError(session, "read table name failed")
		return
	}
	idxNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read index name length failed")
		return
	}
	idxName := make([]byte, idxNameLen)
	if _, err := io.ReadFull(session.br, idxName); err != nil {
		s.writeError(session, "read index name failed")
		return
	}
	fieldNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read field name length failed")
		return
	}
	fieldName := make([]byte, fieldNameLen)
	if _, err := io.ReadFull(session.br, fieldName); err != nil {
		s.writeError(session, "read field name failed")
		return
	}
	db := session.db
	db.mu.Lock()
	defer db.mu.Unlock()
	v, ok := db.tables.Load(string(tableName))
	if !ok {
		s.writeError(session, "table not found")
		return
	}
	table := v.(*Table)
	if !db.CheckPrivilege(session.user, string(tableName), PermDDL) && session.user != db.config.User {
		s.writeError(session, "permission denied")
		return
	}
	if _, exists := table.Meta().Indexes[string(idxName)]; exists {
		s.writeError(session, "index exists")
		return
	}
	found := false
	for _, f := range table.Meta().Fields {
		if f.Name == string(fieldName) {
			found = true
			break
		}
	}
	if !found {
		s.writeError(session, "field not found")
		return
	}
	// 写时复制：基于当前索引快照构建新快照（新索引离线回填），整体发布，
	// 避免在共享 map 上边遍历边写（并发读者会触发运行时致命错误）。
	idxNameStr := string(idxName)
	fieldNameStr := string(fieldName)
	newMeta := cloneMeta(table.Meta(), table.Meta().Name)
	newMeta.Indexes[idxNameStr] = fieldNameStr
	newIdx := make(idxTreeMap, len(newMeta.Indexes))
	for name := range newMeta.Indexes {
		if name == idxNameStr {
			newIdx[name] = newStringKeyTree[[]int64]()
			continue
		}
		if old, ok := table.indexes()[name]; ok {
			newIdx[name] = old
		} else {
			newIdx[name] = newStringKeyTree[[]int64]()
		}
	}
	newTree := newIdx[idxNameStr]
	table.pkTree.scanAll(func(key int64, value []byte) {
		row, _, _ := decodeRow(newMeta, value)
		table.indexAddPKTo(newTree, fieldNameStr, row[fieldNameStr], key)
	})
	table.setMeta(newMeta)
	table.storeIndexes(newIdx)
	db.catalog[string(tableName)] = newMeta
	// 修复: 使用 writeCatalogID 保留原 tableID，而不是 writeCatalog（会重新分配 ID）
	if err := db.writeCatalogID(newMeta, table.tableID); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// ---- BATCH ----
func (s *Server) handleBatch(session *Session) {
	s.db.stats.IncCmd("BATCH")
	s.writeError(session, "batch command not implemented")
}

// ---- BACKUP ----
func (s *Server) handleBackup(session *Session) {
	s.db.stats.IncCmd("BACKUP")
	if session.user != session.db.config.User {
		s.writeError(session, "only root can backup")
		return
	}
	if err := session.db.Backup(); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// ---- 管理命令：状态 / 整理 / 持久化模式 ----

// handleStatus 返回 JSON 状态快照（RespValue + len + json）。
func (s *Server) handleStatus(session *Session) {
	s.db.stats.IncCmd("STATUS")
	snap := s.db.stats.Snapshot()
	snap["durability"] = s.db.config.Durability
	data, err := json.Marshal(snap)
	if err != nil {
		s.writeError(session, err.Error())
		return
	}
	session.bw.Write([]byte{RespValue})
	wU32(session.bw, uint32(len(data)))
	session.bw.Write(data)
}

// handleCompact 触发 WAL 整理：将 WAL 重写为仅含当前状态的精简 v2 快照。
func (s *Server) handleCompact(session *Session) {
	s.db.stats.IncCmd("COMPACT")
	if session.user != session.db.config.User {
		s.writeError(session, "only root can compact")
		return
	}
	if err := s.db.Compact(); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// handleSetDurability 动态切换持久化模式（batch/fsync）。
func (s *Server) handleSetDurability(session *Session) {
	s.db.stats.IncCmd("SET_DURABILITY")
	if session.user != session.db.config.User {
		s.writeError(session, "only root can change durability")
		return
	}
	l, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read durability length failed")
		return
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(session.br, b); err != nil {
		s.writeError(session, "read durability failed")
		return
	}
	mode := Durability(string(b))
	if !mode.Valid() {
		s.writeError(session, "durability must be batch or fsync")
		return
	}
	s.db.cfgMu.Lock()
	s.db.setDurability(mode)
	s.db.cfgMu.Unlock()
	s.writeOK(session)
}

// ---- 存储过程 / 视图 / 触发器 占位 ----
func (s *Server) handleCreateProc(session *Session) {
	s.db.stats.IncCmd("CREATE_PROC")
	s.writeError(session, "not implemented")
}
func (s *Server) handleCallProc(session *Session) {
	s.db.stats.IncCmd("CALL_PROC")
	s.writeError(session, "not implemented")
}
func (s *Server) handleCreateView(session *Session) {
	s.db.stats.IncCmd("CREATE_VIEW")
	s.writeError(session, "not implemented")
}
func (s *Server) handleCreateTrigger(session *Session) {
	s.db.stats.IncCmd("CREATE_TRIGGER")
	s.writeError(session, "not implemented")
}

// ---- GRANT ----
func (s *Server) handleGrant(session *Session) {
	s.db.stats.IncCmd("GRANT")
	tableNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read table name length failed")
		return
	}
	tableName := make([]byte, tableNameLen)
	if _, err := io.ReadFull(session.br, tableName); err != nil {
		s.writeError(session, "read table name failed")
		return
	}
	userLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read user length failed")
		return
	}
	user := make([]byte, userLen)
	if _, err := io.ReadFull(session.br, user); err != nil {
		s.writeError(session, "read user name failed")
		return
	}
	perm, err := rByte(session.br)
	if err != nil {
		s.writeError(session, "read perm failed")
		return
	}
	if session.user != session.db.config.User {
		s.writeError(session, "only root can grant")
		return
	}
	if err := session.db.privileges.Grant(string(user), string(tableName), perm); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// ---- REVOKE ----
func (s *Server) handleRevoke(session *Session) {
	s.db.stats.IncCmd("REVOKE")
	tableNameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read table name length failed")
		return
	}
	tableName := make([]byte, tableNameLen)
	if _, err := io.ReadFull(session.br, tableName); err != nil {
		s.writeError(session, "read table name failed")
		return
	}
	userLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "read user length failed")
		return
	}
	user := make([]byte, userLen)
	if _, err := io.ReadFull(session.br, user); err != nil {
		s.writeError(session, "read user name failed")
		return
	}
	perm, err := rByte(session.br)
	if err != nil {
		s.writeError(session, "read perm failed")
		return
	}
	if session.user != session.db.config.User {
		s.writeError(session, "only root can revoke")
		return
	}
	if err := session.db.privileges.Revoke(string(user), string(tableName), perm); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// ==================== 输入验证函数 ====================

// validateTableName 验证表名合法性
func validateTableName(name []byte) error {
	if len(name) == 0 {
		return fmt.Errorf("empty table name")
	}
	if len(name) > 256 {
		return fmt.Errorf("table name too long (max 256)")
	}
	for i, b := range name {
		if b == '.' || b == '/' || b == '\\' || b == 0 {
			return fmt.Errorf("invalid character in table name at position %d", i)
		}
	}
	return nil
}

