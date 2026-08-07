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

// opsShard 命令计数分片：降低高并发下 opsCount 的锁竞争
type opsShard struct {
	mu sync.Mutex
	m  map[string]uint64
}

func fnv1a(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

type Stats struct {
	cmdCount uint64
	errCount uint64

	// 命令计数分片（每命令一个原子计数，避免单一互斥锁竞争）
	opsShards [16]opsShard

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
	for i := range s.opsShards {
		s.opsShards[i].m = make(map[string]uint64)
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

func (s *Stats) IncCmd(name string) {
	atomic.AddUint64(&s.cmdCount, 1)
	shard := &s.opsShards[fnv1a(name)%uint32(len(s.opsShards))]
	shard.mu.Lock()
	shard.m[name]++
	shard.mu.Unlock()
	if name == "AUTH" || name == "PING" {
		return
	}
	incRate(&s.rateTicks, &s.rateCounts)
	if isWriteCmd(name) {
		incRate(&s.rateTicksW, &s.rateCountsW)
	}
}

// isWriteCmd 判断命令是否属于写操作（用于 TPS 统计）。
func isWriteCmd(name string) bool {
	switch name {
	case "INSERT", "UPDATE", "DELETE", "BATCH", "STRESS_WRITE":
		return true
	}
	return false
}

// incRate 无锁速率统计：100ms 一个桶，CAS 推进刻度避免并发推进同一桶的竞态。
func incRate(ticks *[256]int64, counts *[256]int64) {
	tick := time.Now().UnixMilli() / 100
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
	for i := range s.opsShards {
		shard := &s.opsShards[i]
		shard.mu.Lock()
		for k, v := range shard.m {
			opsCopy[k] = v
		}
		shard.mu.Unlock()
	}
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

	// JSON API
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
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
	})

	// 压力测试（需管理员认证）
	mux.HandleFunc("/stress", adminAuthRequired(func(w http.ResponseWriter, r *http.Request) {
		duration := 10
		workers := 4
		mode := "rw"
		if d := r.URL.Query().Get("duration"); d != "" {
			if v, err := strconv.Atoi(d); err == nil && v > 0 {
				duration = v
			}
		}
		if w := r.URL.Query().Get("workers"); w != "" {
			if v, err := strconv.Atoi(w); err == nil && v > 0 && v <= 100 {
				workers = v
			}
		}
		if m := r.URL.Query().Get("mode"); m != "" && stressModeValid(m) {
			mode = m
		}
		go db.runStressTest(duration, workers, mode)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "started",
			"duration": duration,
			"workers":  workers,
			"mode":     mode,
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
	// 首次设置向导
	mux.HandleFunc("/api/setup/status", db.handleSetupStatus)
	mux.HandleFunc("/api/setup/root-password", db.handleSetupRootPwd)
	mux.HandleFunc("/api/setup/complete", db.handleSetupComplete)
	// 用户管理
	mux.HandleFunc("/api/admin/users", adminAuthRequired(db.handleUserList))
	mux.HandleFunc("/api/admin/users/create", adminAuthRequired(db.handleUserCreate))
	mux.HandleFunc("/api/admin/users/delete", adminAuthRequired(db.handleUserDelete))
	mux.HandleFunc("/api/admin/users/update", adminAuthRequired(db.handleUserUpdate))
	mux.HandleFunc("/api/current-user", db.handleCurrentUser)
	// 写接口与数据读取需认证；登录接口公开
	mux.HandleFunc("/api/admin/tables", adminAuthRequired(db.handleAdminTables))
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
		if err := db.Compact(); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "msg": trMsg(reqLang(r), "compact_done")})
	}))
	mux.HandleFunc("/api/admin/restart", adminAuthRequired(func(w http.ResponseWriter, r *http.Request) {
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

func (db *DB) runStressTest(duration int, workers int, mode string) {
	logf(LOG_VERB, "Stress test: %d workers, %d seconds, mode=%s", workers, duration, mode)
	// 广播停止信号：close(stop) 可同时唤醒所有 worker
	stop := make(chan struct{})
	time.AfterFunc(time.Duration(duration)*time.Second, func() { close(stop) })
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// 每个 worker 确保 test 表存在（写入 catalog，重启后仍可恢复）
			db.mu.Lock()
			if _, ok := db.tables.Load("test"); !ok {
				meta := &TableMeta{
					Name:    "test",
					PK:      "id",
					Fields:  []Field{{Name: "id", Type: TypeInt}, {Name: "val", Type: TypeVarchar, Len: 100}},
					Indexes: map[string]string{},
				}
				table := NewTable(meta, db.walFile)
				db.tables.Store("test", table)
				db.tablesCount.Add(1)
				db.catalog["test"] = meta
				if err := db.writeCatalog(meta); err != nil {
					logf(LOG_ERR, "stress: persist catalog: %v", err)
				}
			}
			db.mu.Unlock()

			var table *Table
			for {
				select {
				case <-stop:
					return
				default:
					key := int64(atomic.AddUint64(&stressSeq, 1))
					if table == nil {
						v, _ := db.tables.Load("test")
						if v != nil {
							table = v.(*Table)
						} else {
							continue
						}
					}
					db.stressStep(table, stressMode(mode), key)
				}
			}
		}(i)
	}
	wg.Wait()
	logf(LOG_VERB, "Stress test completed (mode=%s)", mode)
}

// stressStep 按负载模式执行一次读写/查询操作。
func (db *DB) stressStep(table *Table, mode stressMode, key int64) {
	switch mode {
	case stressWrite:
		row := map[string]interface{}{"id": key, "val": strconv.FormatInt(key, 10)}
		table.Insert(key, row, 0, 0, false)
		db.stats.IncCmd("STRESS_WRITE")
	case stressPoint, stressRead:
		// 点查询：走索引等值路径（point）或主键直查（read）
		if mode == stressPoint {
			table.SelectR(map[string]interface{}{"id": key}, nil, nil, nil, 1)
			db.stats.IncCmd("STRESS_POINT")
		} else {
			table.SelectByPK(key)
			db.stats.IncCmd("STRESS_READ")
		}
	case stressRange:
		// 范围查询：主键区间 [key, key+999] 扫描，限制 100 行（零分配快速路径）
		minKey := key
		maxKey := key + 999
		table.SelectRBytes(&minKey, &maxKey, 100)
		db.stats.IncCmd("STRESS_RANGE")
	default: // stressRW 默认 7:3 读写混合
		if key%10 < 7 {
			table.SelectByPK(key)
			db.stats.IncCmd("STRESS_READ")
		} else {
			row := map[string]interface{}{"id": key, "val": strconv.FormatInt(key, 10)}
			table.Insert(key, row, 0, 0, false)
			db.stats.IncCmd("STRESS_WRITE")
		}
	}
}
