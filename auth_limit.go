package main

import (
	"strings"
	"sync"
	"time"
)

// ==================== 登录失败限速 ====================
// 旧实现只在失败后固定 sleep 300ms：单个 IP 用并发就能把速率拉回每秒几十次，
// 且二进制 / MySQL 两个入口完全没有延迟。这里按 (来源 IP, 用户名) 计数，
// 超过阈值后指数退避封禁，三个入口（Web / 二进制 / MySQL）共用。

type loginFail struct {
	count int
	last  time.Time
	until time.Time
}

type loginGuard struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	backoff  time.Duration
	failures map[string]*loginFail
}

var loginLimit = newLoginGuard(5, 15*time.Minute, 5*time.Minute)

func newLoginGuard(max int, window, backoff time.Duration) *loginGuard {
	return &loginGuard{max: max, window: window, backoff: backoff, failures: make(map[string]*loginFail)}
}

func loginKey(ip, user string) string { return ip + "|" + strings.ToLower(user) }

// Blocked 返回该 key 是否仍在封禁期，以及剩余时长。
func (g *loginGuard) Blocked(key string) (bool, time.Duration) {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.failures[key]
	if f == nil {
		return false, 0
	}
	if now.After(f.until) {
		// 封禁到期且超过统计窗口 → 清空
		if now.Sub(f.last) > g.window {
			delete(g.failures, key)
		}
		return false, 0
	}
	return true, f.until.Sub(now)
}

// Fail 记一次失败；达到阈值后指数退避（2^n 秒，上限 backoff）。
func (g *loginGuard) Fail(key string) {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.failures[key]
	if f == nil || now.Sub(f.last) > g.window {
		g.failures[key] = &loginFail{count: 1, last: now}
		g.cleanupLocked(now)
		return
	}
	f.count++
	f.last = now
	if f.count >= g.max {
		over := f.count - g.max
		d := time.Second << uint(min(over, 8))
		if d > g.backoff || d <= 0 {
			d = g.backoff
		}
		f.until = now.Add(d)
	}
}

// Reset 登录成功后清除计数。
func (g *loginGuard) Reset(key string) {
	g.mu.Lock()
	delete(g.failures, key)
	g.mu.Unlock()
}

// cleanupLocked 丢弃过期记录，避免 map 随攻击面无限增长。
func (g *loginGuard) cleanupLocked(now time.Time) {
	if len(g.failures) < 4096 {
		return
	}
	for k, f := range g.failures {
		if now.Sub(f.last) > g.window && now.After(f.until) {
			delete(g.failures, k)
		}
	}
}
