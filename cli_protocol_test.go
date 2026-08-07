package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestCLIProtocol 验证 CMD_QUERY_SQL 协议端到端（模拟 tsumugi-cli 客户端）。
func TestCLIProtocol(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// 启动原生协议 server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &Server{db: db}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.wg.Add(1)
			go srv.handleConn(conn)
		}
	}()
	defer ln.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	c := newCLIForTest(t, addr, "root", "password")
	defer c.Close()

	// 建库 + 建表 + 插入 + 查询
	assertExec(t, c, "CREATE DATABASE clidb")
	assertExec(t, c, "USE clidb")
	assertExec(t, c, "CREATE TABLE users (id INT, name VARCHAR, age INT, PRIMARY KEY (id))")
	assertExec(t, c, "INSERT INTO users VALUES (1, 'Alice', 30)")
	assertExec(t, c, "INSERT INTO users VALUES (2, 'Bob', 25)")

	cols, rows, _, _, err := c.exec("SELECT * FROM users ORDER BY id")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(cols) != 3 {
		t.Fatalf("cols=%v", cols)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	t.Logf("cols=%v rows=%v", cols, rows)

	// SHOW TABLES
	_, rows, _, _, err = c.exec("SHOW TABLES")
	if err != nil {
		t.Fatalf("show tables: %v", err)
	}
	if len(rows) != 1 || rows[0][0] != "users" {
		t.Fatalf("show tables = %v", rows)
	}

	// 错误 SQL
	if _, _, _, _, err := c.exec("SELECT * FROM not_exist_table"); err == nil {
		t.Fatal("expected error")
	}
	t.Log("protocol e2e ok")
}

// TestCLIManageCommands 验证管理命令：status/compact/backup/set durability。
func TestCLIManageCommands(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &Server{db: db}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.wg.Add(1)
			go srv.handleConn(conn)
		}
	}()
	defer ln.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	c := newCLIForTest(t, addr, "root", "password")
	defer c.Close()

	// backup（root 允许）
	if err := c.simpleCmd(56); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// status 应返回 JSON
	raw, err := c.statusCmd()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(raw, "total_commands") {
		t.Fatalf("status json missing keys: %s", raw)
	}
	t.Logf("status: %.120s...", raw)

	// set durability fsync
	if err := c.setDuraCmd("fsync"); err != nil {
		t.Fatalf("set durability: %v", err)
	}
	if db.config.Durability != DuraFsync {
		t.Fatalf("durability = %s, want %s", db.config.Durability, DuraFsync)
	}
	// 非法模式
	if err := c.setDuraCmd("badmode"); err == nil {
		t.Fatal("expected error for bad durability")
	}

	// compact
	if err := c.simpleCmd(58); err != nil {
		t.Fatalf("compact: %v", err)
	}
	t.Log("manage commands ok")
}

// ---- 测试辅助：复用 CLI 的协议逻辑 ----

type cliTest struct {
	conn net.Conn
}

func newCLIForTest(t *testing.T, addr, user, pass string) *cliTest {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := &cliTest{conn: conn}
	var buf []byte
	buf = append(buf, 10) // CMD_AUTH
	buf = appendU16(buf, uint16(len(user)))
	buf = append(buf, user...)
	buf = appendU16(buf, uint16(len(pass)))
	buf = append(buf, pass...)
	if _, err := conn.Write(buf); err != nil {
		t.Fatal(err)
	}
	r := make([]byte, 1)
	if _, err := io.ReadFull(conn, r); err != nil {
		t.Fatal(err)
	}
	if r[0] != 1 {
		t.Fatalf("auth failed, code=%d", r[0])
	}
	return c
}

func (c *cliTest) Close() { c.conn.Close() }

func (c *cliTest) exec(sql string) ([]string, [][]string, int64, string, error) {
	var buf []byte
	buf = append(buf, 35) // CMD_QUERY_SQL
	buf = appendU16(buf, uint16(len(sql)))
	buf = append(buf, sql...)
	if _, err := c.conn.Write(buf); err != nil {
		return nil, nil, 0, "", err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(c.conn, code); err != nil {
		return nil, nil, 0, "", err
	}
	switch code[0] {
	case 2: // RESP_ERR
		var l uint32
		binary.Read(c.conn, binary.BigEndian, &l)
		b := make([]byte, l)
		io.ReadFull(c.conn, b)
		return nil, nil, 0, "", fmt.Errorf("%s", string(b))
	case 1: // RESP_OK
		var affected uint64
		binary.Read(c.conn, binary.BigEndian, &affected)
		var ml uint16
		binary.Read(c.conn, binary.BigEndian, &ml)
		mb := make([]byte, ml)
		io.ReadFull(c.conn, mb)
		return nil, nil, int64(affected), string(mb), nil
	case 5: // RESP_ROWS
		var ncol uint32
		binary.Read(c.conn, binary.BigEndian, &ncol)
		cols := make([]string, ncol)
		for i := uint32(0); i < ncol; i++ {
			var l uint16
			binary.Read(c.conn, binary.BigEndian, &l)
			b := make([]byte, l)
			io.ReadFull(c.conn, b)
			cols[i] = string(b)
		}
		var nrow uint32
		binary.Read(c.conn, binary.BigEndian, &nrow)
		rows := make([][]string, 0, nrow)
		for i := uint32(0); i < nrow; i++ {
			var vc uint32
			binary.Read(c.conn, binary.BigEndian, &vc)
			row := make([]string, vc)
			for j := uint32(0); j < vc; j++ {
				var l uint32
				binary.Read(c.conn, binary.BigEndian, &l)
				b := make([]byte, l)
				io.ReadFull(c.conn, b)
				row[j] = string(b)
			}
			rows = append(rows, row)
		}
		return cols, rows, 0, "", nil
	}
	return nil, nil, 0, "", fmt.Errorf("unexpected %d", code[0])
}

func assertExec(t *testing.T, c *cliTest, sql string) {
	t.Helper()
	_, _, _, msg, err := c.exec(sql)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	t.Logf("exec %q -> %q", sql, msg)
}

func (c *cliTest) simpleCmd(cmd byte) error {
	if _, err := c.conn.Write([]byte{cmd}); err != nil {
		return err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(c.conn, code); err != nil {
		return err
	}
	if code[0] == 2 { // RESP_ERR
		var l uint32
		binary.Read(c.conn, binary.BigEndian, &l)
		b := make([]byte, l)
		io.ReadFull(c.conn, b)
		return fmt.Errorf("%s", string(b))
	}
	return nil
}

func (c *cliTest) statusCmd() (string, error) {
	if _, err := c.conn.Write([]byte{57}); err != nil {
		return "", err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(c.conn, code); err != nil {
		return "", err
	}
	if code[0] != 3 { // RESP_VALUE
		return "", fmt.Errorf("unexpected %d", code[0])
	}
	var l uint32
	binary.Read(c.conn, binary.BigEndian, &l)
	b := make([]byte, l)
	io.ReadFull(c.conn, b)
	return string(b), nil
}

func (c *cliTest) setDuraCmd(mode string) error {
	var buf []byte
	buf = append(buf, 59) // CMD_SET_DURABILITY
	buf = appendU16(buf, uint16(len(mode)))
	buf = append(buf, mode...)
	if _, err := c.conn.Write(buf); err != nil {
		return err
	}
	code := make([]byte, 1)
	if _, err := io.ReadFull(c.conn, code); err != nil {
		return err
	}
	if code[0] == 2 {
		var l uint32
		binary.Read(c.conn, binary.BigEndian, &l)
		b := make([]byte, l)
		io.ReadFull(c.conn, b)
		return fmt.Errorf("%s", string(b))
	}
	return nil
}
