package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg := loadConfig()
	if err := validateRuntimeConfig(cfg); err != nil {
		logf(LOG_ERR, "config: %v", err)
		os.Exit(1)
	}

	db, err := NewDB(cfg)
	if err != nil {
		logf(LOG_ERR, "init db: %v", err)
		os.Exit(1)
	}
	defer db.Close()

	// MySQL 协议兼容服务器（由 config/tsumugi.json 中 mysql_enabled=true 开启）
	if cfg.MySQLEnabled {
		go startMySQLServer(db, cfg.MySQLPort)
	}

	addr := cfg.listenAddr(cfg.Port)
	srv, err := NewServer(addr, db)
	if err != nil {
		logf(LOG_ERR, "start server on %s: %v", addr, err)
		os.Exit(1)
	}

	// 监听 Ctrl+C / docker stop（SIGTERM）：优雅关闭。
	// 顺序：停止接收新连接并断开活动连接 → 冲刷 WAL → 退出。
	// batch 模式下不处理信号会丢最后一个刷盘周期的写入。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logf(LOG_OK, "received %s, shutting down...", sig)
		done := make(chan struct{})
		go func() {
			srv.Stop()
			db.Close()
			close(done)
		}()
		select {
		case <-done:
			logf(LOG_OK, "shutdown complete")
		case <-time.After(20 * time.Second):
			logf(LOG_ERR, "graceful shutdown timed out after 20s, forcing exit")
		}
		os.Exit(0)
	}()
	srv.Start()
}

// validateRuntimeConfig 在启动阶段拦截明显错误的配置，并对生产风险项给出告警。
// 返回 error 表示不可启动（端口冲突）。
func validateRuntimeConfig(cfg *Config) error {
	if cfg.Port == cfg.MetricsPort {
		return fmt.Errorf("port conflicts: binary protocol and panel both use %d", cfg.Port)
	}
	if cfg.MySQLEnabled && (cfg.MySQLPort == cfg.Port || cfg.MySQLPort == cfg.MetricsPort) {
		return fmt.Errorf("port conflicts: mysql_port=%d is already used by another listener", cfg.MySQLPort)
	}
	if cfg.MySQLEnabled && cfg.PasswordSHA1 == "" {
		logf(LOG_WARN, "mysql_enabled=true but password_sha1 is empty: MySQL logins will be rejected until setup completes")
	}
	if cfg.BindHost == "" {
		logf(LOG_WARN, "bind_host is empty: listening on all interfaces. Production should set bind_host=127.0.0.1 and terminate TLS at a reverse proxy, or configure tls_cert/tls_key")
	}
	if cfg.TLSCert == "" {
		logf(LOG_WARN, "panel is served over plain HTTP; credentials and session tokens are not encrypted in transit")
	}
	if !cfg.Durability.Fsync() {
		logf(LOG_WARN, "durability=%s: a crash may lose up to one flush interval (%s) of acknowledged writes; use durability=fsync when data loss is unacceptable", cfg.Durability, cfg.FlushInterval)
	}
	logf(LOG_OK, "config: data=%s binary=%s panel=%s mysql=%v(tcp/%d) durability=%s max_conns=%d",
		cfg.WALDir, cfg.listenAddr(cfg.Port), cfg.listenAddr(cfg.MetricsPort), cfg.MySQLEnabled, cfg.MySQLPort, cfg.Durability, cfg.MaxConns)
	return nil
}
