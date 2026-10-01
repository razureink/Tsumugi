package main

// ==================== 服务重启（网页端触发） ====================
// /api/admin/restart 用当前可执行文件与相同参数拉起一个新的子进程，
// 随后立即优雅关闭当前进程并释放端口，子进程再绑定端口。
// 顺序很关键：旧实现先 spawn 子进程、父进程睡 1.5s 才关监听——
// 小数据量时子进程会在 1.5s 内抢先 bind 失败退出，父进程再退出 = 服务全下线。

import (
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

// restartInProgress 防止并发触发多次重启（0=空闲，1=进行中）。
var restartInProgress atomic.Int32

// handleRestart 重新拉起服务进程（网页端"重启"按钮）。
func (db *DB) handleRestart() {
	if !restartInProgress.CompareAndSwap(0, 1) {
		logf(LOG_VERB, "restart: already in progress, ignored")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		logf(LOG_ERR, "restart: resolve executable: %v", err)
		restartInProgress.Store(0)
		return
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	if attr := restartProcAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	// 先关闭全部服务监听（二进制/MySQL 由注册表管理，metrics 由 db.Close 关闭）
	// 和当前实例，再拉起子进程，消除子进程 bind "address already in use" 的竞态窗口。
	go func() {
		closeServiceListeners()
		db.Close()
		if err := cmd.Start(); err != nil {
			logf(LOG_ERR, "restart: start process: %v", err)
			logf(LOG_ERR, "restart: listeners are already closed; start the service manually")
			restartInProgress.Store(0)
			return
		}
		logf(LOG_VERB, "restart: new process started (pid=%d)", cmd.Process.Pid)
		// 子进程已接管；稍等其完成启动日志后退出父进程
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}()
}
