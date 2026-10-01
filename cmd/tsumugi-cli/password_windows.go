//go:build windows

package main

// Windows 下关闭控制台回显读取密码：kernel32 Get/SetConsoleMode。

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func readPasswordTTY(prompt string) (string, error) {
	fmt.Print(prompt)
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getMode := kernel32.NewProc("GetConsoleMode")
	setMode := kernel32.NewProc("SetConsoleMode")
	h := syscall.Handle(os.Stdin.Fd())
	var mode uint32
	echoOff := false
	if r, _, _ := getMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r != 0 {
		// 清除 ENABLE_ECHO_INPUT(0x4)，保留行编辑与回车提交
		if r2, _, _ := setMode.Call(uintptr(h), uintptr(mode&^0x0004)); r2 != 0 {
			echoOff = true
			defer setMode.Call(uintptr(h), uintptr(mode))
		}
	}
	if !echoOff {
		fmt.Println("(warning: input will be echoed)")
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	fmt.Println()
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
