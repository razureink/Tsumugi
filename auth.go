package main

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	ip := clientIP(r)
	key := loginKey(ip, req.User)
	if blocked, left := loginLimit.Blocked(key); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(left.Seconds())+1))
		writeJSON(w, map[string]interface{}{"ok": false, "error": trMsg(reqLang(r), "bad_credentials")})
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
		loginLimit.Fail(key)
		time.Sleep(300 * time.Millisecond)
		logf(LOG_WARN, "login FAILED from %s (user=%q)", ip, req.User)
		writeJSON(w, map[string]interface{}{"ok": false, "error": trMsg(reqLang(r), "bad_credentials")})
		return
	}
	// 密码库损坏时禁止登录建立新会话（防止损坏后空库被重新 setup 接管前的窗口期）
	if userStoreCorrupt {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "user store corrupt; manual intervention required"})
		return
	}
	loginLimit.Reset(key)
	// LastLogin 落盘有成本（每次登录写一次 users.json）：每分钟最多更新一次
	if touchLastLogin(u) && globalUsers != nil && globalUsers.Count() > 0 {
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

// maxBodyBytes 管理接口请求体上限：所有 handler 都是裸 json 解码，未认证即可发超大包。
const maxBodyBytes = 1 << 20

// metricsAuthorized 校验 /metrics 抓取权限：接受监控令牌（query/header）
// 或管理会话令牌（人工排查用）。
func (db *DB) metricsAuthorized(r *http.Request) bool {
	want := db.configSnapshot().MetricsToken
	if bt := bearerToken(r); bt != "" {
		// 人工排查可用会话令牌；监控系统用 metrics_token（Prometheus authorization 也走 Bearer）
		if _, ok := adminTokens.valid(bt); ok {
			return true
		}
		if want != "" && subtle.ConstantTimeCompare([]byte(bt), []byte(want)) == 1 {
			return true
		}
		return false
	}
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok = r.Header.Get("X-Metrics-Token")
	}
	if tok == "" {
		return false
	}
	return want != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1
}

// setupLocalOnly 首次安装只允许本机/内网来源：避免公网上先到的人把自己注册成管理员。
func setupLocalOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !isPrivateAddr(ip) {
			logf(LOG_WARN, "rejected setup attempt from public address %s", ip)
			writeJSON(w, map[string]interface{}{
				"ok":    false,
				"error": "setup is only allowed from a private/local address",
			})
			return
		}
		next(w, r)
	}
}

// isPrivateAddr 判断地址是否属于回环/私网/链路本地（含 Docker 网桥）。
func isPrivateAddr(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// lastLoginMu/lastLoginAt 记录每个用户上次落盘的 LastLogin 时间戳：
// 登录成功就写一次 users.json 在频繁登录时会变成写放大，这里按用户限流到每分钟一次。
var (
	lastLoginMu sync.Mutex
	lastLoginAt = map[string]int64{}
)

// touchLastLogin 记录本次登录时间；距上次落盘不足 60s 时返回 false（不写盘）。
func touchLastLogin(u *User) bool {
	now := time.Now().Unix()
	lastLoginMu.Lock()
	last := lastLoginAt[u.Username]
	lastLoginAt[u.Username] = now
	lastLoginMu.Unlock()
	if now-last < 60 {
		return false
	}
	u.LastLogin = now
	return true
}

// remoteIP 提取 TCP 连接的来源 IP（失败时返回原样地址）。
func remoteIP(conn net.Conn) string {
	if conn == nil {
		return ""
	}
	addr := conn.RemoteAddr()
	if addr == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}

// trustedProxies 可信反代网段：只有来源命中这些网段时才采信 X-Forwarded-For，
// 否则任何人都能伪造 XFF 绕过登录限速/审计。
var trustedProxies atomic.Value // []*net.IPNet

func setTrustedProxies(specs []string) {
	var list []*net.IPNet
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			list = append(list, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			list = append(list, n)
		} else {
			logf(LOG_WARN, "invalid trusted_proxies entry %q (want CIDR)", s)
		}
	}
	trustedProxies.Store(list)
}

// clientIP 提取请求的来源 IP：默认只用 RemoteAddr；
// 仅当来源属于 trusted_proxies 时才使用 X-Forwarded-For 的第一跳。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if v := trustedProxies.Load(); v != nil {
		if list := v.([]*net.IPNet); len(list) > 0 {
			if ip := net.ParseIP(host); ip != nil {
				for _, n := range list {
					if !n.Contains(ip) {
						continue
					}
					if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
						first := strings.TrimSpace(strings.Split(xff, ",")[0])
						if net.ParseIP(first) != nil {
							return first
						}
					}
					break
				}
			}
		}
	}
	return host
}
