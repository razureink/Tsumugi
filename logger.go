package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	LOG_OK   = "[OK]  "
	LOG_ERR  = "[ERR] "
	LOG_WARN = "[WARN]"
	LOG_VERB = "[INFO]"
)

// logMu serializes log output: concurrent Printf interleave and tear lines under benchmark/stress.
var (
	logMu   sync.Mutex
	logMute bool // muted for benchmarks/tests
)

// SetLogMute toggles log output.
func SetLogMute(m bool) {
	logMu.Lock()
	logMute = m
	logMu.Unlock()
}

func logf(level, format string, args ...interface{}) {
	logMu.Lock()
	if !logMute {
		fmt.Printf("%s %s %s\n", time.Now().Format("2006-01-02 15:04:05.000"), level, fmt.Sprintf(format, args...))
	}
	logMu.Unlock()
}
