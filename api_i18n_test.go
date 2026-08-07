package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIi18n(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", db.handleLogin)
	mux.HandleFunc("/api/admin/settings", adminAuthRequired(func(w http.ResponseWriter, r *http.Request) {
		db.handleSettingsGet(w, r)
	}))

	// 登录错误 - 中文
	req := httptest.NewRequest("POST", "/api/login", bytes.NewReader([]byte(`{"user":"root","password":"wrong"}`)))
	req.Header.Set("X-Lang", "zh")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var j map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &j)
	if j["error"] != "用户名或密码错误" {
		t.Fatalf("zh login err = %v", j["error"])
	}
	t.Logf("zh: %v", j["error"])

	// 登录错误 - 英文
	req = httptest.NewRequest("POST", "/api/login", bytes.NewReader([]byte(`{"user":"root","password":"wrong"}`)))
	req.Header.Set("X-Lang", "en")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &j)
	if j["error"] != "invalid username or password" {
		t.Fatalf("en login err = %v", j["error"])
	}
	t.Logf("en: %v", j["error"])

	// 未认证 - 中文
	req = httptest.NewRequest("GET", "/api/admin/settings", nil)
	req.Header.Set("X-Lang", "zh")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &j)
	if j["error"] != "未登录或会话已过期" {
		t.Fatalf("zh 401 = %v", j["error"])
	}
	t.Logf("zh401: %v", j["error"])

	// 未认证 - 英文
	req = httptest.NewRequest("GET", "/api/admin/settings", nil)
	req.Header.Set("X-Lang", "en")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &j)
	if j["error"] != "not logged in or session expired" {
		t.Fatalf("en 401 = %v", j["error"])
	}
	t.Logf("en401: %v", j["error"])
}