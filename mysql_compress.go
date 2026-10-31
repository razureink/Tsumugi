package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"errors"
	"io"
)

// ==================== MySQL 压缩协议（CLIENT_COMPRESS / zlib） ====================
// 只有在客户端握手响应里请求了 CLIENT_COMPRESS（且服务端在握手包中声明了它）
// 时才启用。握手包与认证阶段的包不压缩：压缩从认证 OK 之后的第一个命令包开始，
// 这与 MySQL 文档 "Server finishes the Connection Phase with an OK_Packet" 一致。
//
// 帧格式：[3B 压缩后长度][1B 压缩序号][3B 压缩前长度][数据]
//   - 压缩前长度为 0 表示这一帧的数据没有压缩（小包压缩无收益）；
//   - 一帧可以包含多个 MySQL 包，也可以只包含一个包的一部分，读侧按字节流拼接；
//   - 压缩序号与包序号互相独立：客户端一条命令从 0 开始，服务端响应从 1 开始。

const (
	minCompressLen   = 50       // 小于该长度不压缩（对应 MySQL 的 MIN_COMPRESS_LENGTH）
	maxCompressChunk = 0xffffff // 单帧压缩前长度上限（3 字节字段）
)

var errBadCompressedPacket = errors.New("bad compressed packet")

// mysqlCompReader 读取压缩帧，并向调用方提供解压后的 MySQL 字节流。
// 由于一帧可以只包含半个 MySQL 包，读取必须按字节流而不是按包拼接。
type mysqlCompReader struct {
	br      *bufio.Reader
	seq     *uint8 // 指向 mc.compSeq：记下响应帧应使用的压缩序号
	pending []byte
}

// ReadFull 填满 dst，不足时继续读取并解压后续帧。
func (r *mysqlCompReader) ReadFull(dst []byte) error {
	for len(dst) > 0 {
		if len(r.pending) == 0 {
			if err := r.readFrame(); err != nil {
				return err
			}
			continue
		}
		n := copy(dst, r.pending)
		r.pending = r.pending[n:]
		dst = dst[n:]
	}
	return nil
}

func (r *mysqlCompReader) readFrame() error {
	var hdr [7]byte
	if _, err := io.ReadFull(r.br, hdr[:]); err != nil {
		return err
	}
	clen := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	ulen := int(hdr[4]) | int(hdr[5])<<8 | int(hdr[6])<<16
	// 响应帧的压缩序号 = 客户端最后一帧序号 + 1（与包序号规则一致）
	*r.seq = hdr[3] + 1
	if clen == 0 {
		return errBadCompressedPacket
	}
	body := make([]byte, clen)
	if _, err := io.ReadFull(r.br, body); err != nil {
		return err
	}
	if ulen == 0 {
		// 未压缩帧：数据就是原始字节
		r.pending = body
		return nil
	}
	if ulen > maxCompressChunk {
		return errBadCompressedPacket
	}
	zr, err := zlib.NewReader(bytes.NewReader(body))
	if err != nil {
		return err
	}
	out := make([]byte, ulen)
	if _, err := io.ReadFull(zr, out); err != nil {
		zr.Close()
		return err
	}
	// 必须读到流结束，才能校验 zlib 尾部的 adler32；否则截断数据会被静默接受。
	var extra [1]byte
	if n, err := zr.Read(extra[:]); n != 0 || err != io.EOF {
		zr.Close()
		return errBadCompressedPacket
	}
	if err := zr.Close(); err != nil {
		return err
	}
	r.pending = out
	return nil
}

// mysqlCompWriter 收集待发送的 MySQL 包字节，Flush 时按帧压缩写出。
// 之所以先缓冲再分帧，是为了让一帧尽量容纳多个完整包（减少帧头开销），
// 也避免把 4 字节包头的写拆成独立帧。
type mysqlCompWriter struct {
	w   io.Writer
	seq *uint8 // 指向 mc.compSeq
	buf []byte
}

func (w *mysqlCompWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// Flush 把缓冲的字节切成不超过 0xffffff 的块，逐块压缩后写出。
func (w *mysqlCompWriter) Flush() error {
	for len(w.buf) > 0 {
		n := len(w.buf)
		if n > maxCompressChunk {
			n = maxCompressChunk
		}
		chunk := w.buf[:n]
		w.buf = w.buf[n:]
		if err := w.writeFrame(chunk); err != nil {
			return err
		}
	}
	w.buf = w.buf[:0]
	return nil
}

func (w *mysqlCompWriter) writeFrame(chunk []byte) error {
	payload := chunk
	ulen := 0
	if len(chunk) >= minCompressLen {
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		if _, err := zw.Write(chunk); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		// 压缩后反而更大时按未压缩发送（MySQL 同样处理）
		if zbuf.Len() < len(chunk) {
			payload = zbuf.Bytes()
			ulen = len(chunk)
		}
	}
	var hdr [7]byte
	hdr[0] = byte(len(payload))
	hdr[1] = byte(len(payload) >> 8)
	hdr[2] = byte(len(payload) >> 16)
	hdr[3] = *w.seq
	hdr[4] = byte(ulen)
	hdr[5] = byte(ulen >> 8)
	hdr[6] = byte(ulen >> 16)
	*w.seq++
	if _, err := w.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.w.Write(payload)
	return err
}
