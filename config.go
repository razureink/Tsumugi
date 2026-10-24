package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const tsumugiConfigPath = "config/tsumugi.json"

// passwordSHA1Hex 计算 mysql_native_password 认证所需的 SHA1(password) 十六进制摘要。
// 配置文件只持久化该摘要，不再落明文口令。
func passwordSHA1Hex(password string) string {
	h := sha1.Sum([]byte(password))
	return hex.EncodeToString(h[:])
}

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
	BindHost            string // 监听地址：空=全部网卡；生产建议 127.0.0.1 + 反向代理
	TLSCert             string // 面板 HTTPS 证书路径（与 TLSKey 同时配置才生效）
	TLSKey              string
	MaxConns            int      // 二进制协议并发连接上限
	TrustedProxies      []string // 可信反代 CIDR：命中时才采信 X-Forwarded-For
	WALDir              string
	WALFile             string
	PrivilegeFile       string
	User                string
	Password            string // 仅驻内存的兼容字段，不再持久化；认证一律走 PasswordSHA1
	PasswordSHA1        string // SHA1(password) hex，mysql_native_password 校验用
	MetricsToken        string // /metrics 抓取令牌（Prometheus 用；与登录会话令牌分离）
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
	// TxnConflict：事务写冲突策略。
	//   error            —— 默认：提交时校验行版本，冲突则回滚并报错（乐观并发）
	//   last_write_wins  —— 不做冲突校验，后写者直接覆盖（与 MySQL autocommit 一致，
	//                       适合 ORM 默认给每条语句套隐式事务的场景：冲突在 COMMIT 才报，
	//                       驱动层的"语句级重试"根本不会触发）
	TxnConflict string
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
// 注意：不再写出明文 password；兼容读取旧文件的 password 字段并迁移为 SHA1。
type configFile struct {
	Port               *int     `json:"port"`
	BindHost           *string  `json:"bind_host,omitempty"`
	TLSCert            *string  `json:"tls_cert,omitempty"`
	TLSKey             *string  `json:"tls_key,omitempty"`
	MaxConns           *int     `json:"max_conns,omitempty"`
	TrustedProxies     []string `json:"trusted_proxies,omitempty"`
	WALDir             *string  `json:"wal_dir"`
	WALFile            *string  `json:"wal_file"`
	PrivilegeFile      *string  `json:"privilege_file"`
	User               *string  `json:"user"`
	PasswordSHA1       *string  `json:"password_sha1,omitempty"`
	Password           *string  `json:"password,omitempty"` // 旧格式兼容读取，保存时不再写出
	MetricsToken       *string  `json:"metrics_token,omitempty"`
	FlushIntervalMS    *int     `json:"flush_interval_ms"`
	GroupCommitMS      *int     `json:"group_commit_ms"`
	TTLCleanMS         *int     `json:"ttl_clean_ms"`
	IdleTimeoutS       *int     `json:"idle_timeout_s"`
	BackupDir          *string  `json:"backup_dir"`
	MetricsPort        *int     `json:"metrics_port"`
	EnableChecksum     *bool    `json:"enable_checksum"`
	Durability         *string  `json:"durability"`
	MySQLEnabled       *bool    `json:"mysql_enabled"`
	MySQLPort          *int     `json:"mysql_port"`
	AutoCompact        *bool    `json:"auto_compact"`
	CompactIdleSeconds *int     `json:"compact_idle_seconds"`
	CompactMinWALMB    *int     `json:"compact_min_wal_mb"`
	CompactPeakRate    *int     `json:"compact_peak_rate"`
	TxnConflict        *string  `json:"txn_conflict,omitempty"`
}

// loadConfigFile 从 /config/tsumugi.json 读取 Tsumugi 自身配置（若存在）。
// 返回 (文件是否存在, 是否解析成功)。
func loadConfigFile(cfg *Config) (bool, bool) {
	data, err := os.ReadFile(tsumugiConfigPath)
	if err != nil {
		return false, false
	}
	existed := true
	var raw configFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return existed, false
	}
	if raw.Port != nil {
		cfg.Port = *raw.Port
	}
	if raw.BindHost != nil {
		cfg.BindHost = *raw.BindHost
	}
	if raw.TLSCert != nil {
		cfg.TLSCert = *raw.TLSCert
	}
	if raw.TLSKey != nil {
		cfg.TLSKey = *raw.TLSKey
	}
	if raw.MaxConns != nil && *raw.MaxConns > 0 {
		cfg.MaxConns = *raw.MaxConns
	}
	if raw.TrustedProxies != nil {
		cfg.TrustedProxies = raw.TrustedProxies
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
	if raw.MetricsToken != nil {
		cfg.MetricsToken = *raw.MetricsToken
	}
	if raw.PasswordSHA1 != nil {
		cfg.PasswordSHA1 = *raw.PasswordSHA1
	} else if raw.Password != nil {
		// 旧格式迁移：明文只在本次会话内存中保留（兼容尚未改完的认证路径），
		// 摘要立即计算；下次保存起文件里只会有 password_sha1。
		cfg.Password = *raw.Password
		cfg.PasswordSHA1 = passwordSHA1Hex(*raw.Password)
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
	if raw.TxnConflict != nil {
		cfg.TxnConflict = *raw.TxnConflict
	}
	return existed, true
}

// fillFromConfig 用运行时配置填充文件布局（用于保存）。
// 明文 Password 一律不写出（仅 PasswordSHA1）；内存中的兼容值随保存自然淘汰。
func (f *configFile) fillFromConfig(cfg *Config) {
	str := func(s string) *string { return &s }
	intp := func(i int) *int { return &i }
	boolp := func(b bool) *bool { return &b }
	*f = configFile{
		Port:               intp(cfg.Port),
		BindHost:           str(cfg.BindHost),
		TLSCert:            str(cfg.TLSCert),
		TLSKey:             str(cfg.TLSKey),
		MaxConns:           intp(cfg.MaxConns),
		TrustedProxies:     cfg.TrustedProxies,
		WALDir:             str(cfg.WALDir),
		WALFile:            str(cfg.WALFile),
		PrivilegeFile:      str(cfg.PrivilegeFile),
		User:               str(cfg.User),
		PasswordSHA1:       str(cfg.PasswordSHA1),
		MetricsToken:       str(cfg.MetricsToken),
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
		TxnConflict:        str(cfg.TxnConflict),
	}
}

func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes") || strings.EqualFold(v, "on") {
		return true
	}
	if strings.EqualFold(v, "false") || v == "0" || strings.EqualFold(v, "no") || strings.EqualFold(v, "off") {
		return false
	}
	return def
}

func parsePort(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n >= 65536 {
		return 0, false
	}
	return n, true
}

// applyEnvToConfig 把环境变量叠加到 cfg（命令行/进程环境优先，.env 已于迁移阶段并入文件）。
func applyEnvToConfig(cfg *Config) {
	if v := os.Getenv("TSUMUGI_DURABILITY"); Durability(v).Valid() {
		cfg.Durability = Durability(v)
	}
	if v := os.Getenv("TSUMUGI_MYSQL"); v != "" {
		cfg.MySQLEnabled = getenvBool("TSUMUGI_MYSQL", cfg.MySQLEnabled)
	}
	if v := os.Getenv("TSUMUGI_TXN_CONFLICT"); v == "error" || v == "last_write_wins" {
		cfg.TxnConflict = v
	}
	if p := os.Getenv("TSUMUGI_MYSQL_PORT"); p != "" {
		if n, ok := parsePort(p); ok {
			cfg.MySQLPort = n
		}
	}
	if p := os.Getenv("TSUMUGI_METRICS_PORT"); p != "" {
		if n, ok := parsePort(p); ok {
			cfg.MetricsPort = n
		}
	}
	if v := os.Getenv("TSUMUGI_AUTO_COMPACT"); v != "" {
		cfg.AutoCompact = getenvBool("TSUMUGI_AUTO_COMPACT", true)
	}
	if p := os.Getenv("TSUMUGI_PORT"); p != "" {
		if n, ok := parsePort(p); ok {
			cfg.Port = n
		}
	}
	if v := os.Getenv("TSUMUGI_BIND_HOST"); v != "" {
		cfg.BindHost = v
	}
	if v := os.Getenv("TSUMUGI_TLS_CERT"); v != "" {
		cfg.TLSCert = v
	}
	if v := os.Getenv("TSUMUGI_TLS_KEY"); v != "" {
		cfg.TLSKey = v
	}
	if v := os.Getenv("TSUMUGI_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxConns = n
		}
	}
	if v := os.Getenv("TSUMUGI_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxies = strings.Split(v, ",")
	}
}

// listenAddr 拼接监听地址：BindHost 为空表示监听所有网卡（JoinHostPort 会生成 ":port"）。
func (c *Config) listenAddr(port int) string {
	return net.JoinHostPort(c.BindHost, strconv.Itoa(port))
}

// applyDotEnvToConfig 把 .env 解析出的 TSUMUGI_* 值写入 cfg（用于迁移）。
// 返回是否包含至少一个 TSUMUGI_* 键。
func applyDotEnvToConfig(cfg *Config, kv map[string]string) bool {
	get := func(k string) string { return kv[k] }
	found := false
	if v := get("TSUMUGI_DURABILITY"); Durability(v).Valid() {
		cfg.Durability = Durability(v)
		found = true
	}
	if v := get("TSUMUGI_MYSQL"); v != "" {
		cfg.MySQLEnabled = strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "on")
		found = true
	}
	if p := get("TSUMUGI_MYSQL_PORT"); p != "" {
		if n, ok := parsePort(p); ok {
			cfg.MySQLPort = n
			found = true
		}
	}
	if v := get("TSUMUGI_AUTO_COMPACT"); v != "" {
		cfg.AutoCompact = strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes") || strings.EqualFold(v, "on")
		found = true
	}
	if p := get("TSUMUGI_PORT"); p != "" {
		if n, ok := parsePort(p); ok {
			cfg.Port = n
			found = true
		}
	}
	return found
}

// ensureConfigFile 保证 config/tsumugi.json 存在：缺失时用当前 cfg 创建。
// .env 迁移移到 loadConfig 中（先读已有文件再叠加 .env，避免覆盖用户自定义配置）。
func ensureConfigFile(cfg *Config) {
	if _, err := os.Stat(tsumugiConfigPath); err == nil {
		return
	}
	if err := os.MkdirAll("config", 0700); err != nil {
		logf(LOG_ERR, "create config dir: %v", err)
		return
	}
	persistConfig(cfg)
	logf(LOG_OK, "created default config %s", tsumugiConfigPath)
}

// persistConfig 把 cfg 落盘到 config/tsumugi.json（原子写，0600）。
func persistConfig(cfg *Config) {
	var f configFile
	f.fillFromConfig(cfg)
	if err := writeJSONFile(tsumugiConfigPath, &f); err != nil {
		logf(LOG_ERR, "write %s: %v", tsumugiConfigPath, err)
	}
}

// normalizeConfig 钳制所有数值字段到安全范围。手改的 config/tsumugi.json
// 不经任何校验直达引擎：port 非法会让服务拒绝启动，间隔为 0 会让
// time.NewTicker 直接 panic。环境变量与设置页有各自校验，此处兜底。
func normalizeConfig(cfg *Config) {
	if cfg.Port <= 0 || cfg.Port >= 65536 {
		cfg.Port = 9999
	}
	if cfg.MetricsPort <= 0 || cfg.MetricsPort >= 65536 {
		cfg.MetricsPort = 10232
	}
	if cfg.MySQLPort <= 0 || cfg.MySQLPort >= 65536 {
		cfg.MySQLPort = 3309
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 100 * time.Millisecond
	}
	if cfg.GroupCommitInterval <= 0 {
		cfg.GroupCommitInterval = 2 * time.Millisecond
	}
	if cfg.TTLCleanInterval <= 0 {
		cfg.TTLCleanInterval = 30 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.CompactIdleSeconds <= 0 {
		cfg.CompactIdleSeconds = 60
	}
	if cfg.CompactMinWALMB <= 0 {
		cfg.CompactMinWALMB = 64
	}
	if cfg.CompactPeakRate <= 0 {
		cfg.CompactPeakRate = 50
	}
	// 事务冲突策略：只认 error / last_write_wins，其它值回落到默认
	switch cfg.TxnConflict {
	case "error", "last_write_wins":
	default:
		cfg.TxnConflict = "error"
	}
	if !cfg.Durability.Valid() {
		cfg.Durability = DuraBatch
	}
	if cfg.MaxConns < 16 {
		cfg.MaxConns = 512
	}
	if cfg.MaxConns > 65536 {
		cfg.MaxConns = 65536
	}
	// 证书只配了一半时明确告警：否则会静默降级成明文 HTTP
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		logf(LOG_WARN, "tls_cert/tls_key must be configured together; HTTPS disabled")
		cfg.TLSCert, cfg.TLSKey = "", ""
	}
}

func loadConfig() *Config {
	cfg := &Config{
		Port:          9999,
		MaxConns:      512,
		WALDir:        "./data",
		WALFile:       "tsumugi.wal",
		PrivilegeFile: "privileges.json",
		User:          "root",
		// 不再内置默认口令：未完成安装向导时 MySQL 端口没有可用的
		// password_sha1，verifyNativePassword 会直接拒绝（消除 root/password）。
		Password:            "",
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
		TxnConflict:         "error",
	}
	// 顺序很重要：先读已有配置文件（单一事实源），再把 .env 叠加其上。
	// 旧实现先应用 .env 再读文件，升级用户带残留 .env 时自定义配置会被默认值覆盖。
	existed, ok := loadConfigFile(cfg)
	if existed && !ok {
		logf(LOG_WARN, "%s is corrupt; using defaults (file left in place for inspection)", tsumugiConfigPath)
	}
	// .env 迁移：只叠加 TSUMUGI_* 键；含其他工具的变量时保留 .env 不删。
	if kv, okEnv := loadDotEnvKV(); okEnv {
		if applyDotEnvToConfig(cfg, kv) {
			persistConfig(cfg)
			if err := os.Remove(".env"); err != nil {
				logf(LOG_WARN, "migrated .env but failed to remove it: %v", err)
			} else {
				logf(LOG_OK, "migrated .env into %s (removed .env)", tsumugiConfigPath)
			}
		}
	} else if !existed {
		ensureConfigFile(cfg)
	}
	// 进程环境变量可覆盖（不含 .env，.env 已迁移进文件）。
	applyEnvToConfig(cfg)
	normalizeConfig(cfg)
	// 旧文件兼容迁移：明文 password 已换算为 SHA1，立即以新格式回写。
	if existed && ok && cfg.Password != "" {
		persistConfig(cfg)
	}
	// /metrics 抓取令牌：首次启动自动生成并落盘，避免监控端点裸奔。
	if cfg.MetricsToken == "" {
		cfg.MetricsToken = hex.EncodeToString(mustRandom(16))
		persistConfig(cfg)
	}
	return cfg
}
