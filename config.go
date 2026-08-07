package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

const tsumugiConfigPath = "config/tsumugi.json"

// Durability 持久化模式：batch（定时批量刷盘）/ fsync（每条写入立即落盘）。
type Durability string

const (
	DuraBatch Durability = "batch"
	DuraFsync Durability = "fsync"
)

// Valid 判断持久化模式是否为受支持的取值。
func (d Durability) Valid() bool {
	return d == DuraBatch || d == DuraFsync
}

// Fsync 返回该模式是否需要同步落盘。
func (d Durability) Fsync() bool { return d == DuraFsync }

type Config struct {
	Port                int
	WALDir              string
	WALFile             string
	PrivilegeFile       string
	User                string
	Password            string
	FlushInterval       time.Duration
	GroupCommitInterval time.Duration
	TTLCleanInterval    time.Duration
	IdleTimeout         time.Duration
	BackupDir           string
	MetricsPort         int
	EnableChecksum      bool
	Durability          Durability
	MySQLEnabled        bool
	MySQLPort           int
	AutoCompact         bool
	CompactIdleSeconds  int
	CompactMinWALMB     int
	CompactPeakRate     int
}

// loadDotEnvKV 读取 .env 文件（若存在），返回 KEY=VALUE 映射与是否成功。
// 用于启动时把 .env 里的 TSUMUGI_* 配置迁移进 config/tsumugi.json，
// 之后不再依赖 .env，配置统一落在 config/tsumugi.json。
func loadDotEnvKV() (map[string]string, bool) {
	f, err := os.Open(".env")
	if err != nil {
		return nil, false
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		kv[key] = val
	}
	return kv, sc.Err() == nil
}

// configFile 描述 /config/tsumugi.json 的持久化布局（单一事实源）。
// loadConfigFile 读取它，settings.go 保存时也用它，避免两份 JSON 契约漂移。
// 指针字段表示"文件中未提供时不覆盖"，便于增量合并。
type configFile struct {
	Port               *int    `json:"port"`
	WALDir             *string `json:"wal_dir"`
	WALFile            *string `json:"wal_file"`
	PrivilegeFile      *string `json:"privilege_file"`
	User               *string `json:"user"`
	Password           *string `json:"password"`
	FlushIntervalMS    *int    `json:"flush_interval_ms"`
	GroupCommitMS      *int    `json:"group_commit_ms"`
	TTLCleanMS         *int    `json:"ttl_clean_ms"`
	IdleTimeoutS       *int    `json:"idle_timeout_s"`
	BackupDir          *string `json:"backup_dir"`
	MetricsPort        *int    `json:"metrics_port"`
	EnableChecksum     *bool   `json:"enable_checksum"`
	Durability         *string `json:"durability"`
	MySQLEnabled       *bool   `json:"mysql_enabled"`
	MySQLPort          *int    `json:"mysql_port"`
	AutoCompact        *bool   `json:"auto_compact"`
	CompactIdleSeconds *int    `json:"compact_idle_seconds"`
	CompactMinWALMB    *int    `json:"compact_min_wal_mb"`
	CompactPeakRate    *int    `json:"compact_peak_rate"`
}

// loadConfigFile 从 /config/tsumugi.json 读取 Tsumugi 自身配置（若存在）。
// 返回值为 true 表示文件存在且解析成功。
func loadConfigFile(cfg *Config) bool {
	data, err := os.ReadFile(tsumugiConfigPath)
	if err != nil {
		return false
	}
	var raw configFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	if raw.Port != nil {
		cfg.Port = *raw.Port
	}
	if raw.WALDir != nil {
		cfg.WALDir = *raw.WALDir
	}
	if raw.WALFile != nil {
		cfg.WALFile = *raw.WALFile
	}
	if raw.PrivilegeFile != nil {
		cfg.PrivilegeFile = *raw.PrivilegeFile
	}
	if raw.User != nil {
		cfg.User = *raw.User
	}
	if raw.Password != nil {
		cfg.Password = *raw.Password
	}
	if raw.FlushIntervalMS != nil {
		cfg.FlushInterval = time.Duration(*raw.FlushIntervalMS) * time.Millisecond
	}
	if raw.GroupCommitMS != nil {
		cfg.GroupCommitInterval = time.Duration(*raw.GroupCommitMS) * time.Millisecond
	}
	if raw.TTLCleanMS != nil {
		cfg.TTLCleanInterval = time.Duration(*raw.TTLCleanMS) * time.Millisecond
	}
	if raw.IdleTimeoutS != nil {
		cfg.IdleTimeout = time.Duration(*raw.IdleTimeoutS) * time.Second
	}
	if raw.BackupDir != nil {
		cfg.BackupDir = *raw.BackupDir
	}
	if raw.MetricsPort != nil {
		cfg.MetricsPort = *raw.MetricsPort
	}
	if raw.EnableChecksum != nil {
		cfg.EnableChecksum = *raw.EnableChecksum
	}
	if raw.Durability != nil && Durability(*raw.Durability).Valid() {
		cfg.Durability = Durability(*raw.Durability)
	}
	if raw.MySQLEnabled != nil {
		cfg.MySQLEnabled = *raw.MySQLEnabled
	}
	if raw.MySQLPort != nil {
		cfg.MySQLPort = *raw.MySQLPort
	}
	if raw.AutoCompact != nil {
		cfg.AutoCompact = *raw.AutoCompact
	}
	if raw.CompactIdleSeconds != nil {
		cfg.CompactIdleSeconds = *raw.CompactIdleSeconds
	}
	if raw.CompactMinWALMB != nil {
		cfg.CompactMinWALMB = *raw.CompactMinWALMB
	}
	if raw.CompactPeakRate != nil {
		cfg.CompactPeakRate = *raw.CompactPeakRate
	}
	return true
}

// fillFromConfig 用运行时配置填充文件布局（用于保存）。
func (f *configFile) fillFromConfig(cfg *Config) {
	str := func(s string) *string { return &s }
	intp := func(i int) *int { return &i }
	boolp := func(b bool) *bool { return &b }
	*f = configFile{
		Port:               intp(cfg.Port),
		WALDir:             str(cfg.WALDir),
		WALFile:            str(cfg.WALFile),
		PrivilegeFile:      str(cfg.PrivilegeFile),
		User:               str(cfg.User),
		Password:           str(cfg.Password),
		FlushIntervalMS:    intp(int(cfg.FlushInterval / time.Millisecond)),
		GroupCommitMS:      intp(int(cfg.GroupCommitInterval / time.Millisecond)),
		TTLCleanMS:         intp(int(cfg.TTLCleanInterval / time.Millisecond)),
		IdleTimeoutS:       intp(int(cfg.IdleTimeout / time.Second)),
		BackupDir:          str(cfg.BackupDir),
		MetricsPort:        intp(cfg.MetricsPort),
		EnableChecksum:     boolp(cfg.EnableChecksum),
		Durability:         str(string(cfg.Durability)),
		MySQLEnabled:       boolp(cfg.MySQLEnabled),
		MySQLPort:          intp(cfg.MySQLPort),
		AutoCompact:        boolp(cfg.AutoCompact),
		CompactIdleSeconds: intp(cfg.CompactIdleSeconds),
		CompactMinWALMB:    intp(cfg.CompactMinWALMB),
		CompactPeakRate:    intp(cfg.CompactPeakRate),
	}
}

func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes") || strings.EqualFold(v, "on")
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// applyEnvToConfig 把环境变量叠加到 cfg（命令行/进程环境优先，.env 已于迁移阶段并入文件）。
func applyEnvToConfig(cfg *Config) {
	if v := os.Getenv("TSUMUGI_DURABILITY"); Durability(v).Valid() {
		cfg.Durability = Durability(v)
	}
	if v := os.Getenv("TSUMUGI_MYSQL"); v == "true" || v == "1" || strings.EqualFold(v, "on") {
		cfg.MySQLEnabled = true
	}
	if p := os.Getenv("TSUMUGI_MYSQL_PORT"); p != "" {
		if n, e := strconv.Atoi(p); e == nil && n > 0 && n < 65536 {
			cfg.MySQLPort = n
		}
	}
	if v := os.Getenv("TSUMUGI_AUTO_COMPACT"); v != "" {
		cfg.AutoCompact = getenvBool("TSUMUGI_AUTO_COMPACT", true)
	}
	if p := getenvInt("TSUMUGI_PORT", 0); p != 0 {
		cfg.Port = p
	}
}

// applyDotEnvToConfig 把 .env 解析出的 TSUMUGI_* 值写入 cfg（用于迁移）。
func applyDotEnvToConfig(cfg *Config, kv map[string]string) {
	get := func(k string) string { return kv[k] }
	if v := get("TSUMUGI_DURABILITY"); Durability(v).Valid() {
		cfg.Durability = Durability(v)
	}
	if v := get("TSUMUGI_MYSQL"); v == "true" || v == "1" || strings.EqualFold(v, "on") {
		cfg.MySQLEnabled = true
	}
	if p := get("TSUMUGI_MYSQL_PORT"); p != "" {
		if n, e := strconv.Atoi(p); e == nil && n > 0 && n < 65536 {
			cfg.MySQLPort = n
		}
	}
	if v := get("TSUMUGI_AUTO_COMPACT"); v != "" {
		cfg.AutoCompact = strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
	}
	if p := get("TSUMUGI_PORT"); p != "" {
		if n, e := strconv.Atoi(p); e == nil && n > 0 && n < 65536 {
			cfg.Port = n
		}
	}
}

// ensureConfigFile 保证 config/tsumugi.json 存在：
//   - 若文件缺失，用当前 cfg 自动创建（含全部默认值）；
//   - 若存在 .env，把 TSUMUGI_* 迁移进 cfg 后重写文件，并移除 .env；
//
// 返回是否把（可能被迁移过的）配置已写盘。
func ensureConfigFile(cfg *Config) {
	os.MkdirAll("config", 0755)

	// 1) .env 迁移：优先于已有文件记录，作为一次性的存量导入。
	if kv, ok := loadDotEnvKV(); ok {
		applyDotEnvToConfig(cfg, kv)
		persistConfig(cfg)
		// 迁移后移除 .env，此后配置只存在于 config/tsumugi.json
		os.Remove(".env")
		logf(LOG_OK, "migrated .env into %s (removed .env)", tsumugiConfigPath)
		return
	}

	// 2) 已有配置文件存在：直接使用
	if _, err := os.Stat(tsumugiConfigPath); err == nil {
		return
	}

	// 3) 都没有：写一份含默认值的配置
	persistConfig(cfg)
	logf(LOG_OK, "created default config %s", tsumugiConfigPath)
}

// persistConfig 把 cfg 落盘到 config/tsumugi.json。
func persistConfig(cfg *Config) {
	var f configFile
	f.fillFromConfig(cfg)
	if err := writeJSONFile(tsumugiConfigPath, &f); err != nil {
		logf(LOG_ERR, "write %s: %v", tsumugiConfigPath, err)
	}
}

func loadConfig() *Config {
	cfg := &Config{
		Port:                9999,
		WALDir:              "./data",
		WALFile:             "tsumugi.wal",
		PrivilegeFile:       "privileges.json",
		User:                "root",
		Password:            "password",
		FlushInterval:       100 * time.Millisecond,
		GroupCommitInterval: 2 * time.Millisecond,
		TTLCleanInterval:    30 * time.Second,
		IdleTimeout:         60 * time.Second,
		BackupDir:           "./backup",
		MetricsPort:         10232,
		EnableChecksum:      true,
		Durability:          DuraBatch,
		MySQLEnabled:        false,
		MySQLPort:           3309,
		AutoCompact:         true,
		CompactIdleSeconds:  60,
		CompactMinWALMB:     64,
		CompactPeakRate:     50,
	}
	// 迁移 .env 并保证 config/tsumugi.json 存在（自动创建）。
	ensureConfigFile(cfg)
	// 配置文件（含已迁移值）作为单一事实源。
	loadConfigFile(cfg)
	// 进程环境变量可覆盖（不含 .env，.env 已迁移进文件）。
	applyEnvToConfig(cfg)
	return cfg
}
