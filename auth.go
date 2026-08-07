package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
)

// ==================== 管理面板认证 ====================

type loginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func (db *DB) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "bad request"})
		return
	}
	// 检查用户库中的用户
	var u *User
	if globalUsers != nil {
		u = globalUsers.Get(req.User)
	}
	matched := false
	if u != nil {
		matched = u.Password == hashPasswd(req.Password)
	}
	if !matched || u == nil {
		time.Sleep(300 * time.Millisecond)
		logf(LOG_WARN, "login FAILED from %s (user=%q)", clientIP(r), req.User)
		writeJSON(w, map[string]interface{}{"ok": false, "error": trMsg(reqLang(r), "bad_credentials")})
		return
	}
	u.LastLogin = time.Now().Unix()
	if globalUsers != nil && globalUsers.Count() > 0 {
		globalUsers.Update(u)
	}
	tok := adminTokens.issue(req.User)
	logf(LOG_OK, "login OK from %s (user=%q)", clientIP(r), req.User)
	writeJSON(w, map[string]interface{}{"ok": true, "token": tok, "user": req.User, "is_admin": u.IsAdmin})
}

func (db *DB) handleLogout(w http.ResponseWriter, r *http.Request) {
	tok := r.Header.Get("Authorization")
	if len(tok) > 7 && tok[:7] == "Bearer " {
		tok = tok[7:]
	}
	adminTokens.revoke(tok)
	writeJSON(w, map[string]interface{}{"ok": true})
}

// adminAuthRequired 包装需要认证的管理接口。
func adminAuthRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("Authorization")
		if len(tok) > 7 && tok[:7] == "Bearer " {
			tok = tok[7:]
		} else if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if _, ok := adminTokens.valid(tok); !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": trMsg(reqLang(r), "session_expired")})
			return
		}
		next(w, r)
	}
}

// requireUser 提取当前登录用户名。
func requireUser(r *http.Request) string {
	tok := r.Header.Get("Authorization")
	if len(tok) > 7 && tok[:7] == "Bearer " {
		tok = tok[7:]
	} else if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	u, _ := adminTokens.valid(tok)
	return u
}

// clientIP 提取请求的来源 IP。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
