// Watchdog: keeps SWRemote alive. v8.9.0.
//
// Two layers:
//  1. RegisterApplicationRestart — tells Windows to restart us if we crash
//     or hang (detected by Windows Error Reporting).
//  2. In-process watchdog — the GUI thread posts a heartbeat every second;
//     if the heartbeat goes stale for 45s (UI thread stuck, "not
//     responding"), we restart ourselves instead of staying frozen.

package main

import (
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	k32wd                        = windows.NewLazySystemDLL("kernel32.dll")
	pRegisterApplicationRestart  = k32wd.NewProc("RegisterApplicationRestart")
	pUnregisterApplicationRestart = k32wd.NewProc("UnregisterApplicationRestart")

	guiHeartbeat int64 // unix seconds, updated by the GUI thread
	quitting     int32 // set when the user quits cleanly
)

// registerRestart tells Windows to restart the app on crash/hang.
func registerRestart() {
	// flags=0: restart on both crash and hang
	if r, _, _ := pRegisterApplicationRestart.Call(0, 0); r != 0 {
		log("watchdog: RegisterApplicationRestart failed (%x)", r)
	} else {
		log("watchdog: restart-on-crash registered")
	}
}

// unregisterRestart removes the restart registration (clean quit).
func unregisterRestart() {
	pUnregisterApplicationRestart.Call()
}

// heartbeatTick is called from the GUI thread (WM_TIMER) every second.
func heartbeatTick() {
	atomic.StoreInt64(&guiHeartbeat, time.Now().Unix())
}

// startWatchdog launches the monitor goroutine. Call once at startup.
func startWatchdog() {
	atomic.StoreInt64(&guiHeartbeat, time.Now().Unix())
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for range tick.C {
			if atomic.LoadInt32(&quitting) != 0 {
				return
			}
			last := atomic.LoadInt64(&guiHeartbeat)
			stale := time.Now().Unix() - last
			if stale > 45 {
				log("watchdog: GUI thread stuck for %ds — restarting", stale)
				restartSelf()
				return
			}
		}
	}()
}

// restartSelf launches a fresh copy and exits this one.
func restartSelf() {
	// release the single-instance mutex so the new copy can start
	if singleInstanceMutex != 0 {
		windows.CloseHandle(windows.Handle(singleInstanceMutex))
		singleInstanceMutex = 0
	}
	exe, err := os.Executable()
	if err != nil {
		log("watchdog: cannot restart: %v", err)
		os.Exit(1)
	}
	// keep our args (but drop any --watchdog flag)
	args := make([]string, 0, len(os.Args))
	for _, a := range os.Args[1:] {
		if a != "--watchdog" {
			args = append(args, a)
		}
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		log("watchdog: restart failed: %v", err)
		os.Exit(1)
	}
	log("watchdog: restarted as pid %d", cmd.Process.Pid)
	os.Exit(0)
}

// markQuitting records a clean user quit (no auto-restart wanted).
func markQuitting() {
	atomic.StoreInt32(&quitting, 1)
	unregisterRestart()
	writeCleanQuitFlag()
}

func flagDir() string {
	d, err := os.UserCacheDir()
	if err != nil || d == "" {
		d = os.TempDir()
	}
	dir := d + `\SWRemote`
	os.MkdirAll(dir, 0755)
	return dir
}

func cleanQuitFlagPath() string { return flagDir() + `\clean_quit.flag` }

// writeCleanQuitFlag marks that the user quit intentionally.
func writeCleanQuitFlag() {
	os.WriteFile(cleanQuitFlagPath(), []byte("quit"), 0644)
}

// clearCleanQuitFlag removes the flag at startup (we're running again).
func clearCleanQuitFlag() {
	os.Remove(cleanQuitFlagPath())
}

// wasCleanQuit reports whether the last exit was via Quit.
func wasCleanQuit() bool {
	_, err := os.Stat(cleanQuitFlagPath())
	return err == nil
}

// watchdogCheck is the --watchdog entry point for the scheduled task.
// Silently ensures the agent is running; does nothing if the user quit it.
func watchdogCheck() {
	// try the single-instance mutex: if it exists, the agent is running
	mname, _ := windows.UTF16PtrFromString("SWRemote-Agent-Mutex")
	h, _, _ := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(mname)))
	if h != 0 && windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(windows.Handle(h))
		return // already running — nothing to do
	}
	if h != 0 {
		windows.CloseHandle(windows.Handle(h))
	}
	if wasCleanQuit() {
		return // user quit intentionally — stay closed
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe)
	cmd.Start() // best effort; the new process takes it from here
}

// ensureWatchdogTask registers a per-user scheduled task that runs
// --watchdog every 5 minutes. No admin needed.
func ensureWatchdogTask() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// check if the task already exists
	if out, err := exec.Command("schtasks", "/query", "/tn", "SWRemoteWatchdog").CombinedOutput(); err == nil && len(out) > 0 {
		return
	}
	tr := `"` + exe + `" --watchdog`
	out, err := exec.Command("schtasks", "/create", "/tn", "SWRemoteWatchdog",
		"/tr", tr, "/sc", "minute", "/mo", "5", "/f").CombinedOutput()
	if err != nil {
		log("watchdog: scheduled task not created: %s", strings.TrimSpace(string(out)))
		return
	}
	log("watchdog: scheduled task installed (every 5 min)")
}
