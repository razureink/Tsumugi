package main

import "fmt"

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
	srv.Start()
}
