package main

// v8.1: Wake-up service status — shows whether the SWRemote Service is
// installed and running (remote wake-up ready) or not.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"
)

func wakeStatus() (string, bool) {
	// returns (statusText, installed)
	out, err := exec.Command("sc", "query", "SWRemoteService").Output()
	if err != nil {
		return "Not installed — remote wake-up unavailable", false
	}
	s := string(out)
	if strings.Contains(s, "RUNNING") {
		return "✓ Ready — you can wake this PC from the website", true
	}
	if strings.Contains(s, "STOPPED") {
		return "Installed but stopped — start it in Services", true
	}
	return "Status unknown", true
}

func installWakeService() string {
	// install the service from the bundled SWRemote-Service.exe
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	svcExe := filepath.Join(dir, "SWRemote-Service.exe")
	if _, err := os.Stat(svcExe); err != nil {
		// try the installer directory
		svcExe = filepath.Join(dir, "SWRemote-Service.exe")
		return "Service file not found — reinstall SWRemote-Setup.exe"
	}
	binPath := `"` + svcExe + `"`
	if out, err := exec.Command("sc", "create", "SWRemoteService", "binPath=", binPath, "start=", "auto", "DisplayName=", "SWRemote Service").CombinedOutput(); err != nil {
		return "Install failed: " + strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("sc", "start", "SWRemoteService").CombinedOutput(); err != nil {
		return "Installed but could not start: " + strings.TrimSpace(string(out))
	}
	return "✓ Wake-up service installed and running"
}

func refreshWakeStatus() {
	go func() {
		for {
			s, installed := wakeStatus()
			if gCtl[ctlWakeVal] != 0 {
				pSetWindowTextW.Call(gCtl[ctlWakeVal], uintptr(unsafe.Pointer(u16(s))))
			}
			// show/hide the install button
			if gCtl[ctlWakeInstall] != 0 {
				show := uintptr(0)
				if !installed {
					show = 1
				}
				pShowWindow.Call(gCtl[ctlWakeInstall], show)
			}
			time.Sleep(30 * time.Second)
		}
	}()
}
