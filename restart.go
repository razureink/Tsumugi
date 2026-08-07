package main

// ==================== 服务重启（网页端触发） ====================
// 部分配置（如实验性显存镜像）只在启动时生效；保存后需要重启服务。
// /api/admin/restart 用当前可执行文件与相同参数拉起一个新的子进程，
// 随后优雅关闭当前进程。子进程独立于父进程运行，父进程退出不影响它。

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
	if err := cmd.Start(); err != nil {
		logf(LOG_ERR, "restart: start process: %v", err)
		restartInProgress.Store(0)
		return
	}
	logf(LOG_VERB, "restart: new process started (pid=%d), shutting down old instance", cmd.Process.Pid)
	// 给新进程留出绑定端口的时间，再优雅关闭当前实例
	go func() {
		time.Sleep(1500 * time.Millisecond)
		db.Close()
		os.Exit(0)
	}()
}