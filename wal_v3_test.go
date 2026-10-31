package main

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// walV3Frames 按 v3 批次边界切分文件（只做长度解析，不做 CRC 校验），返回每批的 [起始偏移, 负载长度]。
func walV3Frames(t *testing.T, data []byte) [][2]int {
	t.Helper()
	var out [][2]int
	pos := len(WALMagic) + 8
	for pos < len(data) {
		start := pos
		var n uint64
		var shift uint
		for {
			if pos >= len(data) {
				t.Fatalf("batch length varint truncated at offset %d", start)
			}
			b := data[pos]
			pos++
			n |= uint64(b&0x7f) << shift
			if b&0x80 == 0 {
				break
			}
			shift += 7
		}
		payload := pos + walBatchCRCBytes
		if n > uint64(len(data)-payload) {
			t.Fatalf("batch at offset %d declares %d bytes, only %d left", start, n, len(data)-payload)
		}
		out = append(out, [2]int{start, int(n)})
		pos = payload + int(n)
	}
	return out
}

// v3 分帧必须在单批上限处切批，且多批文件能完整重放。
func TestWALV3MultiBatchRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	walMustCreate(t, db, "users")
	tbl := db.getTable("users")
	const rows = 200
	for i := int64(1); i <= rows; i++ {
		if err := tbl.Insert(i, map[string]interface{}{"id": i, "name": "u" + itoa(int(i))}, 0, 0, false); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := db.flushWAL(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	db.Close()

	data, err := os.ReadFile(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if v := binary.BigEndian.Uint32(data[len(WALMagic):]); v != WALVersion4 {
		t.Fatalf("WAL version = %d, want %d (upstream+1)", v, WALVersion4)
	}
	frames := walV3Frames(t, data)
	if len(frames) < 2 {
		t.Fatalf("expected multiple batch frames for %d rows, got %d", rows, len(frames))
	}
	for i, fr := range frames {
		if fr[1] > walBatchMaxBytes {
			t.Fatalf("frame %d is %d bytes, exceeds batch limit %d", i, fr[1], walBatchMaxBytes)
		}
	}

	db2, err := NewDB(walTestConfig(dir))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := walRowCount(t, db2, "users"); got != rows {
		t.Fatalf("recovered rows = %d, want %d", got, rows)
	}
	raw, ok := db2.getTable("users").pkTree.Get(rows)
	if !ok {
		t.Fatalf("row %d missing after recovery", rows)
	}
	row, _, _ := decodeRow(db2.getTable("users").Meta(), raw)
	if row["name"] != "u"+itoa(rows) {
		t.Fatalf("row %d name = %v, want u%d", rows, row["name"], rows)
	}
}

// 中间批次 CRC 不匹配是真实损坏，必须报错而不是静默截断（尾部残缺才截断）。
func TestWALV3ChecksumMismatchIsError(t *testing.T) {
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "wal"), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if err := writeWALHeaderV(f, WALVersion3); err != nil {
		t.Fatalf("header: %v", err)
	}
	frame := func(payload []byte, corrupt bool) []byte {
		b := appendVarUint(nil, uint64(len(payload)))
		b = appendU32(b, crc32.ChecksumIEEE(payload))
		if corrupt {
			bad := append([]byte(nil), payload...)
			bad[len(bad)-1] ^= 0xff
			return append(b, bad...)
		}
		return append(b, payload...)
	}
	if _, err := f.Write(frame([]byte("first-batch-payload"), true)); err != nil {
		t.Fatalf("write batch 1: %v", err)
	}
	if _, err := f.Write(frame([]byte("second-batch-payload"), false)); err != nil {
		t.Fatalf("write batch 2: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, err := f.Seek(int64(len(WALMagic)+8), io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	ctx := &walReplayCtx{file: f, headerLen: len(WALMagic) + 8, enableCRC: true}
	err = ctx.loadV3(st.Size())
	if err == nil {
		t.Fatal("corrupt middle batch must fail replay")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
}

// 损坏的批次长度不能让重放按声明值分配巨量内存，也不能 panic。
func TestWALV3AbsurdBatchLength(t *testing.T) {
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "wal"), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if err := writeWALHeaderV(f, WALVersion3); err != nil {
		t.Fatalf("header: %v", err)
	}
	if _, err := f.Write(appendVarUint(nil, 1<<40)); err != nil {
		t.Fatalf("write length: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, err := f.Seek(int64(len(WALMagic)+8), io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	ctx := &walReplayCtx{file: f, headerLen: len(WALMagic) + 8, enableCRC: true}
	err = ctx.loadV3(st.Size())
	// 语义变更说明：声明长度远超文件剩余字节，与"断电写了半帧"是同一形态，
	// 因此按崩溃残留处理——截断尾部并正常启动，而不是报错让进程起不来。
	// 关键是绝不能按声明长度分配内存（下面校验文件已被截断回头部长度）。
	if err != nil {
		t.Fatalf("absurd length should be treated as torn tail, got error: %v", err)
	}
	st2, err := f.Stat()
	if err != nil {
		t.Fatalf("stat after replay: %v", err)
	}
	if got, want := st2.Size(), int64(len(WALMagic)+8); got != want {
		t.Fatalf("file size after replay = %d, want %d (should truncate torn tail)", got, want)
	}
}
