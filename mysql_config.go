package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ==================== MySQL 模拟配置（config/mysql.json） ====================
// 该文件描述 MySQL 兼容层对外暴露的"系统变量/用户表/数据库列表"，
// 供 SHOW VARIABLES、SELECT ... FROM mysql.user、SHOW DATABASES 等使用。
// 文件随 config 目录持久化，保证配置统一放置。

const mysqlConfigPath = "config/mysql.json" // 默认路径

type MySQLUserRow struct {
	User   string `json:"user"`
	Host   string `json:"host"`
	Plugin string `json:"plugin"`
}

type MySQLConfig struct {
	Version   string            `json:"version"`
	Port      int               `json:"port"`
	Variables map[string]string `json:"variables"`
	Users     []MySQLUserRow    `json:"users"`
	Databases []string          `json:"databases"`
}

type mysqlConfigStore struct {
	mu   sync.RWMutex
	cfg  MySQLConfig
	path string
}

var mysqlCfg = &mysqlConfigStore{path: mysqlConfigPath}

// defaultMySQLConfig 返回默认 MySQL 兼容配置。
func defaultMySQLConfig() MySQLConfig {
	return MySQLConfig{
		Version: "8.0.36-Tsumugi",
		Port:    3306,
		Variables: map[string]string{
			"version":                   "8.0.36-Tsumugi",
			"version_comment":           "Tsumugi (MySQL-compatible in-memory database)",
			"require_secure_transport":  "OFF",
			"ssl_ca":                    "",
			"ssl_capath":                "",
			"ssl_cert":                  "",
			"ssl_cipher":                "",
			"ssl_crl":                   "",
			"ssl_crlpath":               "",
			"ssl_key":                   "",
			"tls_version":               "TLSv1.2,TLSv1.3",
			"have_ssl":                  "DISABLED",
			"have_openssl":              "DISABLED",
			"max_connections":           "100",
			"max_connect_errors":        "100",
			"character_set_server":      "utf8mb4",
			"collation_server":          "utf8mb4_0900_ai_ci",
			"character_set_client":      "utf8mb4",
			"character_set_connection":  "utf8mb4",
			"character_set_results":     "utf8mb4",
			"autocommit":                "ON",
			"sql_mode":                  "STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION",
			"default_storage_engine":    "InnoDB",
			"lower_case_table_names":    "0",
			"innodb_buffer_pool_size":   "134217728",
			"datadir":                   "./data",
			"port":                      "3306",
			"protocol_version":          "10",
			"server_id":                 "1",
			"time_zone":                 "SYSTEM",
			"performance_schema":        "ON",
			"transaction_isolation":     "REPEATABLE-READ",
			"auto_increment_increment":  "1",
			"connect_timeout":           "10",
			"interactive_timeout":       "28800",
			"wait_timeout":              "28800",
			"max_allowed_packet":        "67108864",
			"net_buffer_length":         "16384",
			"query_cache_size":          "0",
			"skip_networking":           "OFF",
			"validate_password_length":  "8",
			"validate_password_policy":  "MEDIUM",
			"log_error":                 "tsumugi-error.log",
		},
		Users: []MySQLUserRow{
			{User: "root", Host: "localhost", Plugin: "mysql_native_password"},
			{User: "root", Host: "%", Plugin: "mysql_native_password"},
		},
		Databases: []string{"information_schema", "mysql", "performance_schema", "sys", "tsumugi"},
	}
}

// loadMySQLConfig 从 config/mysql.json 加载；文件不存在则写入默认值。
func (ms *mysqlConfigStore) load(_ string) {
	ms.path = "config/mysql.json"
	os.MkdirAll("config", 0755)
	def := defaultMySQLConfig()
	data, err := os.ReadFile(ms.path)
	if err == nil {
		if json.Unmarshal(data, &def) == nil {
			// 保证关键字段非空
			if def.Version == "" {
				def.Version = "8.0.36-Tsumugi"
			}
			if def.Users == nil {
				def.Users = defaultMySQLConfig().Users
			}
			if def.Databases == nil {
				def.Databases = defaultMySQLConfig().Databases
			}
			ms.cfg = def
			return
		}
	}
	// 不存在或解析失败：写入默认
	ms.cfg = def
	if err := ms.save(); err != nil {
		logf(LOG_ERR, "write default mysql config: %v", err)
	}
}

func (ms *mysqlConfigStore) saveLocked() error {
	data, err := json.MarshalIndent(ms.cfg, "", "  ")
	if err != nil {
		return err
	}
	// 确保父目录存在
	if dir := filepath.Dir(ms.path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return os.WriteFile(ms.path, data, 0644)
}

func (ms *mysqlConfigStore) set(c MySQLConfig) error {
	ms.mu.Lock()
	ms.cfg = c
	err := ms.saveLocked()
	ms.mu.Unlock()
	return err
}

func (ms *mysqlConfigStore) save() error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.saveLocked()
}

func (ms *mysqlConfigStore) get() MySQLConfig {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.cfg
}

// variable 返回某系统变量（不区分大小写），不存在返回空串。
func (ms *mysqlConfigStore) variable(name string) (string, bool) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	for k, v := range ms.cfg.Variables {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

func (ms *mysqlConfigStore) addDatabase(name string) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for _, d := range ms.cfg.Databases {
		if d == name {
			return false
		}
	}
	ms.cfg.Databases = append(ms.cfg.Databases, name)
	ms.saveLocked()
	return true
}

// hasDatabase 判断虚拟数据库是否存在（不区分大小写）。
func (ms *mysqlConfigStore) hasDatabase(name string) bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	for _, d := range ms.cfg.Databases {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// removeDatabase 从列表移除数据库，返回是否存在。
func (ms *mysqlConfigStore) removeDatabase(name string) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for i, d := range ms.cfg.Databases {
		if strings.EqualFold(d, name) {
			ms.cfg.Databases = append(ms.cfg.Databases[:i], ms.cfg.Databases[i+1:]...)
			ms.saveLocked()
			return true
		}
	}
	return false
}

// dropDatabase 删除虚拟数据库及其下所有表。
// 返回删除的数据库名（用于消息）；若库不存在返回错误。
func (db *DB) dropDatabase(name string) (bool, error) {
	if !mysqlCfg.hasDatabase(name) {
		return false, nil
	}
	// 收集该库下所有表（tsumugi 默认库删除所有无前缀表；其它库删除 db. 前缀表）
	prefix := name + "."
	db.mu.Lock()
	var toDrop []string
	for tname := range db.catalog {
		if name == "tsumugi" {
			if !strings.Contains(tname, ".") {
				toDrop = append(toDrop, tname)
			}
		} else if strings.HasPrefix(tname, prefix) {
			toDrop = append(toDrop, tname)
		}
	}
	db.mu.Unlock()
	for _, tn := range toDrop {
		if err := db.dropTableByName(tn); err != nil {
			return true, err
		}
	}
	mysqlCfg.removeDatabase(name)
	// 若当前 USE 的是被删库，退回默认库
	if strings.EqualFold(db.getCurDB(), name) {
		db.setCurDB("tsumugi")
	}
	return true, nil
}
