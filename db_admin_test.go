package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestAdminDBManage 覆盖 /api/admin/db/create 与 /api/admin/db/drop：
// 新建/删除数据库、非法名拒绝、系统内置库保护。
func TestAdminDBManage(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// 新建
	req := httptest.NewRequest("POST", "/api/admin/db/create", bytes.NewReader([]byte(`{"name":"my_db"}`)))
	rec := httptest.NewRecorder()
	db.handleAdminDBCreate(rec, req)
	var j map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &j)
	if j["ok"] != true {
		t.Fatalf("create db: %v", rec.Body.String())
	}
	if !contains(asDBList(j["databases"]), "my_db") {
		t.Fatalf("databases list should include my_db, got %v", j["databases"])
	}

	// 非法名拒绝（避免 SQL 注入经名称入径）
	req = httptest.NewRequest("POST", "/api/admin/db/create", bytes.NewReader([]byte(`{"name":"bad name;DROP TABLE x"}`)))
	rec = httptest.NewRecorder()
	db.handleAdminDBCreate(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &j)
	if j["ok"] == true {
		t.Fatalf("invalid name accepted: %v", rec.Body.String())
	}

	// 删除系统库被拒
	req = httptest.NewRequest("POST", "/api/admin/db/drop", bytes.NewReader([]byte(`{"name":"information_schema"}`)))
	rec = httptest.NewRecorder()
	db.handleAdminDBDrop(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &j)
	if j["ok"] == true {
		t.Fatalf("system database dropped: %v", rec.Body.String())
	}

	// 正常删除
	req = httptest.NewRequest("POST", "/api/admin/db/drop", bytes.NewReader([]byte(`{"name":"my_db"}`)))
	rec = httptest.NewRecorder()
	db.handleAdminDBDrop(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &j)
	if j["ok"] != true {
		t.Fatalf("drop db: %v", rec.Body.String())
	}
}

// asDBList 把 JSON 解码后的 databases 数组断言为 []interface{}，失败返回 nil。
func asDBList(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

func contains(ss []interface{}, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
