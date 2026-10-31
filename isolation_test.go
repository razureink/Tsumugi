package main

import (
	"bufio"
	"bytes"
	"testing"
)

// ==================== A3：隔离级别语义测试 ====================
// 回归：引擎只提供 READ COMMITTED 语义，但此前客户端 SET REPEATABLE READ
// 会被静默接受（SET 走通用 no-op 路径），应用于是以为拿到了读快照。
// 现在非支持的级别必须明确报错 1235。

// newBareMySQLConn 构造一个不经过握手的 mysqlConn，输出写进内存缓冲。
func newBareMySQLConn() (*mysqlConn, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	mc := &mysqlConn{bw: bufio.NewWriter(buf)}
	return mc, buf
}

// TestIsolationLevelAccepted READ COMMITTED 必须被接受（OK 包 = 0x00 开头）。
func TestIsolationLevelAccepted(t *testing.T) {
	mc, buf := newBareMySQLConn()
	mc.handleSetVar("SET TRANSACTION ISOLATION LEVEL READ COMMITTED")
	if err := mc.bw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if mc.isoLevel != "READ COMMITTED" {
		t.Fatalf("isoLevel=%q want READ COMMITTED", mc.isoLevel)
	}
	got := buf.Bytes()
	// 包结构：[3 字节长度][1 字节 seq][payload...]，payload[0]=0x00 表示 OK
	if len(got) < 5 || got[4] != 0x00 {
		t.Fatalf("expected OK packet, got %x", got)
	}
}

// TestIsolationLevelRejected 非支持级别必须返回 1235，不能静默接受。
func TestIsolationLevelRejected(t *testing.T) {
	cases := []string{
		"SET TRANSACTION ISOLATION LEVEL REPEATABLE READ",
		"SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE",
		"SET TRANSACTION ISOLATION LEVEL READ UNCOMMITTED",
		"SET TRANSACTION ISOLATION LEVEL NONSENSE",
	}
	for _, sql := range cases {
		mc, buf := newBareMySQLConn()
		mc.handleSetVar(sql)
		if err := mc.bw.Flush(); err != nil {
			t.Fatalf("%q flush: %v", sql, err)
		}
		got := buf.Bytes()
		if len(got) < 7 {
			t.Fatalf("%q: response too short: %x", sql, got)
		}
		if got[4] != 0xff {
			t.Fatalf("%q: expected ERR packet, got %x", sql, got)
		}
		code := int(got[5]) | int(got[6])<<8
		if code != 1235 {
			t.Fatalf("%q: errno=%d want 1235", sql, code)
		}
		if mc.isoLevel != "" {
			t.Fatalf("%q: 被拒绝的级别不应生效（isoLevel=%q）", sql, mc.isoLevel)
		}
	}
}

// TestSetAutocommitStillWorks 隔离级别分支不能影响原有的 SET autocommit 处理。
func TestSetAutocommitStillWorks(t *testing.T) {
	mc, buf := newBareMySQLConn()
	mc.handleSetVar("SET autocommit=0")
	if err := mc.bw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if mc.autocommit {
		t.Fatal("autocommit 应为 false")
	}
	if got := buf.Bytes(); len(got) < 5 || got[4] != 0x00 {
		t.Fatalf("expected OK packet, got %x", got)
	}
}
