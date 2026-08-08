package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"sync"
)

// ==================== 用户系统 ====================

// User 代表一个 Dashboard 用户。
type User struct {
	Username  string   `json:"username"`
	Password  string   `json:"password"` // sha256 hex
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
		}
	}
	globalUsers = store
}

func (s *UserStore) save() {
	s.mu.RLock()
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	data, _ := json.MarshalIndent(list, "", "  ")
	os.WriteFile(s.path, data, 0644)
}

func (s *UserStore) Get(username string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[username]
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

func (s *UserStore) Delete(username string) bool {
	s.mu.Lock()
	_, exists := s.users[username]
	if exists {
		delete(s.users, username)
	}
	s.mu.Unlock()
	if exists {
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
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

// multiToken 多用户 token 管理。
type multiToken struct {
	mu     sync.Mutex
	tokens map[string]string // token -> username
}

var adminTokens = &multiToken{tokens: make(map[string]string)}

func (mt *multiToken) issue(username string) string {
	b := make([]byte, 24)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	mt.mu.Lock()
	for t, u := range mt.tokens {
		if u == username {
			delete(mt.tokens, t)
		}
	}
	mt.tokens[tok] = username
	mt.mu.Unlock()
	return tok
}

func (mt *multiToken) valid(tok string) (string, bool) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	u, ok := mt.tokens[tok]
	return u, ok
}

func (mt *multiToken) revoke(tok string) {
	mt.mu.Lock()
	delete(mt.tokens, tok)
	mt.mu.Unlock()
}
