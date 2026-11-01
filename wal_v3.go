package main

// ==================== WAL v3：分帧 + 批量校验 + 流式预读 ====================
// v1/v2 的记录首尾相接、没有长度前缀，因此重放必须把**整个文件**读进内存
// 才能切分记录（旧实现 make([]byte, fileSize)，几个 GB 的 WAL 直接 OOM）。
//
// v3 在每个批次前加长度与校验和：
//
//	[varint batchLen][u32 crc32(payload)][payload]
//
// payload 仍是一串 v1/v2 记录（编码完全不变），因此：
//   - 读侧知道批次边界 → 按批流式读取，内存只与"最大批次"有关；
//   - 一次 CRC 覆盖整批记录 → 批量校验，而不是逐条记录各算一次；
//   - 记录级 CRC 仍保留在 payload 里，作为纵深防御（成本已计入批次校验）。
// 崩溃残留在最后一批时按批次边界截断（与旧行为一致）。

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync/atomic"
)

const (
	// WALVersion3 远端仓库（razureink/Tsumugi）的分帧格式版本。仍可读取。
	WALVersion3 = 3
	// WALVersion4 本项目格式版本 = 远端版本 + 1。
	// 相比远端 v3，本版本在 payload 中新增了批量插入记录（WALInsertBatch），
	// 因此用独立版本号标识，避免与远端写出的 WAL 混淆。
	// 读取侧同时兼容 v1/v2/v3/v4；写入一律用 v4。
	WALVersion4 = WALVersion3 + 1
	// WALVersion5 当前写入版本 = v4 + 1。
	// 分帧格式与 v3/v4 完全相同（同一套 loadV3），差异在记录语义：
	// A5 新增 DATE/DATETIME/DECIMAL/TEXT/BLOB 列类型，并把 VARCHAR 的比较与
	// 索引键改成 utf8mb4_general_ci 折叠。旧版本代码读到这些行会按错误的
	// 列类型解码、按未折叠的索引键查找，因此必须用新版本号把文件标记出来。
	// 读取侧兼容 1/2/3/4/5；写入（新建文件、Compact 重写）一律用 v5。
	WALVersion5 = WALVersion4 + 1
	// WALVersion6 当前写入版本 = v5 + 1：**帧级压缩**。
	// 分帧布局仍是 [varint len][u32 crc][payload]，但 v6 把长度 varint 的
	// 最低位当作压缩标志：存盘长度 = (varint>>1)，最低位 1 表示 payload 是
	// deflate 流（内容为 [varint 原始长度][压缩字节]）。
	// 这样做的原因：极端设备（机械盘/SD 卡/U 盘）的瓶颈是落盘字节与 fsync，
	// 压缩后写入量通常降到 1/2~1/4，代价是 CPU。默认 auto 只在"慢盘 + 多核"
	// 时开启；1 核设备默认关闭，避免 CPU 成为新瓶颈。
	// 读取侧兼容 1~6；新建文件与 Compact 写 v6；v5 旧文件保持原帧布局追加。
	WALVersion6 = WALVersion5 + 1
	// 读取侧预读缓冲（不影响单批内存上限）
	walReadAheadBytes = 1 << 20
	// 批次头的固定开销：4 字节 CRC
	walBatchCRCBytes = 4
	// 压缩阈值：小于这个大小的批不值得压（deflate 头开销占比太高）
	walCompressMinBytes = 512
	// 单帧解压后的上限，防御解压炸弹
	walFrameMaxPayload = 64 << 20
)

// walIsV6 当前 WAL 文件是否使用 v6 帧语义（长度左移 + 压缩标志）。
// 由 loadWAL/新建文件/Compact 设置；写协程与 Compact 都读它。
var walIsV6 atomic.Bool

// walCompress 是否压缩新写入的批。由设备探测按策略设置（也可配置强制）。
var walCompress atomic.Bool

// walVersion 当前 WAL 文件头版本。复制拉取时通过 X-WAL-Version 告知从库，
// 从库据此决定帧长度是否带压缩标志位。
var walVersion atomic.Uint32

// 写入侧单批上限：超过就切批，避免一次攒出巨大缓冲。
// 默认值面向普通 SSD；启动时的设备探测会按落盘延迟/内存压力调整
// （慢盘用更大批次减少帧头与系统调用，小内存用更小批次限制峰值占用）。
var (
	walBatchMaxBytes   = 1 << 20 // 1MB
	walBatchMaxRecords = 256
)

// appendFrameLen 写帧长度前缀。v6 把压缩标志放在 varint 最低位，
// 其余版本写原始长度（保持 v3/v4/v5 完全不变）。
func appendFrameLen(dst []byte, storedLen int, compressed, v6 bool) []byte {
	v := uint64(storedLen)
	if v6 {
		v <<= 1
		if compressed {
			v |= 1
		}
	}
	return appendVarUint(dst, v)
}

// decodeFrameLen 解析帧长度前缀，返回存盘长度与压缩标志。
func decodeFrameLen(raw uint64, v6 bool) (int, bool, error) {
	if !v6 {
		if raw > 1<<31 {
			return 0, false, fmt.Errorf("invalid WAL batch length %d", raw)
		}
		return int(raw), false, nil
	}
	compressed := raw&1 == 1
	n := raw >> 1
	if n > 1<<31 {
		return 0, false, fmt.Errorf("invalid WAL frame length %d", n)
	}
	return int(n), compressed, nil
}

// compressWALPayload 压缩一批记录。返回 nil,false 表示"不值得压"，
// 调用方应写原始 payload——对不可压缩数据（已压缩的 BLOB、随机字节）
// 不做无效功。
func compressWALPayload(src []byte) ([]byte, bool) {
	if len(src) < walCompressMinBytes {
		return nil, false
	}
	var buf bytes.Buffer
	buf.Grow(len(src)/2 + 16)
	zw, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		return nil, false
	}
	if _, err := zw.Write(src); err != nil {
		zw.Close()
		return nil, false
	}
	if err := zw.Close(); err != nil {
		return nil, false
	}
	// 必须覆盖 origLen 前缀与 deflate 固定开销，否则原样写更划算
	if buf.Len()+8 >= len(src) {
		return nil, false
	}
	out := make([]byte, 0, buf.Len()+8)
	out = appendVarUint(out, uint64(len(src)))
	out = append(out, buf.Bytes()...)
	return out, true
}

// decompressWALPayload 解压一帧：[varint 原始长度][deflate 字节]。
// 长度严格校验：既防解压炸弹，也把"声明长度与实际不符"当损坏处理。
func decompressWALPayload(p []byte) ([]byte, error) {
	pos := 0
	n, ok := readVarUint(p, &pos)
	if !ok || n == 0 || n > walFrameMaxPayload {
		return nil, fmt.Errorf("invalid compressed frame length %d", n)
	}
	fr := flate.NewReader(bytes.NewReader(p[pos:]))
	defer fr.Close()
	out := make([]byte, n)
	if _, err := io.ReadFull(fr, out); err != nil {
		return nil, fmt.Errorf("inflate WAL frame: %w", err)
	}
	// 确认流恰好结束：多出一个字节说明声明长度或数据已损坏。
	var one [1]byte
	if m, _ := fr.Read(one[:]); m != 0 {
		return nil, fmt.Errorf("compressed WAL frame longer than declared %d bytes", n)
	}
	return out, nil
}

// walFramed 当前 WAL 文件是否是 v3 分帧格式（writer 与 Compact 会切换）。
var walFramed atomic.Bool

// writeWALHeaderV 写文件头：magic(7) + version(4) + reserved(4)。
func writeWALHeaderV(f *os.File, version uint32) error {
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := f.Write([]byte(WALMagic)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.BigEndian, version); err != nil {
		return err
	}
	if err := binary.Write(f, binary.BigEndian, uint32(0)); err != nil {
		return err
	}
	return f.Sync()
}

// walHeaderVersion 读取并校验文件头，返回版本号。
func walHeaderVersion(f *os.File) (uint32, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return 0, err
	}
	magic := make([]byte, len(WALMagic))
	if _, err := io.ReadFull(f, magic); err != nil {
		return 0, err
	}
	if string(magic) != WALMagic {
		return 0, fmt.Errorf("invalid WAL magic, expected %s", WALMagic)
	}
	var version uint32
	if err := binary.Read(f, binary.BigEndian, &version); err != nil {
		return 0, err
	}
	var reserved uint32
	if err := binary.Read(f, binary.BigEndian, &reserved); err != nil {
		return 0, err
	}
	return version, nil
}

// loadV3 按批流式重放：内存上限 = 最大单批，而不是整个文件。
func (c *walReplayCtx) loadV3(fileSize int64) error {
	c.totalRows = fileSize / 45 // 进度条分母：按平均每行 45 字节粗估
	br := bufio.NewReaderSize(c.file, walReadAheadBytes)
	offset := int64(c.headerLen)
	for {
		batchStart := offset
		n, nbytes, err := readVarUintFromReader(br)
		if err == io.EOF && nbytes == 0 {
			break // 正常结束
		}
		if err != nil {
			// 批次头被截断：按崩溃残留处理
			c.truncateAt(batchStart)
			break
		}
		// v6 帧长度最低位是压缩标志；v3/v4/v5 是原始长度。
		storedLen := n
		compressed := false
		if c.walV6 {
			compressed = n&1 == 1
			storedLen = n >> 1
		}
		// 崩溃残留优先判定：声明长度超出文件剩余字节，说明这一批没写完就被断电了。
		// 必须先按"剩余字节"判断（而不是先比文件总长），否则断电产生的半帧会被
		// 误判成"损坏头"直接报错，导致进程再也起不来——这正是崩溃一致性测试
		// 抓到的场景。判断发生在分配之前，因此不会因为巨大长度而 OOM。
		remaining := fileSize - batchStart - int64(nbytes)
		if remaining < walBatchCRCBytes || storedLen > uint64(remaining-walBatchCRCBytes) {
			c.truncateAt(batchStart)
			break
		}
		// 绝对上限：到此长度已可安全分配，只剩明显不合理的值才当损坏处理
		if storedLen > 1<<31 {
			return fmt.Errorf("invalid WAL batch length %d at offset %d", storedLen, batchStart)
		}
		// 每批单独分配 payload：段式并行重放会把本批记录的 data 切片交给
		// 后台应用 goroutine，复用同一底层数组会让下一批覆盖尚未应用的数据。
		payload := make([]byte, storedLen)
		var crcBuf [walBatchCRCBytes]byte
		if _, err := io.ReadFull(br, crcBuf[:]); err != nil {
			c.truncateAt(batchStart)
			break
		}
		if _, err := io.ReadFull(br, payload); err != nil {
			c.truncateAt(batchStart)
			break
		}
		offset += int64(nbytes) + walBatchCRCBytes + int64(storedLen)
		want := binary.BigEndian.Uint32(crcBuf[:])
		// CRC 覆盖**存盘字节**（压缩后的流），因此先校验再解压。
		if c.enableCRC && crc32.ChecksumIEEE(payload) != want {
			if offset >= fileSize {
				// 最后一批校验不过：视为写了一半的崩溃残留
				c.truncateAt(batchStart)
				break
			}
			return fmt.Errorf("checksum mismatch in WAL batch at offset %d", batchStart)
		}
		if compressed {
			inflated, derr := decompressWALPayload(payload)
			if derr != nil {
				if offset >= fileSize {
					// 最后一帧解压失败：同样按崩溃残留截断
					c.truncateAt(batchStart)
					break
				}
				return fmt.Errorf("WAL batch at offset %d: %w", batchStart, derr)
			}
			payload = inflated
		}
		c.buf = payload
		c.baseOffset = batchStart + int64(nbytes) + walBatchCRCBytes
		c.atEOF = offset >= fileSize
		if err := c.parseV2(); err != nil {
			return err
		}
		if c.atEOF {
			break
		}
	}
	return nil
}

// walBatchWriter 把连续记录攒成 v3 批次写入底层 bufio.Writer。
// Compact 重写 WAL 时同样必须产出分帧格式，否则新文件无法按批重放。
// v6=true 时使用 v6 帧语义（长度最低位为压缩标志），并可按需压缩。
type walBatchWriter struct {
	w        *bufio.Writer
	batch    []byte
	count    int
	err      error
	v6       bool
	compress bool
}

// Write 追加一条记录，达到单批上限时自动切批。
func (b *walBatchWriter) Write(rec []byte) {
	if b.err != nil {
		return
	}
	b.batch = append(b.batch, rec...)
	b.count++
	if len(b.batch) >= walBatchMaxBytes || b.count >= walBatchMaxRecords {
		b.flushBatch()
	}
}

// flushBatch 写出一批：[varint len(<<1|compressed)][u32 crc][payload]。
func (b *walBatchWriter) flushBatch() {
	if len(b.batch) == 0 {
		b.count = 0
		return
	}
	if b.err != nil {
		b.batch = b.batch[:0]
		b.count = 0
		return
	}
	stored := b.batch
	compressed := false
	if b.v6 && b.compress {
		if c, ok := compressWALPayload(b.batch); ok {
			stored = c
			compressed = true
		}
	}
	var head [16]byte
	h := appendFrameLen(head[:0], len(stored), compressed, b.v6)
	h = appendU32(h, crc32.ChecksumIEEE(stored))
	if _, err := b.w.Write(h); err != nil {
		b.err = err
	}
	if _, err := b.w.Write(stored); err != nil && b.err == nil {
		b.err = err
	}
	b.batch = b.batch[:0]
	b.count = 0
}

// Flush 写完残留批次并冲刷 bufio；返回首个错误。
func (b *walBatchWriter) Flush() error {
	b.flushBatch()
	if b.err != nil {
		return b.err
	}
	return b.w.Flush()
}

// truncateAt 把 WAL 截断到指定偏移（崩溃残留清理）。
func (c *walReplayCtx) truncateAt(offset int64) {
	if err := c.file.Truncate(offset); err != nil {
		logf(LOG_ERR, "truncate WAL to %d failed: %v", offset, err)
		return
	}
	if _, err := c.file.Seek(0, io.SeekEnd); err != nil {
		logf(LOG_ERR, "seek WAL after truncate failed: %v", err)
	}
	logf(LOG_VERB, "discarded incomplete WAL tail at offset %d", offset)
}

// readVarUintFromReader 从流里读 LEB128 变长整数。
// 返回 (值, 消耗字节数, 错误)：nbytes==0 且 err==io.EOF 表示流已正常结束。
func readVarUintFromReader(br *bufio.Reader) (uint64, int, error) {
	var v uint64
	var shift uint
	for i := 0; i < 10; i++ {
		b, err := br.ReadByte()
		if err != nil {
			if i == 0 && err == io.EOF {
				return 0, 0, io.EOF
			}
			return 0, i, err
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, i + 1, nil
		}
		shift += 7
	}
	return 0, 10, fmt.Errorf("varint too long in WAL batch header")
}
