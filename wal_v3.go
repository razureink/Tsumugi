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
	// 读取侧预读缓冲（不影响单批内存上限）
	walReadAheadBytes = 1 << 20
	// 批次头的固定开销：4 字节 CRC
	walBatchCRCBytes = 4
)

// 写入侧单批上限：超过就切批，避免一次攒出巨大缓冲。
// 默认值面向普通 SSD；启动时的设备探测会按落盘延迟/内存压力调整
// （慢盘用更大批次减少帧头与系统调用，小内存用更小批次限制峰值占用）。
var (
	walBatchMaxBytes   = 1 << 20 // 1MB
	walBatchMaxRecords = 256
)

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
		// 崩溃残留优先判定：声明长度超出文件剩余字节，说明这一批没写完就被断电了。
		// 必须先按"剩余字节"判断（而不是先比文件总长），否则断电产生的半帧会被
		// 误判成"损坏头"直接报错，导致进程再也起不来——这正是崩溃一致性测试
		// 抓到的场景。判断发生在分配之前，因此不会因为巨大长度而 OOM。
		remaining := fileSize - batchStart - int64(nbytes)
		if remaining < walBatchCRCBytes || n > uint64(remaining-walBatchCRCBytes) {
			c.truncateAt(batchStart)
			break
		}
		// 绝对上限：到此长度已可安全分配，只剩明显不合理的值才当损坏处理
		if n > 1<<31 {
			return fmt.Errorf("invalid WAL batch length %d at offset %d", n, batchStart)
		}
		// 每批单独分配 payload：段式并行重放会把本批记录的 data 切片交给
		// 后台应用 goroutine，复用同一底层数组会让下一批覆盖尚未应用的数据。
		payload := make([]byte, n)
		var crcBuf [walBatchCRCBytes]byte
		if _, err := io.ReadFull(br, crcBuf[:]); err != nil {
			c.truncateAt(batchStart)
			break
		}
		if _, err := io.ReadFull(br, payload); err != nil {
			c.truncateAt(batchStart)
			break
		}
		offset += int64(nbytes) + walBatchCRCBytes + int64(n)
		want := binary.BigEndian.Uint32(crcBuf[:])
		if c.enableCRC && crc32.ChecksumIEEE(payload) != want {
			if offset >= fileSize {
				// 最后一批校验不过：视为写了一半的崩溃残留
				c.truncateAt(batchStart)
				break
			}
			return fmt.Errorf("checksum mismatch in WAL batch at offset %d", batchStart)
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
type walBatchWriter struct {
	w     *bufio.Writer
	batch []byte
	count int
	err   error
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

// flushBatch 写出一批：[varint len][u32 crc][payload]。
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
	var head [16]byte
	h := appendVarUint(head[:0], uint64(len(b.batch)))
	h = appendU32(h, crc32.ChecksumIEEE(b.batch))
	if _, err := b.w.Write(h); err != nil {
		b.err = err
	}
	if _, err := b.w.Write(b.batch); err != nil && b.err == nil {
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
