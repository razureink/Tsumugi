package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestWebDatabaseFilter 验证 web 数据管理按数据库过滤 + 返回数据库列表。
func TestWebDatabaseFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// 建表：默认库 tsumugi + 虚拟库 myapp
	if _, _, _, _, err := db.runSQL("CREATE TABLE base_t (id INT, PRIMARY KEY (id))"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := db.runSQL("CREATE DATABASE myapp"); err != nil {
		t.Fatal(err)
	}
	db.setCurDB("myapp")
	if _, _, _, _, err := db.runSQL("CREATE TABLE users (id INT, PRIMARY KEY (id))"); err != nil {
		t.Fatal(err)
	}
	db.setCurDB("tsumugi")

	// 全量
	rec := httptest.NewRecorder()
	db.handleAdminTables(rec, httptest.NewRequest("GET", "/api/admin/tables", nil))
	var all map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &all)
	tbls := all["tables"].([]interface{})
	if len(tbls) != 2 {
		t.Fatalf("all tables=%d want 2 (%v)", len(tbls), tbls)
	}
	dbs := all["databases"].([]interface{})
	if len(dbs) == 0 {
		t.Fatal("databases list empty")
	}
	t.Logf("all tables=%d databases=%v", len(tbls), dbs)

	// 按 myapp 过滤
	rec = httptest.NewRecorder()
	db.handleAdminTables(rec, httptest.NewRequest("GET", "/api/admin/tables?db=myapp", nil))
	json.Unmarshal(rec.Body.Bytes(), &all)
	tbls = all["tables"].([]interface{})
	if len(tbls) != 1 || tbls[0].(map[string]interface{})["name"] != "myapp.users" {
		t.Fatalf("myapp tables=%v want [myapp.users]", tbls)
	}

	// 按 tsumugi 过滤（无前缀表）
	rec = httptest.NewRecorder()
	db.handleAdminTables(rec, httptest.NewRequest("GET", "/api/admin/tables?db=tsumugi", nil))
	json.Unmarshal(rec.Body.Bytes(), &all)
	tbls = all["tables"].([]interface{})
	if len(tbls) != 1 || tbls[0].(map[string]interface{})["name"] != "base_t" {
		t.Fatalf("tsumugi tables=%v want [base_t]", tbls)
	}
	t.Log("web db filter ok")
}
