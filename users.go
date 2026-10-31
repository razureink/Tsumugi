package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
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
	// ColumnRead 列级 SELECT 白名单：表名（小写；可带 "db." 前缀）→ 允许读取的列。
	//   - 未配置（nil / 空 map / 表名不存在于 map）= 不限制，保持历史行为；
	//   - 配置后只允许列出的列可读，SELECT *（以及 t.*）一律拒绝；
	//   - 键存在但值为空切片 = 该表任何列都不可读（REVOKE 掉全部列的结果）。
	// 列名比较大小写不敏感。持久化随 users.json 一起落盘。
	ColumnRead map[string][]string `json:"column_read,omitempty"`
	CreatedAt  int64               `json:"created_at"`
	LastLogin  int64               `json:"last_login"`
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

// mustRandom 取密码学随机字节。crypto/rand 失败意味着系统熵源不可用，
// 此时绝不能用退化值（可预测的盐/令牌）继续，直接 panic 由上层 recover。
func mustRandom(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return b
}

func generatePassword() string {
	const chars = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	b := mustRandom(12)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// pwdIterations PBKDF2 迭代次数：单次校验约 30~60ms，GPU 爆破成本提高数个量级。
const pwdIterations = 120000

// pbkdf2SHA256 是 RFC 8018 PBKDF2-HMAC-SHA256 的最小实现。
// 项目坚持零第三方依赖，因此不用 golang.org/x/crypto。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)
	var idx [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		idx[0], idx[1], idx[2], idx[3] = byte(block>>24), byte(block>>16), byte(block>>8), byte(block)
		prf.Write(idx[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// hashPasswd 生成 `pbkdf2$<iter>$<salt-hex>$<hash-hex>` 格式的口令摘要。
// 旧版本是单轮 sha256(salt+pwd)，校验时仍兼容并在成功登录后自动升级。
func hashPasswd(password string) string {
	salt := mustRandom(16)
	dk := pbkdf2SHA256([]byte(password), salt, pwdIterations, 32)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pwdIterations, hex.EncodeToString(salt), hex.EncodeToString(dk))
}

// hashEq 常量时间比较十六进制摘要，避免时序侧信道。
func hashEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// verifyPasswd 校验口令，兼容四种历史格式：
// pbkdf2$..., salt+sha256, 96 字符 salt+hash 连写, 无盐 sha256（最老格式）。
// 校验通过且格式较旧时原地升级为 PBKDF2。
func verifyPasswd(user *User, password string) bool {
	if strings.HasPrefix(user.Password, "pbkdf2$") {
		parts := strings.Split(user.Password, "$")
		if len(parts) != 4 {
			return false
		}
		iter, err := strconv.Atoi(parts[1])
		if err != nil || iter <= 0 || iter > 10_000_000 {
			return false
		}
		salt, err := hex.DecodeString(parts[2])
		if err != nil {
			return false
		}
		want := parts[3]
		got := hex.EncodeToString(pbkdf2SHA256([]byte(password), salt, iter, 32))
		return hashEq(want, got)
	}
	ok := false
	if user.Salt != "" {
		h := sha256.Sum256([]byte(user.Salt + password))
		ok = hashEq(user.Password, hex.EncodeToString(h[:]))
	} else if len(user.Password) == 96 {
		salt := user.Password[:32]
		h := sha256.Sum256([]byte(salt + password))
		if hashEq(user.Password[32:], hex.EncodeToString(h[:])) {
			ok = true
		}
	} else {
		h := sha256.Sum256([]byte(password))
		ok = hashEq(user.Password, hex.EncodeToString(h[:]))
	}
	if !ok {
		return false
	}
	// 成功登录即升级到 PBKDF2（不需要用户改密）。Get 返回副本，
	// 必须 Update 回存储，否则升级只发生在局部变量上。
	user.Salt = ""
	user.Password = hashPasswd(password)
	globalUsers.Update(user)
	return true
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
