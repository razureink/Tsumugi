package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// acquireDirLock 对数据目录加独占锁。
//
// Tsumugi 是单进程单写者设计：内存态在进程内，WAL 只有一个 writer。
// 两个进程同时打开同一个 data 目录会各自持有内存态并往同一个 WAL 追加，
// 属于静默数据损坏（后写的覆盖先写的，重放结果不可预期）。
// 因此启动时用文件锁挡住第二个实例——进程退出后锁由内核自动释放，
// 不会留下"幽灵锁"影响重启。
func acquireDirLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, ".tsumugi.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("data directory %s is already used by another Tsumugi instance (single instance per data dir): %w", dir, err)
	}
	return f, nil
}

// releaseDirLock 释放数据目录锁。
func releaseDirLock(f *os.File) {
	if f == nil {
		return
	}
	unlockFile(f)
	f.Close()
}
