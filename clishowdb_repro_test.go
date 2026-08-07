package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCLIShowDBRepro(t *testing.T) {
	cliBin, _ := filepath.Abs("tsumugi-cli.exe")
	if _, err := os.Stat(cliBin); err != nil {
		t.Skip("cli not built")
	}
	db := newTestDB(t)
	defer db.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
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
	a := ln.Addr().String()
	i := lastIndexByte(a, ':')
	host, port := a[:i], a[i+1:]
	run := func(args ...string) string {
		args = append([]string{"-h", host, "-p", port, "-u", "root", "-P", "password"}, args...)
		out, _ := exec.Command(cliBin, args...).CombinedOutput()
		return string(out)
	}
	out := run("-e", "CREATE DATABASE reprodb; SHOW DATABASES;")
	t.Logf("OUTPUT:\n%s", out)
	if !containsStr(out, "reprodb") {
		t.Errorf("reprodb NOT in SHOW DATABASES output")
	}
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}