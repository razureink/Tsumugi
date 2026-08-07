package main

import (
	"fmt"
	"time"
)

const (
	LOG_OK   = "[OK]  "
	LOG_ERR  = "[ERR] "
	LOG_WARN = "[WARN]"
	LOG_VERB = "[INFO]"
)

func logf(level, format string, args ...interface{}) {
	fmt.Printf("%s %s %s\n", time.Now().Format("2006-01-02 15:04:05.000"), level, fmt.Sprintf(format, args...))
}
