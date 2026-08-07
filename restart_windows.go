//go:build windows

package main

import "syscall"

// restartProcAttr 在 Windows 上用 DETACHED_PROCESS 分离新进程。
func restartProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008}
}