package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// ==================== 数据类型 ====================

type FieldType byte

// 字段类型。类型字节会原样写入 catalog WAL，因此新类型只能**追加**在末尾，
// 绝不能重排/复用已有数值，否则旧文件会被解析成错误的列类型。
const (
	TypeInt FieldType = iota
	TypeVarchar
	TypeBool
	// TypeDate：DATE。行内是 int64 Unix 纳秒（UTC 当日 00:00:00）。
	TypeDate
	// TypeDatetime：DATETIME。行内是 int64 Unix 纳秒（UTC）。
	TypeDatetime
	// TypeDecimal：DECIMAL(p,s)。行内是 int64 定点：
	// 真实值 = 存储值 / 10^s，其中 s 存在 Field.Len（DECIMAL 没有字节长度
	// 语义，复用这个槽位可避免改动 catalog 记录格式）。
	TypeDecimal
	// TypeText：TEXT。uint32 长度前缀 + 原始字节，NULL 哨兵 0xFFFFFFFF。
	TypeText
	// TypeBlob：BLOB。编码与 TEXT 相同，内容按字节原样保存。
	TypeBlob
)

// ---- A5 新类型的存储编码与精度边界 ----
// 行布局保持 [version i64][expireAt i64][字段...]，字段顺序与 meta.Fields 一致：
//
//	DATE      8 字节 int64：UTC 零点起的 Unix 纳秒。写入时把时间截断到
//	          当日 00:00:00（UTC），所以同一天的任意时刻都编码为同一个值。
//	DATETIME  8 字节 int64：UTC Unix 纳秒。可表示区间约为
//	          1677-09-21T00:12:44Z ~ 2262-04-11T23:47:16Z（int64 纳秒上限）。
//	DECIMAL   8 字节 int64 定点：值 = 存储值 / 10^scale，scale 取 Field.Len。
//	          受 int64 约束，绝对值上限约为 9.22e18 / 10^scale：
//	            scale 0 → ±9.22e18（18~19 位整数）
//	            scale 2 → ±9.22e16（16 位整数 + 2 位小数）
//	            scale 6 → ±9.22e12（12 位整数 + 6 位小数）
//	          scale 最大 9；文本解析按"四舍五入到 scale、远离零"处理多余小数位。
//	          溢出（超出 int64）在写入校验阶段直接报错，绝不静默回绕。
//	TEXT/BLOB 4 字节 uint32 长度 + 原始字节，长度上限 0xFFFFFFFE 字节
//	          （约 4GB，远高于 VARCHAR 的 65534）；0xFFFFFFFF 为 NULL 哨兵，
//	          与 VARCHAR 的 0xFFFF 哨兵同理。
//
// 有意降级（见最终报告）：DATE/DATETIME/DECIMAL 解码后返回 int64
// （DATE/DATETIME=纳秒时间戳，DECIMAL=放大 10^scale 的整数），不会格式化成
// 'YYYY-MM-DD' / '12.34'。这样比较、索引键、ORDER BY 都能复用同一套整数内核
// 且保持全序；格式化输出留待后续阶段，避免在扫描热路径上每行重新解析。
// 另：DATE/DATETIME/DECIMAL 没有 NULL 语义（NULL 落 0），与既有 INT 一致；
// 只有 VARCHAR/TEXT/BLOB 用哨兵长度区分 NULL。

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
	// WALInsertBatch 一次写入多行（多行 INSERT / 批量导入）。
	// 布局：[cmd][varint tableID][varint txnID][varint count]([varint pk][varint len][data])*count
	// 相比 count 条 WALInsert，省掉每条的记录头与一次 appendWAL 通道交互。
	WALInsertBatch = 7
)

// 单写者 WAL 队列：所有 WAL 记录经此 FIFO 通道交给唯一 writer goroutine 串行落盘，
// 消除热路径上的全局互斥锁与锁唤醒风暴；fsync 模式下由 writer 对并发写者做组提交批量落盘。
// buffer 足够大以吸收并发突发，避免发送者频繁阻塞。
var walCh = make(chan walEntry, 16384)

// walBufPool 复用 WAL 记录缓冲：分层池化，按 size class 减少内存浪费。
var (
	walBufPoolSmall = sync.Pool{New: func() interface{} { return make([]byte, 0, 128) }}
	walBufPoolMed   = sync.Pool{New: func() interface{} { return make([]byte, 0, 512) }}
	walBufPoolLarge = sync.Pool{New: func() interface{} { return make([]byte, 0, 2048) }}
)

func walBufGet(size int) []byte {
	switch {
	case size <= 128:
		return walBufPoolSmall.Get().([]byte)[:0]
	case size <= 512:
		return walBufPoolMed.Get().([]byte)[:0]
	default:
		buf := walBufPoolLarge.Get().([]byte)
		if cap(buf) < size {
			return make([]byte, 0, size)
		}
		return buf[:0]
	}
}

func walBufPut(buf []byte) {
	c := cap(buf)
	switch {
	case c <= 128:
		walBufPoolSmall.Put(buf[:0])
	case c <= 512:
		walBufPoolMed.Put(buf[:0])
	case c <= 2048:
		walBufPoolLarge.Put(buf[:0])
	}
}

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

// 关闭标记：Close 后 appendWAL/flushWAL 直接返回错误，避免阻塞在已退出的 writer 上
var walClosed atomic.Bool

// walCloseCh 关闭哨兵：Close 时关闭，阻塞发送通过 select 监听它避免永久挂起
var walCloseCh = make(chan struct{})

// errWALClosed 表示 WAL 已关闭或不可用：写入必须让调用方看到失败，
// 而不是返回 nil（旧实现会让关闭窗口内的写入"成功"但不落盘）。
var errWALClosed = errors.New("wal is closed")

// walFault 记录 writer 遇到的首个不可恢复 IO 错误（磁盘满 / EIO / EBADF）。
// 一旦置位，后续 appendWAL 直接返回该错误，避免"提交成功但没落盘"。
var (
	walFaultMu sync.Mutex
	walFault   error
)

func walFail(err error) {
	if err == nil {
		return
	}
	walFaultMu.Lock()
	if walFault == nil {
		walFault = err
	}
	walFaultMu.Unlock()
	logf(LOG_ERR, "wal write failed, entering read-only fault state: %v", err)
}

func walFaultErr() error {
	walFaultMu.Lock()
	defer walFaultMu.Unlock()
	return walFault
}

func walFaultReset() {
	walFaultMu.Lock()
	walFault = nil
	walFaultMu.Unlock()
}

// 全局统计钩子：appendWAL 是包级函数，借该指针上报磁盘写入速率
var statsHook *Stats

// rowVersionSeq 行版本号全局序列：每次写入取一个新值，保证同一行的并发写
// 拿到不同版本，提交时能识别出"我写出的那一版是否被覆盖"。
var rowVersionSeq atomic.Int64

func nextRowVersion() int64 { return rowVersionSeq.Add(1) }

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
		// 读失败时把游标推到末尾：调用方据此判断"记录在文件尾部被截断"
		// （WAL 重放的 tail() 依赖 pos >= len(buf)），否则残缺记录会被当成致命错误。
		*pos = len(b)
		return 0, false
	}
	v := uint32(b[*pos])<<24 | uint32(b[*pos+1])<<16 | uint32(b[*pos+2])<<8 | uint32(b[*pos+3])
	*pos += 4
	return v, true
}
func readU64(b []byte, pos *int) (uint64, bool) {
	if *pos+8 > len(b) {
		*pos = len(b)
		return 0, false
	}
	v := uint64(b[*pos])<<56 | uint64(b[*pos+1])<<48 | uint64(b[*pos+2])<<40 | uint64(b[*pos+3])<<32 |
		uint64(b[*pos+4])<<24 | uint64(b[*pos+5])<<16 | uint64(b[*pos+6])<<8 | uint64(b[*pos+7])
	*pos += 8
	return v, true
}
func readI64(b []byte, pos *int) (int64, bool) {
	if *pos+8 > len(b) {
		*pos = len(b)
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

// takeVarBytes 读取 varint 长度前缀的字节串并返回零拷贝切片。
// 长度一律用 uint64 与剩余字节数比较：int(n) 在 64 位机上可能溢出成负数，
// 从而绕过 `pos+int(n) > len(buf)` 的检查并让切片越界 panic（损坏 WAL 触发）。
func takeVarBytes(buf []byte, pos *int) ([]byte, bool) {
	n, ok := readVarUint(buf, pos)
	if !ok {
		return nil, false
	}
	if n > uint64(len(buf)-*pos) {
		// 载荷被截断：同样推游标到末尾，让 tail() 识别为崩溃残留
		*pos = len(buf)
		return nil, false
	}
	out := buf[*pos : *pos+int(n)]
	*pos += int(n)
	return out, true
}

// takeVarString 同 takeVarBytes，返回字符串。
func takeVarString(buf []byte, pos *int) (string, bool) {
	b, ok := takeVarBytes(buf, pos)
	if !ok {
		return "", false
	}
	return string(b), true
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

	// fieldNamesCache SELECT * 列头（字段名列表）的惰性缓存。meta 发布后
	// 字段集不可变，缓存内容恒等；并发首查重复计算无害（原子发布）。
	fieldNamesCache atomic.Pointer[[]string]
}

// fieldNames 返回字段名列表（SELECT * 列头）。调用方只读，不得改写。
func (m *TableMeta) fieldNames() []string {
	if p := m.fieldNamesCache.Load(); p != nil {
		return *p
	}
	names := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		names[i] = f.Name
	}
	m.fieldNamesCache.Store(&names)
	return names
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
		case TypeInt, TypeDate, TypeDatetime, TypeDecimal:
			size += 8
		case TypeVarchar:
			s, _ := v.(string)
			size += 2 + len(s)
		case TypeText, TypeBlob:
			size += 4 + len(valueString(v))
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
			// 兼容 int/uint/float/字符串：表达式求值常产出 float64，
			// 旧实现只认 int64，其它类型静默写成 0（数据损坏）。
			buf = appendI64(buf, coerceInt(v))
		case TypeDate, TypeDatetime:
			buf = appendI64(buf, coerceTime(v, f.Type == TypeDate))
		case TypeDecimal:
			buf = appendI64(buf, coerceDecimal(v, f.Len))
		case TypeVarchar:
			if v == nil {
				// NULL 标记：合法长度上限 65534，0xFFFF 借作 NULL 哨兵，
				// 否则 NULL 与空串在存储层无法区分（MySQL 客户端会读到 ""）。
				buf = appendU16(buf, varcharNullMarker)
				continue
			}
			s, _ := v.(string)
			if len(s) > maxVarcharLen {
				s = s[:maxVarcharLen] // 写入路径已校验，这里只是防御性截断
			}
			buf = appendU16(buf, uint16(len(s)))
			buf = append(buf, s...)
		case TypeText, TypeBlob:
			if v == nil {
				buf = appendU32(buf, textNullMarker)
				continue
			}
			s := valueString(v)
			if len(s) > maxTextLen {
				s = s[:maxTextLen] // 写入路径已校验，这里只是防御性截断
			}
			buf = appendU32(buf, uint32(len(s)))
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

// VARCHAR 编码常量：长度前缀 2 字节，因此合法长度上限 65534；
// 0xFFFF 保留为 NULL 哨兵（旧格式不会产生该值）。
const (
	maxVarcharLen     = 0xFFFE
	varcharNullMarker = 0xFFFF
)

// TEXT/BLOB 编码常量：长度前缀 4 字节，NULL 哨兵用全 1。
const (
	maxTextLen     = 0xFFFFFFFE
	textNullMarker = 0xFFFFFFFF
)

// decimalMaxScale DECIMAL 支持的最大小数位。取 9 是为了让 scale=9 时仍有
// 9 位整数位（int64 上限 9.22e18 / 1e9 ≈ 9.22e9），覆盖面足够而实现简单。
const decimalMaxScale = 9

// valueString 把写入值转成 TEXT/BLOB 的字节序列。SQL 字面量是 string，
// 二进制客户端/表达式可能给 []byte 或数字——后者按 MySQL 的隐式转换处理。
func valueString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return toStr(v)
}

// scalePow10 返回 10^scale（scale 已在 DDL 阶段限制在 0..decimalMaxScale）。
func scalePow10(scale int) int64 {
	p := int64(1)
	for i := 0; i < scale; i++ {
		p *= 10
	}
	return p
}

// mulPow10 n * 10^scale，溢出返回 ok=false。
func mulPow10(n int64, scale int) (int64, bool) {
	p := scalePow10(scale)
	if n > 0 && n > math.MaxInt64/p {
		return 0, false
	}
	if n < 0 && n < math.MinInt64/p {
		return 0, false
	}
	return n * p, true
}

// parseDecimalText 把十进制文本精确转成 scale 位定点整数。
// 多余小数位按"四舍五入、远离零"处理（与 MySQL 一致）；任何溢出/非法字符
// 返回 ok=false，由调用方报错而不是写进一个错误的值。
// 不支持科学计数法（'1e3' 会被拒绝）——DECIMAL 语义上不接受它。
func parseDecimalText(s string, scale int) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return 0, false
	}
	var ip int64
	if intPart != "" {
		n, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return 0, false
		}
		ip = n
	}
	var frac int64
	for i := 0; i < scale; i++ {
		frac *= 10
		if i < len(fracPart) {
			d := fracPart[i]
			if d < '0' || d > '9' {
				return 0, false
			}
			frac += int64(d - '0')
		}
	}
	for i := scale; i < len(fracPart); i++ {
		if d := fracPart[i]; d < '0' || d > '9' {
			return 0, false
		}
	}
	pow := scalePow10(scale)
	if ip > (math.MaxInt64-frac)/pow {
		return 0, false
	}
	val := ip*pow + frac
	if len(fracPart) > scale && fracPart[scale] >= '5' {
		if val == math.MaxInt64 {
			return 0, false
		}
		val++
	}
	if neg {
		val = -val
	}
	return val, true
}

// decimalFromFloat 把浮点值转成 scale 位定点整数（四舍五入）。
// 注意 float64 只有 ~15~17 位有效数字：超过该精度的十进制字面量在进入这里
// 之前就已经失真，精确十进制请用字符串字面量写入。
func decimalFromFloat(f float64, scale int) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	scaled := math.Round(f * float64(scalePow10(scale)))
	// float64(math.MaxInt64) 恰为 2^63，用 >= 判定避免边界上的未定义转换。
	if scaled >= 9223372036854775808.0 || scaled < -9223372036854775808.0 {
		return 0, false
	}
	return int64(scaled), true
}

// coerceDecimalScaled 把任意写入值折算成 DECIMAL 的定点 int64。
// nil 落 0（与 INT 的 NULL 语义一致，见类型注释）。
func coerceDecimalScaled(v interface{}, scale int) (int64, bool) {
	if scale < 0 || scale > decimalMaxScale {
		return 0, false
	}
	switch x := v.(type) {
	case nil:
		return 0, true
	case int64:
		return mulPow10(x, scale)
	case int:
		return mulPow10(int64(x), scale)
	case int32:
		return mulPow10(int64(x), scale)
	case float64:
		return decimalFromFloat(x, scale)
	case float32:
		return decimalFromFloat(float64(x), scale)
	case string:
		return parseDecimalText(x, scale)
	case []byte:
		return parseDecimalText(string(x), scale)
	case bool:
		if x {
			return scalePow10(scale), true
		}
		return 0, true
	}
	return 0, false
}

// coerceDecimal 同 coerceDecimalScaled，但失败时静默返回 0。
// 仅用于"校验已通过"的编码路径（validateRow* 先跑一遍并报错）。
func coerceDecimal(v interface{}, scale int) int64 {
	n, _ := coerceDecimalScaled(v, scale)
	return n
}

// coerceTimeNanos 把写入值折算成 Unix 纳秒：
//   - int64/int/float64 视为已经是 Unix 纳秒（二进制协议/程序化写入）；
//   - 字符串按 parseSQLTime 解析（'2024-01-02'、'2024-01-02 03:04:05'、
//     纯数字字符串按其 Unix 秒解释——沿用 parseSQLTime 的既有行为）；
//   - dateOnly=true 时截断到 UTC 当日 00:00:00，供 DATE 使用。
//
// 时区约定：无时区后缀的日期文本按 UTC 解析（time.Parse 的默认行为），
// 存储层不保存时区信息。
func coerceTimeNanos(v interface{}, dateOnly bool) (int64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case float64:
		return int64(x), true
	case float32:
		return int64(x), true
	case time.Time:
		return timeNanos(x, dateOnly), true
	case string:
		t, ok := parseSQLTime(x)
		if !ok {
			return 0, false
		}
		return timeNanos(t, dateOnly), true
	case []byte:
		return coerceTimeNanos(string(x), dateOnly)
	}
	return 0, false
}

// timeNanos 转 Unix 纳秒；DATE 截断到 UTC 零点。
func timeNanos(t time.Time, dateOnly bool) int64 {
	if dateOnly {
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	return t.UnixNano()
}

func coerceTime(v interface{}, dateOnly bool) int64 {
	n, _ := coerceTimeNanos(v, dateOnly)
	return n
}

// encodeOrderedInt64 符号位翻转的 8 字节大端编码：负数也能按字节序正确排序。
// 新类型（DATE/DATETIME/DECIMAL）统一使用；旧的 TypeInt 编码保持不变，
// 以免改变既有索引数据里已写入的键（INT 负数范围下推的缺陷另案处理）。
func encodeOrderedInt64(n int64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n)^(1<<63))
	return string(b[:])
}

func decodeOrderedInt64(key string) int64 {
	if len(key) != 8 {
		return 0
	}
	var b [8]byte
	copy(b[:], key)
	return int64(binary.BigEndian.Uint64(b[:]) ^ (1 << 63))
}

// encodeRowOrdered 按 meta.Fields 顺序直接编码，不做 map 查找（SQL 位置参数快速路径）。
func encodeRowOrdered(meta *TableMeta, values []interface{}, version int64, expireAt int64) []byte {
	size := 16
	for i, f := range meta.Fields {
		var s string
		switch f.Type {
		case TypeInt, TypeDate, TypeDatetime, TypeDecimal:
			size += 8
		case TypeVarchar:
			if i < len(values) {
				s, _ = values[i].(string)
			}
			size += 2 + len(s)
		case TypeText, TypeBlob:
			if i < len(values) {
				s = valueString(values[i])
			}
			size += 4 + len(s)
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
			buf = appendI64(buf, coerceInt(v))
		case TypeDate, TypeDatetime:
			buf = appendI64(buf, coerceTime(v, f.Type == TypeDate))
		case TypeDecimal:
			buf = appendI64(buf, coerceDecimal(v, f.Len))
		case TypeVarchar:
			if v == nil {
				buf = appendU16(buf, varcharNullMarker)
				continue
			}
			s, _ := v.(string)
			if len(s) > maxVarcharLen {
				s = s[:maxVarcharLen]
			}
			buf = appendU16(buf, uint16(len(s)))
			buf = append(buf, s...)
		case TypeText, TypeBlob:
			if v == nil {
				buf = appendU32(buf, textNullMarker)
				continue
			}
			s := valueString(v)
			if len(s) > maxTextLen {
				s = s[:maxTextLen]
			}
			buf = appendU32(buf, uint32(len(s)))
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
		case TypeDate, TypeDatetime, TypeDecimal:
			// 三者都是定长 int64；DATE=UTC 纳秒、DATETIME=UTC 纳秒、
			// DECIMAL=放大 10^scale 的定点整数（见文件头的类型说明）。
			v, ok := readI64(data, &pos)
			if ok {
				row[f.Name] = v
			} else {
				row[f.Name] = int64(0)
			}
		case TypeVarchar:
			l, ok := readU16(data, &pos)
			if ok && l == varcharNullMarker {
				row[f.Name] = nil
			} else if ok && pos+int(l) <= len(data) {
				// 树内行字节不可变（COW），别名零拷贝即可；字符串在 Go 中不可变，
				// 调用方无法改写这些字节（回归 C：每行 varchar 省一次堆分配）。
				row[f.Name] = unsafeString(data[pos : pos+int(l)])
				pos += int(l)
			} else {
				row[f.Name] = ""
			}
		case TypeText, TypeBlob:
			l, ok := readU32(data, &pos)
			if ok && l == textNullMarker {
				row[f.Name] = nil
			} else if ok && int(l) <= len(data)-pos {
				row[f.Name] = unsafeString(data[pos : pos+int(l)])
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
		case TypeDate, TypeDatetime, TypeDecimal:
			v, _ := readI64(data, &pos)
			into[i] = v
		case TypeVarchar:
			l, _ := readU16(data, &pos)
			if l == varcharNullMarker {
				into[i] = nil
			} else if l > 0 && pos+int(l) <= len(data) {
				into[i] = unsafeString(data[pos : pos+int(l)])
				pos += int(l)
			} else {
				into[i] = ""
			}
		case TypeText, TypeBlob:
			l, _ := readU32(data, &pos)
			if l == textNullMarker {
				into[i] = nil
			} else if l > 0 && int(l) <= len(data)-pos {
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

// rowArena 结果行的连续后备存储：一批行共用一个 []interface{} 大块，
// 每行按 stride 切分，省去每行一次独立 make。块按 4 行起步几何翻倍
// （小结果集不浪费，大扫描摊薄到对数次分配）；块耗尽换新块——已发出的行
// 仍引用旧块，别名天然安全。
// 注意：同一批的行共享后备数组且各行 cap 延伸到块尾，返回的行只读使用，
// 不得对行切片 append（len<cap 时会写穿到下一行的槽位）。
type rowArena struct {
	backing []interface{}
	stride  int
}

func newRowArena(stride int) *rowArena {
	if stride < 1 {
		stride = 1
	}
	return &rowArena{backing: make([]interface{}, 0, stride*4), stride: stride}
}

func (a *rowArena) row() []interface{} {
	if len(a.backing)+a.stride > cap(a.backing) {
		n := (cap(a.backing) / a.stride) * 2
		if n < 4 {
			n = 4
		}
		a.backing = make([]interface{}, 0, a.stride*n)
	}
	start := len(a.backing)
	a.backing = a.backing[:start+a.stride]
	return a.backing[start : start+a.stride]
}

// skipField 跳过行编码中的一个字段，用于窄投影扫描：不把未选中列解码成
// interface{}，只推进读取位置。行格式由 encodeRow 写出，字段顺序与 meta 一致。
func skipField(data []byte, pos *int, typ FieldType) {
	switch typ {
	case TypeInt, TypeDate, TypeDatetime, TypeDecimal:
		*pos += 8
	case TypeVarchar:
		if *pos+2 > len(data) {
			*pos = len(data)
			return
		}
		l, _ := readU16(data, pos)
		if l != varcharNullMarker {
			*pos += int(l)
		}
	case TypeText, TypeBlob:
		if *pos+4 > len(data) {
			*pos = len(data)
			return
		}
		l, _ := readU32(data, pos)
		if l != textNullMarker {
			*pos += int(l)
		}
	case TypeBool:
		*pos++
	}
}

// decodeRowProjected 只解码 fieldToOut 指向的列，输出顺序由 fieldToOut 的
// 值决定（-1 表示跳过）。fieldToOut 为 nil 时等价于 decodeRowFlat。
// 这样 SELECT id FROM ... 不再解码整行 varchar，也省掉二次投影和每行
// 一个完整 flat 行缓冲。
func decodeRowProjected(meta *TableMeta, data []byte, into []interface{}, fieldToOut []int) []interface{} {
	if fieldToOut == nil {
		return decodeRowFlat(meta, data, into)
	}
	pos := 16
	for i, f := range meta.Fields {
		out := fieldToOut[i]
		if out < 0 || out >= len(into) {
			skipField(data, &pos, f.Type)
			continue
		}
		switch f.Type {
		case TypeInt:
			v, _ := readI64(data, &pos)
			into[out] = v
		case TypeDate, TypeDatetime, TypeDecimal:
			v, _ := readI64(data, &pos)
			into[out] = v
		case TypeVarchar:
			l, _ := readU16(data, &pos)
			if l == varcharNullMarker {
				into[out] = nil
			} else if l > 0 && pos+int(l) <= len(data) {
				into[out] = unsafeString(data[pos : pos+int(l)])
				pos += int(l)
			} else {
				into[out] = ""
			}
		case TypeText, TypeBlob:
			l, _ := readU32(data, &pos)
			if l == textNullMarker {
				into[out] = nil
			} else if l > 0 && int(l) <= len(data)-pos {
				into[out] = unsafeString(data[pos : pos+int(l)])
				pos += int(l)
			} else {
				into[out] = ""
			}
		case TypeBool:
			if pos < len(data) {
				into[out] = data[pos] == 1
				pos++
			} else {
				into[out] = false
			}
		}
	}
	return into
}

// selectRowsProjected 无 WHERE 条件的范围/全表扫描：按 output 宽度分片，
// 只解码投影列，不建 map。过期行收集后锁外清理（与 SelectR 一致）。
// limit<0 表示不限制；fieldToOut 为 nil 表示 SELECT * 的恒等投影。
func (t *Table) selectRowsProjected(minKey, maxKey *int64, limit int, fieldToOut []int, outWidth int) [][]interface{} {
	// 预分配上限钳制：LIMIT 值客户端可控，直接用于 make 预分配会被
	// LIMIT 999999999 之类一次打爆内存（fatal OOM）；大 limit 交给 append 摊还扩容。
	capHint := 16
	if limit > 0 && limit <= 4096 {
		capHint = limit
	}
	if outWidth < 1 {
		outWidth = 1
	}
	rows := make([][]interface{}, 0, capHint)
	meta := t.Meta()
	arena := newRowArena(outWidth)
	var expired []int64
	now := time.Now().UnixNano()
	scan := func(key int64, value []byte) bool {
		if e := decodeExpireAt(value); e > 0 && now > e {
			expired = append(expired, key)
			return true
		}
		rows = append(rows, decodeRowProjected(meta, value, arena.row(), fieldToOut))
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
	// 修复: 使用写锁保护整个 save 操作
	ps.mu.Lock()
	defer ps.mu.Unlock()

	// 复制数据，减少持锁时间
	dataCopy := make(map[string]map[string]byte)
	for k, v := range ps.data {
		inner := make(map[string]byte, len(v))
		for uk, uv := range v {
			inner[uk] = uv
		}
		dataCopy[k] = inner
	}

	tmp := ps.filePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		// 如果写入失败，清理临时文件
		if err != nil {
			os.Remove(tmp)
		}
	}()

	enc := json.NewEncoder(f)
	if err := enc.Encode(dataCopy); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
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

// GrantNoSave 仅内存授权（压测临时用户用）：不落盘。
func (ps *PrivilegeStore) GrantNoSave(user, table string, perm byte) {
	ps.mu.Lock()
	if ps.data[table] == nil {
		ps.data[table] = make(map[string]byte)
	}
	ps.data[table][user] |= perm
	ps.mu.Unlock()
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

// DropTable 清除某张表的全部授权。DROP TABLE 后必须调用，否则同名表重建时
// 旧授权仍然有效（权限记录以表名为键，重建即"继承"）。
func (ps *PrivilegeStore) DropTable(table string) error {
	ps.mu.Lock()
	if _, ok := ps.data[table]; !ok {
		ps.mu.Unlock()
		return nil
	}
	delete(ps.data, table)
	ps.mu.Unlock()
	return ps.save()
}

// SnapshotTable 返回某张表的授权副本（user -> perm），供表重建后恢复授权。
func (ps *PrivilegeStore) SnapshotTable(table string) map[string]byte {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	m := ps.data[table]
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]byte, len(m))
	for u, p := range m {
		out[u] = p
	}
	return out
}

// ==================== 表结构 ====================

// idxTreeMap 二级索引集合。作为不可变快照整体发布（写时复制）：
// 读者原子加载一次即可安全访问；CREATE/DROP INDEX 构建新快照后整体替换。
// 索引键为 string（string 的 < 即逐字节字典序，与 bytes.Compare 排序一致）。
type idxTreeMap map[string]*RBTree[string, []int64]

// Table 的 meta 与 idxTrees 均为写时复制快照：读者无锁，写者整体替换后原子发布。
// TableMeta（含 Fields/Indexes）一旦发布即视为不可变；ALTER/CREATE INDEX 必须构建新副本。
type Table struct {
	meta     atomic.Pointer[TableMeta]
	pkTree   *RBTree[int64, []byte]
	idxTrees atomic.Pointer[idxTreeMap]
	walFile  *os.File
	tableID  uint32
	// migrating：表结构重建中，拒绝写入（否则扫描之后写入的行会在新表里丢失）
	migrating atomic.Bool
	// autoPK：自增主键的下一个可用值（单调递增，删除不回收）。
	// INSERT 未给出主键（或给出 0/NULL）时由引擎分配，避免多客户端各自
	// "MAX(pk)+1" 播种导致主键碰撞（实测并发下 37% 插入失败）。
	autoPK atomic.Int64
	// ttlRows 记录本表是否出现过带过期时间的行（粘性标记，只置位不清零）。
	// 覆盖索引要跳过回表，而过期时间编码在行字节里；只要表里可能有过期行，
	// 就不能省掉这次行读取，否则会把已过期行当有效结果返回。
	ttlRows atomic.Bool
}

// allocPK 分配一个自增主键：单调递增、绝不复用。
// autoPK 保存的是"下一个可用的主键"（初始 0 表示还没分配过）。
func (t *Table) allocPK() int64 {
	for {
		cur := t.autoPK.Load()
		if cur < 1 {
			// 第一次分配：抢到 1，并把计数器抬到 2（下一个可用值）
			if t.autoPK.CompareAndSwap(cur, 2) {
				return 1
			}
			continue
		}
		if t.autoPK.CompareAndSwap(cur, cur+1) {
			return cur
		}
	}
}

// notePK 抬高自增计数器：显式插入与 WAL 重放都要调用，
// 保证自增不会撞上历史主键（重启后从 max(pk)+1 继续）。
func (t *Table) notePK(pk int64) {
	for {
		cur := t.autoPK.Load()
		if pk < cur {
			return
		}
		if t.autoPK.CompareAndSwap(cur, pk+1) {
			return
		}
	}
}

func (t *Table) Meta() *TableMeta     { return t.meta.Load() }
func (t *Table) setMeta(m *TableMeta) { t.meta.Store(m) }

// indexes 返回当前二级索引快照。快照本身不可变，内部各 RBTree 自身线程安全。
func (t *Table) indexes() idxTreeMap { return *t.idxTrees.Load() }
func (t *Table) storeIndexes(m idxTreeMap) {
	t.idxTrees.Store(&m)
}

// cloneMeta 深拷贝表元数据（可选替换表名），供 ALTER / RENAME / CREATE INDEX 写时复制。
func cloneMeta(src *TableMeta, newName string) *TableMeta {
	m := &TableMeta{
		Name:    newName,
		PK:      src.PK,
		Fields:  make([]Field, len(src.Fields)),
		Indexes: make(map[string]string, len(src.Indexes)),
	}
	copy(m.Fields, src.Fields)
	for k, v := range src.Indexes {
		m.Indexes[k] = v
	}
	return m
}

func NewTable(meta *TableMeta, walFile *os.File) *Table {
	// NewTable 可独立于 NewDB 使用（如测试直接建表）：注册表未初始化时自动重置，
	// 避免依赖调用方先建库的隐式顺序。
	if walRegByName == nil {
		walRegReset()
	}
	idx := make(idxTreeMap, len(meta.Indexes))
	for idxName := range meta.Indexes {
		idx[idxName] = newStringKeyTree[[]int64]()
	}
	t := &Table{
		pkTree:  newIntKeyTree[[]byte](),
		walFile: walFile,
		tableID: walRegRegister(meta.Name),
	}
	t.meta.Store(meta)
	t.idxTrees.Store(&idx)
	return t
}

// encodeIndexValue 将索引列值编码为可排序的索引键。
// int64 用大端 8 字节保证数值序 == 字节序；string 返回**折叠规范形**
// （utf8mb4_general_ci）：WHERE name='abc' 走索引时也要能命中 'ABC'，
// 所以索引键必须与比较语义使用同一套折叠，二者缺一不可。
//
// 类型相关（列类型从 meta 查，调用方无需传）：
//   - TypeInt：保持历史的大端编码（负数不保序，属于既有缺陷，另案处理）；
//   - DATE/DATETIME/DECIMAL：符号位翻转的大端编码，负数也正确保序；
//   - VARCHAR/TEXT/BLOB：foldKey 折叠；BLOB 也折叠（等值语义与 TEXT 一致，
//     二进制原样相等必然折叠后相等，不影响正确性，只放宽了大小写）；
//   - BOOL：单字节 0/1。
func (t *Table) encodeIndexValue(fieldName string, val interface{}) string {
	meta := t.Meta()
	ftype := TypeVarchar
	scale := 0
	if fi := meta.fieldIndexOf(fieldName); fi >= 0 {
		ftype = meta.Fields[fi].Type
		scale = meta.Fields[fi].Len
	}
	switch ftype {
	case TypeInt:
		switch v := val.(type) {
		case int64:
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(v))
			return string(b[:])
		}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(coerceInt(val)))
		return string(b[:])
	case TypeDate, TypeDatetime:
		return encodeOrderedInt64(coerceTime(val, ftype == TypeDate))
	case TypeDecimal:
		return encodeOrderedInt64(coerceDecimal(val, scale))
	case TypeBool:
		if b, ok := val.(bool); ok && b {
			return "\x01"
		}
		return "\x00"
	case TypeVarchar, TypeText, TypeBlob:
		s := valueString(val)
		return foldKey(s)
	}
	return ""
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
func (t *Table) indexAddPK(idxName string, idxKey string, pk int64) {
	tree, ok := t.indexes()[idxName]
	if !ok || tree == nil {
		return
	}
	list, _ := tree.Get(idxKey)
	newList := make([]int64, len(list), len(list)+1)
	copy(newList, list)
	newList = append(newList, pk)
	tree.Set(idxKey, newList)
}

// indexRemovePK 从索引 key 的倒排列表移除 pk。只剩一个元素时直接删除 key。
func (t *Table) indexRemovePK(idxName string, idxKey string, pk int64) {
	tree, ok := t.indexes()[idxName]
	if !ok || tree == nil {
		return
	}
	list, ok := tree.Get(idxKey)
	if !ok {
		return
	}
	if len(list) <= 1 {
		tree.Delete(idxKey)
		return
	}
	newList := make([]int64, 0, len(list)-1)
	for _, id := range list {
		if id != pk {
			newList = append(newList, id)
		}
	}
	if len(newList) == 0 {
		tree.Delete(idxKey)
		return
	}
	tree.Set(idxKey, newList)
}

// ---- 重放专用：索引原地更新（无并发读者，免去写时复制的列表拷贝）----

// indexAddPKMutable 重放期把 pk 追加到倒排列表。append 可直接复用尾部空闲容量，
// 且此时没有任何读者持有旧切片，因此无需 COW 复制整条列表。
func (t *Table) indexAddPKMutable(idxName string, idxKey string, pk int64) {
	tree, ok := t.indexes()[idxName]
	if !ok || tree == nil {
		return
	}
	list, _ := tree.Get(idxKey)
	tree.SetMutable(idxKey, append(list, pk))
}

// indexRemovePKMutable 重放期从倒排列表移除 pk。
func (t *Table) indexRemovePKMutable(idxName string, idxKey string, pk int64) {
	tree, ok := t.indexes()[idxName]
	if !ok || tree == nil {
		return
	}
	list, ok := tree.Get(idxKey)
	if !ok {
		return
	}
	if len(list) <= 1 {
		tree.DeleteMutable(idxKey)
		return
	}
	// 原地压缩：把不等于 pk 的元素前移，避免额外分配
	n := 0
	for _, id := range list {
		if id != pk {
			list[n] = id
			n++
		}
	}
	if n == 0 {
		tree.DeleteMutable(idxKey)
		return
	}
	tree.SetMutable(idxKey, list[:n])
}

func (t *Table) updateIndexMutable(fieldName, idxName string, oldVal, newVal interface{}, pk int64) {
	if oldVal == newVal {
		return
	}
	if oldVal != nil {
		t.indexRemovePKMutable(idxName, t.encodeIndexValue(fieldName, oldVal), pk)
	}
	if newVal != nil {
		t.indexAddPKMutable(idxName, t.encodeIndexValue(fieldName, newVal), pk)
	}
}

// ==================== 二级索引查询下推 / 覆盖索引 ====================
// 索引键编码是保序的（int64 → 大端 8 字节，string → 原串），因此范围条件可以
// 直接换算成索引树上的键区间，用有序扫描收窄候选，而不是全表逐行求值。

// indexNameForField 返回该字段上的二级索引名。
func (t *Table) indexNameForField(field string) (string, bool) {
	meta := t.Meta()
	for idxName, f := range meta.Indexes {
		if f == field {
			return idxName, true
		}
	}
	for idxName, f := range meta.Indexes {
		if strings.EqualFold(f, field) {
			return idxName, true
		}
	}
	return "", false
}

// indexRangeBounds 把范围条件换算成二级索引键区间。
// 区间允许"偏宽"（例如 lt v 直接用到 v 为止，而不去算 v 的前驱）：命中后
// 仍由完整谓词精筛，所以宁可多取也不能漏。返回 ok=false 表示该条件无法下推。
//
// 注意：索引键的编码方式必须与这里换算出的边界一致——
//   - VARCHAR/TEXT/BLOB 的键是折叠形，边界也走 encodeIndexValue 折叠；
//   - DATE/DATETIME/DECIMAL 的键是符号位翻转的 int64，边界用同一编码；
//   - INT 保持历史编码（见 encodeIndexValue）。
func (t *Table) indexRangeBounds(fieldIdx int, f RangeCond) (lo, hi *string, ok bool) {
	meta := t.Meta()
	typ := meta.Fields[fieldIdx].Type
	scale := meta.Fields[fieldIdx].Len
	// orderedIntBounds 收拢 DATE/DATETIME/DECIMAL 的 int64 量纲：
	// 三者都折算成整数后与 encodeOrderedInt64 一一对应。
	orderedIntBounds := func(v int64, v2 int64, has2 bool) (*string, *string, bool) {
		switch f.Op {
		case "gt":
			if v == math.MaxInt64 {
				return nil, nil, false // 无可行上界，交给全表过滤
			}
			s := encodeOrderedInt64(v + 1)
			return &s, nil, true
		case "ge":
			s := encodeOrderedInt64(v)
			return &s, nil, true
		case "lt", "le":
			// 含端点扫描，等值行由精筛剔除（无法便捷求前驱）
			s := encodeOrderedInt64(v)
			return nil, &s, true
		case "between":
			if !has2 {
				return nil, nil, false
			}
			l, h := encodeOrderedInt64(v), encodeOrderedInt64(v2)
			return &l, &h, true
		}
		return nil, nil, false
	}
	switch typ {
	case TypeInt:
		v, okv := toInt64OK(f.Val)
		if !okv {
			return nil, nil, false
		}
		switch f.Op {
		case "gt":
			if v == math.MaxInt64 {
				return nil, nil, false
			}
			s := t.encodeIndexValue(f.Field, v+1)
			return &s, nil, true
		case "ge":
			s := t.encodeIndexValue(f.Field, v)
			return &s, nil, true
		case "lt", "le":
			s := t.encodeIndexValue(f.Field, v)
			return nil, &s, true
		case "between":
			v2, ok2 := toInt64OK(f.Val2)
			if !ok2 {
				return nil, nil, false
			}
			loS := t.encodeIndexValue(f.Field, v)
			hiS := t.encodeIndexValue(f.Field, v2)
			return &loS, &hiS, true
		}
	case TypeDate, TypeDatetime:
		dateOnly := typ == TypeDate
		v, okv := coerceTimeNanos(f.Val, dateOnly)
		if !okv {
			return nil, nil, false
		}
		if f.Op == "between" {
			v2, ok2 := coerceTimeNanos(f.Val2, dateOnly)
			if !ok2 {
				return nil, nil, false
			}
			return orderedIntBounds(v, v2, true)
		}
		return orderedIntBounds(v, 0, false)
	case TypeDecimal:
		v, okv := coerceDecimalScaled(f.Val, scale)
		if !okv {
			return nil, nil, false
		}
		if f.Op == "between" {
			v2, ok2 := coerceDecimalScaled(f.Val2, scale)
			if !ok2 {
				return nil, nil, false
			}
			return orderedIntBounds(v, v2, true)
		}
		return orderedIntBounds(v, 0, false)
	case TypeVarchar, TypeText, TypeBlob:
		switch f.Op {
		case "gt", "ge":
			s := t.encodeIndexValue(f.Field, f.Val)
			return &s, nil, true
		case "lt", "le":
			s := t.encodeIndexValue(f.Field, f.Val)
			return nil, &s, true
		case "between":
			l := t.encodeIndexValue(f.Field, f.Val)
			h := t.encodeIndexValue(f.Field, f.Val2)
			return &l, &h, true
		}
	}
	return nil, nil, false
}

// indexRangeCandidates 用二级索引的有序范围扫描求出候选行。
// 返回 (行, 是否采用索引)。候选过多时主动放弃并退回主键顺序扫描：
// 宽范围下"索引全扫 + 随机回表"反而比一次顺序扫描更慢。
func (t *Table) indexRangeCandidates(meta *TableMeta, scan *scanConds, filters []RangeCond, limit int, now int64) ([][]byte, bool) {
	const maxCandidates = 4096
	if len(filters) == 0 {
		return nil, false
	}
	pkIdx := meta.fieldIndexOf(meta.PK)
	type fieldBounds struct{ lo, hi *string }
	bounds := make([]*fieldBounds, len(meta.Fields))
	var order []int
	for i := range filters {
		fi := meta.fieldIndexOf(filters[i].Field)
		if fi < 0 || fi == pkIdx {
			continue // 主键范围已在调用方换算成 minKey/maxKey
		}
		lo, hi, ok := t.indexRangeBounds(fi, filters[i])
		if !ok {
			continue
		}
		b := bounds[fi]
		if b == nil {
			b = &fieldBounds{}
			bounds[fi] = b
			order = append(order, fi)
		}
		// 编码保序：取更紧的边界（下界取较大者，上界取较小者）
		if lo != nil && (b.lo == nil || *lo > *b.lo) {
			b.lo = lo
		}
		if hi != nil && (b.hi == nil || *hi < *b.hi) {
			b.hi = hi
		}
	}
	for _, fi := range order {
		idxName, ok := t.indexNameForField(meta.Fields[fi].Name)
		if !ok {
			continue
		}
		tree, ok := t.indexes()[idxName]
		if !ok || tree == nil {
			continue
		}
		b := bounds[fi]
		var pks []int64
		tooWide := false
		tree.scanRangeStop(b.lo, b.hi, func(_ string, v []int64) bool {
			if len(pks)+len(v) > maxCandidates {
				tooWide = true
				return false
			}
			pks = append(pks, v...)
			return true
		})
		if tooWide {
			return nil, false
		}
		// 索引按"索引列"有序给出 pk；上层期望主键序（与顺序扫描一致），
		// 因此这里统一排序后再回表，结果顺序保持不变。
		sort.Slice(pks, func(a, c int) bool { return pks[a] < pks[c] })
		out := make([][]byte, 0, len(pks))
		for _, pk := range pks {
			rowData, found := t.pkTree.Get(pk)
			if !found {
				continue
			}
			if e := decodeExpireAt(rowData); e > 0 && now > e {
				continue // 过期行不返回（后台 CleanExpired 负责清理）
			}
			if !scan.matchRow(meta, rowData) {
				continue
			}
			out = append(out, rowData)
			if limit >= 0 && len(out) >= limit {
				break
			}
		}
		return out, true
	}
	return nil, false
}

// coveringRows 当投影被二级索引完全覆盖时，直接从索引条目构造结果，
// 不再回表读取整行字节（省掉一次主键树查找 + 一次行解码）。
// 支持"该索引列上的等值条件"与"该列上的范围条件"两类。
//
// 安全前提（不满足即返回 ok=false 走常规回表路径）：
//   - 全部条件都落在同一个非主键列上，且该列有二级索引；
//   - 表内从未出现过 TTL 行——过期时间编码在行字节里，跳过回表就看不到它，
//     否则会把已过期行当有效结果返回；
//   - 投影列 ⊆ {索引列, 主键}，等值/范围的值类型与列类型一致；
//   - 索引列是字符串（VARCHAR/TEXT/BLOB）时，投影**不得包含该列自身**：
//     折叠索引键只保存小写规范形，拿它当结果值会改变数据。只投影主键
//     （如 SELECT id FROM t WHERE email='x'）仍可走覆盖索引。
func (t *Table) coveringRows(meta *TableMeta, conds map[string]interface{}, filters []RangeCond, outFields []int, width int, limit int) ([][]interface{}, bool) {
	if width <= 0 || outFields == nil || (len(conds) == 0 && len(filters) == 0) {
		return nil, false
	}
	if t.ttlRows.Load() {
		return nil, false
	}
	// 全部条件必须指向同一个字段（否则索引条目无法证明其他条件成立）
	var field string
	for k := range conds {
		if field == "" {
			field = k
		} else if !strings.EqualFold(field, k) {
			return nil, false
		}
	}
	for i := range filters {
		if field == "" {
			field = filters[i].Field
		} else if !strings.EqualFold(field, filters[i].Field) {
			return nil, false
		}
	}
	pkIdx := meta.fieldIndexOf(meta.PK)
	fi := meta.fieldIndexOf(field)
	if fi < 0 || fi == pkIdx {
		return nil, false
	}
	for _, of := range outFields {
		if of != fi && of != pkIdx {
			return nil, false
		}
	}
	idxName, ok := t.indexNameForField(meta.Fields[fi].Name)
	if !ok {
		return nil, false
	}
	tree, ok := t.indexes()[idxName]
	if !ok || tree == nil {
		return nil, false
	}
	scan := newScanConds(meta, conds, filters)
	typ := meta.Fields[fi].Type
	// 字符串列的索引键是折叠形，无法还原原始大小写。因此只有"投影不含该
	// 字符串列本身"时覆盖索引才安全：`SELECT id FROM t WHERE email='x'`
	// 只需主键，可以直接由索引产出；`SELECT email FROM ...` 必须回表。
	if typ == TypeVarchar || typ == TypeText || typ == TypeBlob {
		for _, of := range outFields {
			if of == fi {
				return nil, false
			}
		}
	}

	// emit 把一条索引条目展开成结果行；values 为该索引键对应的字段值。
	emit := func(val interface{}, pkList []int64, rows [][]interface{}) [][]interface{} {
		if limit >= 0 && len(rows)+len(pkList) > limit {
			pkList = pkList[:limit-len(rows)]
		}
		for _, pk := range pkList {
			row := make([]interface{}, width)
			for i, of := range outFields {
				if of == fi {
					row[i] = val
				} else {
					row[i] = pk
				}
			}
			rows = append(rows, row)
		}
		return rows
	}
	// 结果按主键序输出，与顺序扫描路径保持一致（无 ORDER BY 时也稳定）
	sortPKs := func(pks []int64) {
		sort.Slice(pks, func(a, c int) bool { return pks[a] < pks[c] })
	}

	// 等值：直接取索引条目
	if len(conds) == 1 && len(filters) == 0 {
		var val interface{}
		for _, v := range conds {
			val = v
		}
		// 索引键必须用**原始条件值**编码：DECIMAL 的 encodeIndexValue 会自己
		// 乘 10^scale，如果先折算再传进去会二次放大（查不到任何行）。
		idxKey := t.encodeIndexValue(field, val)
		switch typ {
		case TypeInt:
			if _, ok := val.(int64); !ok {
				return nil, false
			}
		case TypeDate, TypeDatetime:
			n, okn := coerceTimeNanos(val, typ == TypeDate)
			if !okn {
				return nil, false
			}
			val = n
		case TypeDecimal:
			n, okn := coerceDecimalScaled(val, meta.Fields[fi].Len)
			if !okn {
				return nil, false
			}
			val = n
		case TypeBool:
			if _, ok := val.(bool); !ok {
				return nil, false
			}
		}
		pkList, found := tree.Get(idxKey)
		if !found {
			return [][]interface{}{}, true
		}
		pkList = append([]int64(nil), pkList...) // 不改树内列表
		sortPKs(pkList)
		return emit(val, pkList, make([][]interface{}, 0, len(pkList))), true
	}

	// 范围：合并该字段上的所有边界（数值类键保序，字符串键按折叠形比较）。
	// 字符串列走到这里时投影必然不含该列（上面已判定），因此只需匹配索引键、
	// 结果值只取主键，折叠形不会泄漏到输出。
	switch typ {
	case TypeInt, TypeDate, TypeDatetime, TypeDecimal, TypeVarchar, TypeText, TypeBlob:
	default:
		return nil, false
	}
	var lo, hi *string
	for i := range filters {
		loI, hiI, okB := t.indexRangeBounds(fi, filters[i])
		if !okB {
			return nil, false // 有无法下推的条件，交给回表路径
		}
		if loI != nil && (lo == nil || *loI > *lo) {
			lo = loI
		}
		if hiI != nil && (hi == nil || *hiI < *hi) {
			hi = hiI
		}
	}
	// 同一索引键可能对应多个主键，键值只在相邻条目变化一次，避免重复解码
	type pkVal struct {
		pk  int64
		val interface{}
	}
	pairs := make([]pkVal, 0, 16)
	tree.scanRangeStop(lo, hi, func(idxKey string, pkList []int64) bool {
		val := decodeIndexValue(typ, idxKey)
		// 区间是"偏宽"的（例如 lt v 含 v），用完整谓词剔除端点等边界
		if !scan.matchField(fi, scanValOf(val)) {
			return true
		}
		for _, pk := range pkList {
			pairs = append(pairs, pkVal{pk: pk, val: val})
		}
		return true
	})
	// 全局按主键排序：与顺序扫描路径的结果顺序保持一致
	sort.Slice(pairs, func(a, c int) bool { return pairs[a].pk < pairs[c].pk })
	rows := make([][]interface{}, 0, len(pairs))
	for _, p := range pairs {
		row := make([]interface{}, width)
		for i, of := range outFields {
			if of == fi {
				row[i] = p.val
			} else {
				row[i] = p.pk
			}
		}
		rows = append(rows, row)
		if limit >= 0 && len(rows) >= limit {
			break
		}
	}
	return rows, true
}

// decodeIndexValue 把保序索引键还原成字段原始值（encodeIndexValue 的逆运算）。
// 字符串类的键是折叠形（已丢失原始大小写），只用于区间精筛，不能作为结果值；
// 覆盖索引路径对字符串列已关闭（见 coveringRows）。
func decodeIndexValue(typ FieldType, key string) interface{} {
	switch typ {
	case TypeInt:
		if len(key) != 8 {
			return int64(0)
		}
		var b [8]byte
		copy(b[:], key)
		return int64(binary.BigEndian.Uint64(b[:]))
	case TypeDate, TypeDatetime, TypeDecimal:
		return decodeOrderedInt64(key)
	case TypeVarchar, TypeText, TypeBlob:
		return key
	case TypeBool:
		return len(key) == 1 && key[0] == 1
	}
	return nil
}

// scanValOf 把已解码的列值转成 scanVal，复用 matchField 的类型化比较语义。
func scanValOf(v interface{}) scanVal {
	switch x := v.(type) {
	case int64:
		return scanVal{kind: condInt, i: x}
	case string:
		return scanVal{kind: condString, s: x}
	case bool:
		return scanVal{kind: condBool, b: x}
	}
	return scanVal{kind: condNil}
}

// fieldValueAt 从编码行中按字段下标直接读取一个值（零拷贝字符串别名），
// 供 Update/Delete 只提取索引列旧值、跳过整行 decodeRow 的场景。
// 下标越界或行字节截断时返回 nil。
func (m *TableMeta) fieldValueAt(data []byte, idx int) interface{} {
	if idx < 0 || idx >= len(m.Fields) {
		return nil
	}
	pos := 16
	for i := 0; i < idx; i++ {
		switch m.Fields[i].Type {
		case TypeInt, TypeDate, TypeDatetime, TypeDecimal:
			pos += 8
		case TypeVarchar:
			if pos+2 > len(data) {
				return nil
			}
			pos += 2 + (int(data[pos])<<8 | int(data[pos+1]))
		case TypeText, TypeBlob:
			if pos+4 > len(data) {
				return nil
			}
			pos += 4 + (int(data[pos])<<24 | int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3]))
		case TypeBool:
			pos++
		}
	}
	switch m.Fields[idx].Type {
	case TypeInt, TypeDate, TypeDatetime, TypeDecimal:
		v, _ := readI64(data, &pos)
		return v
	case TypeVarchar:
		if pos+2 > len(data) {
			return nil
		}
		l := int(data[pos])<<8 | int(data[pos+1])
		pos += 2
		if l == int(varcharNullMarker) {
			return nil
		}
		if pos+l > len(data) {
			return nil
		}
		return unsafeString(data[pos : pos+l])
	case TypeText, TypeBlob:
		if pos+4 > len(data) {
			return nil
		}
		l := int(data[pos])<<24 | int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4
		if uint32(l) == textNullMarker {
			return nil
		}
		if l < 0 || pos+l > len(data) {
			return nil
		}
		return unsafeString(data[pos : pos+l])
	case TypeBool:
		if pos >= len(data) {
			return nil
		}
		return data[pos] == 1
	}
	return nil
}

// coerceInt 把任意数值/布尔/数值字符串转成 int64，供 INT 列编码使用。
func coerceInt(v interface{}) int64 {
	if n, ok := toIntCell(v); ok {
		return n
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		if f, ok := toFloat(x); ok {
			return int64(f)
		}
	}
	return 0
}

// validateRowValues 校验 map 形式的行值：VARCHAR 超过存储上限会被 2 字节
// 长度前缀静默截断（写入成功但数据损坏），这里显式报错。
func validateRowValues(meta *TableMeta, row map[string]interface{}) error {
	for _, f := range meta.Fields {
		if err := validateFieldValue(f, row[f.Name]); err != nil {
			return err
		}
	}
	return nil
}

// validateRowOrdered 校验按 meta.Fields 顺序传入的值。
func validateRowOrdered(meta *TableMeta, values []interface{}) error {
	for i, f := range meta.Fields {
		if i >= len(values) {
			break
		}
		if err := validateFieldValue(f, values[i]); err != nil {
			return err
		}
	}
	return nil
}

// validateFieldValue 单列写入校验。之所以在编码前单独跑一遍：
// 编码函数为了热路径不带 error 返回值，无法就地报错；而"长度超限被截断"、
// "十进制溢出回绕"、"日期文本无法解析"这类问题一旦静默发生就是数据损坏。
func validateFieldValue(f Field, v interface{}) error {
	switch f.Type {
	case TypeVarchar:
		if s, ok := v.(string); ok && len(s) > maxVarcharLen {
			return fmt.Errorf("value too long for column %s (max %d bytes, got %d)", f.Name, maxVarcharLen, len(s))
		}
	case TypeText, TypeBlob:
		if v == nil {
			return nil
		}
		if s := valueString(v); len(s) > maxTextLen {
			return fmt.Errorf("value too long for column %s (max %d bytes, got %d)", f.Name, maxTextLen, len(s))
		}
	case TypeDate, TypeDatetime:
		if v == nil {
			return nil
		}
		if _, ok := coerceTimeNanos(v, f.Type == TypeDate); !ok {
			return fmt.Errorf("invalid %s value for column %s: %v", fieldTypeName(f.Type), f.Name, v)
		}
	case TypeDecimal:
		if v == nil {
			return nil
		}
		if f.Len > decimalMaxScale {
			return fmt.Errorf("column %s has unsupported DECIMAL scale %d (max %d)", f.Name, f.Len, decimalMaxScale)
		}
		if _, ok := coerceDecimalScaled(v, f.Len); !ok {
			return fmt.Errorf("DECIMAL value out of range or malformed for column %s (scale %d): %v", f.Name, f.Len, v)
		}
	}
	return nil
}

// errPKExists 主键冲突哨兵：INSERT IGNORE / REPLACE / ON DUPLICATE KEY UPDATE
// 需要把"冲突"与其它错误区分开。
var errPKExists = fmt.Errorf("primary key already exists")

func (t *Table) Insert(pk int64, row map[string]interface{}, expireAt int64, txnID uint64, replay bool) error {
	if !replay {
		if t.migrating.Load() {
			return fmt.Errorf("table %s is being altered, retry later", t.Meta().Name)
		}
		if err := validateRowValues(t.Meta(), row); err != nil {
			return err
		}
	}
	if expireAt > 0 {
		t.ttlRows.Store(true)
	}
	rowData := encodeRow(t.Meta(), row, nextRowVersion(), expireAt)
	// 先改内存状态，再写 WAL：Compact 持有 walWriteMu 扫描时快照必然一致，
	// 扫描期完成的写会在压缩换文件后追加到新 WAL（重放幂等，不会丢失）。
	if !t.pkTree.SetNX(pk, rowData) {
		return fmt.Errorf("%w: %d", errPKExists, pk)
	}
	t.notePK(pk)
	for idxName, fieldName := range t.Meta().Indexes {
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
	if !replay {
		if t.migrating.Load() {
			return fmt.Errorf("table %s is being altered, retry later", t.Meta().Name)
		}
		if err := validateRowOrdered(t.Meta(), values); err != nil {
			return err
		}
	}
	if expireAt > 0 {
		t.ttlRows.Store(true)
	}
	rowData := encodeRowOrdered(t.Meta(), values, nextRowVersion(), expireAt)
	if !t.pkTree.SetNX(pk, rowData) {
		return fmt.Errorf("%w: %d", errPKExists, pk)
	}
	t.notePK(pk)
	if len(t.Meta().Indexes) > 0 {
		// 索引列值按字段下标直接取，不构建每行临时 map
		meta := t.Meta()
		for idxName, fieldName := range meta.Indexes {
			var val interface{}
			for i := range meta.Fields {
				if meta.Fields[i].Name == fieldName {
					if i < len(values) {
						val = values[i]
					}
					break
				}
			}
			t.indexAddPK(idxName, t.encodeIndexValue(fieldName, val), pk)
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
	if !replay {
		if t.migrating.Load() {
			return fmt.Errorf("table %s is being altered, retry later", t.Meta().Name)
		}
		if err := validateRowValues(t.Meta(), row); err != nil {
			return err
		}
	}
	meta := t.Meta()
	oldData, ok := t.pkTree.Get(pk)
	if !ok {
		return fmt.Errorf("row not found")
	}
	// 版本号改用全局单调计数器：两个并发事务改同一行时，per-row +1 会得到
	// 相同的 v+1 从而互相认为"没变化"，提交无法发现写冲突。
	// 索引列旧值按下标按需提取——不再解码整行 map。
	if expireAt > 0 {
		t.ttlRows.Store(true)
	}
	newVersion := nextRowVersion()
	newData := encodeRow(meta, row, newVersion, expireAt)

	t.pkTree.Set(pk, newData)
	for idxName, fieldName := range meta.Indexes {
		oldVal := meta.fieldValueAt(oldData, meta.fieldIndexOf(fieldName))
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
	if !replay && t.migrating.Load() {
		return fmt.Errorf("table %s is being altered, retry later", t.Meta().Name)
	}
	meta := t.Meta()
	oldData, ok := t.pkTree.Get(pk)
	if !ok {
		return fmt.Errorf("row not found")
	}
	t.pkTree.Delete(pk)
	for idxName, fieldName := range meta.Indexes {
		oldVal := meta.fieldValueAt(oldData, meta.fieldIndexOf(fieldName))
		t.indexRemovePK(idxName, t.encodeIndexValue(fieldName, oldVal), pk)
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
	// 重放期无并发读者：用 SetNXMutable 原地插入，避免每次写入克隆整条路径
	// （COW 下 n 行重放要分配 n·log n 个节点，原地插入只需 n 个）。
	if !t.pkTree.SetNXMutable(pk, data) {
		return
	}
	// 重放同样要抬高自增计数器：重启后自增不能撞上历史主键
	t.notePK(pk)
	// 恢复"存在 TTL 行"标记：覆盖索引据此决定能否跳过回表
	if decodeExpireAt(data) > 0 {
		t.ttlRows.Store(true)
	}
	if len(t.Meta().Indexes) == 0 {
		return
	}
	row, _, _ := decodeRow(t.Meta(), data)
	for idxName, fieldName := range t.Meta().Indexes {
		t.indexAddPKMutable(idxName, t.encodeIndexValue(fieldName, row[fieldName]), pk)
	}
}

func (t *Table) replayUpdate(pk int64, data []byte) {
	oldData, ok := t.pkTree.Get(pk)
	if !ok {
		return
	}
	if len(t.Meta().Indexes) == 0 {
		t.pkTree.UpdateMutable(pk, data)
		return
	}
	oldRow, _, _ := decodeRow(t.Meta(), oldData)
	newRow, _, _ := decodeRow(t.Meta(), data)
	t.pkTree.UpdateMutable(pk, data)
	for idxName, fieldName := range t.Meta().Indexes {
		t.updateIndexMutable(fieldName, idxName, oldRow[fieldName], newRow[fieldName], pk)
	}
}

func (t *Table) replayDelete(pk int64) {
	if len(t.Meta().Indexes) == 0 {
		t.pkTree.DeleteMutable(pk)
		return
	}
	oldData, ok := t.pkTree.Get(pk)
	if !ok {
		return
	}
	oldRow, _, _ := decodeRow(t.Meta(), oldData)
	t.pkTree.DeleteMutable(pk)
	for idxName, fieldName := range t.Meta().Indexes {
		t.indexRemovePKMutable(idxName, t.encodeIndexValue(fieldName, oldRow[fieldName]), pk)
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
	raw := t.selectRBytes(conditions, filters, minKey, maxKey, limit)
	meta := t.Meta()
	result := make([]map[string]interface{}, 0, len(raw))
	for _, data := range raw {
		row, _, _ := decodeRow(meta, data)
		result = append(result, row)
	}
	return result, nil
}

// selectRBytes SelectR 的原始字节内核：主键点查 / 二级索引等值 / 全表扫描
// 三级分发与 SelectR 语义一致，但返回命中行的原始编码字节（树内 COW 数据，
// 只读别名安全），不为命中行物化任何中间结构——SQL 路径按需投影，
// 二进制协议直接把字节写给客户端（行编码格式相同，省去 decode→encode 往返）。
func (t *Table) selectRBytes(conditions map[string]interface{}, filters []RangeCond, minKey, maxKey *int64, limit int) [][]byte {
	meta := t.Meta()
	scan := newScanConds(meta, conditions, filters)
	// 主键点查优先：等值 + 精确范围
	if pkVal, ok := conditions[meta.PK]; ok {
		if pk, ok := pkVal.(int64); ok {
			rowData, found := t.pkTree.Get(pk)
			if !found {
				return nil
			}
			if e := decodeExpireAt(rowData); e > 0 && time.Now().UnixNano() > e {
				t.pkTree.Delete(pk)
				return nil
			}
			if scan.matchRow(meta, rowData) {
				return [][]byte{rowData}
			}
			return nil
		}
	}
	// 二级索引等值路径
	if len(conditions) > 0 {
		for idxName, fieldName := range meta.Indexes {
			if val, ok := conditions[fieldName]; ok {
				idxKey := t.encodeIndexValue(fieldName, val)
				if tree, ok := t.indexes()[idxName]; ok && tree != nil {
					if pkList, ok := tree.Get(idxKey); ok {
						result := make([][]byte, 0, len(pkList))
						for _, pk := range pkList {
							if rowData, ok := t.pkTree.Get(pk); ok {
								if e := decodeExpireAt(rowData); e > 0 && time.Now().UnixNano() > e {
									t.pkTree.Delete(pk)
									continue
								}
								if scan.matchRow(meta, rowData) {
									result = append(result, rowData)
									if limit >= 0 && len(result) >= limit {
										break
									}
								}
							}
						}
						return result
					}
				}
			}
		}
	}
	// 二级索引范围下推：索引列上的范围条件（>, >=, <, <=, BETWEEN）
	// 用索引树的有序扫描收窄候选，避免全表逐行求值。
	// 候选过多时 indexRangeCandidates 会放弃并返回 used=false，退回下面的顺序扫描。
	now := time.Now().UnixNano()
	if len(filters) > 0 {
		if out, used := t.indexRangeCandidates(meta, scan, filters, limit, now); used {
			return out
		}
	}
	// 全范围扫描（可被主键范围裁剪）：条件预编译为类型化比较器，
	// 在行字节上直接求值（零分配），仅命中行进入结果。
	capHint := 16
	if limit > 0 && limit <= 4096 {
		capHint = limit
	}
	result := make([][]byte, 0, capHint)
	var expired []int64
	t.pkTree.scanRangeStop(minKey, maxKey, func(key int64, value []byte) bool {
		if e := decodeExpireAt(value); e > 0 && now > e {
			expired = append(expired, key)
			return true
		}
		if !scan.matchRow(meta, value) {
			return true
		}
		result = append(result, value)
		if limit >= 0 && len(result) >= limit {
			return false
		}
		return true
	})
	// 回调持读锁，不能在回调内删除；收集后锁外清理
	for _, k := range expired {
		t.pkTree.Delete(k)
	}
	return result
}

// SelectRBytesCond 条件查询的原始字节版本（SelectR 的零物化形态）。
// 返回的行字节与写入时完全一致（含 version/expireAt 前缀）。
func (t *Table) SelectRBytesCond(conditions map[string]interface{}, minKey, maxKey *int64, limit int) [][]byte {
	return t.selectRBytes(conditions, nil, minKey, maxKey, limit)
}

// ---- 行字节上的类型化条件求值（SelectR 扫描快速路径） ----

// condKind 值的动态类型（条件值与行值统一表示，用于零装箱比较）。
type condKind uint8

const (
	condNil    condKind = iota // NULL / 字段缺失
	condInt                    // int64
	condFloat                  // float64
	condString                 // string
	condBool                   // bool
	condOther                  // 其它类型：与行值（int64/string/bool）动态类型必不同
)

// scanVal 行字段值的类型化表示（s 引用行字节，零拷贝零装箱）。
type scanVal struct {
	kind condKind
	i    int64
	s    string
	b    bool
}

// scanEq 预编译的等值条件：语义与 matchConditions 的严格接口值比较一致
// （动态类型相同且值相等；行字段永不为 float/nil，对应条件恒假）。
type scanEq struct {
	idx  int // meta.Fields 下标；-1 = 字段不存在（行值视为 nil）
	kind condKind
	i    int64
	f    float64
	s    string
	b    bool
}

// scanCmp 预编译的单侧比较值（范围条件用）。
type scanCmp struct {
	kind condKind
	i    int64
	f    float64
	s    string
}

// scanFilter 预编译的范围条件（op ∈ gt/ge/lt/le/ne/between），
// 语义与 testRange/cmpVal 一致：int 一律经 float64 比较（保持 toNum 行为），
// 类型不可比时 cmp=0（ge/le/between 判真，gt/lt/ne 判假）。
type scanFilter struct {
	idx  int
	op   string
	a, b scanCmp
}

// scanConds 一次 SelectR 调用的全部预编译条件。
type scanConds struct {
	eqs     []scanEq
	filters []scanFilter
	maxIdx  int
}

func newScanConds(meta *TableMeta, conditions map[string]interface{}, filters []RangeCond) *scanConds {
	sc := &scanConds{maxIdx: -1}
	if len(conditions) > 0 {
		sc.eqs = make([]scanEq, 0, len(conditions))
		for k, v := range conditions {
			e := scanEq{idx: meta.fieldIndexOf(k)}
			var ftype FieldType
			var scale int
			if e.idx >= 0 {
				ftype = meta.Fields[e.idx].Type
				scale = meta.Fields[e.idx].Len
			}
			switch c := v.(type) {
			case nil:
				e.kind = condNil
			case int64:
				e.kind, e.i = condInt, c
			case float64:
				e.kind, e.f = condFloat, c
			case string:
				e.kind, e.s = condString, c
			case bool:
				e.kind, e.b = condBool, c
			default:
				e.kind = condOther
			}
			// 新类型在比较层统一折算成 int64，与行值同量纲：
			// 否则 'WHERE d = '2024-01-02''（字符串）与行里的纳秒永远不等。
			// 折算失败（文本不像日期/十进制）保持原 kind，与行值类型不符 →
			// matchField 返回 false，而不是把错误值当 0 比较。
			if e.idx >= 0 && v != nil {
				switch ftype {
				case TypeDate, TypeDatetime:
					if n, ok := coerceTimeNanos(v, ftype == TypeDate); ok {
						e.kind, e.i = condInt, n
					} else {
						e.kind = condOther
					}
				case TypeDecimal:
					if n, ok := coerceDecimalScaled(v, scale); ok {
						e.kind, e.i = condInt, n
					} else {
						e.kind = condOther
					}
				}
			}
			if e.idx > sc.maxIdx {
				sc.maxIdx = e.idx
			}
			sc.eqs = append(sc.eqs, e)
		}
	}
	if len(filters) > 0 {
		sc.filters = make([]scanFilter, len(filters))
		for i := range filters {
			fs := &sc.filters[i]
			fs.idx = meta.fieldIndexOf(filters[i].Field)
			fs.op = filters[i].Op
			var ftype FieldType
			var scale int
			if fs.idx >= 0 {
				ftype = meta.Fields[fs.idx].Type
				scale = meta.Fields[fs.idx].Len
			}
			fs.a = newScanCmpTyped(filters[i].Val, ftype, scale)
			fs.b = newScanCmpTyped(filters[i].Val2, ftype, scale)
			if fs.idx > sc.maxIdx {
				sc.maxIdx = fs.idx
			}
		}
	}
	return sc
}

// newScanCmpTyped 按列类型预编译比较值：DATE/DATETIME/DECIMAL 折算成 int64，
// 其余类型沿用 newScanCmp 的动态类型语义。
func newScanCmpTyped(v interface{}, ftype FieldType, scale int) scanCmp {
	switch ftype {
	case TypeDate, TypeDatetime:
		if v == nil {
			return scanCmp{kind: condNil}
		}
		if n, ok := coerceTimeNanos(v, ftype == TypeDate); ok {
			return scanCmp{kind: condInt, i: n}
		}
		return scanCmp{kind: condOther}
	case TypeDecimal:
		if v == nil {
			return scanCmp{kind: condNil}
		}
		if n, ok := coerceDecimalScaled(v, scale); ok {
			return scanCmp{kind: condInt, i: n}
		}
		return scanCmp{kind: condOther}
	}
	return newScanCmp(v)
}

func newScanCmp(v interface{}) scanCmp {
	var c scanCmp
	switch x := v.(type) {
	case int64:
		c.kind, c.i = condInt, x
	case int:
		c.kind, c.i = condInt, int64(x)
	case float64:
		c.kind, c.f = condFloat, x
	case string:
		c.kind, c.s = condString, x
	default:
		c.kind = condOther // bool/nil 等与行值类型不可比 → cmp=0
	}
	return c
}

// matchRow 判断编码行是否满足全部预编译条件。单遍按字段推进，
// 只解码 maxIdx 之前的字段；字符串直接在行字节上比较。
// 行字节短于当前 meta（ALTER ADD COLUMN 前写入的旧行）时，缺失字段按
// 零值（0/""/false）求值，与 decodeRow 的零值填充语义一致——若截断判
// false，旧行会被条件查询静默排除，而 SELECT * 无条件路径仍返回它们。
func (sc *scanConds) matchRow(meta *TableMeta, data []byte) bool {
	if len(sc.eqs) == 0 && len(sc.filters) == 0 {
		return true
	}
	// 字段不存在的条件：行值视为 nil（与 map 缺键一致）
	for i := range sc.eqs {
		if sc.eqs[i].idx < 0 && sc.eqs[i].kind != condNil {
			return false
		}
	}
	for i := range sc.filters {
		if sc.filters[i].idx < 0 && !sc.filters[i].match(scanVal{kind: condNil}) {
			return false
		}
	}
	pos := 16
	for fi := range meta.Fields {
		if fi > sc.maxIdx {
			break
		}
		var v scanVal
		switch meta.Fields[fi].Type {
		case TypeInt:
			if pos+8 <= len(data) {
				v = scanVal{kind: condInt, i: int64(uint64(data[pos])<<56 | uint64(data[pos+1])<<48 |
					uint64(data[pos+2])<<40 | uint64(data[pos+3])<<32 | uint64(data[pos+4])<<24 |
					uint64(data[pos+5])<<16 | uint64(data[pos+6])<<8 | uint64(data[pos+7]))}
				pos += 8
			} else {
				v = scanVal{kind: condInt} // 截断零值 0
			}
		case TypeDate, TypeDatetime, TypeDecimal:
			// 与 decodeRow 一致的定长 int64 解码；截断时按零值 0 求值。
			if pos+8 <= len(data) {
				v = scanVal{kind: condInt, i: int64(uint64(data[pos])<<56 | uint64(data[pos+1])<<48 |
					uint64(data[pos+2])<<40 | uint64(data[pos+3])<<32 | uint64(data[pos+4])<<24 |
					uint64(data[pos+5])<<16 | uint64(data[pos+6])<<8 | uint64(data[pos+7]))}
				pos += 8
			} else {
				v = scanVal{kind: condInt}
			}
		case TypeVarchar:
			if pos+2 <= len(data) {
				l := int(data[pos])<<8 | int(data[pos+1])
				pos += 2
				if pos+l <= len(data) {
					v = scanVal{kind: condString, s: unsafeString(data[pos : pos+l])}
					pos += l
				} else {
					v = scanVal{kind: condString} // 截断零值 ""
				}
			} else {
				v = scanVal{kind: condString} // 截断零值 ""
			}
		case TypeText, TypeBlob:
			if pos+4 <= len(data) {
				l := int(data[pos])<<24 | int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
				pos += 4
				if uint32(l) == textNullMarker {
					v = scanVal{kind: condString} // NULL 与 "" 同样不匹配等值条件
				} else if l >= 0 && l <= len(data)-pos {
					v = scanVal{kind: condString, s: unsafeString(data[pos : pos+l])}
					pos += l
				} else {
					v = scanVal{kind: condString}
				}
			} else {
				v = scanVal{kind: condString} // 截断零值 ""
			}
		case TypeBool:
			if pos < len(data) {
				v = scanVal{kind: condBool, b: data[pos] == 1}
				pos++
			} else {
				v = scanVal{kind: condBool} // 截断零值 false
			}
		default:
			continue
		}
		if !sc.matchField(fi, v) {
			return false
		}
	}
	return true
}

// matchField 对单个行字段求值所有指向它的条件。
func (sc *scanConds) matchField(fi int, v scanVal) bool {
	for i := range sc.eqs {
		e := &sc.eqs[i]
		if e.idx != fi {
			continue
		}
		switch e.kind {
		case condInt:
			if v.kind != condInt || v.i != e.i {
				return false
			}
		case condString:
			// utf8mb4_general_ci：等值忽略大小写，与索引键的折叠编码一致。
			if v.kind != condString || !eqFold(v.s, e.s) {
				return false
			}
		case condBool:
			if v.kind != condBool || v.b != e.b {
				return false
			}
		default:
			// condNil（行字段永非 nil）/ condFloat（行值永不为 float）/
			// condOther（动态类型不同）：接口值比较必不相等
			return false
		}
	}
	for i := range sc.filters {
		fl := &sc.filters[i]
		if fl.idx == fi && !fl.match(v) {
			return false
		}
	}
	return true
}

// match 求值单条范围条件（testRange 语义）。
func (f *scanFilter) match(v scanVal) bool {
	ca := f.a.cmpRow(v)
	switch f.op {
	case "gt":
		return ca > 0
	case "ge":
		return ca >= 0
	case "lt":
		return ca < 0
	case "le":
		return ca <= 0
	case "ne":
		return ca != 0
	case "between":
		return ca >= 0 && f.b.cmpRow(v) <= 0
	}
	return true
}

// cmpRow 按行值类型求与比较值的 cmpVal 语义比较；类型不可比返回 0。
func (c *scanCmp) cmpRow(v scanVal) int {
	switch v.kind {
	case condInt:
		switch c.kind {
		case condInt:
			return cmpF64(float64(v.i), float64(c.i))
		case condFloat:
			return cmpF64(float64(v.i), c.f)
		}
	case condString:
		if c.kind == condString {
			return cmpFold(v.s, c.s)
		}
	}
	return 0 // bool 行值 / 类型不可比
}

func cmpF64(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// fieldIndexOf 按字段名（大小写不敏感，与 SQL 列名解析一致）返回 meta.Fields 下标；
// 未找到返回 -1（调用方按"行中无此字段 → 值为 nil"语义处理）。
func (m *TableMeta) fieldIndexOf(name string) int {
	for i := range m.Fields {
		if m.Fields[i].Name == name {
			return i
		}
	}
	for i := range m.Fields {
		if strings.EqualFold(m.Fields[i].Name, name) {
			return i
		}
	}
	return -1
}

// SelectRBytes 范围查询的零分配快速路径：返回原始行字节，跳过 decodeRow 的 map 分配。
// 仅适用于 conditions==nil && filters==nil 的纯范围扫描场景（如压测）。
func (t *Table) SelectRBytes(minKey, maxKey *int64, limit int) [][]byte {
	return t.selectRBytes(nil, nil, minKey, maxKey, limit)
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
		// 字符串按 utf8mb4_general_ci 折叠比较：与 WHERE / ORDER BY / 索引键
		// 使用同一套语义，避免"过滤命中但排序位置相反"这类不一致。
		return cmpFold(as, bs)
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
	row, _, expireAt := decodeRow(t.Meta(), rowData)
	if expireAt > 0 && time.Now().UnixNano() > expireAt {
		t.pkTree.Delete(pk)
		return nil, false
	}
	return row, true
}

// ---------------- WAL 操作 ----------------

// writeLogBatch 把一批 INSERT 写成一条 WAL 记录（多行 INSERT / 批量导入）。
// 与逐条 writeLog 相比：记录头只写一次、只做一次通道交互、CRC 也只算一次。
func (t *Table) writeLogBatch(txnID uint64, pks []int64, rows [][]byte) error {
	if t.walFile == nil || len(pks) == 0 {
		return nil
	}
	size := 32
	for _, r := range rows {
		size += len(r) + 16
	}
	buf := make([]byte, 0, size)
	buf = append(buf, WALInsertBatch)
	buf = appendVarUint(buf, uint64(t.tableID))
	buf = appendVarUint(buf, txnID)
	buf = appendVarUint(buf, uint64(len(pks)))
	for i, pk := range pks {
		buf = appendVarUint(buf, uint64(pk))
		buf = appendVarUint(buf, uint64(len(rows[i])))
		buf = append(buf, rows[i]...)
	}
	if walCRC.Load() {
		buf = appendU32(buf, crc32.ChecksumIEEE(buf))
	}
	sync := walFsync.Load() && txnID == 0
	return appendWAL(buf, sync)
}

// insertBatchOrdered 批量插入（同一语句的多行）。语义与"逐行 txnApply + InsertOrdered"完全一致：
//   - 事务内登记每行前像（old=nil 表示原本不存在，回滚即删除）；
//   - 先整体校验 + 编码，失败时没有任何副作用；
//   - 一条 WAL 记录写完（省掉 n-1 次记录头与通道交互）；
//   - 二级索引按"索引键"聚合更新：低基数索引从 n 次树写降为每键 1 次。
func (db *DB) insertBatchOrdered(t *Table, pks []int64, rows [][]interface{}, txnID uint64) error {
	if len(pks) == 0 {
		return nil
	}
	batchInsertCalls.Add(1) // 测试钩子：确认批量路径确实被使用
	if t.migrating.Load() {
		return fmt.Errorf("table %s is being altered, retry later", t.Meta().Name)
	}
	meta := t.Meta()
	// 1) 校验 + 编码（此时尚未触碰任何状态）
	encoded := make([][]byte, len(pks))
	for i := range pks {
		if err := validateRowOrdered(meta, rows[i]); err != nil {
			return err
		}
		encoded[i] = encodeRowOrdered(meta, rows[i], nextRowVersion(), 0)
	}
	// 2) 语句内主键去重
	seen := make(map[int64]struct{}, len(pks))
	for _, pk := range pks {
		if _, dup := seen[pk]; dup {
			return fmt.Errorf("%w: %d", errPKExists, pk)
		}
		seen[pk] = struct{}{}
	}
	// 3) 与逐行路径相同的写互斥（避免与其他事务交错）
	if txnID != 0 {
		db.txnWriteMu.Lock()
		defer db.txnWriteMu.Unlock()
	}
	// 4) 存在性检查（锁内做，避免 TOCTOU）
	for _, pk := range pks {
		if _, exists := t.pkTree.Get(pk); exists {
			return fmt.Errorf("%w: %d", errPKExists, pk)
		}
	}
	// 5) 写入主键树
	inserted := 0
	undo := func() {
		for j := 0; j < inserted; j++ {
			t.pkTree.Delete(pks[j])
		}
		t.rebuildIndexes()
	}
	for i, pk := range pks {
		if !t.pkTree.SetNX(pk, encoded[i]) {
			undo()
			return fmt.Errorf("%w: %d", errPKExists, pk)
		}
		t.notePK(pk)
		inserted++
	}
	// 6) 二级索引：按索引键聚合，每个键只写一次倒排表
	if len(meta.Indexes) > 0 {
		for idxName, fieldName := range meta.Indexes {
			tree, ok := t.indexes()[idxName]
			if !ok || tree == nil {
				continue
			}
			fieldIdx := meta.fieldIndexOf(fieldName)
			buckets := make(map[string][]int64, len(pks))
			for i := range pks {
				var val interface{}
				if fieldIdx >= 0 && fieldIdx < len(rows[i]) {
					val = rows[i][fieldIdx]
				}
				key := t.encodeIndexValue(fieldName, val)
				buckets[key] = append(buckets[key], pks[i])
			}
			for key, added := range buckets {
				list, _ := tree.Get(key)
				merged := make([]int64, 0, len(list)+len(added))
				merged = append(merged, list...)
				merged = append(merged, added...)
				tree.Set(key, merged)
			}
		}
	}
	// 7) 一条 WAL 记录
	if err := t.writeLogBatch(txnID, pks, encoded); err != nil {
		undo()
		return err
	}
	// 8) 事务登记（前像为 nil：回滚即删除这些键）
	if txnID != 0 {
		name := meta.Name
		db.txnMu.Lock()
		if txn, ok := db.txns[txnID]; ok {
			if txn.Changes[name] == nil {
				txn.Changes[name] = make(map[int64]txnChange, len(pks))
			}
			for i, pk := range pks {
				txn.Changes[name][pk] = txnChange{old: nil, newVersion: decodeVersion(encoded[i])}
			}
			txn.Tables[name] = true
		}
		db.txnMu.Unlock()
	}
	return nil
}

func (t *Table) writeLog(cmd byte, txnID uint64, pk int64, data []byte) error {
	if t.walFile == nil {
		return nil
	}
	// v1 紧凑记录（varint + 表 ID）：小行写入固定开销从 ~38B 降到 ~10B。
	// 编码在锁外完成；落盘由单写者 writer goroutine 串行处理，此处无并发锁，避免高并发锁风暴。
	buf := walBufGet(24 + len(data))
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
	// batch 模式一律异步返回；fsync 模式下只有“没有后续 COMMIT 可等待”的记录
	// 才在这里同步等待。事务数据由 CommitTxn 的组提交统一落盘，避免每行/每条
	// WAL 记录各做一次 fsync；COMMIT 记录本身也异步写，等待在 txnMu 外完成。
	sync := walFsync.Load() && txnID == 0 && cmd != WALCommit && cmd != WALRollback
	return appendWAL(buf, sync)
}

// appendWAL 将记录提交给单写者队列。sync=false 时发送即返回；sync=true 时
// 等待 writer 将该记录（连同同批并发写者）一起 Flush+Sync 后返回。
func appendWAL(rec []byte, sync bool) error {
	if err := walFaultErr(); err != nil {
		return err
	}
	if walClosed.Load() {
		return errWALClosed
	}
	entry := walEntry{rec: rec}
	if sync {
		done := make(chan error, 1)
		entry.sync = true
		entry.done = done
	}
	// 先试非阻塞入队（热路径零开销）；队列满时转入阻塞等待，
	// 关闭哨兵兜底——自旋 + Gosched 会在突发写满时烧满一个核。
	select {
	case walCh <- entry:
	case <-walCloseCh:
		return errWALClosed
	}
	if entry.sync {
		return <-entry.done
	}
	return nil
}

// 通知 writer 冲刷缓冲并同步落盘（供 DDL/Backup/Close/ticker 使用），完成后返回。
// select 同时监听关闭哨兵：若 Close 发生在"检查 walClosed 之后、发送之前"，
// 此处不会永久阻塞（回归 L3）。
func (db *DB) flushWAL() error {
	done := make(chan error, 1)
	select {
	case walCh <- walEntry{flush: true, done: done}:
	case <-walCloseCh:
		return errWALClosed
	}
	return <-done
}

// walWriterLoop 是唯一写入 WAL 缓冲/文件的 goroutine：
//   - rec      写入 bufio 缓冲（进程内存）；
//   - rec.sync 或 flush：Flush 到 OS 页缓存并 Sync 落盘；多处 sync 被合并为一次组提交；
//   - stop     冲刷缓冲后退出（供 Compact 换文件、Close 收尾）。
//
// walWriteBufBytes WAL 单写者的 bufio 缓冲大小。启动时按设备落盘延迟调整：
// 慢盘用更大的缓冲摊薄系统调用与写放大，快盘保持小缓冲以降低内存与延迟。
var walWriteBufBytes = 256 * 1024

func walWriterLoop(file *os.File, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}
	bw := bufio.NewWriterSize(file, walWriteBufBytes)
	// writer 本地批量记账：单写者线程内累加字节数，每 32 条（或换文件/冲刷前）
	// 统一上报。速率桶粒度为 100ms，少量跨桶误差可忽略，换来写路径 31/32
	// 的取钟与原子累加省略。
	var pendingBytes int
	pendingRecs := 0
	reportWal := func() {
		if statsHook != nil && pendingBytes > 0 {
			statsHook.IncWalWrite(pendingBytes)
		}
		pendingBytes = 0
		pendingRecs = 0
	}
	// v3 分帧：把连续的多条记录攒成一批，写成 [varint len][u32 crc][records]。
	// 读侧因此可以按批流式读入（不必整文件入内存），并且一次 CRC 校验整批。
	var batch []byte
	batchCount := 0
	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		var head [16]byte
		h := appendVarUint(head[:0], uint64(len(batch)))
		h = appendU32(h, crc32.ChecksumIEEE(batch))
		if _, err := bw.Write(h); err != nil {
			walFail(err)
		}
		if _, err := bw.Write(batch); err != nil {
			walFail(err)
		}
		batch = batch[:0]
		batchCount = 0
	}
	// writeRec 写一条记录并记账；错误不在这里丢弃，而是进入故障态，
	// 让等待者与后续 appendWAL 都看到失败（旧实现：错误被吞、一律回 nil）。
	writeRec := func(rec []byte) {
		pendingBytes += len(rec)
		pendingRecs++
		if walFramed.Load() {
			batch = append(batch, rec...)
			batchCount++
			walBufPut(rec)
			if len(batch) >= walBatchMaxBytes || batchCount >= walBatchMaxRecords {
				flushBatch()
			}
			return
		}
		if _, err := bw.Write(rec); err != nil {
			walFail(err)
		}
		walBufPut(rec)
	}
	// flushSync 冲刷并落盘，返回首个错误。
	flushSync := func() error {
		flushBatch()
		reportWal()
		err := bw.Flush()
		if err == nil {
			err = file.Sync()
		}
		if err == nil && statsHook != nil {
			statsHook.IncFsync()
		}
		walFail(err)
		return err
	}
	for {
		e := <-walCh
		switch {
		case e.stop:
			err := flushSync()
			walSignal(e.done, err)
			return
		case e.flush:
			walSignal(e.done, flushSync())
			continue
		}
		// 普通记录
		writeRec(e.rec)
		if pendingRecs >= 32 {
			reportWal()
		}
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
					writeRec(e2.rec)
					if e2.sync {
						waiters = append(waiters, e2.done)
					}
				}
			default:
				drain = false
			}
		}
		err := flushSync()
		for _, d := range waiters {
			walSignal(d, err)
		}
		if stopAfter {
			walSignal(stopCh, err)
			return
		}
	}
}

// walSignal 安全地向完成通道发送结果（仅一次，nil 通道为 noop）。
func walSignal(d chan error, err error) {
	if d != nil {
		d <- err
	}
}

// Compact 将 WAL 重写为仅含当前内存状态的 v1 紧凑快照（header + catalog + INSERT），
// 清除已删除/被覆盖的记录，缩小文件并加速后续恢复。
// 通过 FIFO 通道建立 barrier：所有先于 barrier 入队的记录必然被冲刷进当前文件且内存态已生效
// （写者先改树后入队，故其内存态先于入队），快照必然覆盖它们；barrier 之后入队的记录
// 在换文件后追加到新 WAL（重放幂等，不丢失）。因此压缩期间写入无需全局锁。
func (db *DB) Compact() error {
	if walClosed.Load() {
		return fmt.Errorf("wal is closed")
	}
	compactMu.Lock()
	defer compactMu.Unlock()
	compactBusy.Store(true)
	defer compactBusy.Store(false)

	// 1) barrier：等待 writer 把此前所有已入队记录冲刷并同步到当前文件
	// 带超时：与 Close 竞争时阻塞发送可能永久挂起（回归 M4）
	bdone := make(chan error, 1)
	select {
	case walCh <- walEntry{flush: true, done: bdone}:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("compact: wal writer busy")
	}
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
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// 只写临时文件的头，不动全局分帧开关：压缩失败时 writer 仍会在旧文件上追加，
	// 若提前把开关切成 true，v1/v2 旧文件会被塞进批次头而无法重放。
	if err := writeWALHeaderV(f, WALVersion5); err != nil {
		f.Close()
		return err
	}
	w := bufio.NewWriterSize(f, 64*1024)
	// 压缩产物是 v5 文件，因此记录必须按批分帧写入，
	// 直接写裸记录会让重放时把首条记录当成批次长度解析。
	wb := &walBatchWriter{w: w}

	for _, t := range tables {
		// v1 紧凑 catalog 记录：[100][subop=1][varint tableID]...
		rec := make([]byte, 0, 96+len(t.Meta().Name)*3)
		rec = append(rec, 100, 1)
		rec = appendVarUint(rec, uint64(walRegRegister(t.Meta().Name)))
		rec = appendVarLenStr(rec, t.Meta().Name)
		rec = appendVarLenStr(rec, t.Meta().PK)
		rec = appendVarUint(rec, uint64(len(t.Meta().Fields)))
		for _, fld := range t.Meta().Fields {
			rec = appendVarLenStr(rec, fld.Name)
			rec = append(rec, byte(fld.Type))
			rec = appendVarUint(rec, uint64(fld.Len))
		}
		rec = appendVarUint(rec, uint64(len(t.Meta().Indexes)))
		for idxName, idxField := range t.Meta().Indexes {
			rec = appendVarLenStr(rec, idxName)
			rec = appendVarLenStr(rec, idxField)
		}
		if walCRC.Load() {
			rec = appendU32(rec, crc32.ChecksumIEEE(rec))
		}
		wb.Write(rec)

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
			wb.Write(rec)
			compactRecPool.Put(rec[:0])
		})
	}
	if err := wb.Flush(); err != nil {
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
		nf, e2 := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR, 0600)
		if e2 != nil {
			// 无法重建可写句柄：禁止 writer 在已关闭的文件上静默丢数据
			walClosed.Store(true)
			return fmt.Errorf("compact: rename failed and reopen failed: %v (reopen: %v)", err, e2)
		}
		nf.Seek(0, 2)
		db.walFile = nf
		return err
	}
	nf, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		walClosed.Store(true)
		return err
	}
	// 压缩后文件已含完整内容，新句柄须定位到末尾以支持后续追加
	if _, err := nf.Seek(0, 2); err != nil {
		nf.Close()
		return err
	}
	db.walFile = nf
	// 压缩产物确定落位后才切换分帧开关（旧文件可能是 v1/v2 裸记录格式）
	walFramed.Store(true)
	// 重启单写者 writer：接管新文件，并排空压缩期间积压在 walCh 的记录
	restarted = true
	go walWriterLoop(nf, nil)
	return nil
}

// rebuildIndexes 基于当前主键树重建全部二级索引：离线构建新快照后整体发布，
// 避免边遍历边修改共享 map（写时复制，读者不受影响）。
func (t *Table) rebuildIndexes() {
	newIdx := make(idxTreeMap, len(t.Meta().Indexes))
	for idxName, fieldName := range t.Meta().Indexes {
		tree := newStringKeyTree[[]int64]()
		newIdx[idxName] = tree
		t.pkTree.scanAll(func(key int64, value []byte) {
			row, _, _ := decodeRow(t.Meta(), value)
			t.indexAddPKTo(tree, fieldName, row[fieldName], key)
		})
	}
	t.storeIndexes(newIdx)
}

// indexAddPKTo 将 pk 追加到指定树（离线构建索引用，不经过快照查找）。
func (t *Table) indexAddPKTo(tree *RBTree[string, []int64], fieldName string, val interface{}, pk int64) {
	idxKey := t.encodeIndexValue(fieldName, val)
	list, _ := tree.Get(idxKey)
	newList := make([]int64, len(list), len(list)+1)
	copy(newList, list)
	newList = append(newList, pk)
	tree.Set(idxKey, newList)
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
	if len(t.Meta().Indexes) == 0 {
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
		oldRow, _, _ := decodeRow(t.Meta(), oldData)
		t.pkTree.Delete(k)
		for idxName, fieldName := range t.Meta().Indexes {
			t.indexRemovePK(idxName, t.encodeIndexValue(fieldName, oldRow[fieldName]), k)
		}
	}
}

// setDurability 切换持久化模式：落盘语义（walFsync）与统计口径即时生效。
// 调用方需持有 cfgMu（或处于单写者上下文）。
func (db *DB) setDurability(mode Durability) {
	if !mode.Valid() {
		return
	}
	// fsync 语义切换前先冲刷，确保此前的 batch 记录落盘
	db.flushWAL()
	// 调用方必须持有 cfgMu（settings 页已持有；协议入口自行加锁）
	db.config.Durability = mode
	db.stats.durability.Store(string(mode))
	walFsync.Store(mode.Fsync())
}

// ==================== 数据库引擎 ====================

const (
	WALMagic   = "TSUMUGI"
	WALVersion = 1
)

type DB struct {
	config      *Config
	tables      sync.Map // map[string]*Table，无锁并发安全
	tablesCount atomic.Int64
	catalog     map[string]*TableMeta
	privileges  *PrivilegeStore
	walFile     *os.File
	mu          sync.RWMutex // 保护 catalog、users 等
	curDB       string
	curDBMu     sync.RWMutex
	txns        map[uint64]*Transaction
	txnMu       sync.Mutex
	// txnWriteMu 串行化事务写路径的"写行 + 记录新版本"，使提交时的版本校验
	// 看到的一定是本事务刚写出的值（否则并发事务可交错出假一致）。
	txnWriteMu        sync.Mutex
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
	closed            atomic.Bool
	// cfgMu 串行化设置页对 db.config 的热更新写入；读路径为高频只读字段，
	// 写入全部收敛到 cfgMu 持有者（settings/setup/durability）。
	cfgMu sync.RWMutex
	// webSchemas SQL 控制台按 token 的会话库（USE 作用域），token -> dbname
	webSchemas sync.Map
	// dirLock 数据目录独占锁句柄：保证同一 data 目录只有一个进程在跑
	// （两个进程共享 WAL 会互相覆盖数据）。
	dirLock *os.File
}

type Transaction struct {
	ID     uint64
	Active bool
	Tables map[string]bool
	// Changes 记录本事务对每行的改动：旧值（回滚用）+ 写入后的行版本（提交校验用）。
	Changes map[string]map[int64]txnChange
	// ReadVersions 保留读取版本记录（历史字段，冲突检测已改用 txnChange.newVersion）。
	ReadVersions map[string]map[int64]int64
	// Savepoints 命名保存点栈（A4）。每个点保存"当时的行字节 + 事务记账"快照，
	// 用于部分回滚（ROLLBACK TO SAVEPOINT）。
	Savepoints []*txnSavepoint
}

// txnChange 一条事务内行变更。
//   - old != nil && newVersion != 0：更新，old 为事务开始前的行字节
//   - old == nil && newVersion != 0：插入
//   - newVersion == 0：删除（old 为删除前的行字节）
//
// batchInsertCalls 批量插入路径的调用次数（仅用于测试/诊断，不影响逻辑）。
var batchInsertCalls atomic.Int64

type txnChange struct {
	old        []byte
	newVersion int64
}

func NewDB(cfg *Config) (*DB, error) {
	// 数据目录含 WAL 明文数据：只允许属主访问
	if err := os.MkdirAll(cfg.WALDir, 0700); err != nil {
		return nil, err
	}
	// 用户库跟随数据目录：硬编码 "data" 会让测试/非默认部署把 users.json
	// 写到工作目录下的 data/，造成测试互相污染、部署目录不一致。
	initUserStore(cfg.WALDir)
	walPath := cfg.WALDir + "/" + cfg.WALFile
	walFile, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// 数据目录独占锁：同一目录只允许一个实例。两个进程共享 WAL 会互相覆盖
	// （各自持有内存态与独立 writer），属于静默数据损坏，必须在启动时挡住。
	dirLock, err := acquireDirLock(cfg.WALDir)
	if err != nil {
		walFile.Close()
		return nil, err
	}
	// 以下任一失败都必须同时关闭文件并释放目录锁，否则同一目录无法再次启动
	//（崩溃一致性测试发现：坏 WAL 会让目录锁永久泄漏）。
	info, _ := walFile.Stat()
	if info.Size() == 0 {
		if err := writeWALHeader(walFile); err != nil {
			walFile.Close()
			releaseDirLock(dirLock)
			return nil, err
		}
	} else {
		if err := verifyWALHeader(walFile); err != nil {
			walFile.Close()
			releaseDirLock(dirLock)
			return nil, err
		}
		// 旧文件按原格式追加（writer 的分帧开关跟文件版本走），
		// 下次 Compact 会把文件重写成 v3。这里必须显式写入两个分支：
		// 包级开关会被同进程内先前的 v3 实例置位，漏写 false 会让 v1 文件被塞进批次头。
		v, err := walHeaderVersion(walFile)
		if err != nil {
			walFile.Close()
			releaseDirLock(dirLock)
			return nil, err
		}
		walFramed.Store(v == WALVersion3 || v == WALVersion4 || v == WALVersion5)
	}

	privStore, err := NewPrivilegeStore(cfg.WALDir + "/" + cfg.PrivilegeFile)
	if err != nil {
		walFile.Close()
		releaseDirLock(dirLock)
		return nil, err
	}

	db := &DB{
		config:     cfg,
		catalog:    make(map[string]*TableMeta),
		privileges: privStore,
		walFile:    walFile,
		dirLock:    dirLock,
		txns:       make(map[uint64]*Transaction),
		stopCh:     make(chan struct{}),
		stats:      NewStats(),
		backupDir:  cfg.BackupDir,
	}
	db.stats.durability.Store(string(cfg.Durability))
	statsHook = db.stats
	// 反代来源白名单：决定是否采信 X-Forwarded-For（登录限速/审计按真实 IP）
	setTrustedProxies(cfg.TrustedProxies)
	mysqlCfg.load(cfg.WALDir)
	db.groupCommit = NewGroupCommitter(db, cfg.GroupCommitInterval)
	// 设备探测：按落盘延迟/核数调整组提交窗口、WAL 缓冲与重放并行度。
	// 必须在 WAL writer 启动前执行（writer 创建 bufio 时读取缓冲大小）。
	tune := applyTuning(cfg)
	db.groupCommit.setWindowBounds(tune.CommitMin, tune.CommitMax, 0)
	// 索引树实现必须在 loadWAL 之前确定：重放过程就是重建主键树与二级索引树，
	// 之后再改只会影响新表，导致同一进程里两套结构并存（语义一致但难排查）。
	setTreeKind(parseTreeKind(cfg.TreeKind))
	setTreeMode(parseTreeMode(cfg.TreeMode))
	logf(LOG_VERB, "index tree: %s (%s mode)", currentTreeKind(), currentTreeMode())
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
		// 加载失败必须完整回滚：停止已启动的后台循环、关闭监控端口、
		// 释放数据目录锁并关闭 WAL 文件。
		// （此前直接 return，导致目录锁泄漏——重启同一目录会一直失败，
		//   同时后台 goroutine 会带着半初始化的 DB 继续跑。由崩溃一致性测试发现。）
		close(db.stopCh)
		db.flushTicker.Stop()
		db.ttlTicker.Stop()
		if db.autoCompactTicker != nil {
			db.autoCompactTicker.Stop()
		}
		if srv := db.metricsServer.Load(); srv != nil {
			srv.Close()
		}
		db.wg.Wait()
		releaseDirLock(dirLock)
		walFile.Close()
		return nil, err
	}
	// 创建 WAL 写缓冲（loadWAL 完成后，确保文件偏移在末尾）
	if _, err := walFile.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	walFsync.Store(cfg.Durability.Fsync())
	walCRC.Store(cfg.EnableChecksum)
	walClosed.Store(false)
	walFaultReset()
	// 重建关闭哨兵：上一轮 Close 已 close 过。全局 WAL 生命周期一次只允许
	// 一个活跃 DB（既有设计约束，见 walCh 等包级全局注释）。
	walCloseCh = make(chan struct{})
	// 注意：此处绝不能 walRegReset()——loadWAL 已通过 catalog 重放把注册表
	// 绑定到文件中的表 ID；清空会让重启后新建的表从 1 重新编号，与既有表
	// 的 tableID 冲突，下次重放时数据会串表（回归测试见 walreg_repro_test.go）。
	db.walFile = walFile
	// 启动唯一 WAL writer：此后所有 appendWAL/flushWAL 经通道交给它串行落盘
	db.wg.Add(1)
	go walWriterLoop(walFile, &db.wg)
	logf(LOG_VERB, "WAL loaded (%d tables, %d rows) in %v", db.tablesCount.Load(), atomic.LoadInt64(&walLoadRows), time.Since(walLoadStart))
	return db, nil
}

// writeWALHeader 新建 WAL 文件时写 v5 头（分帧 + 批量校验）。
func writeWALHeader(f *os.File) error {
	if err := writeWALHeaderV(f, WALVersion5); err != nil {
		return err
	}
	walFramed.Store(true)
	return nil
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
	// v1 紧凑格式版本号即为 1；v2 同格式；v3/v4/v5 为分帧格式（带批次长度与校验和）
	if version != 1 && version != 2 && version != WALVersion3 && version != WALVersion4 && version != WALVersion5 {
		return fmt.Errorf("unsupported WAL version %d, expected 1, 2, %d, %d or %d", version, WALVersion3, WALVersion4, WALVersion5)
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
	cfg := db.configSnapshot()
	if !cfg.AutoCompact || compactBusy.Load() {
		return
	}
	minMB := cfg.CompactMinWALMB
	if minMB <= 0 {
		minMB = 64
	}
	if walFileSizeMB(db) < float64(minMB) {
		return
	}
	// 低峰判定：最近 1 秒命令数低于阈值则视为空闲
	peak := cfg.CompactPeakRate
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
	// 幂等保护：Close 可能被 defer 与显式调用各触发一次
	if !db.closed.CompareAndSwap(false, true) {
		return
	}
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
	// 标记关闭并广播哨兵：此后 appendWAL/flushWAL 直接返回/退出等待，
	// 避免阻塞在已退出的 writer 上
	walClosed.Store(true)
	close(walCloseCh)
	// 释放数据目录独占锁：进程退出/DB 关闭后，同一目录可以被重新启动
	releaseDirLock(db.dirLock)
	// 停止 writer：冲刷并同步所有已入队记录后退出
	done := make(chan error, 1)
	select {
	case walCh <- walEntry{stop: true, done: done}:
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			logf(LOG_ERR, "WAL writer did not stop within timeout")
		}
	case <-time.After(5 * time.Second):
		logf(LOG_ERR, "failed to send stop signal to WAL writer")
	}
	// 等后台协程退出：加超时兜底。WAL 通道是进程级单例，多实例场景下
	// 停止信号可能被另一个实例的 writer 取走，此时本实例的 writer 不会退出，
	// 无限等待会把进程卡死在关闭阶段。
	waitDone := make(chan struct{})
	go func() {
		db.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		logf(LOG_ERR, "background goroutines did not exit within 10s, continuing shutdown")
	}
	db.walFile.Close()
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

	st, err := file.Stat()
	if err != nil {
		return err
	}
	headerLen := len(WALMagic) + 4 + 4
	if st.Size() < int64(headerLen) {
		return nil
	}
	version, err := walHeaderVersion(file)
	if err != nil {
		return err
	}
	ctx := &walReplayCtx{
		db:          db,
		file:        file,
		headerLen:   headerLen,
		enableCRC:   db.config.EnableChecksum,
		pending:     make(map[string][]walOp),
		pendingTxns: make(map[uint64][]walOp),
		baseOffset:  int64(headerLen),
	}
	// 启动段式重放的应用侧：解析与回填重叠进行
	ctx.startApplier()
	defer ctx.stopApplier()
	switch version {
	case WALVersion3, WALVersion4, WALVersion5:
		// v3/v4/v5 分帧：按批流式读入（内存只与最大批次相关，不再整文件入内存）
		// 三个版本的分帧布局一致，复用 loadV3；版本号只用于标识记录语义差异。
		// 显式定位到记录区，不依赖 walHeaderVersion 读完后遗留的偏移
		if _, err := file.Seek(int64(headerLen), io.SeekStart); err != nil {
			return err
		}
		err = ctx.loadV3(st.Size())
		// 等所有在途段应用完再统计行数/收尾进度条（异步应用可能仍在跑）
		ctx.flushPending()
		walLoadRows = ctx.loadedRows.Load()
		ctx.walProgressFinish()
		return err
	case WALVersion, 2:
		// 旧格式没有长度前缀，只能整文件读入后顺序切分
		if st.Size() > 1<<32 {
			return fmt.Errorf("wal file size out of range: %d", st.Size())
		}
		if _, err := file.Seek(0, 0); err != nil {
			return err
		}
		all := make([]byte, int(st.Size()))
		if _, err := io.ReadFull(file, all); err != nil {
			return err
		}
		ctx.buf = all[headerLen:]
		ctx.atEOF = true
		return ctx.loadV2()
	default:
		return fmt.Errorf("unsupported WAL version %d, expected 1, 2, %d, %d or %d", version, WALVersion3, WALVersion4, WALVersion5)
	}
}

// walReplayCtx 承载 v1/v2/v3 共用的重放基础设施
type walReplayCtx struct {
	db          *DB
	file        *os.File
	headerLen   int
	buf         []byte
	enableCRC   bool
	pending     map[string][]walOp
	pendingTxns map[uint64][]walOp
	// pendingBytes 当前缓冲里记录数据的总字节数，与 pendingOps 共同决定何时切段。
	pendingOps   int
	pendingBytes int64
	loadedRows   atomic.Int64
	totalRows    int64
	lastPct      int
	barDone      bool
	// baseOffset 当前 buf 在文件中的起始偏移（v2 = 文件头长度；v3 = 当前批次 payload 起点）。
	// truncateTail 用它把"缓冲内相对偏移"换算成文件绝对偏移。
	baseOffset int64
	// atEOF 当前 buf 是否已是文件最后一段：只有这里才允许把残缺记录当崩溃残留截断，
	// 中间批次的残缺是真正的损坏。
	atEOF bool
	// 段式并行：解析（读 varint、切 data 引用）与应用（树插入、索引维护）原本串行
	// 交替。这里把"应用"交给单个后台 goroutine：解析完一段就投递并继续下一段，
	// 实现解析与应用重叠；段内仍按表并行。段严格按投递顺序串行应用，因此
	// "同表记录保持写入顺序"的语义不变。
	applyCh   chan map[string][]walOp
	applyWG   *sync.WaitGroup
	applyDone chan struct{}
	// progressMu 保护 lastPct/barDone：进度条会被解析侧与应用侧两个 goroutine
	// 同时刷新（解析推进与段应用完成都会调用 walProgressBar）。
	progressMu sync.Mutex
}

// 段式重放参数：单段记录数/字节上限决定流水线的粒度与峰值内存。
// 默认面向常见机器；启动探测会按核数与内存压力调整（小机器缩小段大小，
// 大机器限制并行 worker 数量避免调度开销）。
var (
	replaySegmentOps   = 65536   // 单段记录数上限
	replaySegmentBytes = 8 << 20 // 单段数据字节上限（含行载荷）
)

const replayApplyDepth = 2 // 在途段数：1 = 纯重叠，2 = 再多一段缓冲

// startApplier 启动应用侧 goroutine（每次重放调用一次）。
func (c *walReplayCtx) startApplier() {
	if !replayPipeline {
		// 单核/极小内存：不做流水线。解析完一段就同步回填，
		// 内存同样按段有界，但省掉 goroutine 与通道开销。
		return
	}
	c.applyCh = make(chan map[string][]walOp, replayApplyDepth)
	c.applyWG = &sync.WaitGroup{}
	c.applyDone = make(chan struct{})
	go func() {
		defer close(c.applyDone)
		for seg := range c.applyCh {
			c.applySegment(seg)
			c.applyWG.Done()
		}
	}()
}

// stopApplier 冲刷余下段并等待应用侧退出。
func (c *walReplayCtx) stopApplier() {
	if c.applyCh == nil {
		return
	}
	close(c.applyCh)
	<-c.applyDone
	c.applyCh = nil
}

// queuePending 把当前缓冲切成一段投递（异步）；缓冲为空时为空操作。
func (c *walReplayCtx) queuePending() {
	if len(c.pending) == 0 {
		return
	}
	seg := c.pending
	// 换成新 map：解析侧继续往新缓冲写，应用侧独占旧 map
	c.pending = make(map[string][]walOp, len(seg))
	c.pendingOps = 0
	c.pendingBytes = 0
	if c.applyCh == nil {
		// 同步模式（无流水线）：直接应用这一段
		c.applySegment(seg)
		return
	}
	c.applyWG.Add(1)
	c.applyCh <- seg
}

// drainPending 投递当前缓冲并等待所有在途段应用完成（屏障）。
// 目录变更、事务边界与重放收尾都必须过这道屏障，保证元数据/顺序可见。
func (c *walReplayCtx) drainPending() {
	c.queuePending()
	if c.applyWG != nil {
		c.applyWG.Wait()
	}
}

// applySegment 应用一段内的全部记录：段内按表并行，同表串行。
func (c *walReplayCtx) applySegment(seg map[string][]walOp) {
	if len(seg) == 0 {
		return
	}
	c.walProgressBar(c.loadedRows.Load())
	workers := replayWorkerCount(len(seg))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var applied atomic.Int64
	for tableName, ops := range seg {
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
			applied.Add(int64(len(recs))) // 每表一次，而非每行一次
		}(table, ops)
	}
	wg.Wait()
	c.loadedRows.Add(applied.Load())
	c.walProgressBar(c.loadedRows.Load())
}

// walProgressBar 渲染并刷新一条 Minecraft 风味的加载进度条（\r 原地刷新，不换行）。
// 仅在解析推进时更新，避免打字风暴；返回是否真正打印过。
func (c *walReplayCtx) walProgressBar(rows int64) bool {
	total := c.totalRows
	// 小 WAL（<10 万行）瞬时即可完成：进度条反而刷屏（污染基准/CI 输出）
	if total < 100000 {
		return false
	}
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	var pct int
	if rows >= total {
		pct = 100
	} else if total > 100 {
		pct = int(rows / (total / 100))
	} else {
		pct = int(rows * 100 / total)
	}
	if pct > 100 {
		pct = 100
	}
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
	if c.barDone || c.totalRows < 100000 {
		return
	}
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	if c.barDone {
		return
	}
	width := 24
	bar := strings.Repeat("#", width)
	rows := c.loadedRows.Load()
	fmt.Printf("\r[INFO] Loading databases... [%s] 100%% (%d rows)\n", bar, rows)
	c.barDone = true
}

// applyOpOnTable 应用一条记录到指定表，不做计数。
// 计数由调用方在整批/整表应用完后一次性累加：每行一次原子加会让所有并行
// worker 争用同一 cache line，是大表重放里明显的串行化点。
func (c *walReplayCtx) applyOpOnTable(t *Table, cmd byte, pk int64, data []byte) {
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

// flushPending 是同步屏障：投递缓冲并等待全部在途段应用完成。
// 目录变更、事务 BEGIN、重放收尾都依赖它维持顺序与元数据可见性。
func (c *walReplayCtx) flushPending() {
	c.drainPending()
}

// 崩溃/强杀留下的残缺尾部记录：截断丢弃而不是启动失败
func (c *walReplayCtx) truncateTail(recordStart int) {
	fileOffset := c.baseOffset + int64(recordStart)
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
	c.totalRows = c.estimateRows()
	err := c.parseV2()
	c.flushPending()
	walLoadRows = c.loadedRows.Load()
	c.walProgressFinish()
	return err
}

// estimateRows 快速估算 WAL 中的总行数（仅扫描 varint 跳过数据）。
// 用于进度条分母，精度不需要100%，只需给出合理的上限。
func (c *walReplayCtx) estimateRows() int64 {
	buf := c.buf
	pos := 0
	count := int64(0)
	skipVarint := func() {
		for pos < len(buf) && buf[pos]&0x80 != 0 {
			pos++
		}
		if pos < len(buf) {
			pos++
		}
	}
	skipNVarints := func(n int) {
		for i := 0; i < n && pos < len(buf); i++ {
			skipVarint()
		}
	}
	for pos < len(buf) {
		startPos := pos
		if pos+1 > len(buf) {
			break
		}
		cmd := buf[pos]
		pos++
		if cmd == 100 { // catalog
			if pos >= len(buf) {
				break
			}
			pos++           // sub
			skipNVarints(2) // id + name
			// skip meta: pkLen + fieldCount + fields
			if pos < len(buf) {
				skipVarint() // pkLen
				if pos < len(buf) {
					fc := int(buf[pos])
					pos++
					for f := 0; f < fc && pos < len(buf); f++ {
						skipVarint() // nameLen
						if pos < len(buf) {
							nl := pos
							skipVarint() // name bytes
							nl = pos - nl - 1
							if pos+nl > len(buf) {
								pos = len(buf)
							} else {
								pos += nl
							}
						}
						if pos < len(buf) {
							pos++ // type
						}
						skipVarint() // len
					}
				}
			}
			if pos <= startPos {
				break
			}
			continue
		}
		// 数据记录
		isTxnCmd := cmd == WALBegin || cmd == WALCommit || cmd == WALRollback
		if cmd == WALInsertBatch {
			// [cmd][tableID][txnID][count]([pk][len][data])*count
			_, ok1 := readVarUint(buf, &pos)
			_, ok2 := readVarUint(buf, &pos)
			cnt, ok3 := readVarUint(buf, &pos)
			if !ok1 || !ok2 || !ok3 {
				break
			}
			for i := uint64(0); i < cnt; i++ {
				if _, ok := readVarUint(buf, &pos); !ok {
					break
				}
				n, ok := readVarUint(buf, &pos)
				if !ok {
					break
				}
				if int(n) > len(buf)-pos {
					pos = len(buf)
				} else {
					pos += int(n)
				}
				count++
			}
			if c.enableCRC && pos+4 <= len(buf) {
				pos += 4
			}
			continue
		}
		fields := 1
		if !isTxnCmd {
			fields = 3
		}
		skipNVarints(fields)
		if !isTxnCmd {
			var dataLen uint64
			shift := 0
			for pos < len(buf) {
				b := buf[pos]
				pos++
				dataLen |= uint64(b&0x7f) << shift
				shift += 7
				if b&0x80 == 0 {
					break
				}
				if shift > 63 {
					break
				}
			}
			if int(dataLen) > len(buf)-pos {
				pos = len(buf)
			} else {
				pos += int(dataLen)
			}
		}
		if c.enableCRC && pos+4 <= len(buf) {
			pos += 4
		}
		if pos <= startPos {
			break
		}
		count++
	}
	return count
}

func (c *walReplayCtx) parseV2() error {
	buf := c.buf
	pos := 0

	// 当一条记录的数据在缓冲内耗尽时，若恰好位于文件末尾则视为崩溃残留，
	// 截断尾部并正常结束解析（返回 true 表示已截断尾部）。
	tail := func(recordStart int) bool {
		if pos >= len(buf) {
			if !c.atEOF {
				return false // 中间批次被截断 = 真实损坏，交给调用方报错
			}
			c.truncateTail(recordStart)
			return true
		}
		return false
	}

loop:
	for pos < len(buf) {
		c.walProgressBar(c.loadedRows.Load())
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
				name, ok := takeVarString(buf, &pos)
				if !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog create name")
				}
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
						if tail(recordStart) {
							break loop
						}
						return fmt.Errorf("incomplete catalog checksum")
					}
					if crc32.ChecksumIEEE(buf[recordStart:pos-4]) != expected {
						return fmt.Errorf("checksum mismatch in catalog table %s", name)
					}
				}
				c.db.applyCatalogV2(uint32(id64), meta)
			case 5: // ALTER-ALL：整表替换（新 meta + 全部行，单条记录原子生效）
				id64, ok := readVarUint(buf, &pos)
				if !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog alter id")
				}
				name, ok := takeVarString(buf, &pos)
				if !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog alter name")
				}
				meta, err := parseMetaV2(buf, &pos, name)
				if err != nil {
					if tail(recordStart) {
						break loop
					}
					return err
				}
				rowCount, ok := readVarUint(buf, &pos)
				if !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog alter row count")
				}
				rows := make([]alterRowBytes, 0, rowCount)
				for i := uint64(0); i < rowCount; i++ {
					pk, ok := readI64(buf, &pos)
					if !ok {
						if tail(recordStart) {
							break loop
						}
						return fmt.Errorf("incomplete catalog alter row pk")
					}
					n, ok := readVarUint(buf, &pos)
					if !ok {
						if tail(recordStart) {
							break loop
						}
						return fmt.Errorf("incomplete catalog alter row length")
					}
					if pos+int(n) > len(buf) {
						// 行数据被截断：把游标推到末尾，让 tail 判定为"崩溃残留"
						pos = len(buf)
						if tail(recordStart) {
							break loop
						}
						return fmt.Errorf("incomplete catalog alter row data")
					}
					rows = append(rows, alterRowBytes{pk: pk, data: buf[pos : pos+int(n)]})
					pos += int(n)
				}
				if c.enableCRC {
					expected, ok := readU32(buf, &pos)
					if !ok {
						if tail(recordStart) {
							break loop
						}
						return fmt.Errorf("incomplete catalog checksum")
					}
					if crc32.ChecksumIEEE(buf[recordStart:pos-4]) != expected {
						return fmt.Errorf("checksum mismatch in catalog alter table %s", name)
					}
				}
				c.db.applyCatalogAlterAllV2(uint32(id64), meta, rows)
			case 2: // DROP
				if _, ok := readVarUint(buf, &pos); !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog drop id")
				}
				name, ok := takeVarString(buf, &pos)
				if !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog drop name")
				}
				if c.enableCRC {
					expected, ok := readU32(buf, &pos)
					if !ok {
						if tail(recordStart) {
							break loop
						}
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
				nname, ok := takeVarString(buf, &pos)
				if !ok {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete catalog rename name")
				}
				if c.enableCRC {
					expected, ok := readU32(buf, &pos)
					if !ok {
						if tail(recordStart) {
							break loop
						}
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
		// 仅 [cmd][varint txnID]；行记录为 [cmd][varint tableID][varint txnID][varint pk][varint len][data]；
		// 批量插入为 [cmd][varint tableID][varint txnID][varint count]([varint pk][varint len][data])*count。
		isTxnCmd := cmd == WALBegin || cmd == WALCommit || cmd == WALRollback
		var tableName string
		var txnID uint64
		var pk int64
		var data []byte
		var batchOps []walOp
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
			if cmd == WALInsertBatch {
				// 批量插入：count 组 (pk, data)
				count, okc := readVarUint(buf, &pos)
				if !okc {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete batch count")
				}
				batchOps = make([]walOp, 0, count)
				for i := uint64(0); i < count; i++ {
					pkv, okp := readVarUint(buf, &pos)
					if !okp {
						if tail(recordStart) {
							break loop
						}
						return fmt.Errorf("incomplete batch pk")
					}
					raw, okr := takeVarBytes(buf, &pos)
					if !okr {
						if tail(recordStart) {
							break loop
						}
						return fmt.Errorf("incomplete batch data")
					}
					// 同上：行字节必须拷贝，不能引用启动时的整文件缓冲
					batchOps = append(batchOps, walOp{tableName, WALInsert, int64(pkv), append([]byte(nil), raw...)})
				}
			} else {
				pkv, okp := readVarUint(buf, &pos)
				if !okp {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete pk")
				}
				pk = int64(pkv)
				raw, okr := takeVarBytes(buf, &pos)
				if !okr {
					if tail(recordStart) {
						break loop
					}
					return fmt.Errorf("incomplete record data")
				}
				// 拷贝出记录数据：整个 WAL 在启动时一次性读入内存，若行值直接引用
				// 该缓冲，载入后大文件字节将随行数据永久驻留无法回收。
				data = append([]byte(nil), raw...)
			}
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
		case WALInsertBatch:
			if txnID == 0 {
				for _, op := range batchOps {
					c.pending[tableName] = append(c.pending[tableName], op)
					c.pendingOps++
					c.pendingBytes += int64(len(op.data))
				}
				if c.pendingOps >= replaySegmentOps || c.pendingBytes >= int64(replaySegmentBytes) {
					c.queuePending()
				}
			} else {
				c.pendingTxns[txnID] = append(c.pendingTxns[txnID], batchOps...)
			}
		case WALInsert, WALUpdate, WALDelete:
			if txnID == 0 {
				c.pending[tableName] = append(c.pending[tableName], walOp{tableName, cmd, pk, data})
				c.pendingOps++
				c.pendingBytes += int64(len(data))
				// 攒够一段就投递给应用侧，边解析边回填
				if c.pendingOps >= replaySegmentOps || c.pendingBytes >= int64(replaySegmentBytes) {
					c.queuePending()
				}
			} else {
				c.pendingTxns[txnID] = append(c.pendingTxns[txnID], walOp{tableName, cmd, pk, data})
			}
		case WALBegin:
			c.flushPending()
			c.pendingTxns[txnID] = nil
		case WALCommit:
			// 先把在途的非事务段刷完，否则事务写入可能与先前的自动提交写入乱序
			if len(c.pending) > 0 {
				c.drainPending()
			}
			var curT *Table
			var curName string
			var n int64
			for _, op := range c.pendingTxns[txnID] {
				if curT == nil || op.table != curName {
					curT = c.db.getOrCreateTable(op.table)
					curName = op.table
				}
				if curT != nil {
					c.applyOpOnTable(curT, op.cmd, op.pk, op.data)
					n++
				}
			}
			c.loadedRows.Add(n)
			delete(c.pendingTxns, txnID)
		case WALRollback:
			delete(c.pendingTxns, txnID)
		}
	}
	return nil
}

// parseMetaV2 解析 compact catalog CREATE 记录中表名之后的 meta 部分。
func parseMetaV2(b []byte, pos *int, tableName string) (*TableMeta, error) {
	pk, ok := takeVarString(b, pos)
	if !ok {
		return nil, fmt.Errorf("incomplete pk")
	}

	fieldCount, ok := readVarUint(b, pos)
	if !ok {
		return nil, fmt.Errorf("incomplete field count")
	}
	// 上限防护：字段数来自文件，直接用作 make 容量会被损坏文件诱导巨额分配。
	if fieldCount > maxMetaFields {
		return nil, fmt.Errorf("field count %d exceeds limit %d", fieldCount, maxMetaFields)
	}
	fields := make([]Field, 0, int(fieldCount))
	for i := uint64(0); i < fieldCount; i++ {
		fName, ok := takeVarString(b, pos)
		if !ok {
			return nil, fmt.Errorf("incomplete field name")
		}
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
	if idxCount > maxMetaFields {
		return nil, fmt.Errorf("index count %d exceeds limit %d", idxCount, maxMetaFields)
	}
	indexes := make(map[string]string, int(idxCount))
	for i := uint64(0); i < idxCount; i++ {
		idxName, ok := takeVarString(b, pos)
		if !ok {
			return nil, fmt.Errorf("incomplete index name")
		}
		idxField, ok := takeVarString(b, pos)
		if !ok {
			return nil, fmt.Errorf("incomplete index field")
		}
		indexes[idxName] = idxField
	}
	return &TableMeta{Name: tableName, PK: pk, Fields: fields, Indexes: indexes}, nil
}

// maxMetaFields 单表字段/索引数量上限（防御损坏 WAL 的巨额预分配）。
const maxMetaFields = 4096

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
// 以写时复制方式发布新 meta 使新列生效（旧行按零值解码，向后兼容），否则新建。
func (db *DB) applyCatalogV2(id uint32, meta *TableMeta) {
	walRegRebind(id, meta.Name)
	db.mu.Lock()
	if v, ok := db.tables.Load(meta.Name); ok {
		// 既有表（如 ALTER ADD COLUMN）：整体替换 meta 快照，使新列在 SELECT 解码时生效。
		v.(*Table).setMeta(meta)
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
		tbl := val.(*Table)
		// 写时复制：发布改名后的新 meta 快照，避免原地修改共享的 TableMeta
		newMeta := cloneMeta(tbl.Meta(), newName)
		tbl.setMeta(newMeta)
		db.tables.Delete(oldName)
		db.tables.Store(newName, val)
		// catalog 必须同步改名：DROP / ALTER / 表列表都走 catalog 查找，
		// 旧实现只在"表不在 tables 里"时写 catalog，改名后 catalog 条目直接丢失。
		db.catalog[newName] = newMeta
	} else if m, ok := db.catalog[oldName]; ok {
		db.catalog[newName] = cloneMeta(m, newName)
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
	db.mu.RLock()
	meta, ok := db.catalog[name]
	db.mu.RUnlock()
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
	if err := appendWAL(buf, false); err != nil {
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
	if err := appendWAL(buf, false); err != nil {
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
	if err := appendWAL(buf, false); err != nil {
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
		Changes:      make(map[string]map[int64]txnChange),
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
	// 修复: 不立即解锁 txnMu，防止并发提交/回滚

	// 预收集所有表引用（单次 RLock）
	type tableRef struct {
		name  string
		table *Table
	}
	var tables []tableRef
	seen := make(map[string]bool)
	for tableName := range txn.Changes {
		if seen[tableName] {
			continue
		}
		seen[tableName] = true
	}
	db.mu.RLock()
	for tableName := range seen {
		if val, ok := db.tables.Load(tableName); ok {
			tables = append(tables, tableRef{tableName, val.(*Table)})
		}
	}
	db.mu.RUnlock()

	// 冲突检测：把"当前行版本"与本事务最后一次写入后的版本比对。
	// 语义：同一行有并发写入时，后提交者（版本被别人改过）失败并回滚，
	// 写入最晚的一方胜出——而不是旧实现里事务内写自己的行也必然报冲突。
	// 策略 last_write_wins：跳过校验，后写者直接覆盖（与 MySQL autocommit 一致）。
	// 适用场景：ORM 默认给每条语句套一层隐式事务（GORM 的 SkipDefaultTransaction
	// 就是在关这个）——此时冲突要到 COMMIT 才暴露，驱动的"语句级重试"根本不会触发。
	if db.configSnapshot().TxnConflict != "last_write_wins" {
		for _, tr := range tables {
			changes := txn.Changes[tr.name]
			for pk, ch := range changes {
				currentData, exists := tr.table.pkTree.Get(pk)
				conflict := false
				switch {
				case ch.newVersion == 0: // 本事务删除：提交时该行必须仍不存在
					conflict = exists
				default: // 插入或更新：当前行版本必须正是本事务写出的版本
					conflict = !exists || decodeVersion(currentData) != ch.newVersion
				}
				if conflict {
					txn.Active = false
					delete(db.txns, id)
					db.txnMu.Unlock()
					db.rollbackTxnData(txn)
					return fmt.Errorf("conflict: row %d version changed", pk)
				}
			}
		}
	}

	// 写 WAL COMMIT
	for _, tr := range tables {
		if err := tr.table.writeLog(WALCommit, id, 0, nil); err != nil {
			txn.Active = false
			delete(db.txns, id)
			db.txnMu.Unlock()
			db.rollbackTxnData(txn)
			return err
		}
	}
	// batch 模式：COMMIT 记录入队后立即返回，由 flushLoop 按 flush_interval
	// 定期落盘（可能丢失最后一个刷盘窗口，配置语义如此）。
	// fsync 模式：COMMIT 记录同样异步入队，但提交者在这里登记组提交，并在
	// 释放 txnMu 后等待一次 Flush+Sync。这样多个并发提交可以共享一次 fsync，
	// 而不是每个事务在 txnMu 内单独等盘。
	// 等待在锁外进行：Join 仅登记等待者（微秒级），真正的刷盘等待（毫秒级）
	// 不占 txnMu，让其他事务的冲突检测/落盘登记得以并行——冲突检测本身
	// 已在上方临界区内完成，等待挪出锁外不削弱正确性。
	var done <-chan error
	if walFsync.Load() {
		done = db.groupCommit.Join()
	}

	// 清理事务
	txn.Active = false
	delete(db.txns, id)
	db.txnMu.Unlock()

	if done != nil {
		if err := <-done; err != nil {
			return err
		}
	}
	return nil
}

// rollbackTxnData 回滚事务数据（调用方已从 db.txns 中移除该事务）。
// 只撤销"仍然是本事务写出的那一版"的行：若并发事务已覆盖该行，保持现状，
// 否则回滚会用陈旧快照覆盖别人已提交的数据。
func (db *DB) rollbackTxnData(txn *Transaction) {
	db.txnWriteMu.Lock()
	defer db.txnWriteMu.Unlock()
	for tableName, changes := range txn.Changes {
		val, ok := db.tables.Load(tableName)
		if !ok {
			continue
		}
		table := val.(*Table)
		needRebuild := false
		for pk, ch := range changes {
			current, exists := table.pkTree.Get(pk)
			if ch.newVersion == 0 { // 本事务删除：只有仍缺失时才恢复旧行
				if !exists && ch.old != nil {
					table.pkTree.Set(pk, ch.old)
					needRebuild = true
				}
				continue
			}
			if !exists || decodeVersion(current) != ch.newVersion {
				continue // 已被其他事务改写，不覆盖
			}
			if ch.old == nil {
				table.pkTree.Delete(pk)
			} else {
				table.pkTree.Set(pk, ch.old)
				needRebuild = true
			}
		}
		if needRebuild {
			table.rebuildIndexes()
		}
	}
}

func (db *DB) RollbackTxn(id uint64) error {
	db.txnMu.Lock()
	txn, ok := db.txns[id]
	if !ok || !txn.Active {
		db.txnMu.Unlock()
		return fmt.Errorf("transaction not active")
	}
	txn.Active = false
	delete(db.txns, id)
	db.txnMu.Unlock()

	db.rollbackTxnData(txn)
	return nil
}

// txnApply 在事务上下文中执行一次行写入并记录 old/new 版本。
// txnID == 0（非事务写）直接执行不记录。写与记录在同一把锁下完成，
// 保证提交时"当前版本 == 本事务写出的版本"这一判断不会被并发写交错欺骗。
func (db *DB) txnApply(txnID uint64, t *Table, pk int64, apply func() error) error {
	if txnID == 0 {
		return apply()
	}
	db.txnWriteMu.Lock()
	defer db.txnWriteMu.Unlock()
	old, _ := t.pkTree.Get(pk)
	if err := apply(); err != nil {
		return err
	}
	var newVersion int64
	if cur, ok := t.pkTree.Get(pk); ok {
		newVersion = decodeVersion(cur)
	}
	name := t.Meta().Name
	db.txnMu.Lock()
	if txn, ok := db.txns[txnID]; ok {
		if txn.Changes[name] == nil {
			txn.Changes[name] = make(map[int64]txnChange)
		}
		if prev, ok := txn.Changes[name][pk]; ok {
			old = prev.old // 事务内多次写同一行：回滚目标始终是事务开始时的行
		}
		txn.Changes[name][pk] = txnChange{old: old, newVersion: newVersion}
		txn.Tables[name] = true
	}
	db.txnMu.Unlock()
	return nil
}

func (db *DB) CheckPrivilege(user, table string, perm byte) bool {
	if user == db.rootUser() {
		return true
	}
	return db.privileges.Check(user, table, perm)
}

// rootUser 返回配置中的管理员账号（读加锁：设置页可能并发改写 config）。
func (db *DB) rootUser() string {
	db.cfgMu.RLock()
	defer db.cfgMu.RUnlock()
	return db.config.User
}

// configSnapshot 返回配置的浅拷贝，供请求处理路径读取而不长期持锁。
func (db *DB) configSnapshot() Config {
	db.cfgMu.RLock()
	defer db.cfgMu.RUnlock()
	return *db.config
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
	// 写时复制：构建新 meta 快照整体发布，读者可能正在遍历旧 Fields 切片
	newMeta := cloneMeta(meta, meta.Name)
	newMeta.Fields = append(newMeta.Fields, Field{Name: fieldName, Type: fieldType, Len: fieldLen})
	if err := db.writeCatalog(newMeta); err != nil {
		return err
	}
	db.catalog[tableName] = newMeta
	if v, ok := db.tables.Load(tableName); ok {
		v.(*Table).setMeta(newMeta)
	}
	return nil
}

// Backup
func (db *DB) Backup() error {
	if err := os.MkdirAll(db.backupDir, 0700); err != nil {
		return err
	}
	// 与 Compact 互斥：压缩期间会关闭/改名 WAL 文件，
	// 并发备份会拷到截断（Windows）或陈旧 inode（Linux）
	compactMu.Lock()
	defer compactMu.Unlock()
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
	// 备份含完整业务数据：0600，与 WAL 同权限
	dst, err := os.OpenFile(backupFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
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
		privDst, err := os.OpenFile(fmt.Sprintf("%s/privileges_%s.json", db.backupDir, timestamp), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err == nil {
			defer privDst.Close()
			io.Copy(privDst, privSrc)
		}
	}
	logf(LOG_VERB, "backup created: %s", backupFile)
	return nil
}

// 组提交：只服务 fsync 模式。把并发提交的 COMMIT 合并成一次
// flushWAL（bufio.Flush + file.Sync）；同一时刻只允许一个 flush 在飞，
// 避免前一组还在 fsync 时又排入新的 flush，把 HDD 的磁头来回打断。
type GroupCommitter struct {
	db       *DB
	interval time.Duration // 配置热更新失败时的兜底窗口
	mu       sync.Mutex
	waiters  []chan error
	timer    *time.Timer
	flushing bool
	// adaptive 自适应提交窗口：固定窗口在两种极端下都不划算——
	// 低并发时白等一个窗口才落盘（纯延迟），高并发时窗口太短攒不够批
	// （fsync 次数降不下来）。这里用"上一批攒了多少人 + 一次 fsync 多贵"
	// 反馈调节窗口，等价于 PostgreSQL/Kafka 的自适应组提交。
	adaptive time.Duration
	// winMin/winMax 自适应窗口的上下界，由启动时的设备探测设定：
	// 慢盘（机械盘/网络盘）一次 fsync 就要十几毫秒，窗口该开得更大才攒得起批。
	winMin time.Duration
	winMax time.Duration
}

// 自适应窗口的默认上下界：下界避免无意义空等，上界避免明显放大尾延迟。
const (
	gcWindowMin = 200 * time.Microsecond
	gcWindowMax = 20 * time.Millisecond
)

// setWindowBounds 由设备探测调用，设定窗口上下界与初始窗口。
func (gc *GroupCommitter) setWindowBounds(minW, maxW, initial time.Duration) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if minW > 0 {
		gc.winMin = minW
	}
	if maxW > 0 {
		gc.winMax = maxW
	}
	if initial > 0 {
		gc.adaptive = initial
	}
}

func (gc *GroupCommitter) boundsLocked() (time.Duration, time.Duration) {
	lo, hi := gc.winMin, gc.winMax
	if lo <= 0 {
		lo = gcWindowMin
	}
	if hi <= 0 {
		hi = gcWindowMax
	}
	return lo, hi
}

func NewGroupCommitter(db *DB, interval time.Duration) *GroupCommitter {
	return &GroupCommitter{db: db, interval: interval}
}

// commitWindowLocked 计算本次窗口（调用方须持有 gc.mu）。
func (gc *GroupCommitter) commitWindowLocked() time.Duration {
	base := gc.interval
	if gc.db != nil {
		if d := gc.db.configSnapshot().GroupCommitInterval; d > 0 {
			base = d
		}
	}
	if base <= 0 {
		base = 2 * time.Millisecond
	}
	if gc.adaptive > 0 {
		return gc.adaptive
	}
	return base
}

func (gc *GroupCommitter) commitWindow() time.Duration {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.commitWindowLocked()
}

// observe 用上一批的规模与落盘耗时调整下一批窗口。
//   - 一批攒到很多写者：说明还有并发，窗口加倍能继续摊薄 fsync；
//   - 一批只有 1 个写者：等待纯属加延迟，窗口减半；
//   - 中间规模：小幅放大。
//
// 另外把窗口钳制在"一次 fsync 耗时的数倍"以内——再等下去也不会提升吞吐。
func (gc *GroupCommitter) observe(n int, fsyncCost time.Duration) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	cur := gc.commitWindowLocked()
	var next time.Duration
	switch {
	case n >= 8:
		next = cur * 2
	case n >= 2:
		next = cur + cur/4
	default:
		next = cur / 2
	}
	if fsyncCost > 0 {
		if maxWindow := fsyncCost * 4; next > maxWindow {
			next = maxWindow
		}
	}
	lo, hi := gc.boundsLocked()
	if next < lo {
		next = lo
	}
	if next > hi {
		next = hi
	}
	gc.adaptive = next
}

func (gc *GroupCommitter) Join() <-chan error {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	ch := make(chan error, 1)
	gc.waiters = append(gc.waiters, ch)
	if gc.timer == nil && !gc.flushing {
		gc.timer = time.AfterFunc(gc.commitWindowLocked(), gc.flush)
	}
	return ch
}

func (gc *GroupCommitter) flush() {
	gc.mu.Lock()
	if gc.flushing {
		gc.mu.Unlock()
		return
	}
	waiters := gc.waiters
	if len(waiters) == 0 {
		gc.timer = nil
		gc.mu.Unlock()
		return
	}
	gc.waiters = nil
	gc.timer = nil
	gc.flushing = true
	gc.mu.Unlock()

	begin := time.Now()
	err := gc.db.flushWAL()
	cost := time.Since(begin)
	// 无锁区间内更新窗口：不阻塞等待者唤醒
	gc.observe(len(waiters), cost)

	for _, ch := range waiters {
		ch <- err
		close(ch)
	}

	gc.mu.Lock()
	gc.flushing = false
	if len(gc.waiters) > 0 && gc.timer == nil {
		gc.timer = time.AfterFunc(gc.commitWindowLocked(), gc.flush)
	}
	gc.mu.Unlock()
}
