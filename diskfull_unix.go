//go:build !windows

package main

import (
	"errors"
	"strings"
	"syscall"
)

// isDiskFull 判断底层 IO 错误是否为"磁盘/配额写满"。
// ENOSPC = 设备无剩余空间；EDQUOT = 超出用户/组配额。
// 与 Windows 版一致：用 errors.As 取原始 Errno 直接比较，
// 不依赖 errors.Is 对任意 Errno 目标的匹配能力。
func isDiskFull(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if errno == syscall.ENOSPC || errno == syscall.EDQUOT {
			return true
		}
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space left") ||
		strings.Contains(s, "disk quota exceeded")
}
