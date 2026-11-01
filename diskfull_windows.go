//go:build windows

package main

import (
	"errors"
	"strings"
	"syscall"
)

// Windows 系统错误码（Go 的 syscall 包未导出这几个常量）。
//   ERROR_HANDLE_DISK_FULL = 39  写句柄所在卷已满
//   ERROR_DISK_FULL        = 112 磁盘已满
//   ERROR_DISK_QUOTA_EXCEEDED = 1295 超出磁盘配额
const (
	errnoHandleDiskFull  = syscall.Errno(39)
	errnoDiskFull        = syscall.Errno(112)
	errnoDiskQuota       = syscall.Errno(1295)
)

// isDiskFull 判断底层 IO 错误是否为"磁盘/配额写满"。
// 注意不能用 errors.Is(err, syscall.Errno(...))：Windows 的 Errno.Is 只匹配
// 少数 os 哨兵错误，对任意 Errno 目标返回 false。这里用 errors.As 取出
// 原始 Errno 再比较；字符串兜底覆盖被上层重新包装后丢失类型的场景。
func isDiskFull(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case errnoDiskFull, errnoHandleDiskFull, errnoDiskQuota:
			return true
		}
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not enough space") ||
		strings.Contains(s, "disk full") ||
		strings.Contains(s, "no space left")
}
