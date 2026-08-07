package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// ==================== 数据类型 ====================

type FieldType byte

const (
	TypeInt FieldType = iota
	TypeVarchar
	TypeBool
)

// 权限位
const (
	PermSelect = 1 << 0
	PermInsert = 1 << 1
	PermUpdate = 1 << 2
	PermDelete = 1 << 3
	PermDDL    = 1 << 4
	PermAll    = PermSelect | PermInsert | PermUpdate | PermDelete | PermDDL
)

// WAL 操作码
const (
	WALBegin    = 1
	WALInsert   = 2
	WALUpdate   = 3
	WALDelete   = 4
	WALCommit   = 5
	WALRollback = 6
)

// 单写者 WAL 队列：所有 WAL 记录经此 FIFO 通道交给唯一 writer goroutine 串行落盘，
// 消除热路径上的全局互斥锁与锁唤醒风暴；fsync 模式下由 writer 对并发写者做组提交批量落盘。
// buffer 足够大以吸收并发突发，避免发送者频繁阻塞。
var walCh = make(chan walEntry, 16384)

// walBufPool 复用 WAL 记录缓冲：writeLog 从池取、writer 写完 rec 后归还，
// 消除每条写日志的堆分配（组提交高频写入场景收益明显）。
var walBufPool = sync.Pool{New: func() interface{} { return make([]byte, 0, 64) }}

// rowValsPool 复用 encodeRow 的瞬态取值切片，避免每行编码一次堆分配。
var rowValsPool = sync.Pool{New: func() interface{} { return make([]interface{}, 0, 8) }}

// compactRecPool 复用 Compact 逐行 WAL 记录缓冲：w.Write 会先拷贝到 bufio，写后可安全复用。
var compactRecPool = sync.Pool{New: func() interface{} { return make([]byte, 0, 64) }}

// walEntry 是一次 WAL 提交请求：rec 为待写记录；sync=true 表示需要同步落盘（fsync 模式）；
// done 非空时发送者等待处理完成（flush/stop/sync）；stop=true 令 writer 冲刷后退出（Compact/Close 换文件用）。
type walEntry struct {
	rec   []byte
	sync  bool
	done  chan error
	stop  bool
	flush bool
}

// 串行化 Compact：多个压缩请求不能并发换文件
var compactMu sync.Mutex

// 持久化模式：fsync 模式下每条记录同步落盘（writer 组提交批量 fsync），batch 模式下由 flushLoop 定时批量刷盘
var walFsync atomic.Bool

// 关闭标记：Close 后 appendWAL/flushWAL 直接返回，避免阻塞在已退出的 writer 上
var walClosed atomic.Bool

// 全局统计钩子：appendWAL 是包级函数，借该指针上报磁盘写入速率
var statsHook *Stats

// walFileSizeMB 返回 WAL 文件当前大小（MB）
func walFileSizeMB(db *DB) float64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	info, err := db.walFile.Stat()
	if err != nil {
		return 0
	}
	return float64(info.Size()) / 1024 / 1024
}

// WAL 加载计时
var walLoadStart time.Time

// WAL 加载行数
var walLoadRows int64

type Field struct {
	Name string
	Type FieldType
	Len  int
}

// ---- 手动字节编码/解码（替代 binary.Write/Read 反射，热路径提速）----

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}
func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
func appendU64(b []byte, v uint64) []byte {
	return append(b,
		byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
func appendI64(b []byte, v int64) []byte { return appendU64(b, uint64(v)) }

func readU16(b []byte, pos *int) (uint16, bool) {
	if *pos+2 > len(b) {
		return 0, false
	}
	v := uint16(b[*pos])<<8 | uint16(b[*pos+1])
	*pos += 2
	return v, true
}
func readU32(b []byte, pos *int) (uint32, bool) {
	if *pos+4 > len(b) {
		return 0, false
	}
	v := uint32(b[*pos])<<24 | uint32(b[*pos+1])<<16 | uint32(b[*pos+2])<<8 | uint32(b[*pos+3])
	*pos += 4
	return v, true
}
func readU64(b []byte, pos *int) (uint64, bool) {
	if *pos+8 > len(b) {
		return 0, false
	}
	v := uint64(b[*pos])<<56 | uint64(b[*pos+1])<<48 | uint64(b[*pos+2])<<40 | uint64(b[*pos+3])<<32 |
		uint64(b[*pos+4])<<24 | uint64(b[*pos+5])<<16 | uint64(b[*pos+6])<<8 | uint64(b[*pos+7])
	*pos += 8
	return v, true
}
func readI64(b []byte, pos *int) (int64, bool) {
	if *pos+8 > len(b) {
		return 0, false
	}
	v := int64(uint64(b[*pos])<<56 | uint64(b[*pos+1])<<48 | uint64(b[*pos+2])<<40 | uint64(b[*pos+3])<<32 |
		uint64(b[*pos+4])<<24 | uint64(b[*pos+5])<<16 | uint64(b[*pos+6])<<8 | uint64(b[*pos+7]))
	*pos += 8
	return v, true
}

// ---- LEB128 变长整数（WAL 紧凑编码：小整数只占 1~2 字节）----

func appendVarUint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendVarLenStr(b []byte, s string) []byte {
	b = appendVarUint(b, uint64(len(s)))
	return append(b, s...)
}

func readVarUint(b []byte, pos *int) (uint64, bool) {
	var v uint64
	var shift uint
	for {
		if *pos >= len(b) {
			return 0, false
		}
		c := b[*pos]
		*pos++
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, true
		}
		shift += 7
		if shift > 63 {
			return 0, false
		}
	}
}

// ---- WAL 表注册表 ----
// 每条数据记录只写 1~5 字节的表 ID，不再每次重复写入完整表名，
// 显著降低小行写入的固定开销（表名通常十字节以上）。
var (
	walRegMu     sync.Mutex
	walRegByName map[string]uint32
	walRegByID   map[uint32]string
	walRegNext   uint32
)

func walRegReset() {
	walRegMu.Lock()
	defer walRegMu.Unlock()
	walRegByName = make(map[string]uint32)
	walRegByID = make(map[uint32]string)
	walRegNext = 1
}

// walRegRegister 返回 name 对应的注册表 ID（不存在则分配一个新的）。
func walRegRegister(name string) uint32 {
	walRegMu.Lock()
	defer walRegMu.Unlock()
	if id, ok := walRegByName[name]; ok {
		return id
	}
	id := walRegNext
	walRegNext++
	walRegByName[name] = id
	walRegByID[id] = name
	return id
}

func walRegName(id uint32) (string, bool) {
	walRegMu.Lock()
	defer walRegMu.Unlock()
	n, ok := walRegByID[id]
	return n, ok
}

// walRegRename 将 id 从 oldName 改绑到 newName（重命名用）。
func walRegRename(oldName, newName string, id uint32) {
	walRegMu.Lock()
	defer walRegMu.Unlock()
	// 若旧名映射到其它 ID，释放
	if theOld, ok := walRegByName[oldName]; ok && theOld != id {
		delete(walRegByID, theOld)
	}
	// 若 newName 已绑定到其它 ID，释放
	if other, ok := walRegByName[newName]; ok && other != id {
		delete(walRegByID, other)
	}
	walRegByName[oldName] = 0
	delete(walRegByName, oldName)
	walRegByName[newName] = id
	walRegByID[id] = newName
}

func walRegUnregister(name string) {
	walRegMu.Lock()
	defer walRegMu.Unlock()
	if id, ok := walRegByName[name]; ok {
		delete(walRegByName, name)
		delete(walRegByID, id)
	}
}

// walRegRebind 恢复期用：将 name 强制绑定到文件中记录的 ID，并同步单调计数器
func walRegRebind(id uint32, name string) {
	walRegMu.Lock()
	defer walRegMu.Unlock()
	if old, ok := walRegByName[name]; ok && old != id {
		delete(walRegByID, old)
	}
	walRegByName[name] = id
	walRegByID[id] = name
	if id >= walRegNext {
		walRegNext = id + 1
	}
}

// WAL 是否写/校 CRC：与配置 EnableChecksum 一致（修复 v1 写入总是带 CRC、
// 恢复却按开关读取的不一致问题）。
var walCRC atomic.Bool

type TableMeta struct {
	Name    string
	PK      string
	Fields  []Field
	Indexes map[string]string
}

// 行编码
func encodeRow(meta *TableMeta, values map[string]interface{}, version int64, expireAt int64) []byte {
	// 先取值再编码：每个字段只做一次 map 查找，避免两个遍历各查一次。
	vals := rowValsPool.Get().([]interface{})[:0]
	if cap(vals) < len(meta.Fields) {
		vals = make([]interface{}, len(meta.Fields))
	} else {
		vals = vals[:len(meta.Fields)]
	}
	size := 16
	for i, f := range meta.Fields {
		v := values[f.Name]
		vals[i] = v
		switch f.Type {
		case TypeInt:
			size += 8
		case TypeVarchar:
			s, _ := v.(string)
			size += 2 + len(s)
		case TypeBool:
			size += 1
		}
	}
	buf := make([]byte, 0, size)
	buf = appendI64(buf, version)
	buf = appendI64(buf, expireAt)
	for i, f := range meta.Fields {
		v := vals[i]
		switch f.Type {
		case TypeInt:
			var iv int64
			if n, ok := v.(int64); ok {
				iv = n
			}
			buf = appendI64(buf, iv)
		case TypeVarchar:
			s, _ := v.(string)
			buf = appendU16(buf, uint16(len(s)))
			buf = append(buf, s...)
		case TypeBool:
			var b byte
			if vb, ok := v.(bool); ok && vb {
				b = 1
			}
			buf = append(buf, b)
		}
	}
	rowValsPool.Put(vals[:0])
	return buf
}

// encodeRowOrdered 按 meta.Fields 顺序直接编码，不做 map 查找（SQL 位置参数快速路径）。
func encodeRowOrdered(meta *TableMeta, values []interface{}, version int64, expireAt int64) []byte {
	size := 16
	for i, f := range meta.Fields {
		var s string
		switch f.Type {
		case TypeInt:
			size += 8
		case TypeVarchar:
			if i < len(values) {
				s, _ = values[i].(string)
			}
			size += 2 + len(s)
		case TypeBool:
			size += 1
		}
	}
	buf := make([]byte, 0, size)
	buf = appendI64(buf, version)
	buf = appendI64(buf, expireAt)
	for i, f := range meta.Fields {
		var v interface{}
		if i < len(values) {
			v = values[i]
		}
		switch f.Type {
		case TypeInt:
			var iv int64
			if n, ok := v.(int64); ok {
				iv = n
			}
			buf = appendI64(buf, iv)
		case TypeVarchar:
			s, _ := v.(string)
			buf = appendU16(buf, uint16(len(s)))
			buf = append(buf, s...)
		case TypeBool:
			var b byte
			if vb, ok := v.(bool); ok && vb {
				b = 1
			}
			buf = append(buf, b)
		}
	}
	return buf
}

func decodeRow(meta *TableMeta, data []byte) (map[string]interface{}, int64, int64) {
	pos := 0
	version, _ := readI64(data, &pos)
	expireAt, _ := readI64(data, &pos)
	row := make(map[string]interface{}, len(meta.Fields))
	for _, f := range meta.Fields {
		switch f.Type {
		case TypeInt:
			v, ok := readI64(data, &pos)
			if ok {
				row[f.Name] = v
			} else {
				row[f.Name] = int64(0)
			}
		case TypeVarchar:
			l, ok := readU16(data, &pos)
			if ok && pos+int(l) <= len(data) {
				row[f.Name] = string(data[pos : pos+int(l)])
				pos += int(l)
			} else {
				row[f.Name] = ""
			}
		case TypeBool:
			if pos < len(data) {
				row[f.Name] = data[pos] == 1
				pos++
			} else {
				row[f.Name] = false
			}
		}
	}
	return row, version, expireAt
}

// unsafeString 零拷贝 []byte→string，复用底层缓冲。仅用于树内行数据（不可变、COW）：
// encodeRow 每次生成新缓冲，Update/Delete 整值替换而非原地修改，故别名安全且免除拷贝。
func unsafeString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// decodeRowFlat 将行数据直接解码为按 meta.Fields 顺序排列的 []interface{}，不建 map。
// 用于无条件全表扫描路径，避免每行 map 分配与后续二次投影（SELECT * 场景 allocation 大头）。
func decodeRowFlat(meta *TableMeta, data []byte, into []interface{}) []interface{} {
	pos := 16
	if into == nil || cap(into) < len(meta.Fields) {
		into = make([]interface{}, len(meta.Fields))
	}
	into = into[:len(meta.Fields)]
	for i, f := range meta.Fields {
		switch f.Type {
		case TypeInt:
			v, _ := readI64(data, &pos)
			into[i] = v
		case TypeVarchar:
			l, _ := readU16(data, &pos)
			if l > 0 && pos+int(l) <= len(data) {
				into[i] = unsafeString(data[pos : pos+int(l)])
				pos += int(l)
			} else {
				into[i] = ""
			}
		case TypeBool:
			if pos < len(data) {
				into[i] = data[pos] == 1
				pos++
			} else {
				into[i] = false
			}
		}
	}
	return into
}

// selectRowsFlat 无 WHERE 条件的全表扫描：直接解码为 []interface{} 行，不建 map。
// 过期行收集后锁外清理（与 SelectR 一致）。limit<0 表示不限制。
func (t *Table) selectRowsFlat(minKey, maxKey *int64, limit int) [][]interface{} {
	capHint := 16
	if limit > 0 {
		capHint = limit
	}
	rows := make([][]interface{}, 0, capHint)
	var expired []int64
	now := time.Now().UnixNano()
	scan := func(key int64, value []byte) bool {
		if e := decodeExpireAt(value); e > 0 && now > e {
			expired = append(expired, key)
			return true
		}
		rows = append(rows, decodeRowFlat(t.meta, value, nil))
		if limit >= 0 && len(rows) >= limit {
			return false
		}
		return true
	}
	t.pkTree.scanRangeStop(minKey, maxKey, scan)
	for _, k := range expired {
		t.pkTree.Delete(k)
	}
	return rows
}

// decodeExpireAt 仅读取过期时间，不分配行 map。用于 TTL 清理等只需 expireAt 的路径，
// 避免热路径上每行创建 map 造成 GC 压力（低内存设备受益明显）。
func decodeExpireAt(data []byte) int64 {
	if len(data) < 16 {
		return 0
	}
	return int64(uint64(data[8])<<56 | uint64(data[9])<<48 | uint64(data[10])<<40 | uint64(data[11])<<32 |
		uint64(data[12])<<24 | uint64(data[13])<<16 | uint64(data[14])<<8 | uint64(data[15]))
}

// decodeVersion 仅读取行版本号（前 8 字节），不分配行 map。
// 用于事务冲突检测等只需版本的路径。
func decodeVersion(data []byte) int64 {
	if len(data) < 8 {
		return 0
	}
	return int64(uint64(data[0])<<56 | uint64(data[1])<<48 | uint64(data[2])<<40 | uint64(data[3])<<32 |
		uint64(data[4])<<24 | uint64(data[5])<<16 | uint64(data[6])<<8 | uint64(data[7]))
}

// ==================== 通用红黑树 ====================
// 泛型实现见 rb_tree.go：IntRBTree/RBTree 已被 RBTree[K,V] 统一替换。

// ==================== 权限存储 ====================

type PrivilegeStore struct {
	filePath string
	mu       sync.RWMutex
	data     map[string]map[string]byte
}

func NewPrivilegeStore(path string) (*PrivilegeStore, error) {
	ps := &PrivilegeStore{
		filePath: path,
		data:     make(map[string]map[string]byte),
	}
	if err := ps.load(); err != nil {
		return nil, err
	}
	return ps, nil
}
func (ps *PrivilegeStore) load() error {
	f, err := os.Open(ps.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if err := dec.Decode(&ps.data); err != nil {
		return err
	}
	return nil
}
func (ps *PrivilegeStore) save() error {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	tmp := ps.filePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(ps.data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return os.Rename(tmp, ps.filePath)
}
func (ps *PrivilegeStore) Check(user, table string, perm byte) bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	tablePerms, ok := ps.data[table]
	if !ok {
		return false
	}
	return tablePerms[user]&perm == perm
}
func (ps *PrivilegeStore) Grant(user, table string, perm byte) error {
	ps.mu.Lock()
	if ps.data[table] == nil {
		ps.data[table] = make(map[string]byte)
	}
	ps.data[table][user] |= perm
	ps.mu.Unlock()
	return ps.save()
}
func (ps *PrivilegeStore) Revoke(user, table string, perm byte) error {
	ps.mu.Lock()
	if ps.data[table] != nil {
		ps.data[table][user] &^= perm
		if ps.data[table][user] == 0 {
			delete(ps.data[table], user)
		}
	}
	ps.mu.Unlock()
	return ps.save()
}

// ==================== 表结构 ====================

type Table struct {
	meta     *TableMeta
	pkTree   *RBTree[int64, []byte]
	idxTrees map[string]*RBTree[[]byte, []int64]
	walFile  *os.File
	tableID  uint32
	mu       sync.RWMutex
}

func NewTable(meta *TableMeta, walFile *os.File) *Table {
	// NewTable 可独立于 NewDB 使用（如测试直接建表）：注册表未初始化时自动重置，
	// 避免依赖调用方先建库的隐式顺序。
	if walRegByName == nil {
		walRegReset()
	}
	t := &Table{
		meta:     meta,
		pkTree:   newIntKeyTree[[]byte](),
		idxTrees: make(map[string]*RBTree[[]byte, []int64]),
		walFile:  walFile,
		tableID:  walRegRegister(meta.Name),
	}
	for idxName := range meta.Indexes {
		t.idxTrees[idxName] = newBytesKeyTree[[]int64]()
	}
	return t
}

func (t *Table) encodeIndexValue(fieldName string, val interface{}) []byte {
	switch v := val.(type) {
	case int64:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(v))
		return b
	case string:
		return []byte(v)
	default:
		return []byte{}
	}
}

func (t *Table) updateIndex(fieldName, idxName string, oldVal, newVal interface{}, pk int64) {
	if oldVal == newVal {
		return
	}
	if oldVal != nil {
		t.indexRemovePK(idxName, t.encodeIndexValue(fieldName, oldVal), pk)
	}
	if newVal != nil {
		t.indexAddPK(idxName, t.encodeIndexValue(fieldName, newVal), pk)
	}
}

// indexAddPK 将 pk 追加到索引 key 的倒排列表。写时复制：避免与共享底层数组的
// 并发读者/写者竞态覆盖，同时避免污染树内存储的列表。
func (t *Table) indexAddPK(idxName string, idxKey []byte, pk int64) {
	list, _ := t.idxTrees[idxName].Get(idxKey)
	newList := make([]int64, len(list), len(list)+1)
	copy(newList, list)
	newList = append(newList, pk)
	t.idxTrees[idxName].Set(idxKey, newList)
}

// indexRemovePK 从索引 key 的倒排列表移除 pk。只剩一个元素时直接删除 key。
func (t *Table) indexRemovePK(idxName string, idxKey []byte, pk int64) {
	list, ok := t.idxTrees[idxName].Get(idxKey)
	if !ok {
		return
	}
	if len(list) <= 1 {
		t.idxTrees[idxName].Delete(idxKey)
		return
	}
	newList := make([]int64, 0, len(list)-1)
	for _, id := range list {
		if id != pk {
			newList = append(newList, id)
		}
	}
	if len(newList) == 0 {
		t.idxTrees[idxName].Delete(idxKey)
		return
	}
	t.idxTrees[idxName].Set(idxKey, newList)
}

func (t *Table) Insert(pk int64, row map[string]interface{}, expireAt int64, txnID uint64, replay bool) error {
	rowData := encodeRow(t.meta, row, 0, expireAt)
	// 先改内存状态，再写 WAL：Compact 持有 walWriteMu 扫描时快照必然一致，
	// 扫描期完成的写会在压缩换文件后追加到新 WAL（重放幂等，不会丢失）。
	if !t.pkTree.SetNX(pk, rowData) {
		return fmt.Errorf("primary key %d already exists", pk)
	}
	for idxName, fieldName := range t.meta.Indexes {
		t.indexAddPK(idxName, t.encodeIndexValue(fieldName, row[fieldName]), pk)
	}
	if !replay {
		if err := t.writeLog(WALInsert, txnID, pk, rowData); err != nil {
			t.pkTree.Delete(pk)
			t.rebuildIndexes()
			return err
		}
	}
	return nil
}

// InsertOrdered 按 meta.Fields 顺序传入值的快速路径：跳过 map 构建与按名回查，
// 用于 SQL 位置参数形式（INSERT INTO t VALUES (...)）。
func (t *Table) InsertOrdered(pk int64, values []interface{}, expireAt int64, txnID uint64, replay bool) error {
	rowData := encodeRowOrdered(t.meta, values, 0, expireAt)
	if !t.pkTree.SetNX(pk, rowData) {
		return fmt.Errorf("primary key %d already exists", pk)
	}
	if len(t.meta.Indexes) > 0 {
		byName := make(map[string]interface{}, len(values))
		for i, f := range t.meta.Fields {
			if i < len(values) {
				byName[f.Name] = values[i]
			}
		}
		for idxName, fieldName := range t.meta.Indexes {
			t.indexAddPK(idxName, t.encodeIndexValue(fieldName, byName[fieldName]), pk)
		}
	}
	if !replay {
		if err := t.writeLog(WALInsert, txnID, pk, rowData); err != nil {
			t.pkTree.Delete(pk)
			t.rebuildIndexes()
			return err
		}
	}
	return nil
}

func (t *Table) Update(pk int64, row map[string]interface{}, expireAt int64, txnID uint64, replay bool) error {
	oldData, ok := t.pkTree.Get(pk)
	if !ok {
		return fmt.Errorf("row not found")
	}
	oldRow, oldVersion, _ := decodeRow(t.meta, oldData)
	newVersion := oldVersion + 1
	newData := encodeRow(t.meta, row, newVersion, expireAt)

	t.pkTree.Set(pk, newData)
	for idxName, fieldName := range t.meta.Indexes {
		oldVal := oldRow[fieldName]
		newVal := row[fieldName]
		t.updateIndex(fieldName, idxName, oldVal, newVal, pk)
	}
	if !replay {
		if err := t.writeLog(WALUpdate, txnID, pk, newData); err != nil {
			t.pkTree.Set(pk, oldData)
			t.rebuildIndexes()
			return err
		}
	}
	return nil
}

func (t *Table) Delete(pk int64, txnID uint64, replay bool) error {
	oldData, ok := t.pkTree.Get(pk)
	if !ok {
		return fmt.Errorf("row not found")
	}
	oldRow, _, _ := decodeRow(t.meta, oldData)
	t.pkTree.Delete(pk)
	for idxName, fieldName := range t.meta.Indexes {
		t.indexRemovePK(idxName, t.encodeIndexValue(fieldName, oldRow[fieldName]), pk)
	}
	if !replay {
		if err := t.writeLog(WALDelete, txnID, pk, nil); err != nil {
			t.pkTree.Set(pk, oldData)
			t.rebuildIndexes()
			return err
		}
	}
	return nil
}

// ---- WAL 恢复快速路径 ----
// WAL 中已包含完整编码行（encodeRow 输出），恢复时直接写入树，避免 decode→encode 往返。
// 无二级索引的表走最简路径；有索引的表按需重建索引。
func (t *Table) replayInsert(pk int64, data []byte) {
	if !t.pkTree.SetNX(pk, data) {
		return
	}
	if len(t.meta.Indexes) == 0 {
		return
	}
	row, _, _ := decodeRow(t.meta, data)
	for idxName, fieldName := range t.meta.Indexes {
		t.indexAddPK(idxName, t.encodeIndexValue(fieldName, row[fieldName]), pk)
	}
}

func (t *Table) replayUpdate(pk int64, data []byte) {
	oldData, ok := t.pkTree.Get(pk)
	if !ok {
		return
	}
	if len(t.meta.Indexes) == 0 {
		t.pkTree.Set(pk, data)
		return
	}
	oldRow, _, _ := decodeRow(t.meta, oldData)
	newRow, _, _ := decodeRow(t.meta, data)
	t.pkTree.Set(pk, data)
	for idxName, fieldName := range t.meta.Indexes {
		t.updateIndex(fieldName, idxName, oldRow[fieldName], newRow[fieldName], pk)
	}
}

func (t *Table) replayDelete(pk int64) {
	if len(t.meta.Indexes) == 0 {
		t.pkTree.Delete(pk)
		return
	}
	oldData, ok := t.pkTree.Get(pk)
	if !ok {
		return
	}
	oldRow, _, _ := decodeRow(t.meta, oldData)
	t.pkTree.Delete(pk)
	for idxName, fieldName := range t.meta.Indexes {
		t.indexRemovePK(idxName, t.encodeIndexValue(fieldName, oldRow[fieldName]), pk)
	}
}

func (t *Table) Select(conditions map[string]interface{}, minKey, maxKey *int64) ([]map[string]interface{}, error) {
	return t.SelectR(conditions, nil, minKey, maxKey, -1)
}

func matchConditions(row map[string]interface{}, conds map[string]interface{}) bool {
	for k, v := range conds {
		if row[k] != v {
			return false
		}
	}
	return true
}

// RangeCond 一个非主键字段的范围比较条件。
// Op: gt / ge / lt / le / ne / between / like
type RangeCond struct {
	Field string
	Op    string
	Val   interface{}
	Val2  interface{} // between 上限
}

// SelectR 增强查询：支持主键范围裁剪 + 非主键字段范围过滤 + 数量短路。
// - conditions 仍为等值条件（走主键/索引快速路径）
// - ranges 为非主键字段的范围条件，在扫描阶段过滤
// - minKey/maxKey 显式主键扫描区间（配合主键范围条件，复用树裁剪）
// - limit<0 表示不限制数量；否则凑够即停止，避免全表物化
func (t *Table) SelectR(conditions map[string]interface{}, filters []RangeCond, minKey, maxKey *int64, limit int) ([]map[string]interface{}, error) {
	// 主键点查优先：等值 + 精确范围
	if pkVal, ok := conditions[t.meta.PK]; ok {
		if pk, ok := pkVal.(int64); ok {
			rowData, found := t.pkTree.Get(pk)
			if !found {
				return nil, nil
			}
			if e := decodeExpireAt(rowData); e > 0 && time.Now().UnixNano() > e {
				t.pkTree.Delete(pk)
				return nil, nil
			}
			row, _, _ := decodeRow(t.meta, rowData)
			if matchConditions(row, conditions) && matchRange(row, filters) {
				return []map[string]interface{}{row}, nil
			}
			return nil, nil
		}
	}
	// 二级索引等值路径
	for idxName, fieldName := range t.meta.Indexes {
		if val, ok := conditions[fieldName]; ok {
			idxKey := t.encodeIndexValue(fieldName, val)
			if pkList, ok := t.idxTrees[idxName].Get(idxKey); ok {
				result := make([]map[string]interface{}, 0, len(pkList))
				for _, pk := range pkList {
					if rowData, ok := t.pkTree.Get(pk); ok {
						if e := decodeExpireAt(rowData); e > 0 && time.Now().UnixNano() > e {
							t.pkTree.Delete(pk)
							continue
						}
						row, _, _ := decodeRow(t.meta, rowData)
						if matchConditions(row, conditions) && matchRange(row, filters) {
							result = append(result, row)
							if limit >= 0 && len(result) >= limit {
								break
							}
						}
					}
				}
				return result, nil
			}
		}
	}
	// 全范围扫描（可被主键范围裁剪）
	capHint := 16
	if limit > 0 {
		capHint = limit
	}
	result := make([]map[string]interface{}, 0, capHint)
	var expired []int64
	now := time.Now().UnixNano()
	scan := func(key int64, value []byte) bool {
		if e := decodeExpireAt(value); e > 0 && now > e {
			expired = append(expired, key)
			return true
		}
		row, _, _ := decodeRow(t.meta, value)
		if matchConditions(row, conditions) && matchRange(row, filters) {
			result = append(result, row)
			if limit >= 0 && len(result) >= limit {
				return false
			}
		}
		return true
	}
	t.pkTree.scanRangeStop(minKey, maxKey, scan)
	// 回调持读锁，不能在回调内删除；收集后锁外清理
	for _, k := range expired {
		t.pkTree.Delete(k)
	}
	return result, nil
}

// SelectRBytes 范围查询的零分配快速路径：返回原始行字节，跳过 decodeRow 的 map 分配。
// 仅适用于 conditions==nil && filters==nil 的纯范围扫描场景（如压测）。
func (t *Table) SelectRBytes(minKey, maxKey *int64, limit int) [][]byte {
	capHint := 16
	if limit > 0 {
		capHint = limit
	}
	result := make([][]byte, 0, capHint)
	var expired []int64
	now := time.Now().UnixNano()
	scan := func(key int64, value []byte) bool {
		if e := decodeExpireAt(value); e > 0 && now > e {
			expired = append(expired, key)
			return true
		}
		result = append(result, value)
		if limit >= 0 && len(result) >= limit {
			return false
		}
		return true
	}
	t.pkTree.scanRangeStop(minKey, maxKey, scan)
	for _, k := range expired {
		t.pkTree.Delete(k)
	}
	return result
}

// SelectRaw 返回原始行字节（含 version+expireAt 前缀），用于 handleSelect 无条件快速路径。
func (t *Table) SelectRaw(minKey, maxKey *int64, limit int) [][]byte {
	return t.SelectRBytes(minKey, maxKey, limit)
}

// matchRange 判断行是否满足所有范围条件。
func matchRange(row map[string]interface{}, ranges []RangeCond) bool {
	for _, c := range ranges {
		if !testRange(row[c.Field], c.Op, c.Val, c.Val2) {
			return false
		}
	}
	return true
}

// testRange 单值范围测试，支持整数/字符串最值与 between。
func testRange(v interface{}, op string, a, b interface{}) bool {
	switch op {
	case "gt":
		return cmpVal(v, a) > 0
	case "ge":
		return cmpVal(v, a) >= 0
	case "lt":
		return cmpVal(v, a) < 0
	case "le":
		return cmpVal(v, a) <= 0
	case "ne":
		return cmpVal(v, a) != 0
	case "between":
		return cmpVal(v, a) >= 0 && cmpVal(v, b) <= 0
	}
	return true
}

func cmpVal(v, other interface{}) int {
	av, aok := toNum(v)
	bv, bok := toNum(other)
	if aok && bok {
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
		return 0
	}
	as, aokS := v.(string)
	bs, bokS := other.(string)
	if aokS && bokS {
		if as < bs {
			return -1
		}
		if as > bs {
			return 1
		}
		return 0
	}
	return 0
}

func toNum(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// SelectByPK 主键点查快速路径：省去条件 map 分配与 matchConditions 判断。
func (t *Table) SelectByPK(pk int64) (map[string]interface{}, bool) {
	rowData, found := t.pkTree.Get(pk)
	if !found {
		return nil, false
	}
	row, _, expireAt := decodeRow(t.meta, rowData)
	if expireAt > 0 && time.Now().UnixNano() > expireAt {
		t.pkTree.Delete(pk)
		return nil, false
	}
	return row, true
}

// ---------------- WAL 操作 ----------------

func (t *Table) writeLog(cmd byte, txnID uint64, pk int64, data []byte) error {
	if t.walFile == nil {
		return nil
	}
	// v1 紧凑记录（varint + 表 ID）：小行写入固定开销从 ~38B 降到 ~10B。
	// 编码在锁外完成；落盘由单写者 writer goroutine 串行处理，此处无并发锁，避免高并发锁风暴。
	buf := walBufPool.Get().([]byte)[:0]
	if cap(buf) < 24+len(data) {
		buf = make([]byte, 0, 24+len(data))
	}
	buf = append(buf, cmd)
	isTxnCmd := cmd == WALBegin || cmd == WALCommit || cmd == WALRollback
	if !isTxnCmd {
		buf = appendVarUint(buf, uint64(t.tableID))
	}
	buf = appendVarUint(buf, txnID)
	if !isTxnCmd {
		buf = appendVarUint(buf, uint64(pk))
		buf = appendVarUint(buf, uint64(len(data)))
		buf = append(buf, data...)
	}
	if walCRC.Load() {
		buf = appendU32(buf, crc32.ChecksumIEEE(buf))
	}
	return appendWAL(buf)
}

// appendWAL 将记录提交给单写者队列。batch 模式下发送即返回（writer 异步写入缓冲并由定时刷盘）；
// fsync 模式下发送并等待 writer 将该记录（连同同批并发写者）一起落盘后返回，实现组提交。
func appendWAL(rec []byte) error {
	if walClosed.Load() {
		return nil
	}
	if walFsync.Load() {
		done := make(chan error, 1)
		walCh <- walEntry{rec: rec, sync: true, done: done}
		return <-done
	}
	walCh <- walEntry{rec: rec}
	return nil
}

// 通知 writer 冲刷缓冲并同步落盘（供 DDL/Backup/Close/ticker 使用），完成后返回。
func (db *DB) flushWAL() error {
	if walClosed.Load() {
		return nil
	}
	done := make(chan error, 1)
	walCh <- walEntry{flush: true, done: done}
	return <-done
}

// walWriterLoop 是唯一写入 WAL 缓冲/文件的 goroutine：
//   - rec      写入 bufio 缓冲（进程内存）；
//   - rec.sync 或 flush：Flush 到 OS 页缓存并 Sync 落盘；多处 sync 被合并为一次组提交；
//   - stop     冲刷缓冲后退出（供 Compact 换文件、Close 收尾）。
func walWriterLoop(file *os.File, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}
	bw := bufio.NewWriterSize(file, 64*1024)
	for {
		e := <-walCh
		switch {
		case e.stop:
			if bw != nil {
				bw.Flush()
			}
			file.Sync()
			walSignal(e.done)
			return
		case e.flush:
			bw.Flush()
			file.Sync()
			if statsHook != nil {
				statsHook.IncFsync()
			}
			walSignal(e.done)
			continue
		}
		// 普通记录
		bw.Write(e.rec)
		if statsHook != nil {
			statsHook.IncWalWrite(len(e.rec))
		}
		walBufPool.Put(e.rec[:0])
		if !e.sync {
			continue
		}
		// fsync 模式：收集同批等待者 + 顺带排队的其余记录，一次 Flush+Sync 全量提交
		waiters := []chan error{e.done}
		stopAfter := false
		var stopCh chan error
		drain := true
		for drain {
			select {
			case e2 := <-walCh:
				switch {
				case e2.stop:
					stopAfter = true
					stopCh = e2.done
					drain = false
				case e2.flush:
					waiters = append(waiters, e2.done)
				default:
					bw.Write(e2.rec)
					if statsHook != nil {
						statsHook.IncWalWrite(len(e2.rec))
					}
					walBufPool.Put(e2.rec[:0])
					if e2.sync {
						waiters = append(waiters, e2.done)
					}
				}
			default:
				drain = false
			}
		}
		bw.Flush()
		file.Sync()
		if statsHook != nil {
			statsHook.IncFsync()
		}
		for _, d := range waiters {
			walSignal(d)
		}
		if stopAfter {
			walSignal(stopCh)
			return
		}
	}
}

// walSignal 安全地向完成通道发送结果（仅一次，nil 为 noop）。
func walSignal(d chan error) {
	if d != nil {
		d <- nil
	}
}

// Compact 将 WAL 重写为仅含当前内存状态的 v1 紧凑快照（header + catalog + INSERT），
// 清除已删除/被覆盖的记录，缩小文件并加速后续恢复。
// 通过 FIFO 通道建立 barrier：所有先于 barrier 入队的记录必然被冲刷进当前文件且内存态已生效
// （写者先改树后入队，故其内存态先于入队），快照必然覆盖它们；barrier 之后入队的记录
// 在换文件后追加到新 WAL（重放幂等，不丢失）。因此压缩期间写入无需全局锁。
func (db *DB) Compact() error {
	compactMu.Lock()
	defer compactMu.Unlock()
	compactBusy.Store(true)
	defer compactBusy.Store(false)

	// 1) barrier：等待 writer 把此前所有已入队记录冲刷并同步到当前文件
	bdone := make(chan error, 1)
	walCh <- walEntry{flush: true, done: bdone}
	if err := <-bdone; err != nil {
		return err
	}
	// 2) 停止 writer（冲刷后退出）；期间新写入在 walCh 排队。任何失败路径都要恢复 writer。
	stopped := false
	restarted := false
	defer func() {
		if stopped && !restarted {
			go walWriterLoop(db.walFile, nil)
		}
	}()
	sdone := make(chan error, 1)
	walCh <- walEntry{stop: true, done: sdone}
	if err := <-sdone; err != nil {
		return err
	}
	stopped = true

	db.mu.RLock()
	tables := make([]*Table, 0, int(db.tablesCount.Load()))
	db.tables.Range(func(key, value interface{}) bool {
		tables = append(tables, value.(*Table))
		return true
	})
	db.mu.RUnlock()

	walPath := db.config.WALDir + "/" + db.config.WALFile
	tmpPath := walPath + ".compact"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if err := writeWALHeader(f); err != nil {
		f.Close()
		return err
	}
	w := bufio.NewWriterSize(f, 64*1024)

	for _, t := range tables {
		// v1 紧凑 catalog 记录：[100][subop=1][varint tableID]...
		rec := make([]byte, 0, 96+len(t.meta.Name)*3)
		rec = append(rec, 100, 1)
		rec = appendVarUint(rec, uint64(walRegRegister(t.meta.Name)))
		rec = appendVarLenStr(rec, t.meta.Name)
		rec = appendVarLenStr(rec, t.meta.PK)
		rec = appendVarUint(rec, uint64(len(t.meta.Fields)))
		for _, fld := range t.meta.Fields {
			rec = appendVarLenStr(rec, fld.Name)
			rec = append(rec, byte(fld.Type))
			rec = appendVarUint(rec, uint64(fld.Len))
		}
		rec = appendVarUint(rec, uint64(len(t.meta.Indexes)))
		for idxName, idxField := range t.meta.Indexes {
			rec = appendVarLenStr(rec, idxName)
			rec = appendVarLenStr(rec, idxField)
		}
		if walCRC.Load() {
			rec = appendU32(rec, crc32.ChecksumIEEE(rec))
		}
		if _, err := w.Write(rec); err != nil {
			f.Close()
			return err
		}

		t.pkTree.scanAll(func(pk int64, value []byte) {
			rec := compactRecPool.Get().([]byte)[:0]
			if cap(rec) < 24+len(value) {
				rec = make([]byte, 0, 24+len(value))
			}
			rec = append(rec, WALInsert)
			rec = appendVarUint(rec, uint64(t.tableID))
			rec = appendVarUint(rec, 0) // 非事务
			rec = appendVarUint(rec, uint64(pk))
			rec = appendVarUint(rec, uint64(len(value)))
			rec = append(rec, value...)
			if walCRC.Load() {
				rec = appendU32(rec, crc32.ChecksumIEEE(rec))
			}
			w.Write(rec)
			compactRecPool.Put(rec[:0])
		})
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// 原子替换：关闭旧文件后覆盖式改名，避免 Windows 上占用冲突
	if err := db.walFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, walPath); err != nil {
		nf, e2 := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR, 0644)
		if e2 == nil {
			nf.Seek(0, 2)
			db.walFile = nf
		}
		return err
	}
	nf, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	// 压缩后文件已含完整内容，新句柄须定位到末尾以支持后续追加
	if _, err := nf.Seek(0, 2); err != nil {
		nf.Close()
		return err
	}
	db.walFile = nf
	// 重启单写者 writer：接管新文件，并排空压缩期间积压在 walCh 的记录
	restarted = true
	go walWriterLoop(nf, nil)
	return nil
}

func (t *Table) rebuildIndexes() {
	for idxName, fieldName := range t.meta.Indexes {
		t.idxTrees[idxName] = newBytesKeyTree[[]int64]()
		t.pkTree.scanAll(func(key int64, value []byte) {
			row, _, _ := decodeRow(t.meta, value)
			t.indexAddPK(idxName, t.encodeIndexValue(fieldName, row[fieldName]), key)
		})
	}
}

func (t *Table) CleanExpired(now int64) {
	var expired []int64
	t.pkTree.scanAll(func(key int64, value []byte) {
		if e := decodeExpireAt(value); e > 0 && e < now {
			expired = append(expired, key)
		}
	})
	if len(expired) == 0 {
		return
	}
	if len(t.meta.Indexes) == 0 {
		for _, k := range expired {
			t.pkTree.Delete(k)
		}
		return
	}
	for _, k := range expired {
		oldData, ok := t.pkTree.Get(k)
		if !ok {
			continue
		}
		oldRow, _, _ := decodeRow(t.meta, oldData)
		t.pkTree.Delete(k)
		for idxName, fieldName := range t.meta.Indexes {
			t.indexRemovePK(idxName, t.encodeIndexValue(fieldName, oldRow[fieldName]), k)
		}
	}
}

// ==================== 数据库引擎 ====================

const (
	WALMagic   = "TSUMUGI"
	WALVersion = 1
)

type DB struct {
	config            *Config
	tables            sync.Map // map[string]*Table，无锁并发安全
	tablesCount       atomic.Int64
	catalog           map[string]*TableMeta
	catalogMu         sync.RWMutex
	users             map[string]string
	privileges        *PrivilegeStore
	walFile           *os.File
	mu                sync.RWMutex // 保护 catalog、users 等
	curDB             string
	curDBMu           sync.RWMutex
	txns              map[uint64]*Transaction
	txnMu             sync.Mutex
	txnSeq            uint64
	flushTicker       *time.Ticker
	groupCommit       *GroupCommitter
	stopCh            chan struct{}
	wg                sync.WaitGroup
	ttlTicker         *time.Ticker
	stats             *Stats
	backupDir         string
	autoCompactTicker *time.Ticker
	metricsServer     atomic.Pointer[http.Server]
}

type Transaction struct {
	ID           uint64
	Active       bool
	Tables       map[string]bool
	Changes      map[string]map[int64][]byte
	ReadVersions map[string]map[int64]int64
}

func NewDB(cfg *Config) (*DB, error) {
	if err := os.MkdirAll(cfg.WALDir, 0755); err != nil {
		return nil, err
	}
	os.MkdirAll("data", 0755)
	initUserStore("data")

	// 首次安装标记：若用户库为空，写入标记文件供安装向导检测
	// 不在此处自动创建 root 用户，由安装向导完成首次设置
	if globalUsers.Count() == 0 {
		os.WriteFile("data/.first_run", []byte("1"), 0644)
		logf(LOG_OK, "first install detected, setup wizard will be shown")
	}
	walPath := cfg.WALDir + "/" + cfg.WALFile
	walFile, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	info, _ := walFile.Stat()
	if info.Size() == 0 {
		if err := writeWALHeader(walFile); err != nil {
			walFile.Close()
			return nil, err
		}
	} else {
		if err := verifyWALHeader(walFile); err != nil {
			walFile.Close()
			return nil, err
		}
	}

	privStore, err := NewPrivilegeStore(cfg.WALDir + "/" + cfg.PrivilegeFile)
	if err != nil {
		walFile.Close()
		return nil, err
	}

	db := &DB{
		config:     cfg,
		catalog:    make(map[string]*TableMeta),
		users:      map[string]string{},
		privileges: privStore,
		walFile:    walFile,
		txns:       make(map[uint64]*Transaction),
		stopCh:     make(chan struct{}),
		stats:      NewStats(),
		backupDir:  cfg.BackupDir,
	}
	db.stats.durability = cfg.Durability
	statsHook = db.stats
	mysqlCfg.load(cfg.WALDir)
	db.groupCommit = NewGroupCommitter(db, cfg.GroupCommitInterval)
	db.flushTicker = time.NewTicker(cfg.FlushInterval)
	db.ttlTicker = time.NewTicker(cfg.TTLCleanInterval)
	db.wg.Add(2)
	go db.flushLoop()
	go db.ttlLoop()

	// 低峰期自动整理 WAL（间隔 = CompactIdleSeconds；默认 60s）
	interval := time.Duration(cfg.CompactIdleSeconds) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	db.autoCompactTicker = time.NewTicker(interval)
	db.wg.Add(1)
	go db.autoCompactLoop()

	// 启动 HTTP 监控（在 metrics.go 中定义）
	go db.startMetricsServer()

	if err := db.loadWAL(); err != nil {
		return nil, err
	}
	// 创建 WAL 写缓冲（loadWAL 完成后，确保文件偏移在末尾）
	if _, err := walFile.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	walFsync.Store(cfg.Durability.Fsync())
	walCRC.Store(cfg.EnableChecksum)
	walClosed.Store(false)
	walRegReset()
	db.walFile = walFile
	// 启动唯一 WAL writer：此后所有 appendWAL/flushWAL 经通道交给它串行落盘
	db.wg.Add(1)
	go walWriterLoop(walFile, &db.wg)
	logf(LOG_VERB, "WAL loaded (%d tables, %d rows) in %v", db.tablesCount.Load(), atomic.LoadInt64(&walLoadRows), time.Since(walLoadStart))
	return db, nil
}

func writeWALHeader(f *os.File) error {
	if _, err := f.Write([]byte(WALMagic)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.BigEndian, uint32(WALVersion)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.BigEndian, uint32(0)); err != nil {
		return err
	}
	return f.Sync()
}

func verifyWALHeader(f *os.File) error {
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	magic := make([]byte, len(WALMagic))
	if _, err := io.ReadFull(f, magic); err != nil {
		return err
	}
	if string(magic) != WALMagic {
		return fmt.Errorf("invalid WAL magic, expected %s", WALMagic)
	}
	var version uint32
	if err := binary.Read(f, binary.BigEndian, &version); err != nil {
		return err
	}
	// v1 紧凑格式版本号即为 1；兼容读取 v2 标记窗口（同样格式）下写入的文件
	if version != 1 && version != 2 {
		return fmt.Errorf("unsupported WAL version %d, expected %d", version, WALVersion)
	}
	var reserved uint32
	if err := binary.Read(f, binary.BigEndian, &reserved); err != nil {
		return err
	}
	_, err := f.Seek(0, 2)
	return err
}

func (db *DB) flushLoop() {
	defer db.wg.Done()
	for {
		select {
		case <-db.flushTicker.C:
			db.flushWAL()
		case <-db.stopCh:
			return
		}
	}
}

// compactBusy 阻止手动 Compact 与自动整理并发执行。
var compactBusy atomic.Bool

// autoCompactLoop 在低峰期自动整理 WAL：将 WAL 重写为仅含当前状态的精简快照，
// 以合并先前累积的 insert/update/delete（例如+1 后又 -1，整理后只保留净结果）。
func (db *DB) autoCompactLoop() {
	defer db.wg.Done()
	if db.autoCompactTicker == nil {
		return
	}
	for {
		select {
		case <-db.autoCompactTicker.C:
			db.maybeCompact()
		case <-db.stopCh:
			return
		}
	}
}

// maybeCompact 判断是否处于低峰期且 WAL 足够大，满足条件则触发一次整理。
func (db *DB) maybeCompact() {
	if !db.config.AutoCompact || compactBusy.Load() {
		return
	}
	minMB := db.config.CompactMinWALMB
	if minMB <= 0 {
		minMB = 64
	}
	if walFileSizeMB(db) < float64(minMB) {
		return
	}
	// 低峰判定：最近 1 秒命令数低于阈值则视为空闲
	peak := db.config.CompactPeakRate
	if peak <= 0 {
		peak = 50
	}
	if db.stats.SnapshotQPS() > int64(peak) {
		return
	}
	logf(LOG_VERB, "AutoCompact: low-peak detected, compacting WAL...")
	if err := db.Compact(); err != nil {
		logf(LOG_ERR, "AutoCompact failed: %v", err)
	}
}

func (db *DB) ttlLoop() {
	defer db.wg.Done()
	for {
		select {
		case <-db.ttlTicker.C:
			db.cleanExpired()
		case <-db.stopCh:
			return
		}
	}
}

func (db *DB) cleanExpired() {
	now := time.Now().UnixNano()
	db.mu.RLock()
	tables := make([]*Table, 0, int(db.tablesCount.Load()))
	db.tables.Range(func(key, value interface{}) bool {
		tables = append(tables, value.(*Table))
		return true
	})
	db.mu.RUnlock()
	for _, t := range tables {
		t.CleanExpired(now)
	}
}

func (db *DB) Close() {
	// 阻止自动整理在关闭期间并发执行
	compactBusy.Store(true)
	close(db.stopCh)
	db.flushTicker.Stop()
	db.ttlTicker.Stop()
	if db.autoCompactTicker != nil {
		db.autoCompactTicker.Stop()
	}
	if srv := db.metricsServer.Load(); srv != nil {
		srv.Close()
	}
	// 标记关闭：此后 appendWAL/flushWAL 直接返回，避免阻塞在已退出的 writer 上
	walClosed.Store(true)
	// 停止 writer：冲刷并同步所有已入队记录后退出
	done := make(chan error, 1)
	walCh <- walEntry{stop: true, done: done}
	<-done
	db.wg.Wait()
	db.walFile.Close()
}

// WAL 恢复（带校验）
type walOp struct {
	table string
	cmd   byte
	pk    int64
	data  []byte
}

func (db *DB) loadWAL() error {
	walLoadStart = time.Now()
	// 重放期间 applyCatalogV2/walRegRebind 会读写注册表，必须先初始化（map 为 nil 会 panic）
	walRegReset()
	file := db.walFile

	// 一次性读入内存解析：避免逐字段 syscall（30MB WAL 原需 40s+）。
	// 按文件大小预分配，避免 io.ReadAll 多次扩容拷贝。
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	st, err := file.Stat()
	if err != nil {
		return err
	}
	if st.Size() < 0 || st.Size() > 1<<32 {
		return fmt.Errorf("wal file size out of range: %d", st.Size())
	}
	all := make([]byte, int(st.Size()))
	if _, err := io.ReadFull(file, all); err != nil {
		return err
	}
	headerLen := len(WALMagic) + 4 + 4
	if len(all) < headerLen {
		return nil
	}
	version := binary.BigEndian.Uint32(all[len(WALMagic):])
	ctx := &walReplayCtx{
		db:          db,
		file:        file,
		headerLen:   headerLen,
		buf:         all[headerLen:],
		enableCRC:   db.config.EnableChecksum,
		pending:     make(map[string][]walOp),
		pendingTxns: make(map[uint64][]walOp),
	}
	switch version {
	case WALVersion, 2:
		return ctx.loadV2()
	default:
		return fmt.Errorf("unsupported WAL version %d, expected %d", version, WALVersion)
	}
}

// walReplayCtx 承载 v1/v2 共用的按表并行重放基础设施
type walReplayCtx struct {
	db          *DB
	file        *os.File
	headerLen   int
	buf         []byte
	enableCRC   bool
	pending     map[string][]walOp
	pendingTxns map[uint64][]walOp
	loadedRows  int64
	lastPct     int
	barDone     bool
}

// walProgressBar 渲染并刷新一条 Minecraft 风味的加载进度条（\r 原地刷新，不换行）。
// 仅在解析推进时更新，避免打字风暴；返回是否真正打印过。
func (c *walReplayCtx) walProgressBar(pos, total int, rows int64) bool {
	if total <= 0 {
		return false
	}
	pct := pos * 100 / total
	if pct < c.lastPct+1 {
		return false
	}
	c.lastPct = pct
	width := 24
	filled := pct * width / 100
	bar := strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
	fmt.Printf("\r[INFO] Loading databases... [%s] %3d%% (%d rows)", bar, pct, rows)
	return true
}

// walProgressFinish 在加载完成时显示 100% 并换行，避免进度条与后续日志粘连。
func (c *walReplayCtx) walProgressFinish() {
	if c.barDone {
		return
	}
	total := len(c.buf)
	width := 24
	bar := strings.Repeat("#", width)
	var pct int
	if total > 0 {
		pct = 100
	}
	rows := atomic.LoadInt64(&c.loadedRows)
	fmt.Printf("\r[INFO] Loading databases... [%s] %3d%% (%d rows)\n", bar, pct, rows)
	c.barDone = true
}

// applyOpOnTable 直接应用到已解析的表，避免每 op 一次表查找（RLock + map）。
func (c *walReplayCtx) applyOpOnTable(t *Table, cmd byte, pk int64, data []byte) {
	atomic.AddInt64(&c.loadedRows, 1)
	switch cmd {
	case WALInsert:
		t.replayInsert(pk, data)
	case WALUpdate:
		t.replayUpdate(pk, data)
	case WALDelete:
		t.replayDelete(pk)
	}
}

func (c *walReplayCtx) applyOp(tableName string, cmd byte, pk int64, data []byte) {
	table := c.db.getOrCreateTable(tableName)
	if table == nil {
		return
	}
	c.applyOpOnTable(table, cmd, pk, data)
}

// 并行应用当前缓冲：不同表互不冲突，可安全并行；同表记录保持顺序。
// 使用有界 worker 池（GOMAXPROCS）而非每表一 goroutine，避免表很多时线程风暴。
func (c *walReplayCtx) flushPending() {
	if len(c.pending) == 0 {
		return
	}
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for tableName, ops := range c.pending {
		table := c.db.getOrCreateTable(tableName)
		if table == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(t *Table, recs []walOp) {
			defer wg.Done()
			defer func() { <-sem }()
			for _, op := range recs {
				c.applyOpOnTable(t, op.cmd, op.pk, op.data)
			}
		}(table, ops)
	}
	wg.Wait()
	c.pending = make(map[string][]walOp)
}

// 崩溃/强杀留下的残缺尾部记录：截断丢弃而不是启动失败
func (c *walReplayCtx) truncateTail(recordStart int) {
	fileOffset := int64(c.headerLen + recordStart)
	if err := c.file.Truncate(fileOffset); err != nil {
		logf(LOG_ERR, "truncate WAL to %d failed: %v", fileOffset, err)
		return
	}
	c.file.Seek(0, io.SeekEnd)
	logf(LOG_VERB, "discarded incomplete WAL tail record at offset %d", fileOffset)
}

// tailOrErr 解析缺口：若该缺口位于文件末尾（已耗尽缓冲）则截断，否则报错
func (c *walReplayCtx) tailOrErr(recordStart int, pos int, format string, args ...interface{}) error {
	if pos >= len(c.buf) {
		c.truncateTail(recordStart)
		return nil
	}
	return fmt.Errorf(format, args...)
}

// ---- v1 兼容读取（旧文件重放，格式不变）----

// ---- v1 紧凑格式解析（varint + 表 ID + 每记录 CRC）----

func (c *walReplayCtx) loadV2() error {
	// 解析循环内可能因残缺尾部提前退出，因此在此统一收尾：
	// 任何情况下都必须先把已解析的有效记录重放完，再算统计。
	err := c.parseV2()
	c.flushPending()
	walLoadRows = c.loadedRows
	// 进度条收尾：显示 100% 后换行，让随后的 "WAL loaded" 日志另起一行。
	c.walProgressFinish()
	return err
}

func (c *walReplayCtx) parseV2() error {
	buf := c.buf
	pos := 0

	// 当一条记录的数据在缓冲内耗尽时，若恰好位于文件末尾则视为崩溃残留，
	// 截断尾部并正常结束解析（返回 true 表示已截断尾部）。
	tail := func(recordStart int) bool {
		if pos >= len(buf) {
			c.truncateTail(recordStart)
			return true
		}
		return false
	}

loop:
	for pos < len(buf) {
		c.walProgressBar(pos, len(buf), c.loadedRows)
		recordStart := pos
		cmd := buf[pos]
		pos++

		if cmd == 100 { // Catalog 记录
			if tail(recordStart) {
				break loop
			}
			// catalog（建表/改表/删表）为并行重放的屏障：先把此前缓存的非事务记录落库，
			// 避免它们在与 drop+重建同名表时泄漏/复活到新表。
			c.flushPending()
			sub := buf[pos]
			pos++
			switch sub {
			case 1: // CREATE / ALTER（含完整 meta）
				id64, ok := readVarUint(buf, &pos)
				if !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog create id")
				}
				nameLen, ok := readVarUint(buf, &pos)
				if !ok || pos+int(nameLen) > len(buf) {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog create name")
				}
				name := string(buf[pos : pos+int(nameLen)])
				pos += int(nameLen)
				meta, err := parseMetaV2(buf, &pos, name)
				if err != nil {
					if tail(recordStart) {
						break loop
					}
					return err
				}
				if c.enableCRC {
					expected, ok := readU32(buf, &pos)
					if !ok {
						return fmt.Errorf("incomplete catalog checksum")
					}
					if crc32.ChecksumIEEE(buf[recordStart:pos-4]) != expected {
						return fmt.Errorf("checksum mismatch in catalog table %s", name)
					}
				}
				c.db.applyCatalogV2(uint32(id64), meta)
			case 2: // DROP
				if _, ok := readVarUint(buf, &pos); !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog drop id")
				}
				nameLen, ok := readVarUint(buf, &pos)
				if !ok || pos+int(nameLen) > len(buf) {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog drop name")
				}
				name := string(buf[pos : pos+int(nameLen)])
				pos += int(nameLen)
				if c.enableCRC {
					expected, ok := readU32(buf, &pos)
					if !ok {
						return fmt.Errorf("incomplete catalog checksum")
					}
					if crc32.ChecksumIEEE(buf[recordStart:pos-4]) != expected {
						return fmt.Errorf("checksum mismatch in catalog drop")
					}
				}
				c.db.applyCatalogDropV2(name)
			case 3: // RENAME：id + newName（同一表，仅改名，保留行数据）
				id64, ok := readVarUint(buf, &pos)
				if !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog rename id")
				}
				nnameLen, ok := readVarUint(buf, &pos)
				if !ok || pos+int(nnameLen) > len(buf) {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog rename name")
				}
				nname := string(buf[pos : pos+int(nnameLen)])
				pos += int(nnameLen)
				if c.enableCRC {
					expected, ok := readU32(buf, &pos)
					if !ok {
						return fmt.Errorf("incomplete catalog checksum")
					}
					if crc32.ChecksumIEEE(buf[recordStart:pos-4]) != expected {
						return fmt.Errorf("checksum mismatch in catalog rename")
					}
				}
				c.db.applyCatalogRenameV2(uint32(id64), nname)
			default:
				return fmt.Errorf("unknown catalog subcommand %d", sub)
			}
			continue
		}

		// 数据记录：事务命令（BEGIN/COMMIT/ROLLBACK）在写侧不编码表 ID 与 pk/data，
		// 仅 [cmd][varint txnID]；行记录为 [cmd][varint tableID][varint txnID][varint pk][varint len][data]。
		isTxnCmd := cmd == WALBegin || cmd == WALCommit || cmd == WALRollback
		var tableName string
		var txnID uint64
		var pk int64
		var data []byte
		if isTxnCmd {
			var ok bool
			txnID, ok = readVarUint(buf, &pos)
			if !ok {
				if tail(recordStart) {
					break loop
				}
				return fmt.Errorf("incomplete txn id")
			}
		} else {
			id64, ok := readVarUint(buf, &pos)
			if !ok {
				if tail(recordStart) {
					break loop
				}
				return fmt.Errorf("incomplete table id")
			}
			var ok2 bool
			tableName, ok2 = walRegName(uint32(id64))
			if !ok2 {
				return fmt.Errorf("WAL references unknown table id %d", id64)
			}
			if txnID, ok = readVarUint(buf, &pos); !ok {
				if tail(recordStart) {
					break loop
				}
				return fmt.Errorf("incomplete txn id")
			}
			pkv, ok := readVarUint(buf, &pos)
			if !ok {
				if tail(recordStart) {
					break loop
				}
				return fmt.Errorf("incomplete pk")
			}
			pk = int64(pkv)
			dataLen, ok := readVarUint(buf, &pos)
			if !ok {
				if tail(recordStart) {
					break loop
				}
				return fmt.Errorf("incomplete data length")
			}
			if pos+int(dataLen) > len(buf) {
				if tail(recordStart) {
					break loop
				}
				return fmt.Errorf("incomplete record data")
			}
			data = buf[pos : pos+int(dataLen)]
			pos += int(dataLen)
		}

		if c.enableCRC {
			expected, ok := readU32(buf, &pos)
			if !ok {
				if tail(recordStart) {
					break loop
				}
				return fmt.Errorf("incomplete checksum")
			}
			calc := crc32.ChecksumIEEE(buf[recordStart : pos-4])
			if calc != expected {
				if pos == len(buf) {
					c.truncateTail(recordStart)
					break loop
				}
				return fmt.Errorf("checksum mismatch at cmd=%d table=%s txn=%d", cmd, tableName, txnID)
			}
		}

		switch cmd {
		case WALInsert, WALUpdate, WALDelete:
			if txnID == 0 {
				c.pending[tableName] = append(c.pending[tableName], walOp{tableName, cmd, pk, data})
			} else {
				c.pendingTxns[txnID] = append(c.pendingTxns[txnID], walOp{tableName, cmd, pk, data})
			}
		case WALBegin:
			c.flushPending()
			c.pendingTxns[txnID] = nil
		case WALCommit:
			var curT *Table
			var curName string
			for _, op := range c.pendingTxns[txnID] {
				if curT == nil || op.table != curName {
					curT = c.db.getOrCreateTable(op.table)
					curName = op.table
				}
				if curT != nil {
					c.applyOpOnTable(curT, op.cmd, op.pk, op.data)
				}
			}
			delete(c.pendingTxns, txnID)
		case WALRollback:
			delete(c.pendingTxns, txnID)
		}
	}
	return nil
}

// parseMetaV2 解析 compact catalog CREATE 记录中表名之后的 meta 部分。
func parseMetaV2(b []byte, pos *int, tableName string) (*TableMeta, error) {
	pkLen, ok := readVarUint(b, pos)
	if !ok {
		return nil, fmt.Errorf("incomplete pk length")
	}
	if *pos+int(pkLen) > len(b) {
		return nil, fmt.Errorf("incomplete pk")
	}
	pk := string(b[*pos : *pos+int(pkLen)])
	*pos += int(pkLen)

	fieldCount, ok := readVarUint(b, pos)
	if !ok {
		return nil, fmt.Errorf("incomplete field count")
	}
	fields := make([]Field, 0, int(fieldCount))
	for i := uint64(0); i < fieldCount; i++ {
		nameLen, ok := readVarUint(b, pos)
		if !ok || *pos+int(nameLen) > len(b) {
			return nil, fmt.Errorf("incomplete field name")
		}
		fName := string(b[*pos : *pos+int(nameLen)])
		*pos += int(nameLen)
		if *pos >= len(b) {
			return nil, fmt.Errorf("incomplete field type")
		}
		fType := FieldType(b[*pos])
		*pos++
		fLen, ok := readVarUint(b, pos)
		if !ok {
			return nil, fmt.Errorf("incomplete field len")
		}
		fields = append(fields, Field{Name: fName, Type: fType, Len: int(fLen)})
	}

	idxCount, ok := readVarUint(b, pos)
	if !ok {
		return nil, fmt.Errorf("incomplete index count")
	}
	indexes := make(map[string]string, int(idxCount))
	for i := uint64(0); i < idxCount; i++ {
		idxNameLen, ok := readVarUint(b, pos)
		if !ok || *pos+int(idxNameLen) > len(b) {
			return nil, fmt.Errorf("incomplete index name")
		}
		idxName := string(b[*pos : *pos+int(idxNameLen)])
		*pos += int(idxNameLen)
		idxFieldLen, ok := readVarUint(b, pos)
		if !ok || *pos+int(idxFieldLen) > len(b) {
			return nil, fmt.Errorf("incomplete index field")
		}
		idxField := string(b[*pos : *pos+int(idxFieldLen)])
		*pos += int(idxFieldLen)
		indexes[idxName] = idxField
	}
	return &TableMeta{Name: tableName, PK: pk, Fields: fields, Indexes: indexes}, nil
}

func (db *DB) createTableFromMeta(meta *TableMeta) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.tables.Load(meta.Name); ok {
		return
	}
	table := NewTable(meta, db.walFile)
	db.tables.Store(meta.Name, table)
	db.tablesCount.Add(1)
	db.catalog[meta.Name] = meta
}

// applyCatalogV2 恢复建表/改表：将注册表绑定到文件中的 ID。若同名表已存在（如 ALTER ADD COLUMN 追加列），
// 更新其 meta 使新列生效（旧行按零值解码，向后兼容），否则新建。
func (db *DB) applyCatalogV2(id uint32, meta *TableMeta) {
	walRegRebind(id, meta.Name)
	db.mu.Lock()
	if v, ok := db.tables.Load(meta.Name); ok {
		// 既有表（如 ALTER ADD COLUMN）：更新其 meta 使新列在 SELECT 解码时生效。
		v.(*Table).meta = meta
		db.catalog[meta.Name] = meta
		db.mu.Unlock()
		return
	}
	db.mu.Unlock()
	db.createTableFromMeta(meta)
}

func (db *DB) applyCatalogDropV2(name string) {
	db.mu.Lock()
	db.tables.Delete(name)
	db.tablesCount.Add(-1)
	delete(db.catalog, name)
	walRegUnregister(name)
	db.mu.Unlock()
}

// applyCatalogRenameV2 恢复期表重命名：将 id 对应的既有表改名为 newName（保留行数据）。
func (db *DB) applyCatalogRenameV2(id uint32, newName string) {
	db.mu.Lock()
	oldName, ok := walRegName(id)
	if !ok {
		// 表尚未注册（日志顺序异常）：直接按新名建空表兜底
		db.mu.Unlock()
		return
	}
	val, exists := db.tables.Load(oldName)
	if exists {
		mt := val.(*Table).meta
		mt.Name = newName
		db.tables.Delete(oldName)
		db.tables.Store(newName, val)
	} else if m, ok := db.catalog[oldName]; ok {
		m.Name = newName
		db.catalog[newName] = m
	}
	delete(db.catalog, oldName)
	walRegRename(oldName, newName, id)
	db.mu.Unlock()
}

func (db *DB) getOrCreateTable(name string) *Table {
	// 先尝试无锁 Load
	if v, ok := db.tables.Load(name); ok {
		return v.(*Table)
	}
	db.catalogMu.RLock()
	meta, ok := db.catalog[name]
	db.catalogMu.RUnlock()
	if !ok {
		return nil
	}
	// 二次检查 + 原子 Store
	db.mu.Lock()
	defer db.mu.Unlock()
	if v, ok := db.tables.Load(name); ok {
		return v.(*Table)
	}
	table := NewTable(meta, db.walFile)
	db.tables.Store(name, table)
	return table
}

// Catalog 持久化（调用方需持有 db.mu）
// v1 紧凑格式：[100][subop=1][varint tableID][varint nameLen][name][varint pkLen][pk]
//
//	[varint fieldCount]（每字段 [varint nameLen][name][byte type][varint len]）
//	[varint idxCount]（每索引 [varint nameLen][name][varint fieldLen][field]）[CRC]
func (db *DB) writeCatalog(meta *TableMeta) error {
	return db.writeCatalogID(meta, 0)
}

// writeCatalogID 与 writeCatalog 相同，但允许显式指定注册表 ID（0 = 自动分配）。
// 用于表重命名：保留原 ID 使既存行数据仍与其关联。
func (db *DB) writeCatalogID(meta *TableMeta, fixedID uint32) error {
	var id uint32
	if fixedID != 0 {
		id = fixedID
	} else {
		id = walRegRegister(meta.Name)
	}
	buf := make([]byte, 0, 96+len(meta.Name)*3)
	buf = append(buf, 100, 1) // catalog 记录 + CREATE 子命令
	buf = appendVarUint(buf, uint64(id))
	buf = appendVarLenStr(buf, meta.Name)
	buf = appendVarLenStr(buf, meta.PK)
	buf = appendVarUint(buf, uint64(len(meta.Fields)))
	for _, f := range meta.Fields {
		buf = appendVarLenStr(buf, f.Name)
		buf = append(buf, byte(f.Type))
		buf = appendVarUint(buf, uint64(f.Len))
	}
	buf = appendVarUint(buf, uint64(len(meta.Indexes)))
	for idxName, fieldName := range meta.Indexes {
		buf = appendVarLenStr(buf, idxName)
		buf = appendVarLenStr(buf, fieldName)
	}
	if walCRC.Load() {
		buf = appendU32(buf, crc32.ChecksumIEEE(buf))
	}
	if err := appendWAL(buf); err != nil {
		return err
	}
	return db.flushWAL()
}

// writeCatalogDrop 追加一条 catalog DROP 记录（供重命名等场景剥离旧名绑定）。
func (db *DB) writeCatalogDrop(name string) error {
	buf := make([]byte, 0, 16+len(name))
	buf = append(buf, 100, 2)
	id := walRegRegister(name)
	buf = appendVarUint(buf, uint64(id))
	buf = appendVarLenStr(buf, name)
	if walCRC.Load() {
		buf = appendU32(buf, crc32.ChecksumIEEE(buf))
	}
	if err := appendWAL(buf); err != nil {
		return err
	}
	return db.flushWAL()
}

// writeCatalogRename 追加一条 catalog RENAME 记录：[100][3][varint id][varint len][newName][CRC]。
func (db *DB) writeCatalogRename(id uint32, newName string) error {
	buf := make([]byte, 0, 24+len(newName))
	buf = append(buf, 100, 3)
	buf = appendVarUint(buf, uint64(id))
	buf = appendVarLenStr(buf, newName)
	if walCRC.Load() {
		buf = appendU32(buf, crc32.ChecksumIEEE(buf))
	}
	if err := appendWAL(buf); err != nil {
		return err
	}
	return db.flushWAL()
}

// 事务
func (db *DB) BeginTxn() uint64 {
	db.txnMu.Lock()
	defer db.txnMu.Unlock()
	db.txnSeq++
	id := db.txnSeq
	db.txns[id] = &Transaction{
		ID:           id,
		Active:       true,
		Tables:       make(map[string]bool),
		Changes:      make(map[string]map[int64][]byte),
		ReadVersions: make(map[string]map[int64]int64),
	}
	return id
}

func (db *DB) recordReadVersion(txnID uint64, table string, pk int64, version int64) {
	db.txnMu.Lock()
	defer db.txnMu.Unlock()
	txn, ok := db.txns[txnID]
	if !ok || !txn.Active {
		return
	}
	if txn.ReadVersions[table] == nil {
		txn.ReadVersions[table] = make(map[int64]int64)
	}
	txn.ReadVersions[table][pk] = version
}

func (db *DB) CommitTxn(id uint64) error {
	db.txnMu.Lock()
	txn, ok := db.txns[id]
	if !ok || !txn.Active {
		db.txnMu.Unlock()
		return fmt.Errorf("transaction not active")
	}
	db.txnMu.Unlock()

	for tableName, changes := range txn.Changes {
		db.mu.RLock()
		val, ok := db.tables.Load(tableName)
		db.mu.RUnlock()
		if !ok {
			continue
		}
		table := val.(*Table)
		for pk, oldData := range changes {
			currentData, ok := table.pkTree.Get(pk)
			if !ok {
				if oldData != nil {
					db.RollbackTxn(id)
					return fmt.Errorf("conflict: row %d deleted", pk)
				}
				continue
			}
			currentVersion := decodeVersion(currentData)
			var oldVersion int64
			if txn.ReadVersions[tableName] != nil {
				oldVersion = txn.ReadVersions[tableName][pk]
			}
			if currentVersion != oldVersion {
				db.RollbackTxn(id)
				return fmt.Errorf("conflict: row %d version changed", pk)
			}
		}
	}

	for tableName := range txn.Tables {
		db.mu.RLock()
		val, ok := db.tables.Load(tableName)
		db.mu.RUnlock()
		if ok {
			table := val.(*Table)
			table.writeLog(WALCommit, id, 0, nil)
		}
	}
	// batch 模式下经 GroupCommitter 合并等待一次定时刷盘保证提交持久化；
	// fsync 模式下 appendWAL 已在 writer 中同步落盘，无需再等（避免双重等待）。
	if !walFsync.Load() {
		<-db.groupCommit.Join()
	}

	db.txnMu.Lock()
	txn.Active = false
	delete(db.txns, id)
	db.txnMu.Unlock()
	return nil
}

func (db *DB) RollbackTxn(id uint64) error {
	db.txnMu.Lock()
	txn, ok := db.txns[id]
	if !ok || !txn.Active {
		db.txnMu.Unlock()
		return fmt.Errorf("transaction not active")
	}
	db.txnMu.Unlock()

	for tableName, changes := range txn.Changes {
		db.mu.RLock()
		val, ok := db.tables.Load(tableName)
		db.mu.RUnlock()
		if !ok {
			continue
		}
		table := val.(*Table)
		needRebuild := false
		for pk, oldData := range changes {
			if oldData == nil {
				table.Delete(pk, id, true)
			} else {
				table.pkTree.Set(pk, oldData)
				needRebuild = true
			}
		}
		if needRebuild {
			table.rebuildIndexes()
		}
	}

	db.txnMu.Lock()
	txn.Active = false
	delete(db.txns, id)
	db.txnMu.Unlock()
	return nil
}

func (db *DB) CheckPrivilege(user, table string, perm byte) bool {
	if user == db.config.User {
		return true
	}
	return db.privileges.Check(user, table, perm)
}

// 系统表
func (db *DB) getSystemTable(name string) ([]map[string]interface{}, error) {
	switch name {
	case "__tables":
		return db.getTablesTable(), nil
	case "__indexes":
		return db.getIndexesTable(), nil
	case "__stats":
		return db.getStatsTable(), nil
	default:
		return nil, fmt.Errorf("unknown system table: %s", name)
	}
}

func (db *DB) getTablesTable() []map[string]interface{} {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var rows []map[string]interface{}
	for name, meta := range db.catalog {
		row := map[string]interface{}{
			"table_name":  name,
			"pk":          meta.PK,
			"field_count": len(meta.Fields),
			"index_count": len(meta.Indexes),
		}
		rows = append(rows, row)
	}
	return rows
}

func (db *DB) getIndexesTable() []map[string]interface{} {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var rows []map[string]interface{}
	for tableName, meta := range db.catalog {
		for idxName, fieldName := range meta.Indexes {
			row := map[string]interface{}{
				"table_name": tableName,
				"index_name": idxName,
				"field":      fieldName,
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func (db *DB) getStatsTable() []map[string]interface{} {
	snapshot := db.stats.Snapshot()
	row := map[string]interface{}{
		"total_commands": snapshot["total_commands"],
		"total_errors":   snapshot["total_errors"],
		"commands":       snapshot["commands"],
	}
	return []map[string]interface{}{row}
}

// ALTER TABLE
func (db *DB) AlterTableAddColumn(tableName, fieldName string, fieldType FieldType, fieldLen int) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	meta, ok := db.catalog[tableName]
	if !ok {
		return fmt.Errorf("table not found")
	}
	for _, f := range meta.Fields {
		if f.Name == fieldName {
			return fmt.Errorf("field %s already exists", fieldName)
		}
	}
	meta.Fields = append(meta.Fields, Field{Name: fieldName, Type: fieldType, Len: fieldLen})
	if err := db.writeCatalog(meta); err != nil {
		return err
	}
	return nil
}

// Backup
func (db *DB) Backup() error {
	if err := os.MkdirAll(db.backupDir, 0755); err != nil {
		return err
	}
	db.mu.Lock()
	// 先冲刷 WAL 缓冲并落盘，确保备份文件完整
	if err := db.flushWAL(); err != nil {
		db.mu.Unlock()
		return err
	}
	db.mu.Unlock()

	timestamp := time.Now().Format("20060102_150405")
	backupFile := fmt.Sprintf("%s/tsumugi_%s.wal", db.backupDir, timestamp)
	src, err := os.Open(db.walFile.Name())
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(backupFile)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	// 备份权限
	privSrc, err := os.Open(db.config.WALDir + "/" + db.config.PrivilegeFile)
	if err == nil {
		defer privSrc.Close()
		privDst, err := os.Create(fmt.Sprintf("%s/privileges_%s.json", db.backupDir, timestamp))
		if err == nil {
			defer privDst.Close()
			io.Copy(privDst, privSrc)
		}
	}
	logf(LOG_VERB, "backup created: %s", backupFile)
	return nil
}

// 组提交
type GroupCommitter struct {
	db       *DB
	interval time.Duration
	mu       sync.Mutex
	waiters  []chan struct{}
	timer    *time.Timer
}

func NewGroupCommitter(db *DB, interval time.Duration) *GroupCommitter {
	return &GroupCommitter{db: db, interval: interval}
}
func (gc *GroupCommitter) Join() <-chan struct{} {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	ch := make(chan struct{}, 1)
	gc.waiters = append(gc.waiters, ch)
	if gc.timer == nil {
		gc.timer = time.AfterFunc(gc.interval, gc.flush)
	}
	return ch
}
func (gc *GroupCommitter) flush() {
	gc.mu.Lock()
	waiters := gc.waiters
	gc.waiters = nil
	gc.timer = nil
	gc.mu.Unlock()
	if len(waiters) == 0 {
		return
	}
	gc.db.flushWAL()
	for _, ch := range waiters {
		close(ch)
	}
}
