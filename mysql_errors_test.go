package main

import (
	"fmt"
	"testing"
)

// ==================== A7：MySQL 错误码映射测试 ====================

// TestMySQLErrnoMapping 校验内部错误能映射到正确的 MySQL errno / SQLSTATE。
// ORM 依赖这些码做重试与分支，映射错=迁移后行为错。
func TestMySQLErrnoMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   int
		state  string
	}{
		{"dup-entry", fmt.Errorf("%w: 42", errPKExists), ERDupEntry, "23000"},
		{"deadlock", fmt.Errorf("conflict: row 7 version changed"), ERLockDeadlock, "40001"},
		{"lock-timeout", fmt.Errorf("lock wait timeout exceeded"), ERLockWaitTimeout, "HY000"},
		{"no-such-table", fmt.Errorf("table not found: users"), ERNoSuchTable, "42S02"},
		{"table-exists", fmt.Errorf("table users already exists"), ERTableExistsError, "42S01"},
		{"bad-field", fmt.Errorf("no matching columns"), ERBadFieldError, "42S22"},
		{"column-count", fmt.Errorf("column count mismatch"), ERWrongValueCount, "21S01"},
		{"permission", fmt.Errorf("permission denied"), ERDBAccessDenied, "42000"},
		{"bad-type", fmt.Errorf("primary key must be integer"), ERTruncatedWrongValue, "HY000"},
		{"too-long", fmt.Errorf("value too long for column name"), ERDataTooLong, "22001"},
		{"syntax", fmt.Errorf("unexpected token SELECT"), ERParseError, "42000"},
	}
	for _, c := range cases {
		if got := mysqlErrno(c.err); got != c.code {
			t.Fatalf("%s: errno=%d want %d (err=%v)", c.name, got, c.code, c.err)
		}
		if got := sqlStateFor(c.code); got != c.state {
			t.Fatalf("%s: sqlstate=%s want %s", c.name, got, c.state)
		}
	}
}

// TestMySQLErrTextDupEntry 校验 1062 的报文是 MySQL 的经典格式，
// 客户端/ORM 会用正则去匹配 "Duplicate entry ... for key"。
func TestMySQLErrTextDupEntry(t *testing.T) {
	msg := mysqlErrText(fmt.Errorf("%w: 42", errPKExists))
	want := "Duplicate entry '42' for key 'PRIMARY'"
	if msg != want {
		t.Fatalf("dup entry text = %q want %q", msg, want)
	}
	// 普通错误原样透出
	if got := mysqlErrText(fmt.Errorf("table not found: x")); got != "table not found: x" {
		t.Fatalf("plain text = %q", got)
	}
}

// TestMySQLErrnoNil 校验 nil 错误不会产生错误码（调用方据此判断成功）。
func TestMySQLErrnoNil(t *testing.T) {
	if got := mysqlErrno(nil); got != 0 {
		t.Fatalf("nil errno=%d want 0", got)
	}
	if got := mysqlErrText(nil); got != "" {
		t.Fatalf("nil text=%q want empty", got)
	}
}
