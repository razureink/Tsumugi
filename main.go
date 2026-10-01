package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg := loadConfig()
	db, err := NewDB(cfg)
	if err != nil {
		logf(LOG_ERR, "init db: %v", err)
		return
	}
	defer db.Close()

	// MySQL 协议兼容服务器（由 config/tsumugi.json 中 mysql_enabled=true 开启）
	if cfg.MySQLEnabled {
		go startMySQLServer(db, cfg.MySQLPort)
		logf(LOG_VERB, "MySQL protocol server listening on :%d", cfg.MySQLPort)
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv, err := NewServer(addr, db)
	if err != nil {
		logf(LOG_ERR, "start server: %v", err)
		return
	}
	// 监听 Ctrl+C / docker stop（SIGTERM）：优雅关闭，冲刷 WAL 后退出
	//（batch 模式下不处理信号会丢最后一个刷盘周期的写入）。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logf(LOG_OK, "received %s, shutting down...", sig)
		srv.Stop()
		db.Close()
		os.Exit(0)
	}()
	srv.Start()
}
