// 示例客户端：演示如何通过二进制 TCP 协议与 Tsumugi 服务器交互。
//
// 运行方式（先启动服务端：go run .）：
//
//	go run ./examples
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

const (
	CMD_AUTH         = 10
	CMD_CREATE_TABLE = 20
	CMD_INSERT       = 30
	CMD_SELECT       = 31

	RESP_OK     = 1
	RESP_ERR    = 2
	RESP_ROWS   = 5
	RESP_TXN_ID = 6
)

type field struct {
	name string
	typ  byte // 0=int,1=varchar,2=bool
}

func writeStr(w *bytes.Buffer, s string) {
	binary.Write(w, binary.BigEndian, uint16(len(s)))
	w.WriteString(s)
}

func writeIntValue(w *bytes.Buffer, name string, v int64) {
	writeStr(w, name)
	w.WriteByte(0) // TypeInt
	binary.Write(w, binary.BigEndian, v)
}

func writeVarcharValue(w *bytes.Buffer, name, v string) {
	writeStr(w, name)
	w.WriteByte(1) // TypeVarchar
	writeStr(w, v)
}

func readResp(c net.Conn) (byte, error) {
	buf := make([]byte, 1)
	if _, err := io.ReadFull(c, buf); err != nil {
		return 0, err
	}
	return buf[0], nil
}

func readErr(c net.Conn) error {
	var l uint32
	if err := binary.Read(c, binary.BigEndian, &l); err != nil {
		return err
	}
	msg := make([]byte, l)
	if _, err := io.ReadFull(c, msg); err != nil {
		return err
	}
	return fmt.Errorf("%s", string(msg))
}

func auth(c net.Conn) error {
	var buf bytes.Buffer
	buf.WriteByte(CMD_AUTH)
	writeStr(&buf, "root")
	writeStr(&buf, "password")
	if _, err := c.Write(buf.Bytes()); err != nil {
		return err
	}
	r, err := readResp(c)
	if err != nil {
		return err
	}
	if r == RESP_ERR {
		return readErr(c)
	}
	fmt.Println("[ok] AUTH")
	return nil
}

func createTable(c net.Conn) error {
	var buf bytes.Buffer
	buf.WriteByte(CMD_CREATE_TABLE)
	writeStr(&buf, "users")
	binary.Write(&buf, binary.BigEndian, uint16(3))
	writeStr(&buf, "id")
	buf.WriteByte(0) // TypeInt
	binary.Write(&buf, binary.BigEndian, uint32(8))
	writeStr(&buf, "name")
	buf.WriteByte(1) // TypeVarchar
	binary.Write(&buf, binary.BigEndian, uint32(100))
	writeStr(&buf, "age")
	buf.WriteByte(0) // TypeInt
	binary.Write(&buf, binary.BigEndian, uint32(8))
	writeStr(&buf, "id") // 主键
	binary.Write(&buf, binary.BigEndian, uint16(0))
	if _, err := c.Write(buf.Bytes()); err != nil {
		return err
	}
	r, err := readResp(c)
	if err != nil {
		return err
	}
	if r == RESP_ERR {
		return readErr(c)
	}
	fmt.Println("[ok] CREATE TABLE users")
	return nil
}

func insert(c net.Conn, pk int64, name string, age int64) error {
	var buf bytes.Buffer
	buf.WriteByte(CMD_INSERT)
	writeStr(&buf, "users")
	binary.Write(&buf, binary.BigEndian, pk)
	binary.Write(&buf, binary.BigEndian, int64(0))
	binary.Write(&buf, binary.BigEndian, uint16(3))
	writeIntValue(&buf, "id", pk)
	writeVarcharValue(&buf, "name", name)
	writeIntValue(&buf, "age", age)
	if _, err := c.Write(buf.Bytes()); err != nil {
		return err
	}
	r, err := readResp(c)
	if err != nil {
		return err
	}
	if r == RESP_ERR {
		return readErr(c)
	}
	fmt.Printf("[ok] INSERT id=%d name=%s age=%d\n", pk, name, age)
	return nil
}

func selectAll(c net.Conn) error {
	var buf bytes.Buffer
	buf.WriteByte(CMD_SELECT)
	writeStr(&buf, "users")
	binary.Write(&buf, binary.BigEndian, uint16(0)) // 无条件
	buf.WriteByte(0)                                // hasMin
	buf.WriteByte(0)                                // hasMax
	if _, err := c.Write(buf.Bytes()); err != nil {
		return err
	}
	r, err := readResp(c)
	if err != nil {
		return err
	}
	if r == RESP_ERR {
		return readErr(c)
	}
	if r != RESP_ROWS {
		return fmt.Errorf("unexpected resp %d", r)
	}
	var count uint32
	if err := binary.Read(c, binary.BigEndian, &count); err != nil {
		return err
	}
	fmt.Printf("[rows] total=%d\n", count)
	for i := uint32(0); i < count; i++ {
		var l uint32
		if err := binary.Read(c, binary.BigEndian, &l); err != nil {
			return err
		}
		data := make([]byte, l)
		if _, err := io.ReadFull(c, data); err != nil {
			return err
		}
		// 解码：version int64 + expireAt int64 + 各字段
		rr := bytes.NewReader(data)
		var version, expireAt int64
		binary.Read(rr, binary.BigEndian, &version)
		binary.Read(rr, binary.BigEndian, &expireAt)
		var id, age int64
		binary.Read(rr, binary.BigEndian, &id)
		var nameLen uint16
		binary.Read(rr, binary.BigEndian, &nameLen)
		name := make([]byte, nameLen)
		rr.Read(name)
		binary.Read(rr, binary.BigEndian, &age)
		fmt.Printf("  - id=%d name=%s age=%d (version=%d)\n", id, name, age, version)
	}
	return nil
}

func main() {
	addr := "127.0.0.1:9999"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Println("[err]", err)
		os.Exit(1)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := auth(conn); err != nil {
		fmt.Println("[err] AUTH:", err)
		os.Exit(1)
	}
	// 已存在则忽略 CREATE TABLE 错误
	if err := createTable(conn); err != nil {
		fmt.Println("[warn] CREATE TABLE:", err)
	}
	if err := insert(conn, 1, "Alice", 30); err != nil {
		fmt.Println("[err] INSERT:", err)
		os.Exit(1)
	}
	if err := insert(conn, 2, "Bob", 25); err != nil {
		fmt.Println("[err] INSERT:", err)
		os.Exit(1)
	}
	if err := insert(conn, 3, "Carol", 28); err != nil {
		fmt.Println("[err] INSERT:", err)
		os.Exit(1)
	}
	if err := selectAll(conn); err != nil {
		fmt.Println("[err] SELECT:", err)
		os.Exit(1)
	}
	fmt.Println("[done]")
}
