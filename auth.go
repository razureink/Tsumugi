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
		matched = verifyPasswd(u, req.Password)
	}
	if !matched || u == nil {
		time.Sleep(300 * time.Millisecond)
		logf(LOG_WARN, "login FAILED from %s (user=%q)", clientIP(r), req.User)
		writeJSON(w, map[string]interface{}{"ok": false, "error": trMsg(reqLang(r), "bad_credentials")})
		return
	}
	// 密码库损坏时禁止登录建立新会话（防止损坏后空库被重新 setup 接管前的窗口期）
	if userStoreCorrupt {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "user store corrupt; manual intervention required"})
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
	adminTokens.revoke(bearerToken(r))
	writeJSON(w, map[string]interface{}{"ok": true})
}

// bearerToken 仅从 Authorization: Bearer 头提取令牌。
// 不再接受 URL query 参数：令牌会落入访问日志 / 浏览器历史 / Referer，构成持久凭据泄漏。
func bearerToken(r *http.Request) string {
	tok := r.Header.Get("Authorization")
	if len(tok) > 7 && strings.EqualFold(tok[:7], "Bearer ") {
		return tok[7:]
	}
	return ""
}

// adminAuthRequired 包装需要认证的管理接口。
func adminAuthRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminTokens.valid(bearerToken(r)); !ok {
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
	u, _ := adminTokens.valid(bearerToken(r))
	return u
}

// clientIP 提取请求的来源 IP。
// 仅在显式配置了可信反代时才应信任 XFF；此处默认只用 RemoteAddr，避免伪造日志来源。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
