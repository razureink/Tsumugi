package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"runtime/metrics"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ==================== 统计 ====================

// maxOpsSlots 命令计数槽位上限。命令名是一个固定小词表（~35 个），
// 槽位耗尽后落到共享的溢出槽（计数合并，仅影响可观测性不影响正确性）。
const maxOpsSlots = 256
const opsOverflowSlot = maxOpsSlots - 1

type Stats struct {
	cmdCount uint64
	errCount uint64

	// 命令计数：name -> 槽位（sync.Map，注册一次后只读）+ 槽位原子计数。
	// 取代旧的互斥锁分片 map——旧实现把同名命令全部压到同一把锁上，
	// 高并发 INSERT 实测每条命令多花 ~285ns 且互斥。
	opsNames  [maxOpsSlots]string
	opsCounts [maxOpsSlots]atomic.Uint64
	opsSlots  sync.Map // name -> uint32
	opsMu     sync.Mutex
	opsNext   uint32

	// 速率统计：100ms 一个桶，256 桶共 25.6s，任意吞吐下不封顶、内存固定
	// rate* 统计全部命令（QPS），rate*W 统计写入命令（TPS）
	// 无锁实现：bucket 用 CAS 推进刻度，count 用原子累加
	rateTicks   [256]int64
	rateCounts  [256]int64
	rateTicksW  [256]int64
	rateCountsW [256]int64

	// 磁盘写入速率：WAL 累计字节数 + 每秒写入速率（100ms 桶）
	walBytes      uint64 // 累计写入 WAL 的字节数
	fsyncCount    uint64 // 累计 fsync 次数
	walRateTicks  [256]int64
	walRateBytes  [256]int64
	walRateTicksF [256]int64
	walRateFsync  [256]int64

	startTime time.Time

	cpuMu        sync.Mutex
	cpuLast      time.Time
	cpuLastTotal float64

	// 持久化模式：batch（定时批量 fsync）或 fsync（每条写立即落盘）
	durability Durability
}

func NewStats() *Stats {
	s := &Stats{
		startTime:    time.Now(),
		cpuLast:      time.Now(),
		cpuLastTotal: readProcessCPUSeconds(),
	}
	return s
}

// readProcessCPUSeconds 返回进程累计 CPU 时间（秒），跨平台且仅用标准库。
func readProcessCPUSeconds() float64 {
	names := []string{"/cpu/classes/user:cpu-seconds", "/cpu/classes/system:cpu-seconds"}
	samples := make([]metrics.Sample, len(names))
	for i, n := range names {
		samples[i].Name = n
	}
	metrics.Read(samples)
	var total float64
	for _, s := range samples {
		if s.Value.Kind() == metrics.KindFloat64 {
			total += s.Value.Float64()
		}
	}
	return total
}

// calcCPUPercent 计算自上次采样以来进程 CPU 占用（相对全部核的百分比，0~100）。
func (s *Stats) calcCPUPercent() float64 {
	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()
	now := time.Now()
	total := readProcessCPUSeconds()
	dt := now.Sub(s.cpuLast).Seconds()
	dcpu := total - s.cpuLastTotal
	s.cpuLast = now
	s.cpuLastTotal = total
	if dt <= 0 {
		return 0
	}
	pct := dcpu / dt / float64(runtime.NumCPU()) * 100
	if pct < 0 {
		return 0
	}
	return pct
}

// slotFor 返回命令名的计数槽位（注册一次，此后原子读）。
func (s *Stats) slotFor(name string) uint32 {
	if v, ok := s.opsSlots.Load(name); ok {
		return v.(uint32)
	}
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if v, ok := s.opsSlots.Load(name); ok {
		return v.(uint32)
	}
	next := atomic.AddUint32(&s.opsNext, 1) - 1
	slot := next
	if slot >= opsOverflowSlot {
		slot = opsOverflowSlot // 槽位耗尽：合并到溢出槽
	} else {
		s.opsNames[slot] = name
	}
	s.opsSlots.Store(name, slot)
	return slot
}

func (s *Stats) IncCmd(name string) {
	atomic.AddUint64(&s.cmdCount, 1)
	s.opsCounts[s.slotFor(name)].Add(1)
	if name == "AUTH" || name == "PING" {
		return
	}
	// QPS/TPS 两个桶共用同一刻度：只取一次时钟（Windows 上 time.Now 走 QPC，
	// 每条命令两次取钟约占 IncCmd 总开销的 1/3）。
	tick := time.Now().UnixMilli() / 100
	incRateTick(&s.rateTicks, &s.rateCounts, tick)
	if isWriteCmd(name) {
		incRateTick(&s.rateTicksW, &s.rateCountsW, tick)
	}
}

// isWriteCmd 判断命令是否属于写操作（用于 TPS 统计）。
func isWriteCmd(name string) bool {
	switch name {
	case "INSERT", "UPDATE", "DELETE", "BATCH":
		return true
	}
	return false
}

// incRate 无锁速率统计：100ms 一个桶，CAS 推进刻度避免并发推进同一桶的竞态。
func incRate(ticks *[256]int64, counts *[256]int64) {
	incRateTick(ticks, counts, time.Now().UnixMilli()/100)
}

// incRateTick 同 incRate，但刻度由调用方传入（同一命令的多个速率桶共用一次取钟）。
func incRateTick(ticks *[256]int64, counts *[256]int64, tick int64) {
	i := int(tick % 256)
	for {
		cur := atomic.LoadInt64(&ticks[i])
		if cur == tick {
			atomic.AddInt64(&counts[i], 1)
			return
		}
		if atomic.CompareAndSwapInt64(&ticks[i], cur, tick) {
			atomic.StoreInt64(&counts[i], 1)
			return
		}
	}
}

// incRateBytes 无锁速率统计，桶内累加字节数（用于磁盘写入速率）。
func incRateBytes(ticks *[256]int64, counts *[256]int64, n int64) {
	tick := time.Now().UnixMilli() / 100
	i := int(tick % 256)
	for {
		cur := atomic.LoadInt64(&ticks[i])
		if cur == tick {
			atomic.AddInt64(&counts[i], n)
			return
		}
		if atomic.CompareAndSwapInt64(&ticks[i], cur, tick) {
			atomic.StoreInt64(&counts[i], n)
			return
		}
	}
}

func calcRate(ticks *[256]int64, counts *[256]int64) int64 {
	now := time.Now().UnixMilli() / 100
	var q int64
	for i := 0; i < 256; i++ {
		if now-atomic.LoadInt64(&ticks[i]) <= 10 { // 最近 1 秒内的桶
			q += atomic.LoadInt64(&counts[i])
		}
	}
	return q
}

// 记录一次 WAL 落盘：累计字节数与每秒磁盘写入速率、fsync 次数。
func (s *Stats) IncWalWrite(n int) {
	atomic.AddUint64(&s.walBytes, uint64(n))
	incRateBytes(&s.walRateTicks, &s.walRateBytes, int64(n))
}
func (s *Stats) IncFsync() {
	atomic.AddUint64(&s.fsyncCount, 1)
	incRateBytes(&s.walRateTicksF, &s.walRateFsync, 1)
}

func (s *Stats) IncErr() {
	atomic.AddUint64(&s.errCount, 1)
}

func (s *Stats) Snapshot() map[string]interface{} {
	opsCopy := make(map[string]uint64)
	n := atomic.LoadUint32(&s.opsNext)
	if n > opsOverflowSlot {
		n = opsOverflowSlot
	}
	for i := uint32(0); i < n; i++ {
		if name := s.opsNames[i]; name != "" {
			opsCopy[name] = s.opsCounts[i].Load()
		}
	}
	// 溢出槽中的名字（极少发生）：单独列出避免丢计数
	s.opsSlots.Range(func(k, v interface{}) bool {
		if v.(uint32) == opsOverflowSlot {
			opsCopy[k.(string)+"*"] = s.opsCounts[opsOverflowSlot].Load()
		}
		return true
	})
	qps, tps := s.calcRates()
	walMBs := float64(calcRate(&s.walRateTicks, &s.walRateBytes)) / 1024 / 1024
	fsyncPerS := float64(calcRate(&s.walRateTicksF, &s.walRateFsync))
	totalWalMB := float64(atomic.LoadUint64(&s.walBytes)) / 1024 / 1024

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var memPct float64
	if ms.Sys > 0 {
		memPct = float64(ms.HeapAlloc) / float64(ms.Sys) * 100
	}
	cpuPct := s.calcCPUPercent()

	return map[string]interface{}{
		"total_commands": atomic.LoadUint64(&s.cmdCount),
		"total_errors":   atomic.LoadUint64(&s.errCount),
		"commands":       opsCopy,
		"qps":            qps,
		"tps":            tps,
		"cpu_percent":    cpuPct,
		"mem_percent":    memPct,
		"mem_mb":         float64(ms.HeapAlloc) / 1024 / 1024,
		"goroutines":     runtime.NumGoroutine(),
		"num_cpu":        runtime.NumCPU(),
		"uptime":         int64(time.Since(s.startTime).Seconds()),
		"durability":     s.durability,
		"wal_write_mb_s": walMBs,
		"wal_total_mb":   totalWalMB,
		"fsync_per_s":    fsyncPerS,
		"fsync_count":    atomic.LoadUint64(&s.fsyncCount),
	}
}

func (s *Stats) calcRates() (qps, tps float64) {
	return float64(calcRate(&s.rateTicks, &s.rateCounts)),
		float64(calcRate(&s.rateTicksW, &s.rateCountsW))
}

// SnapshotQPS 返回最近 1 秒的命令速率（低峰判定用）。
func (s *Stats) SnapshotQPS() int64 { return calcRate(&s.rateTicks, &s.rateCounts) }

// ==================== HTTP 监控 ====================

func (db *DB) startMetricsServer() {
	mux := http.NewServeMux()
	// Prometheus
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		snapshot := db.stats.Snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP tsumugi_total_commands Total commands\n")
		fmt.Fprintf(w, "# TYPE tsumugi_total_commands counter\n")
		fmt.Fprintf(w, "tsumugi_total_commands %d\n", snapshot["total_commands"])
		fmt.Fprintf(w, "# HELP tsumugi_total_errors Total errors\n")
		fmt.Fprintf(w, "# TYPE tsumugi_total_errors counter\n")
		fmt.Fprintf(w, "tsumugi_total_errors %d\n", snapshot["total_errors"])
		fmt.Fprintf(w, "# HELP tsumugi_qps Queries per second\n")
		fmt.Fprintf(w, "# TYPE tsumugi_qps gauge\n")
		fmt.Fprintf(w, "tsumugi_qps %f\n", snapshot["qps"])
		fmt.Fprintf(w, "# HELP tsumugi_tps Transactions per second\n")
		fmt.Fprintf(w, "# TYPE tsumugi_tps gauge\n")
		fmt.Fprintf(w, "tsumugi_tps %f\n", snapshot["tps"])
		fmt.Fprintf(w, "# HELP tsumugi_wal_write_mb_s WAL bytes written per second\n")
		fmt.Fprintf(w, "# TYPE tsumugi_wal_write_mb_s gauge\n")
		fmt.Fprintf(w, "tsumugi_wal_write_mb_s %f\n", snapshot["wal_write_mb_s"])
		fmt.Fprintf(w, "# HELP tsumugi_wal_total_mb Total WAL bytes written\n")
		fmt.Fprintf(w, "# TYPE tsumugi_wal_total_mb counter\n")
		fmt.Fprintf(w, "tsumugi_wal_total_mb %f\n", snapshot["wal_total_mb"])
		fmt.Fprintf(w, "# HELP tsumugi_fsync_per_s Fsyncs per second\n")
		fmt.Fprintf(w, "# TYPE tsumugi_fsync_per_s gauge\n")
		fmt.Fprintf(w, "tsumugi_fsync_per_s %f\n", snapshot["fsync_per_s"])
		if cmds, ok := snapshot["commands"].(map[string]uint64); ok {
			for name, count := range cmds {
				fmt.Fprintf(w, "# HELP tsumugi_cmd_%s Count\n", name)
				fmt.Fprintf(w, "# TYPE tsumugi_cmd_%s counter\n", name)
				fmt.Fprintf(w, "tsumugi_cmd_%s %d\n", name, count)
			}
		}
	})

	// JSON API（需管理员认证）
	mux.HandleFunc("/api/stats", adminAuthRequired(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snap := db.stats.Snapshot()
		// 附加服务/存储信息（专业面板）
		db.mu.RLock()
		snap["table_count"] = len(db.catalog)
		db.mu.RUnlock()
		var rowTotal int64
		db.mu.RLock()
		db.tables.Range(func(key, value interface{}) bool {
			rowTotal += value.(*Table).pkTree.Size()
			return true
		})
		db.mu.RUnlock()
		snap["total_rows"] = rowTotal
		snap["wal_file_mb"] = walFileSizeMB(db)
		snap["server_version"] = "Tsumugi-0.1"
		snap["binary_port"] = db.config.Port
		snap["mysql_enabled"] = db.config.MySQLEnabled
		snap["mysql_port"] = db.config.MySQLPort
		snap["config"] = map[string]interface{}{
			"flush_interval_ms": int(db.config.FlushInterval / time.Millisecond),
			"group_commit_ms":   int(db.config.GroupCommitInterval / time.Millisecond),
			"ttl_clean_ms":      int(db.config.TTLCleanInterval / time.Millisecond),
			"checksum":          db.config.EnableChecksum,
		}
		json.NewEncoder(w).Encode(snap)
	}))

	// 压力测试：管理员或显式授予 can_stress 的用户；时长钳制且不允许叠跑
	mux.HandleFunc("/stress", adminAuthRequired(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil || (!u.IsAdmin && !u.CanStress) {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "stress test requires can_stress permission"})
			return
		}
		if !stressRunning.CompareAndSwap(false, true) {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "a stress test is already running"})
			return
		}
		duration := 10
		workers := 4
		mode := "rw"
		if d := r.URL.Query().Get("duration"); d != "" {
			if v, err := strconv.Atoi(d); err == nil && v > 0 {
				duration = v
			}
		}
		if duration > 300 {
			duration = 300
		}
		if w := r.URL.Query().Get("workers"); w != "" {
			if v, err := strconv.Atoi(w); err == nil && v > 0 && v <= 100 {
				workers = v
			}
		}
		if m := r.URL.Query().Get("mode"); m != "" && stressModeValid(m) {
			mode = m
		}
		// 压测协议：native（二进制协议，默认）或 mysql（MySQL 线缆协议）
		proto := "native"
		if p := r.URL.Query().Get("protocol"); p == "mysql" || p == "native" {
			proto = p
		}
		go func() {
			defer stressRunning.Store(false)
			db.runStressTest(duration, workers, mode, proto)
		}()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "started",
			"duration": duration,
			"workers":  workers,
			"mode":     mode,
			"protocol": proto,
		})
	}))

	// 统一应用（侧边栏布局，按路由渲染 监控 / 数据管理）
	mux.HandleFunc("/", db.handleRootPage)
	mux.HandleFunc("/dashboard", db.handleAppPage)
	mux.HandleFunc("/admin", db.handleAppPage)
	mux.HandleFunc("/users", db.handleAppPage)

	// 管理面板 API
	mux.HandleFunc("/api/login", db.handleLogin)
	mux.HandleFunc("/api/logout", db.handleLogout)
	// 首次设置向导（root-password 端点已删除：前端从不调用，且安装前无鉴权地
	// 返回生成密码只增加攻击面；安装统一走 /api/setup/complete）
	mux.HandleFunc("/api/setup/status", db.handleSetupStatus)
	mux.HandleFunc("/api/setup/complete", db.handleSetupComplete)
	// 用户管理
	mux.HandleFunc("/api/admin/users", adminAuthRequired(db.handleUserList))
	mux.HandleFunc("/api/admin/users/create", adminAuthRequired(db.handleUserCreate))
	mux.HandleFunc("/api/admin/users/delete", adminAuthRequired(db.handleUserDelete))
	mux.HandleFunc("/api/admin/users/update", adminAuthRequired(db.handleUserUpdate))
	mux.HandleFunc("/api/current-user", db.handleCurrentUser)
	// 写接口与数据读取需认证；登录接口公开
	mux.HandleFunc("/api/admin/tables", adminAuthRequired(db.handleAdminTables))
	mux.HandleFunc("/api/admin/db/create", adminAuthRequired(db.handleAdminDBCreate))
	mux.HandleFunc("/api/admin/db/drop", adminAuthRequired(db.handleAdminDBDrop))
	mux.HandleFunc("/api/admin/rows", adminAuthRequired(db.handleAdminRows))
	mux.HandleFunc("/api/admin/query", adminAuthRequired(db.handleAdminQuery))
	mux.HandleFunc("/api/admin/insert", adminAuthRequired(db.handleAdminInsert))
	mux.HandleFunc("/api/admin/delete", adminAuthRequired(db.handleAdminDelete))
	mux.HandleFunc("/api/admin/settings", adminAuthRequired(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			db.handleSettingsPost(w, r)
			return
		}
		db.handleSettingsGet(w, r)
	}))
	mux.HandleFunc("/api/admin/compact", adminAuthRequired(func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, "compact is admin-only") {
			return
		}
		if err := db.Compact(); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "msg": trMsg(reqLang(r), "compact_done")})
	}))
	mux.HandleFunc("/api/admin/restart", adminAuthRequired(func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, "restart is admin-only") {
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
		db.handleRestart()
	}))

	addr := fmt.Sprintf(":%d", db.config.MetricsPort)
	srv := &http.Server{Addr: addr, Handler: mux}
	db.metricsServer.Store(srv)
	logf(LOG_VERB, "metrics server listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logf(LOG_ERR, "metrics server error: %v", err)
	}
}

// ==================== 压力测试 ====================

// 压测递增序号（保证多个 worker 生成不重复主键）
var stressSeq uint64

// stressRunning 压测叠跑保护：同一时刻只允许一个压测任务。
var stressRunning atomic.Bool

// stressMode 压测负载模式。
type stressMode string

const (
	stressRead  stressMode = "read"  // 纯读取：主键点查
	stressWrite stressMode = "write" // 纯写入
	stressRW    stressMode = "rw"    // 读写混合（默认 7:3）
	stressPoint stressMode = "point" // 点查询：SelectR 条件过滤
	stressRange stressMode = "range" // 范围查询：主键范围扫描
)

func stressModeValid(m string) bool {
	switch stressMode(m) {
	case stressRead, stressWrite, stressRW, stressPoint, stressRange:
		return true
	}
	return false
}

