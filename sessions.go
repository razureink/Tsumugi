package main

// ==================== 会话注册表（SHOW PROCESSLIST 的数据源） ====================
// 之前 SHOW PROCESSLIST 只返回列名、没有任何行——运维看不到"谁在跑什么"，
// 排查锁等待/长事务时完全瞎猜。
//
// 注册表同时收录原生协议与 MySQL 协议的连接：两者是两套独立的连接结构，
// 所以这里用协议无关的 View 结构，由各自在建立/关闭连接时登记。
// 容量固定（环形 ID 单调递增），不保存连接对象本身，避免阻止 GC。

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// sessionView 一条正在运行的会话快照。
type sessionView struct {
	ID      uint64
	Proto   string // native / mysql
	User    string
	Host    string
	DB      string
	Command string // Sleep / Query
	Started time.Time
	// LastSQL / LastAt 最近一条语句（Sleep 状态下展示上一条）
	LastSQL string
	LastAt  time.Time
}

type sessionRegistry struct {
	mu   sync.Mutex
	m    map[uint64]*sessionView
	seq  atomic.Uint64
	max  int
	warn bool // 超过上限时只记一次日志
}

const sessionRegistryMax = 4096

var globalSessions = &sessionRegistry{m: make(map[uint64]*sessionView), max: sessionRegistryMax}

// registerSession 登记一条连接，返回会话 ID（用于后续更新/注销）。
func registerSession(proto, user, host string) uint64 {
	id := globalSessions.seq.Add(1)
	sv := &sessionView{ID: id, Proto: proto, User: user, Host: host, Command: "Connect", Started: time.Now()}
	globalSessions.mu.Lock()
	if len(globalSessions.m) >= globalSessions.max && !globalSessions.warn {
		globalSessions.warn = true
		globalSessions.mu.Unlock()
		logf(LOG_WARN, "session registry full (%d), SHOW PROCESSLIST will be incomplete", globalSessions.max)
		globalSessions.mu.Lock()
	}
	globalSessions.m[id] = sv
	globalSessions.mu.Unlock()
	return id
}

// unregisterSession 注销（连接关闭时调用）。
func unregisterSession(id uint64) {
	globalSessions.mu.Lock()
	delete(globalSessions.m, id)
	globalSessions.mu.Unlock()
}

// touchSession 更新会话状态（认证完成、执行语句前后）。
func touchSession(id uint64, fn func(*sessionView)) {
	if id == 0 {
		return
	}
	globalSessions.mu.Lock()
	if sv, ok := globalSessions.m[id]; ok {
		fn(sv)
	}
	globalSessions.mu.Unlock()
}

// snapshotSessions 返回按 ID 升序的会话快照。
func snapshotSessions() []sessionView {
	globalSessions.mu.Lock()
	out := make([]sessionView, 0, len(globalSessions.m))
	for _, sv := range globalSessions.m {
		out = append(out, *sv)
	}
	globalSessions.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// processlistRows 组装 SHOW PROCESSLIST 的结果集（列顺序与列名见 processlistColumns）。
var processlistColumns = []string{"Id", "User", "Host", "db", "Command", "Time", "State", "Info"}

func processlistRows() [][]interface{} {
	sessions := snapshotSessions()
	now := time.Now()
	rows := make([][]interface{}, 0, len(sessions))
	for _, sv := range sessions {
		idle := int64(now.Sub(sv.Started).Seconds())
		if idle < 0 {
			idle = 0
		}
		var info interface{} // 空闲会话 MySQL 显示 NULL 而不是空串
		if sv.LastSQL != "" {
			info = sv.LastSQL
		}
		rows = append(rows, []interface{}{
			int64(sv.ID), sv.User, sv.Host, nullable(sv.DB),
			sv.Command, idle, sv.Proto, info,
		})
	}
	return rows
}
