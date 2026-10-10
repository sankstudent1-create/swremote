package main

// v8.1: Wake-up service status — shows whether the SWRemote Service is
// installed and running (remote wake-up ready) or not.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"
)

func downloadFile(url, dest string) error {
	c := &http.Client{Timeout: 5 * time.Minute}
	r, err := c.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("server returned %d", r.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r.Body)
	return err
}

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
	// install the service — find SWRemote-Service.exe locally or download it
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	svcExe := filepath.Join(dir, "SWRemote-Service.exe")
	if _, err := os.Stat(svcExe); err != nil {
		// not bundled — download from the relay (v8.2.1)
		svcExe = filepath.Join(dir, "SWRemote-Service.exe")
		if dlErr := downloadFile(serverHTTPBase()+"/download/service", svcExe); dlErr != nil {
			return "Could not get service file: " + dlErr.Error()
		}
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
