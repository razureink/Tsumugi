//go:build !windows

package main

// Unix 下关闭终端回显读取密码：调用 stty（无第三方依赖的轻量做法）。

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func readPasswordTTY(prompt string) (string, error) {
	fmt.Print(prompt)
	echoOff := false
	if cmd := exec.Command("stty", "-echo"); cmd.Run() == nil {
		echoOff = true
		defer exec.Command("stty", "echo").Run()
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
	return strings.TrimRight(line, "\n"), nil
}
