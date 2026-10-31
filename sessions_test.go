package main

import (
	"fmt"
	"testing"
	"time"
)

// ==================== C5 补充：会话注册表 / SHOW PROCESSLIST ====================

// TestSessionRegistryLifecycle 登记 → 更新 → 注销，快照必须反映每一步。
func TestSessionRegistryLifecycle(t *testing.T) {
	before := len(snapshotSessions())

	id := registerSession("native", "alice", "127.0.0.1:1234")
	if got := len(snapshotSessions()); got != before+1 {
		t.Fatalf("登记后会话数=%d want %d", got, before+1)
	}
	touchSession(id, func(sv *sessionView) {
		sv.Command = "Query"
		sv.LastSQL = "SELECT 1"
		sv.DB = "tsumugi"
	})
	var found *sessionView
	for _, sv := range snapshotSessions() {
		if sv.ID == id {
			s := sv
			found = &s
			break
		}
	}
	if found == nil {
		t.Fatal("快照里找不到刚登记的会话")
	}
	if found.User != "alice" || found.Command != "Query" || found.LastSQL != "SELECT 1" || found.DB != "tsumugi" {
		t.Fatalf("会话状态未更新: %+v", *found)
	}
	if found.Proto != "native" {
		t.Fatalf("Proto=%q want native", found.Proto)
	}

	unregisterSession(id)
	for _, sv := range snapshotSessions() {
		if sv.ID == id {
			t.Fatal("注销后仍能查到该会话")
		}
	}
}

// TestProcesslistRows 校验 SHOW PROCESSLIST 的列与行内容。
func TestProcesslistRows(t *testing.T) {
	id := registerSession("mysql", "bob", "10.0.0.5:3306")
	defer unregisterSession(id)
	touchSession(id, func(sv *sessionView) {
		sv.Command = "Query"
		sv.LastSQL = "SELECT * FROM t"
	})

	rows := processlistRows()
	if len(rows) == 0 {
		t.Fatal("PROCESSLIST 没有任何行")
	}
	idx := map[string]int{}
	for i, c := range processlistColumns {
		idx[c] = i
	}
	var hit []interface{}
	for _, r := range rows {
		if r[idx["Id"]].(int64) == int64(id) {
			hit = r
			break
		}
	}
	if hit == nil {
		t.Fatal("PROCESSLIST 里没有刚登记的会话")
	}
	if hit[idx["User"]] != "bob" {
		t.Fatalf("User=%v", hit[idx["User"]])
	}
	if hit[idx["Command"]] != "Query" {
		t.Fatalf("Command=%v", hit[idx["Command"]])
	}
	if hit[idx["Info"]] != "SELECT * FROM t" {
		t.Fatalf("Info=%v", hit[idx["Info"]])
	}
	if hit[idx["State"]] != "mysql" {
		t.Fatalf("State(协议)=%v want mysql", hit[idx["State"]])
	}
}

// TestShowProcesslistSQL 端到端：SHOW PROCESSLIST 必须有真实行，且至少包含本会话。
func TestShowProcesslistSQL(t *testing.T) {
	SetLogMute(true)
	db, err := NewDB(walTestConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	// 模拟一个已登录会话
	id := registerSession("native", "carol", "192.168.1.9:5555")
	defer unregisterSession(id)
	touchSession(id, func(sv *sessionView) { sv.Command = "Sleep" })

	cols, rows, _, _, err := db.runSQL("SHOW PROCESSLIST")
	if err != nil {
		t.Fatalf("SHOW PROCESSLIST: %v", err)
	}
	if len(cols) != len(processlistColumns) {
		t.Fatalf("列数=%d want %d", len(cols), len(processlistColumns))
	}
	if len(rows) == 0 {
		t.Fatal("SHOW PROCESSLIST 返回 0 行（仍是空壳？）")
	}
	found := false
	for _, r := range rows {
		if r[1] == "carol" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("SHOW PROCESSLIST 里找不到 carol: %v", rows)
	}
	_ = fmt.Sprint(rows)
	_ = time.Now()
}
