//go:build !windows

package main

import "syscall"

// restartProcAttr 在 Linux/macOS 用会话分离（setsid）避免随父进程终止。
func restartProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
