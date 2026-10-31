// apps.go — installed-app enumeration, launch and kill (v8.9.8).
//
// The console's Apps panel asks for this list ("apps-list"), launches an
// entry ("app-launch") and stops a running one ("app-kill"). Listing comes
// from the Start Menu (the same programs the user sees), running state
// from a Toolhelp process snapshot, launch is allowlisted to the enumerated
// list, and kill is by PID only — never by name, so one app can't take
// down another process that happens to share an exe name.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type appInfo struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Running bool     `json:"running"`
	PIDs    []uint32 `json:"pids,omitempty"`
}

var appsMu sync.Mutex
var lastApps []appInfo
var lastAppsAt time.Time

var clsidShellLink = windows.GUID{Data1: 0x00021401, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
var iidShellLinkW = windows.GUID{Data1: 0x000214F9, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}

// resolveShortcut returns the .exe target of a .lnk file, or "".
func resolveShortcut(lnk string) string {
	sl, hr := coCreate(clsidShellLink, iidShellLinkW)
	if hr != 0 || sl == 0 {
		return ""
	}
	defer comRelease(sl)
	var buf [260]uint16
	var findData [592]byte // WIN32_FIND_DATAW
	// IShellLinkW vtable: 0 QI, 1 AddRef, 2 Release, 3 GetPath
	if r := comCallN(sl, 3, uintptr(unsafe.Pointer(&buf[0])), 260,
		uintptr(unsafe.Pointer(&findData[0])), 0); r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:])
}

func listApps() []appInfo {
	comInit() // per-thread COM; safe to call again on an initialized thread
	// running processes, by lowercased exe name
	procs := map[string][]uint32{}
	if snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0); err == nil {
		defer windows.CloseHandle(snap)
		var pe windows.ProcessEntry32
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32First(snap, &pe); err == nil {
			for {
				if name := windows.UTF16ToString(pe.ExeFile[:]); name != "" {
					key := strings.ToLower(name)
					procs[key] = append(procs[key], pe.ProcessID)
				}
				if err := windows.Process32Next(snap, &pe); err != nil {
					break
				}
			}
		}
	}
	roots := []string{
		filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`),
		filepath.Join(os.Getenv("ProgramData"), `Microsoft\Windows\Start Menu\Programs`),
	}
	seen := map[string]bool{}
	var apps []appInfo
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".lnk") {
				return nil
			}
			name := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
			if strings.Contains(strings.ToLower(name), "uninstall") {
				return nil
			}
			target := resolveShortcut(p)
			if target == "" || !strings.EqualFold(filepath.Ext(target), ".exe") {
				return nil
			}
			key := strings.ToLower(target)
			if seen[key] {
				return nil
			}
			seen[key] = true
			apps = append(apps, appInfo{Name: name, Path: target})
			return nil
		})
	}
	sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	if len(apps) > 400 {
		apps = apps[:400]
	}
	for i := range apps {
		if pids, ok := procs[strings.ToLower(filepath.Base(apps[i].Path))]; ok && len(pids) > 0 {
			apps[i].Running = true
			if len(pids) > 8 {
				pids = pids[:8]
			}
			apps[i].PIDs = pids
		}
	}
	appsMu.Lock()
	lastApps = apps
	lastAppsAt = time.Now()
	appsMu.Unlock()
	return apps
}

// launchApp starts an app only if it is in the enumerated list.
func launchApp(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	apps := listApps()
	real := ""
	for _, a := range apps {
		if strings.EqualFold(a.Path, path) {
			real = a.Path
			break
		}
	}
	if real == "" {
		return fmt.Errorf("not in the installed apps list")
	}
	cmd := exec.Command(real)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err == nil {
		log("app launched: %s", real)
		return nil
	}
	// fallback: ShellExecute "open"
	op, _ := syscall.UTF16PtrFromString("open")
	f, _ := syscall.UTF16PtrFromString(real)
	sh := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteW")
	r, _, _ := sh.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(f)), 0, 0, 1)
	if r <= 32 {
		return fmt.Errorf("launch failed (%d)", r)
	}
	log("app launched: %s", real)
	return nil
}

// killApp terminates one process by PID. It never kills the agent itself.
func killApp(pid int32) error {
	if pid <= 0 {
		return fmt.Errorf("bad pid")
	}
	if int(pid) == os.Getpid() {
		return fmt.Errorf("refusing to kill the agent itself")
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return err
	}
	log("app killed: pid %d", pid)
	return nil
}

func appErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
