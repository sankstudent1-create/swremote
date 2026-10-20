// Watchdog: keeps SWRemote alive. v8.9.0.
//
// Two layers:
//  1. RegisterApplicationRestart — tells Windows to restart us if we crash
//     or hang (detected by Windows Error Reporting).
//  2. In-process watchdog — the GUI thread posts a heartbeat every second;
//     if the heartbeat goes stale (20s, two consecutive checks, watched
//     only after the first tick) we dump goroutines and restart ourselves
//     instead of staying frozen.

package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

var (
	k32wd                         = windows.NewLazySystemDLL("kernel32.dll")
	pRegisterApplicationRestart   = k32wd.NewProc("RegisterApplicationRestart")
	pUnregisterApplicationRestart = k32wd.NewProc("UnregisterApplicationRestart")

	guiHeartbeat int64 // unix seconds, updated by the GUI thread
	quitting     int32 // set when the user quits cleanly
	armed        int32 // set by heartbeatTick on the first tick
)

// openAgentMutex creates/opens the agent mutex and reports whether another
// instance already holds it. v8.9.2: the old code called CreateMutexW through
// a lazy proc and then read GetLastError in a SEPARATE call — the error from
// the proc call comes back as the .Call's third return, and the separate
// GetLastError raced, so duplicates were almost never detected and copies
// piled up every 5 minutes from the scheduled task.
func openAgentMutex() (windows.Handle, bool) {
	name, _ := windows.UTF16PtrFromString("SWRemote-Agent-Mutex")
	h, err := windows.CreateMutex(nil, false, name)
	return h, err == windows.ERROR_ALREADY_EXISTS
}

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
	atomic.StoreInt32(&armed, 1) // watchdog arms only after the pump ticks
}

// startWatchdog launches the monitor goroutine. Call once at startup.
// v8.9.2: it used to start counting immediately (45s from process start)
// even though startup itself — schtasks, config, prereq COM checks — runs
// before the GUI's first timer tick, so fresh instances were killed mid-
// startup. Now: checks are skipped until the first heartbeat tick, the
// threshold is 20s, the stall must persist across 2 consecutive checks,
// and a full goroutine dump is logged before restarting so the next hang
// tells us exactly where it stuck.
func startWatchdog() {
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		strikes := 0
		for range tick.C {
			if atomic.LoadInt32(&quitting) != 0 {
				return
			}
			if atomic.LoadInt32(&armed) == 0 {
				continue // GUI hasn't ticked yet — still starting up
			}
			last := atomic.LoadInt64(&guiHeartbeat)
			stale := time.Now().Unix() - last
			if stale <= 20 {
				strikes = 0
				continue
			}
			strikes++
			if strikes < 2 {
				continue
			}
			log("watchdog: GUI thread stuck for %ds — restarting", stale)
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			log("watchdog: HANG DUMP:\n%s", buf[:n])
			restartSelf()
			return
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
	h, exists := openAgentMutex()
	if h != 0 {
		windows.CloseHandle(h)
	}
	if exists {
		return // already running — nothing to do
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
