package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAclWebFiltering 验证非管理员的库/表级权限在数据管理接口生效。
func TestAclWebFiltering(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if _, _, _, _, err := db.runSQL("CREATE TABLE base_t (id INT, PRIMARY KEY (id))"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := db.runSQL("CREATE DATABASE myapp"); err != nil {
		t.Fatal(err)
	}
	db.setCurDB("myapp")
	if _, _, _, _, err := db.runSQL("CREATE TABLE secret (id INT, PRIMARY KEY (id))"); err != nil {
		t.Fatal(err)
	}
	db.setCurDB("tsumugi")

	// 受限用户：只能看 tsumugi 库，只能看 base_t 表
	globalUsers.Add(&User{
		Username:  "limited",
		Password:  hashPasswd("secret123"),
		IsAdmin:   false,
		Databases: []string{"tsumugi"},
		Tables:    []string{"base_t"},
	})
	tok := adminTokens.issue("limited")
	limitedReq := func(method, path string, body []byte) *http.Request {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		return r
	}

	// 1. tables 列表：只返回 tsumugi 及其表
	rec := httptest.NewRecorder()
	db.handleAdminTables(rec, limitedReq("GET", "/api/admin/tables", nil))
	var d map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &d)
	dbs := d["databases"].([]interface{})
	if len(dbs) != 1 || dbs[0] != "tsumugi" {
		t.Fatalf("limited databases=%v want [tsumugi]", dbs)
	}
	tbls := d["tables"].([]interface{})
	if len(tbls) != 1 || tbls[0].(map[string]interface{})["name"] != "base_t" {
		t.Fatalf("limited tables=%v want [base_t]", tbls)
	}

	// 2. 访问 myapp 库被拒
	rec = httptest.NewRecorder()
	db.handleAdminTables(rec, limitedReq("GET", "/api/admin/tables?db=myapp", nil))
	d = map[string]interface{}{}
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d["ok"] != nil && d["ok"] == false {
		// permission denied
	} else {
		t.Fatalf("expected permission denied for myapp, got %v", d)
	}

	// 3. 查询 myapp.secret 被拒
	rec = httptest.NewRecorder()
	db.handleAdminRows(rec, limitedReq("GET", "/api/admin/rows?table=myapp.secret", nil))
	d = map[string]interface{}{}
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d["ok"] == nil || d["ok"] == true {
		t.Fatalf("expected permission denied for myapp.secret rows, got %v", d)
	}
	rec = httptest.NewRecorder()
	db.handleAdminRows(rec, limitedReq("GET", "/api/admin/rows?table=base_t", nil))
	d = map[string]interface{}{}
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d["ok"] == false {
		t.Fatalf("base_t should be allowed: %v", d)
	}

	// 4. SQL 控制台：写语句被拒，只读放行
	q := func(sql string) map[string]interface{} {
		rec := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"sql": sql})
		db.handleAdminQuery(rec, limitedReq("POST", "/api/admin/query", body))
		var out map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := q("DROP TABLE base_t"); out["ok"] == true {
		t.Fatalf("DROP should be denied: %v", out)
	}
	if out := q("SELECT * FROM base_t"); out["ok"] == false {
		t.Fatalf("SELECT base_t should be allowed: %v", out)
	}
	if out := q("SELECT * FROM myapp.secret"); out["ok"] == true {
		t.Fatalf("SELECT myapp.secret should be denied: %v", out)
	}
	if out := q("SELECT * FROM information_schema.TABLES"); out["ok"] == false {
		t.Fatalf("information_schema should be readable: %v", out)
	}

	t.Log("acl web filtering ok")
}
