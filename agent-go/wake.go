package main

// v8.1: Wake-up service status — shows whether the SWRemote Service is
// installed and running (remote wake-up ready) or not.

import (
	"os/exec"
	"strings"
	"time"
	"unsafe"
)

func wakeStatus() string {
	// query the Windows Service
	out, err := exec.Command("sc", "query", "SWRemoteService").Output()
	if err != nil {
		return "Not installed — remote wake-up unavailable"
	}
	s := string(out)
	if strings.Contains(s, "RUNNING") {
		return "✓ Ready — you can wake this PC from the website"
	}
	if strings.Contains(s, "STOPPED") {
		return "Installed but stopped — start it in Services"
	}
	return "Status unknown"
}

func refreshWakeStatus() {
	go func() {
		for {
			s := wakeStatus()
			if gCtl[ctlWakeVal] != 0 {
				pSetWindowTextW.Call(gCtl[ctlWakeVal], uintptr(unsafe.Pointer(u16(s))))
			}
			time.Sleep(30 * time.Second)
		}
	}()
}
