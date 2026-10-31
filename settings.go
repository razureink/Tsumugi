package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ==================== 设置 / 配置管理 API ====================
// GET  /api/admin/settings  读取当前可调运行时配置
// POST /api/admin/settings  保存到 /config/tsumugi.json 与 /config/mysql.json 并尽量热生效
// 全部走 admin 认证，见 metrics.go 的路由注册。

// settingsPayload 对应可在设置页修改并落盘的字段
type settingsPayload struct {
	User               string            `json:"user"`
	Password           string            `json:"password"`
	MySQLEnabled       *bool             `json:"mysql_enabled"`
	MySQLPort          *int              `json:"mysql_port"`
	Durability         Durability        `json:"durability"`
	MetricsPort        *int              `json:"metrics_port"`
	Port               *int              `json:"binary_port"`
	FlushMS            *int              `json:"flush_interval_ms"`
	GroupCommitMS      *int              `json:"group_commit_ms"`
	TTLCleanMS         *int              `json:"ttl_clean_ms"`
	Checksum           *bool             `json:"checksum"`
	AutoCompact        *bool             `json:"auto_compact"`
	CompactIdleSeconds *int              `json:"compact_idle_seconds"`
	CompactMinWALMB    *int              `json:"compact_min_wal_mb"`
	CompactPeakRate    *int              `json:"compact_peak_rate"`
	TreeKind           string            `json:"tree_kind"`
	TreeMode           string            `json:"tree_mode"`
	Variables          map[string]string `json:"variables"`
}

func (db *DB) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "settings are admin-only") {
		return
	}
	mc := mysqlCfg.get()
	db.cfgMu.RLock()
	defer db.cfgMu.RUnlock()
	writeJSON(w, map[string]interface{}{
		"ok": true,
		"server": map[string]interface{}{
			"user":                 db.config.User,
			"password":             "",
			"mysql_enabled":        db.config.MySQLEnabled,
			"mysql_port":           db.config.MySQLPort,
			"durability":           db.config.Durability,
			"metrics_port":         db.config.MetricsPort,
			"binary_port":          db.config.Port,
			"flush_interval_ms":    int(db.config.FlushInterval / time.Millisecond),
			"group_commit_ms":      int(db.config.GroupCommitInterval / time.Millisecond),
			"ttl_clean_ms":         int(db.config.TTLCleanInterval / time.Millisecond),
			"checksum":             db.config.EnableChecksum,
			"auto_compact":         db.config.AutoCompact,
			"compact_idle_seconds": db.config.CompactIdleSeconds,
			"compact_min_wal_mb":   db.config.CompactMinWALMB,
			"compact_peak_rate":    db.config.CompactPeakRate,
			"tree_kind":            db.config.TreeKind,
			"tree_active":          currentTreeKind().String(),
			"tree_mode":            db.config.TreeMode,
			"tree_mode_active":     currentTreeMode().String(),
		},
		"mysql": map[string]interface{}{
			"version":   mc.Version,
			"variables": mc.Variables,
		},
	})
}

func (db *DB) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "settings are admin-only") {
		return
	}
	var p settingsPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "bad request"})
		return
	}

	// ---- 热更新数据库运行时配置（写者互斥，避免与其他设置写入交错）----
	db.cfgMu.Lock()
	defer db.cfgMu.Unlock()
	if p.User != "" {
		db.config.User = p.User
	}
	if p.Password != "" {
		db.config.Password = p.Password
		db.config.PasswordSHA1 = passwordSHA1Hex(p.Password)
	}
	if p.MySQLEnabled != nil {
		db.config.MySQLEnabled = *p.MySQLEnabled
	}
	if p.MySQLPort != nil && *p.MySQLPort > 0 && *p.MySQLPort < 65536 {
		db.config.MySQLPort = *p.MySQLPort
	}
	if p.Durability.Valid() {
		db.setDurability(p.Durability)
	}
	// 以下字段此前未应用（接口返回 ok 但什么都没保存）；现补齐并做范围校验，
	// 端口类字段需重启生效，间隔类字段热生效。
	if p.Port != nil && *p.Port > 0 && *p.Port < 65536 {
		db.config.Port = *p.Port
	}
	if p.MetricsPort != nil && *p.MetricsPort > 0 && *p.MetricsPort < 65536 {
		db.config.MetricsPort = *p.MetricsPort
	}
	if p.FlushMS != nil && *p.FlushMS > 0 && *p.FlushMS <= 600000 {
		db.config.FlushInterval = time.Duration(*p.FlushMS) * time.Millisecond
		if db.flushTicker != nil {
			db.flushTicker.Reset(db.config.FlushInterval)
		}
	}
	if p.GroupCommitMS != nil && *p.GroupCommitMS > 0 && *p.GroupCommitMS <= 1000 {
		db.config.GroupCommitInterval = time.Duration(*p.GroupCommitMS) * time.Millisecond
	}
	if p.TTLCleanMS != nil && *p.TTLCleanMS >= 1000 && *p.TTLCleanMS <= 3600000 {
		db.config.TTLCleanInterval = time.Duration(*p.TTLCleanMS) * time.Millisecond
		if db.ttlTicker != nil {
			db.ttlTicker.Reset(db.config.TTLCleanInterval)
		}
	}
	if p.Checksum != nil {
		db.config.EnableChecksum = *p.Checksum
		walCRC.Store(*p.Checksum)
	}
	if p.AutoCompact != nil {
		db.config.AutoCompact = *p.AutoCompact
	}
	if p.CompactIdleSeconds != nil && *p.CompactIdleSeconds > 0 {
		db.config.CompactIdleSeconds = *p.CompactIdleSeconds
	}
	if p.CompactMinWALMB != nil && *p.CompactMinWALMB > 0 {
		db.config.CompactMinWALMB = *p.CompactMinWALMB
	}
	if p.CompactPeakRate != nil && *p.CompactPeakRate > 0 {
		db.config.CompactPeakRate = *p.CompactPeakRate
	}
	// 索引树实现：只记录到配置，重启后从 WAL 重建索引时生效。
	// 已有树的节点结构是既成事实，无法原地替换（换了会读不出旧数据）。
	switch strings.ToLower(p.TreeKind) {
	case "btree", "b-tree":
		db.config.TreeKind = "btree"
	case "llrb", "rb", "rbtree":
		db.config.TreeKind = "llrb"
	}
	// 并发模式同理：只记配置，重启生效
	switch strings.ToLower(p.TreeMode) {
	case "lock", "rw", "rwlock", "mutable":
		db.config.TreeMode = "lock"
	case "cow":
		db.config.TreeMode = "cow"
	case "auto", "adaptive":
		db.config.TreeMode = "auto"
	}

	// ---- 落盘到 /config/tsumugi.json（原子写；配置含 MySQL 认证摘要，0600）----
	var cfgFile configFile
	cfgFile.fillFromConfig(db.config)
	if err := writeJSONFile(tsumugiConfigPath, &cfgFile); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "save config: " + err.Error()})
		return
	}

	// ---- 热更新 mysql 变量 ----
	if len(p.Variables) > 0 {
		ms := mysqlCfg
		ms.mu.Lock()
		for k, v := range p.Variables {
			if v == "" {
				delete(ms.cfg.Variables, k)
			} else {
				ms.cfg.Variables[k] = v
			}
		}
		saveErr := ms.saveLocked()
		ms.mu.Unlock()
		if saveErr != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "save mysql config: " + saveErr.Error()})
			return
		}
	}

	writeJSON(w, map[string]interface{}{"ok": true, "msg": trMsg(reqLang(r), "settings_saved")})
}

// writeJSONFile 原子写：tmp + rename。含敏感摘要的配置文件权限收紧为 0600。
func writeJSONFile(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// 确保父目录存在（避免相对路径工作目录变化导致保存失败）
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
