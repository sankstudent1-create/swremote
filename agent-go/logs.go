package main

// v8.1: Remote log fetching — a same-account viewer can request the PC's
// agent.log from the website. The log is truncated to the last 100KB.

import (
	"os"
	"path/filepath"
)

func agentLogPath() string {
	d, _ := os.UserCacheDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "SWRemote", "agent.log")
}

func sendLogs() {
	p := agentLogPath()
	data, err := os.ReadFile(p)
	if err != nil {
		sendWSJSON(map[string]any{"t": "logs", "error": "no log file yet"})
		return
	}
	// last 100KB only
	if len(data) > 100*1024 {
		data = data[len(data)-100*1024:]
		// trim to first newline so we don't start mid-line
		for i, b := range data {
			if b == '\n' {
				data = data[i+1:]
				break
			}
		}
	}
	sendWSJSON(map[string]any{"t": "logs", "data": string(data)})
}
