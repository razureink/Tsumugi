package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// ==================== 用户系统 ====================

// tokenTTL 会话令牌有效期：过期后需重新登录。
const tokenTTL = 24 * time.Hour

// userStoreCorrupt 标记 users.json 损坏。损坏时禁止重新进入安装向导：
// 否则任何先到达 :10232 的人都能把自己注册成管理员（接管）。
var userStoreCorrupt bool

// User 代表一个 Dashboard 用户。
type User struct {
	Username  string   `json:"username"`
	Password  string   `json:"password"` // sha256(salt+password) hex
	Salt      string   `json:"salt,omitempty"`
	IsAdmin   bool     `json:"is_admin"`
	CanStress bool     `json:"can_stress"`
	CanManage bool     `json:"can_manage"`
	Databases []string `json:"databases"` // 空=全部
	Tables    []string `json:"tables"`    // 空=该库下全部表；值为带前缀的物理表名
	CreatedAt int64    `json:"created_at"`
	LastLogin int64    `json:"last_login"`
}

// UserStore 管理所有用户，持久化到 data/users.json。
type UserStore struct {
	mu    sync.RWMutex
	users map[string]*User
	path  string
	// saveMu 串行化落盘：save 在 RLock 快照后写文件，若无此锁两个并发 save
	// 的 tmp+rename 可能交错（后写覆盖前写尚可，但 tmp 文件互相踩踏）。
	saveMu sync.Mutex
}

var globalUsers *UserStore

func initUserStore(dataDir string) {
	path := dataDir + "/users.json"
	store := &UserStore{users: make(map[string]*User), path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		var list []*User
		if json.Unmarshal(data, &list) == nil {
			for _, u := range list {
				store.users[u.Username] = u
			}
		} else {
			// 损坏的用户库：保留现场并禁止安装向导/登录，等人工处理。
			// 静默当空库会让下一个访问面板的人把自己设成管理员。
			userStoreCorrupt = true
			stamp := time.Now().Format("20060102_150405")
			os.Rename(path, path+".corrupt-"+stamp)
			logf(LOG_ERR, "users.json is corrupt; moved to users.json.corrupt-%s. Setup and login are disabled until the file is fixed.", stamp)
		}
	}
	globalUsers = store
}

// save 原子落盘：tmp + rename + 0600。文件含密码哈希，收紧为仅属主可读。
func (s *UserStore) save() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logf(LOG_ERR, "save users.json: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		logf(LOG_ERR, "rename users.json: %v", err)
	}
}

// Get 返回用户的副本：调用方（verifyPasswd 的哈希迁移、handler 的字段修改）
// 修改副本不会与 store 内对象产生数据竞态；需要落盘时用 Update 原子替换。
func (s *UserStore) Get(username string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.users[username]
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

func (s *UserStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

func (s *UserStore) List() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	return list
}

func (s *UserStore) Add(u *User) {
	s.mu.Lock()
	s.users[u.Username] = u
	s.mu.Unlock()
	s.save()
}

// AddNoSave 仅内存注册（压测临时用户用）：不落盘，进程结束即消失。
func (s *UserStore) AddNoSave(u *User) {
	s.mu.Lock()
	s.users[u.Username] = u
	s.mu.Unlock()
}

func (s *UserStore) Delete(username string) bool {
	s.mu.Lock()
	_, exists := s.users[username]
	if exists {
		delete(s.users, username)
	}
	s.mu.Unlock()
	if exists {
		adminTokens.revokeAll(username)
		s.save()
	}
	return exists
}

func (s *UserStore) Update(u *User) {
	s.mu.Lock()
	s.users[u.Username] = u
	s.mu.Unlock()
	s.save()
}

func generatePassword() string {
	const chars = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 12)
	rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func hashPasswd(password string) string {
	b := make([]byte, 16)
	rand.Read(b)
	salt := hex.EncodeToString(b)
	h := sha256.Sum256([]byte(salt + password))
	return salt + hex.EncodeToString(h[:])
}

// hashEq 常量时间比较十六进制摘要，避免时序侧信道。
func hashEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func verifyPasswd(user *User, password string) bool {
	if user.Salt != "" {
		h := sha256.Sum256([]byte(user.Salt + password))
		return hashEq(user.Password, hex.EncodeToString(h[:]))
	}
	if len(user.Password) == 96 {
		salt := user.Password[:32]
		h := sha256.Sum256([]byte(salt + password))
		if hashEq(user.Password[32:], hex.EncodeToString(h[:])) {
			user.Salt = salt
			user.Password = user.Password[32:]
			globalUsers.save()
			return true
		}
	}
	h := sha256.Sum256([]byte(password))
	if hashEq(user.Password, hex.EncodeToString(h[:])) {
		salt := make([]byte, 16)
		rand.Read(salt)
		user.Salt = hex.EncodeToString(salt)
		h2 := sha256.Sum256([]byte(user.Salt + password))
		user.Password = hex.EncodeToString(h2[:])
		globalUsers.save()
		return true
	}
	return false
}

// tokenEntry 带过期时间的会话令牌。
type tokenEntry struct {
	user    string
	expires time.Time
}

// multiToken 多用户 token 管理：单用户单活动会话 + TTL 过期。
type multiToken struct {
	mu     sync.Mutex
	tokens map[string]tokenEntry // token -> entry
}

var adminTokens = &multiToken{tokens: make(map[string]tokenEntry)}

func (mt *multiToken) issue(username string) string {
	b := make([]byte, 24)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	mt.mu.Lock()
	for t, e := range mt.tokens {
		if e.user == username {
			delete(mt.tokens, t)
		}
	}
	mt.tokens[tok] = tokenEntry{user: username, expires: time.Now().Add(tokenTTL)}
	mt.mu.Unlock()
	return tok
}

func (mt *multiToken) valid(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	mt.mu.Lock()
	defer mt.mu.Unlock()
	e, ok := mt.tokens[tok]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expires) {
		delete(mt.tokens, tok)
		return "", false
	}
	return e.user, true
}

func (mt *multiToken) revoke(tok string) {
	mt.mu.Lock()
	delete(mt.tokens, tok)
	mt.mu.Unlock()
}

// revokeAll 吊销某用户的全部令牌（删除用户 / 改密后调用）。
func (mt *multiToken) revokeAll(username string) {
	mt.mu.Lock()
	for t, e := range mt.tokens {
		if e.user == username {
			delete(mt.tokens, t)
		}
	}
	mt.mu.Unlock()
}

// validUser 返回 token 对应的用户对象；用户已被删除时令牌立即失效。
func (mt *multiToken) validUser(tok string) *User {
	name, ok := mt.valid(tok)
	if !ok {
		return nil
	}
	return globalUsers.Get(name)
}
