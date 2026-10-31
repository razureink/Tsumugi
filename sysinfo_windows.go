//go:build windows

package main

// Windows 下的物理内存与磁盘剩余空间探测。
// 用 syscall.NewLazyDLL 直接调 kernel32，不引入任何外部依赖。

import (
	"syscall"
	"unsafe"
)

var (
	// kernel32 已在 lock_windows.go 中声明，这里复用同一个句柄
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// totalMemoryBytes 返回物理内存总量；探测失败返回 0。
func totalMemoryBytes() uint64 {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0
	}
	return m.TotalPhys
}

// freeDiskBytes 返回指定路径所在卷的可用字节数；探测失败返回 0。
func freeDiskBytes(dir string) uint64 {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0
	}
	var freeAvail, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0
	}
	return freeAvail
}
