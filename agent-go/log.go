// SWRemote agent diagnostics: file log + panic-safe goroutines.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	logMu   sync.Mutex
	logFile *os.File
)

func logPath() string {
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return filepath.Join(la, "SWRemote", "agent.log")
	}
	return "swremote-agent.log"
}

func initLog() {
	dir := filepath.Dir(logPath())
	_ = os.MkdirAll(dir, 0755)
	if fi, err := os.Stat(logPath()); err == nil && fi.Size() > 2<<20 {
		_ = os.Rename(logPath(), logPath()+".old")
	}
	f, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	logFile = f
	log("=== SWRemote v%s starting (pid %d) ===", appVersion, os.Getpid())
}

func log(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, args...) + "\r\n"
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		_, _ = logFile.WriteString(line)
	}
}

// safeGo runs fn in a goroutine and logs any panic instead of killing the app.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log("PANIC in %s: %v", name, r)
				guiSetStatus("Something went wrong — see agent.log", false)
			}
		}()
		fn()
	}()
}
