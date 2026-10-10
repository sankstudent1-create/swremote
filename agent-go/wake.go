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
	"syscall"
	"time"
	"unsafe"
	"golang.org/x/sys/windows"
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

var pShellExecuteW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteW")

// runElevated runs a program with administrator rights via the UAC prompt.
func runElevated(name string, args ...string) error {
	op, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(name)
	params, _ := windows.UTF16PtrFromString(strings.Join(args, " "))
	ret, _, _ := pShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(params)), 0, 1)
	if ret <= 32 {
		return fmt.Errorf("elevation failed (code %d)", ret)
	}
	return nil
}

// scHidden runs an `sc` command with no console window flash.
func scHidden(args ...string) *exec.Cmd {
	cmd := exec.Command("sc", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func wakeStatus() (string, bool) {
	// returns (statusText, installed)
	out, err := scHidden("query", "SWRemoteService").Output()
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
	// Try normal install first
	out, err := scHidden("create", "SWRemoteService", "binPath=", binPath, "start=", "auto", "DisplayName=", "SWRemote Service").CombinedOutput()
	if err != nil {
		errStr := strings.TrimSpace(string(out))
		// Access denied / OpenSCManager failed = need admin. Retry elevated via UAC.
		if strings.Contains(errStr, "Access") || strings.Contains(errStr, "OpenSCManager") || strings.Contains(errStr, "denied") {
			if elevErr := runElevated("sc", "create", "SWRemoteService", "binPath=", binPath, "start=", "auto", "DisplayName=", "SWRemote Service"); elevErr != nil {
				return "Install needs administrator approval — " + elevErr.Error()
			}
			// elevated install launched; now try to start (also needs admin, so elevate that too)
			runElevated("sc", "start", "SWRemoteService")
			return "✓ Wake-up service install requested — approve the Windows prompt"
		}
		return "Install failed: " + errStr
	}
	if out, err := scHidden("start", "SWRemoteService").CombinedOutput(); err != nil {
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
