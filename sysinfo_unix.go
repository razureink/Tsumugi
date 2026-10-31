//go:build !windows

package main

// 类 Unix 下的物理内存与磁盘剩余空间探测（linux / darwin / bsd）。
// 只用标准库：/proc/meminfo + syscall.Statfs。

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// totalMemoryBytes 返回物理内存总量；探测失败返回 0。
func totalMemoryBytes() uint64 {
	// Linux：/proc/meminfo 的 MemTotal（单位 kB）
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		buf := make([]byte, 4096)
		n, _ := f.Read(buf)
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			if !strings.HasPrefix(line, "MemTotal:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}

// freeDiskBytes 返回指定路径所在文件系统的可用字节数；探测失败返回 0。
func freeDiskBytes(dir string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize)
}
