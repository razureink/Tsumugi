package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	CMD_AUTH           = 10
	CMD_PING           = 11
	CMD_CREATE_TABLE   = 20
	CMD_DROP_TABLE     = 21
	CMD_DESCRIBE       = 22
	CMD_ALTER_TABLE    = 23
	CMD_INSERT         = 30
	CMD_SELECT         = 31
	CMD_UPDATE         = 32
	CMD_DELETE         = 33
	CMD_QUERY_SQL      = 35
	CMD_BEGIN          = 40
	CMD_COMMIT         = 41
	CMD_ROLLBACK       = 42
	CMD_CREATE_INDEX   = 50
	CMD_BATCH          = 55
	CMD_BACKUP         = 56
	CMD_STATUS         = 57
	CMD_COMPACT        = 58
	CMD_SET_DURABILITY = 59
	CMD_CREATE_PROC    = 60
	CMD_CALL_PROC      = 61
	CMD_CREATE_VIEW    = 70
	CMD_CREATE_TRIGGER = 80
	CMD_GRANT          = 90
	CMD_REVOKE         = 91
)

const (
	RESP_OK        = 1
	RESP_ERR       = 2
	RESP_VALUE     = 3
	RESP_NOT_FOUND = 4
	RESP_ROWS      = 5
	RESP_TXN_ID    = 6
)

// ==================== Server ====================

type Server struct {
	ln     net.Listener
	db     *DB
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewServer(addr string, db *DB) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
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
	lastActive time.Time
}

// read 从会话缓冲读取指定字节数（优先用 bufio，减少 syscall）。
// readFull 等价 io.ReadFull(session.conn)。
func (s *Session) readFull(b []byte) (int, error) { return io.ReadFull(s.br, b) }
func (s *Session) writeByte(b byte) error          { return s.bw.WriteByte(b) }

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
	session := &Session{
		conn:       conn,
		br:         bufio.NewReaderSize(conn, 32*1024),
		bw:         bufio.NewWriterSize(conn, 32*1024),
		db:         s.db,
		auth:       false,
		lastActive: time.Now(),
	}
	defer session.bw.Flush()

	for {
		if session.auth && time.Since(session.lastActive) > s.db.config.IdleTimeout {
			logf(LOG_VERB, "idle timeout, closing connection")
			return
		}

		cmd, err := session.br.ReadByte()
		if err != nil {
			if err != io.EOF {
				logf(LOG_ERR, "read cmd: %v", err)
			}
			return
		}
		session.lastActive = time.Now()

		if !session.auth && cmd != CMD_AUTH {
			s.writeError(session, "authentication required")
			return
		}

		switch cmd {
		case CMD_AUTH:
			s.handleAuth(session)
		case CMD_PING:
			s.handlePing(session)
		case CMD_CREATE_TABLE:
			s.handleCreateTable(session)
		case CMD_DROP_TABLE:
			s.handleDropTable(session)
		case CMD_DESCRIBE:
			s.handleDescribe(session)
		case CMD_ALTER_TABLE:
			s.handleAlterTable(session)
		case CMD_INSERT:
			s.handleInsert(session)
		case CMD_SELECT:
			s.handleSelect(session)
		case CMD_UPDATE:
			s.handleUpdate(session)
		case CMD_DELETE:
			s.handleDelete(session)
		case CMD_QUERY_SQL:
			s.handleQuerySQL(session)
		case CMD_BEGIN:
			s.handleBegin(session)
		case CMD_COMMIT:
			s.handleCommit(session)
		case CMD_ROLLBACK:
			s.handleRollback(session)
		case CMD_CREATE_INDEX:
			s.handleCreateIndex(session)
		case CMD_BATCH:
			s.handleBatch(session)
		case CMD_BACKUP:
			s.handleBackup(session)
		case CMD_STATUS:
			s.handleStatus(session)
		case CMD_COMPACT:
			s.handleCompact(session)
		case CMD_SET_DURABILITY:
			s.handleSetDurability(session)
		case CMD_CREATE_PROC:
			s.handleCreateProc(session)
		case CMD_CALL_PROC:
			s.handleCallProc(session)
		case CMD_CREATE_VIEW:
			s.handleCreateView(session)
		case CMD_CREATE_TRIGGER:
			s.handleCreateTrigger(session)
		case CMD_GRANT:
			s.handleGrant(session)
		case CMD_REVOKE:
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
	session.bw.WriteByte(RESP_ERR)
	wU32(session.bw, uint32(len(msg)))
	session.bw.WriteString(msg)
	s.db.stats.IncErr()
}

func (s *Server) writeOK(session *Session) {
	session.bw.WriteByte(RESP_OK)
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
	matched := u != nil && u.Password == hashPasswd(passwd)
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

// ---- DROP TABLE ----
func (s *Server) handleDropTable(session *Session) {
	s.db.stats.IncCmd("DROP_TABLE")
	nameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "invalid name length")
		return
	}
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(session.br, name); err != nil {
		s.writeError(session, "read name failed")
		return
	}
	tableName := string(name)

	if !session.db.CheckPrivilege(session.user, tableName, PermDDL) && session.user != session.db.config.User {
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
		s.writeOK(session)
	} else {
		s.writeError(session, "table not found")
	}
}

// ---- DESCRIBE ----
func (s *Server) handleDescribe(session *Session) {
	s.db.stats.IncCmd("DESCRIBE")
	nameLen, err := rU16(session.br)
	if err != nil {
		s.writeError(session, "invalid name length")
		return
	}
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(session.br, name); err != nil {
		s.writeError(session, "read name failed")
		return
	}
	tableName := string(name)

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
	if !session.db.CheckPrivilege(session.user, string(tableName), PermDDL) && session.user != session.db.config.User {
		s.writeError(session, "permission denied")
		return
	}
	if err := session.db.AlterTableAddColumn(string(tableName), string(fieldName), FieldType(fType), int(fLen)); err != nil {
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
		session.bw.WriteByte(RESP_ROWS)
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
		session.bw.WriteByte(RESP_ROWS)
		wU32(session.bw, uint32(len(rawRows)))
		for _, data := range rawRows {
			wU32(session.bw, uint32(len(data)))
			session.bw.Write(data)
		}
		return
	}
	rows, err := table.Select(conditions, minKey, maxKey)
	if err != nil {
		s.writeError(session, err.Error())
		return
	}
	session.bw.WriteByte(RESP_ROWS)
	wU32(session.bw, uint32(len(rows)))
	for _, row := range rows {
		data := encodeRow(table.meta, row, 0, 0)
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
		// 非结果集语句：RESP_OK + affected + rawMsg
		session.bw.WriteByte(RESP_OK)
		binary.Write(session.bw, binary.BigEndian, uint64(affected))
		wU16(session.bw, uint16(len(rawMsg)))
		session.bw.WriteString(rawMsg)
		return
	}
	// 结果集：RESP_ROWS + 列数 + 列名列表 + 行数 + 行数据。
	// 先写入本地缓冲，一次 Write 发送，减少系统调用与 TCP 小包。
	buf := bytesBufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bytesBufPool.Put(buf)
	}()
	buf.Reset()
	buf.WriteByte(RESP_ROWS)
	wU32(buf, uint32(len(columns)))
	for _, c := range columns {
		wU16(buf, uint16(len(c)))
		buf.WriteString(c)
	}
	wU32(buf, uint32(len(rows)))
	for _, r := range rows {
		wU32(buf, uint32(len(r)))
		for _, v := range r {
			sv := fmt.Sprintf("%v", v)
			wU32(buf, uint32(len(sv)))
			buf.WriteString(sv)
		}
	}
	session.bw.Write(buf.Bytes())
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
	txnID := session.db.BeginTxn()
	session.inTxn = true
	session.txnID = txnID
	session.bw.WriteByte(RESP_TXN_ID)
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
	if _, exists := table.meta.Indexes[string(idxName)]; exists {
		s.writeError(session, "index exists")
		return
	}
	found := false
	for _, f := range table.meta.Fields {
		if f.Name == string(fieldName) {
			found = true
			break
		}
	}
	if !found {
		s.writeError(session, "field not found")
		return
	}
	table.meta.Indexes[string(idxName)] = string(fieldName)
	table.idxTrees[string(idxName)] = newBytesKeyTree[[]int64]()
	table.pkTree.scanAll(func(key int64, value []byte) {
		row, _, _ := decodeRow(table.meta, value)
		val := row[string(fieldName)]
		idxKey := table.encodeIndexValue(string(fieldName), val)
		var list []int64
		if v, ok := table.idxTrees[string(idxName)].Get(idxKey); ok {
			list = v
		}
		list = append(list, key)
		table.idxTrees[string(idxName)].Set(idxKey, list)
	})
	db.catalog[string(tableName)] = table.meta
	if err := db.writeCatalog(table.meta); err != nil {
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

// handleStatus 返回 JSON 状态快照（RESP_VALUE + len + json）。
func (s *Server) handleStatus(session *Session) {
	s.db.stats.IncCmd("STATUS")
	snap := s.db.stats.Snapshot()
	snap["durability"] = s.db.config.Durability
	data, err := json.Marshal(snap)
	if err != nil {
		s.writeError(session, err.Error())
		return
	}
	session.bw.Write([]byte{RESP_VALUE})
	wU32(session.bw, uint32(len(data)))
	session.bw.Write(data)
}

// handleCompact 触发 WAL 整理：将 WAL 重写为仅含当前状态的精简 v2 快照。
func (s *Server) handleCompact(session *Session) {
	s.db.stats.IncCmd("COMPACT")
	if err := s.db.Compact(); err != nil {
		s.writeError(session, err.Error())
		return
	}
	s.writeOK(session)
}

// handleSetDurability 动态切换持久化模式（batch/fsync）。
func (s *Server) handleSetDurability(session *Session) {
	s.db.stats.IncCmd("SET_DURABILITY")
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
	// 先冲刷缓冲，确保此前的 batch 记录落盘后再切到 fsync 语义
	s.db.flushWAL()
	s.db.config.Durability = mode
	walFsync.Store(mode.Fsync())
	s.db.stats.durability = mode
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
