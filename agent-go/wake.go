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
		if wakeUACRecent() {
			return "Stopped — waiting for admin approval", true
		}
		return "Stopped — restarting…", true
	}
	return "Status unknown", true
}

// wakeUACMarker records when we last asked for admin (UAC) for the service.
// v8.5.1: file-based so it survives restarts — previously an in-memory
// throttle reset on every launch and the user got a prompt per restart.
func wakeUACMarkerPath() string {
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return filepath.Join(la, "SWRemote", "wake_uac.txt")
	}
	return "wake_uac.txt"
}

func wakeUACRecent() bool {
	b, err := os.ReadFile(wakeUACMarkerPath())
	if err != nil {
		return false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return false
	}
	return time.Since(t) < 24*time.Hour
}

func markWakeUAC() {
	_ = os.MkdirAll(filepath.Dir(wakeUACMarkerPath()), 0755)
	_ = os.WriteFile(wakeUACMarkerPath(), []byte(time.Now().Format(time.RFC3339)), 0644)
}

// ensureWakeRunning keeps the wake-up service alive: if it is installed but
// stopped, start it (with one UAC prompt per day at most when admin is needed).
func ensureWakeRunning() {
	out, err := scHidden("query", "SWRemoteService").Output()
	if err != nil {
		return // not installed
	}
	s := string(out)
	if strings.Contains(s, "RUNNING") || !strings.Contains(s, "STOPPED") {
		return
	}
	if _, err := scHidden("start", "SWRemoteService").Output(); err == nil {
		log("wake: service was stopped — started it")
		return
	}
	// needs admin: explain first (so the prompt isn't a mystery), then ask
	// via UAC — at most once a day to avoid nagging.
	if !wakeUACRecent() {
		log("wake: service needs admin to start — requesting elevation (once a day at most)")
		guiSetStatus("Starting wake-up service — please approve the Windows admin prompt…", false)
		_ = runElevated("sc", "start", "SWRemoteService")
		markWakeUAC()
	} else {
		log("wake: service still stopped (admin approval pending)")
	}
}

func installWakeService() string {
	// v8.5.0: install or repair the wake-up service and make sure it RUNS —
	// refresh the binary, set auto-start + restart-on-crash, then start it
	// (elevated via UAC when the agent isn't admin).
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	svcExe := filepath.Join(dir, "SWRemote-Service.exe")
	// stop first so the exe isn't locked while we refresh it
	_ = scHidden("stop", "SWRemoteService").Run()
	time.Sleep(500 * time.Millisecond)
	// always refresh the service binary so it matches the relay's build
	if dlErr := downloadFile(serverHTTPBase()+"/download/service", svcExe); dlErr != nil {
		if _, st := os.Stat(svcExe); st != nil {
			return "Could not get service file: " + dlErr.Error()
		}
		log("wake: service download failed, keeping existing binary: %v", dlErr)
	} else {
		log("wake: service binary refreshed")
	}
	binPath := `"` + svcExe + `"`
	// create if missing
	if out, err := scHidden("query", "SWRemoteService").Output(); err != nil || !strings.Contains(string(out), "SERVICE_NAME") {
		out, err := scHidden("create", "SWRemoteService", "binPath=", binPath, "start=", "auto", "DisplayName=", "SWRemote Service").CombinedOutput()
		if err != nil {
			errStr := strings.TrimSpace(string(out))
			// Access denied / OpenSCManager failed = need admin. Retry elevated via UAC.
			if strings.Contains(errStr, "Access") || strings.Contains(errStr, "OpenSCManager") || strings.Contains(errStr, "denied") {
				if elevErr := runElevated("sc", "create", "SWRemoteService", "binPath=", binPath, "start=", "auto", "DisplayName=", "SWRemote Service"); elevErr != nil {
					return "Install needs administrator approval — " + elevErr.Error()
				}
				// elevated install launched; configure recovery + start elevated too
				runElevated("sc", "failure", "SWRemoteService", "reset=", "86400", "actions=", "restart/60000/restart/60000/restart/60000")
				runElevated("sc", "start", "SWRemoteService")
				return "✓ Wake-up service install requested — approve the Windows prompt"
			}
			return "Install failed: " + errStr
		}
		log("wake: service created")
	}
	// bulletproof it: auto-start + restart if it ever crashes
	_, _ = scHidden("config", "SWRemoteService", "start=", "auto").Output()
	if out, err := scHidden("failure", "SWRemoteService", "reset=", "86400",
		"actions=", "restart/60000/restart/60000/restart/60000").CombinedOutput(); err != nil {
		_ = runElevated("sc", "failure", "SWRemoteService", "reset=", "86400",
			"actions=", "restart/60000/restart/60000/restart/60000")
		_ = out
	}
	_, _ = scHidden("description", "SWRemoteService",
		"Keeps this PC reachable from your SWRemote dashboard and starts the agent on demand.").Output()
	// start it
	if out, err := scHidden("start", "SWRemoteService").CombinedOutput(); err != nil {
		errStr := strings.TrimSpace(string(out))
		if strings.Contains(errStr, "1056") || strings.Contains(strings.ToLower(errStr), "already") {
			return "✓ Wake-up service installed and running"
		}
		if strings.Contains(errStr, "Access") || strings.Contains(errStr, "denied") {
			_ = runElevated("sc", "start", "SWRemoteService")
			return "✓ Wake-up service ready — approve the Windows prompt to start it"
		}
		return "Installed but could not start: " + errStr
	}
	return "✓ Wake-up service installed and running"
}

// reinstallWakeService removes a stuck/broken service install and does a
// clean install. v8.9.0: the service can sit in "waiting for admin" forever —
// a fresh uninstall+install clears that state.
func reinstallWakeService() string {
	log("wake: fresh reinstall requested")
	_ = scHidden("stop", "SWRemoteService").Run()
	time.Sleep(1000 * time.Millisecond)
	if out, err := scHidden("delete", "SWRemoteService").CombinedOutput(); err != nil {
		errStr := strings.TrimSpace(string(out))
		if strings.Contains(errStr, "Access") || strings.Contains(errStr, "denied") {
			_ = runElevatedWait("sc.exe", "delete", "SWRemoteService")
			time.Sleep(1500 * time.Millisecond)
		} else {
			log("wake: delete: %s", errStr)
		}
	} else {
		log("wake: old service deleted")
	}
	time.Sleep(1000 * time.Millisecond)
	return installWakeService()
}

func refreshWakeStatus() {
	go func() {
		for {
			ensureWakeRunning() // v8.5.0: restart it if it stopped
			s, installed := wakeStatus()
			if gCtl[ctlWakeVal] != 0 {
				pSetWindowTextW.Call(gCtl[ctlWakeVal], uintptr(unsafe.Pointer(u16(s))))
			}
			// show the install/reinstall button (v8.9.0: always visible for fresh reinstalls)
			if gCtl[ctlWakeInstall] != 0 {
				pShowWindow.Call(gCtl[ctlWakeInstall], 1)
				lbl := "Install"
				if installed {
					lbl = "Reinstall"
				}
				pSetWindowTextW.Call(gCtl[ctlWakeInstall], uintptr(unsafe.Pointer(u16(lbl))))
			}
			time.Sleep(30 * time.Second)
		}
	}()
}
