package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIBinary 用真实 tsumugi-cli 二进制执行文档示例流程。
// 需要先 go build ./cmd/tsumugi-cli。
func TestCLIBinary(t *testing.T) {
	cliBin := "tsumugi-cli.exe"
	absBin, err := filepath.Abs(cliBin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(absBin); err != nil {
		t.Skip("tsumugi-cli.exe not built")
	}
	cliBin = absBin

	db := newTestDB(t)
	defer db.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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
	addr := ln.Addr().String()
	i := strings.LastIndex(addr, ":")
	host, port := addr[:i], addr[i+1:]

	run := func(args ...string) (string, error) {
		args = append([]string{"-h", host, "-p", port, "-u", "root", "-P", "password"}, args...)
		cmd := exec.Command(cliBin, args...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("-e", "CREATE DATABASE clidb; USE clidb; CREATE TABLE users (id INT, name VARCHAR, age INT, PRIMARY KEY (id)); INSERT INTO users VALUES (1, 'Alice', 30); INSERT INTO users VALUES (2, 'Bob', 25); SELECT * FROM users ORDER BY id;")
	if err != nil {
		t.Fatalf("cli error: %v\n%s", err, out)
	}
	t.Logf("CLI output:\n%s", out)

	if !strings.Contains(out, "Alice") || !strings.Contains(out, "Bob") {
		t.Fatalf("missing data rows:\n%s", out)
	}
	if !strings.Contains(out, "rows in set") {
		t.Fatalf("missing result table:\n%s", out)
	}
}
