//go:build !windows

package main

import (
	"os"
	"syscall"
)

// lockFile 尝试对文件加独占锁（不阻塞）。
// 进程退出或文件关闭时内核自动释放，不会留下"幽灵锁"。
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlockFile 释放独占锁。
func unlockFile(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
