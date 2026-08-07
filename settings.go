package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
	Variables          map[string]string `json:"variables"`
}

func (db *DB) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	mc := mysqlCfg.get()
	writeJSON(w, map[string]interface{}{
		"ok": true,
		"server": map[string]interface{}{
			"user":                 db.config.User,
			"password":             db.config.Password,
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
		},
		"mysql": map[string]interface{}{
			"version":   mc.Version,
			"variables": mc.Variables,
		},
	})
}

func (db *DB) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	var p settingsPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "bad request"})
		return
	}

	// ---- 热更新数据库运行时配置（部分字段）----
	if p.User != "" {
		db.config.User = p.User
	}
	if p.Password != "" {
		db.config.Password = p.Password
	}
	if p.MySQLEnabled != nil {
		db.config.MySQLEnabled = *p.MySQLEnabled
	}
	if p.MySQLPort != nil && *p.MySQLPort > 0 && *p.MySQLPort < 65536 {
		db.config.MySQLPort = *p.MySQLPort
	}
	if p.Durability.Valid() {
		db.config.Durability = p.Durability
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

	// ---- 落盘到 /config/tsumugi.json ----
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
	return os.WriteFile(path, data, 0644)
}
