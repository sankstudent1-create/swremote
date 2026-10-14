// SWRemote Setup — native Windows installer (no PowerShell, no scripts).
//
// v8.6.2: the old PowerShell/WPF wizard died silently on some PCs (a console
// flashed and nothing appeared). This rewrite is a plain Win32 program like
// the agent itself: welcome -> install -> finish, plus --uninstall.
//
// What it does:
//   - stops any running SWRemote-Agent (clean close first, taskkill fallback)
//   - copies the agent + wake-up service to %LOCALAPPDATA%\SWRemote
//   - desktop / Start Menu shortcuts (IShellLink), Run-key startup entry
//   - installs the SWRemoteService (auto-start, restart-on-crash)
//   - registers an Add/Remove Programs entry whose UninstallString is
//     "<agent>" --uninstall
package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var appVersion = "0.0.0" // set by -ldflags

//go:embed SWRemote-Agent.exe
var agentExe []byte

//go:embed SWRemote-Service.exe
var svcExe []byte

// ---------------------------------------------------------------------------
// Win32 plumbing (same style as the agent)
// ---------------------------------------------------------------------------

var (
	u32 = windows.NewLazySystemDLL("user32.dll")
	g32 = windows.NewLazySystemDLL("gdi32.dll")
	s32 = windows.NewLazySystemDLL("shell32.dll")
	o32 = windows.NewLazySystemDLL("ole32.dll")
	c32 = windows.NewLazySystemDLL("comctl32.dll")

	pDefWindowProcW   = u32.NewProc("DefWindowProcW")
	pRegisterClassExW = u32.NewProc("RegisterClassExW")
	pCreateWindowExW  = u32.NewProc("CreateWindowExW")
	pShowWindow       = u32.NewProc("ShowWindow")
	pUpdateWindow     = u32.NewProc("UpdateWindow")
	pGetMessageW      = u32.NewProc("GetMessageW")
	pTranslateMessage = u32.NewProc("TranslateMessage")
	pDispatchMessageW = u32.NewProc("DispatchMessageW")
	pPostQuitMessage  = u32.NewProc("PostQuitMessage")
	pDestroyWindow    = u32.NewProc("DestroyWindow")
	pPostMessageW     = u32.NewProc("PostMessageW")
	pSetWindowTextW   = u32.NewProc("SetWindowTextW")
	pMessageBoxW      = u32.NewProc("MessageBoxW")
	pBeginPaint       = u32.NewProc("BeginPaint")
	pEndPaint         = u32.NewProc("EndPaint")
	pFillRect         = u32.NewProc("FillRect")
	pDrawTextW        = u32.NewProc("DrawTextW")
	pSetBkMode        = u32.NewProc("SetBkMode")
	pSetTextColor     = u32.NewProc("SetTextColor")
	pSelectObject     = u32.NewProc("SelectObject")
	pFindWindowW      = u32.NewProc("FindWindowW")
	pGetSystemMetrics = u32.NewProc("GetSystemMetrics")
	pCreateSolidBrush = g32.NewProc("CreateSolidBrush")
	pCreateFontW      = g32.NewProc("CreateFontW")
	pDeleteObject     = g32.NewProc("DeleteObject")
	pLoadImageW       = u32.NewProc("LoadImageW")
	pSendMessageW     = u32.NewProc("SendMessageW")
	pInitCommon       = c32.NewProc("InitCommonControlsEx")
	pCoCreateInstance = o32.NewProc("CoCreateInstance")
	pCoInitializeEx  = o32.NewProc("CoInitializeEx")
	pSHGetFolderPathW = s32.NewProc("SHGetFolderPathW")
)

const (
	WM_DESTROY = 2
	WM_CLOSE   = 16
	WM_PAINT   = 15
	WM_COMMAND = 273
	WM_APP     = 0x8000

	WM_APP_PROGRESS = WM_APP + 1
	WM_APP_DONE     = WM_APP + 2

	WS_CHILD   = 0x40000000
	WS_VISIBLE = 0x10000000
	WS_TABSTOP = 0x00010000

	BS_PUSHBUTTON    = 0
	BS_DEFPUSHBUTTON = 1
	BS_AUTOCHECKBOX  = 3

	SS_LEFT   = 0
	SS_CENTER = 1

	BM_GETCHECK = 240
	BM_SETCHECK = 241
	BST_CHECKED = 1

	PBM_SETRANGE32 = 1030
	PBM_SETPOS     = 1026
	PBS_SMOOTH     = 1

	SW_SHOWNORMAL = 1
	MB_ICONERROR  = 0x10
	TRANSPARENT   = 1
)

type wndClassEx struct {
	size, style uint32
	wndProc     uintptr
	clsExtra    int32
	wndExtra    int32
	instance    uintptr
	icon        uintptr
	cursor      uintptr
	bg          uintptr
	menuName    *uint16
	className   *uint16
	iconSm      uintptr
}

type rect struct{ l, t, r, b int32 }

func u16(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }
func colorRef(r, g, b byte) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }

func comCallN(unk uintptr, idx int, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(unk))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(idx*8)))
	all := append([]uintptr{unk}, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

func mkFont(name string, size int, bold bool) uintptr {
	w := int32(400)
	if bold {
		w = 700
	}
	h, _, _ := pCreateFontW.Call(
		uintptr(-size*96/72), 0, 0, 0, uintptr(w), 0, 0, 0,
		1 /*DEFAULT_CHARSET*/, 0, 0, 0, 0, uintptr(unsafe.Pointer(u16(name))))
	return h
}

func ctl(parent uintptr, className string, text string, style uintptr, x, y, w, h int32, id uintptr) uintptr {
	hwnd, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16(className))),
		uintptr(unsafe.Pointer(u16(text))),
		style|WS_CHILD|WS_VISIBLE,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, id, 0, 0)
	pSendMessageW.Call(hwnd, 48 /*WM_SETFONT*/, fBody, 1)
	return hwnd
}

func setText(h uintptr, s string) { pSetWindowTextW.Call(h, uintptr(unsafe.Pointer(u16(s)))) }
func showCtl(h uintptr, show bool) {
	cmd := uintptr(0)
	if show {
		cmd = SW_SHOWNORMAL
	}
	pShowWindow.Call(h, cmd)
}
func isChecked(h uintptr) bool {
	r, _, _ := pSendMessageW.Call(h, BM_GETCHECK, 0, 0)
	return r == BST_CHECKED
}
func fatal(msg string) {
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(msg))), uintptr(unsafe.Pointer(u16("SWRemote Setup"))), MB_ICONERROR)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// logging (to %TEMP%\SWRemote-Setup.log — always written, shown on errors)
// ---------------------------------------------------------------------------

var logPath string

func logf(f string, a ...interface{}) {
	if logPath == "" {
		logPath = filepath.Join(os.TempDir(), "SWRemote-Setup.log")
	}
	fh, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if fh != nil {
		fmt.Fprintf(fh, time.Now().Format("15:04:05 ")+f+"\n", a...)
		fh.Close()
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160]
	}
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// shell helpers: shortcuts, folders, Run key
// ---------------------------------------------------------------------------

func guidOf(d1 uint32, d2, d3 uint16, d4 [8]byte) windows.GUID {
	return windows.GUID{Data1: d1, Data2: d2, Data3: d3, Data4: d4}
}

var (
	clsidShellLink  = guidOf(0x00021401, 0x0000, 0x0000, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46})
	iidIShellLinkW  = guidOf(0x000214F9, 0x0000, 0x0000, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46})
	iidIPersistFile = guidOf(0x0000010B, 0x0000, 0x0000, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46})
)

// createShortcut builds a .lnk via IShellLinkW — no PowerShell needed.
func createShortcut(target, workDir, desc, lnkPath string) error {
	pCoInitializeEx.Call(0, 0)
	var sl uintptr
	hr, _, _ := pCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidShellLink)), 0, 1,
		uintptr(unsafe.Pointer(&iidIShellLinkW)),
		uintptr(unsafe.Pointer(&sl)))
	if hr != 0 || sl == 0 {
		return fmt.Errorf("CoCreateInstance ShellLink failed (%x)", hr)
	}
	defer comCallN(sl, 2) // Release
	comCallN(sl, 20, uintptr(unsafe.Pointer(u16(target)))) // SetPath
	comCallN(sl, 9, uintptr(unsafe.Pointer(u16(workDir)))) // SetWorkingDirectory
	comCallN(sl, 7, uintptr(unsafe.Pointer(u16(desc))))    // SetDescription
	var pf uintptr
	if hr := comCallN(sl, 0, uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&pf))); hr != 0 || pf == 0 {
		return fmt.Errorf("IPersistFile failed (%x)", hr)
	}
	defer comCallN(pf, 2)
	if hr := comCallN(pf, 6, uintptr(unsafe.Pointer(u16(lnkPath))), 1); hr != 0 { // Save
		return fmt.Errorf("shortcut save failed (%x)", hr)
	}
	return nil
}

func knownFolder(csidl int) string {
	var buf [260]uint16
	if r, _, _ := pSHGetFolderPathW.Call(0, uintptr(csidl), 0, 0, uintptr(unsafe.Pointer(&buf[0]))); r == 0 {
		return windows.UTF16ToString(buf[:])
	}
	return ""
}

func installDir() string { return filepath.Join(os.Getenv("LOCALAPPDATA"), "SWRemote") }

func setRunKey(target string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("SWRemote", `"`+target+`"`)
}
func delRunKey() {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err == nil {
		k.DeleteValue("SWRemote")
		k.Close()
	}
}

func hiddenCmd(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func processRunning(imageName string) bool {
	out := hiddenCmd("tasklist", "/FI", "IMAGENAME eq "+imageName, "/NH")
	return strings.Contains(out, imageName)
}

// stopAgent asks the running agent window to close, waits, then force-kills.
func stopAgent() {
	hwnd, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(u16("SWRemoteWindow"))), 0)
	if hwnd != 0 {
		logf("stop: asking agent window to close")
		pPostMessageW.Call(hwnd, WM_CLOSE, 0, 0)
		for i := 0; i < 20; i++ {
			if !processRunning("SWRemote-Agent.exe") {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if processRunning("SWRemote-Agent.exe") {
		logf("stop: force killing")
		hiddenCmd("taskkill", "/F", "/IM", "SWRemote-Agent.exe")
		time.Sleep(1500 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// wake-up service install
// ---------------------------------------------------------------------------

func scOK(out string) bool { return strings.Contains(out, "SUCCESS") }

func runElevated(exe string, args ...string) error {
	op, _ := windows.UTF16PtrFromString("runas")
	app, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(args, " "))
	type seiT struct {
		cbSize, fMask, hwnd, verb, file, params, dir uintptr
		show                                                   int32
		instApp, idList, class, hkey, hotKey, icon, process    uintptr
	}
	var sei seiT
	sei.cbSize = uintptr(unsafe.Sizeof(sei))
	sei.verb = uintptr(unsafe.Pointer(op))
	sei.file = uintptr(unsafe.Pointer(app))
	sei.params = uintptr(unsafe.Pointer(params))
	sei.show = SW_SHOWNORMAL
	pExec := s32.NewProc("ShellExecuteExW")
	r, _, _ := pExec.Call(uintptr(unsafe.Pointer(&sei)))
	if r == 0 {
		return fmt.Errorf("elevation failed")
	}
	return nil
}

func installService(dir string) string {
	svcExePath := filepath.Join(dir, "SWRemote-Service.exe")
	if _, err := os.Stat(svcExePath); err != nil {
		return "service file missing"
	}
	hiddenCmd("sc", "stop", "SWRemoteService")
	hiddenCmd("sc", "delete", "SWRemoteService")
	time.Sleep(500 * time.Millisecond)
	out := hiddenCmd("sc", "create", "SWRemoteService",
		"binPath=", `"`+svcExePath+`"`, "start=", "auto",
		"DisplayName=", "SWRemote Service")
	logf("sc create: %s", oneLine(out))
	if !scOK(out) {
		if strings.Contains(out, "FAILED 5") || strings.Contains(out, "Access") {
			logf("service needs admin — trying elevated")
			if err := runElevated("sc", "create", "SWRemoteService",
				"binPath=", `"`+svcExePath+`"`, "start=", "auto",
				"DisplayName=", "SWRemote Service"); err != nil {
				return "needs one admin approval — press 'Install wake-up' in the agent later"
			}
			runElevated("sc", "start", "SWRemoteService")
			return ""
		}
		return "service install failed"
	}
	hiddenCmd("sc", "description", "SWRemoteService",
		"Keeps this PC reachable from your SWRemote dashboard and starts the agent on demand.")
	hiddenCmd("sc", "failure", "SWRemoteService",
		"reset=", "86400", "actions=", "restart/60000/restart/60000/restart/60000")
	out = hiddenCmd("sc", "start", "SWRemoteService")
	logf("sc start: %s", oneLine(out))
	if strings.Contains(out, "FAILED") && !strings.Contains(out, "1056") {
		return "installed but could not start"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Add/Remove Programs entry
// ---------------------------------------------------------------------------

func writeUninstallKey(target string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Uninstall\SWRemote`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	k.SetStringValue("DisplayName", "SWRemote by SWInfoSystems")
	k.SetStringValue("DisplayVersion", appVersion)
	k.SetStringValue("Publisher", "SWInfoSystems")
	k.SetStringValue("UninstallString", `"`+target+`" --uninstall`)
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
	return nil
}
func delUninstallKey() {
	registry.DeleteKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Uninstall\SWRemote`)
}

// ---------------------------------------------------------------------------
// UI state
// ---------------------------------------------------------------------------

var (
	gHWND uintptr
	fTitle, fBody, fBig, fCheck uintptr
	brBlue, brWhite, brFooter    uintptr

	cWelTitle, cWelBody                   uintptr
	cChkDesk, cChkMenu, cChkStart         uintptr
	cProTitle, cProBar, cProText          uintptr
	cFinMark, cFinTitle, cFinBody         uintptr
	cBtnPrimary, cBtnClose                uintptr

	uninstallMode bool
	installing    bool
	finished      string // "", "launch", "close"
	finalTarget   string

	progMu   sync.Mutex
	progPct  int
	progText string
)

const (
	idPrimary = 1001
	idClose   = 1002
)

const winW, winH = 540, 640
const headerH = 104
const footerY = 560

func showPage(page string) {
	wel := page == "welcome"
	pro := page == "progress"
	fin := page == "finish"
	for _, h := range []uintptr{cWelTitle, cWelBody} {
		showCtl(h, wel)
	}
	for _, h := range []uintptr{cChkDesk, cChkMenu, cChkStart} {
		showCtl(h, wel && !uninstallMode)
	}
	for _, h := range []uintptr{cProTitle, cProBar, cProText} {
		showCtl(h, pro)
	}
	for _, h := range []uintptr{cFinMark, cFinTitle, cFinBody} {
		showCtl(h, fin)
	}
	showCtl(cBtnPrimary, page != "progress")
}

func reportProgress(pct int, text string) {
	progMu.Lock()
	progPct, progText = pct, text
	progMu.Unlock()
	logf("setup: %d%% %s", pct, text)
	pPostMessageW.Call(gHWND, WM_APP_PROGRESS, 0, 0)
	time.Sleep(180 * time.Millisecond)
}

func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	f.Close()
	return err
}

// ---------------------------------------------------------------------------
// install / uninstall workers (run on a background goroutine)
// ---------------------------------------------------------------------------

func doInstall() {
	dir := installDir()
	target := filepath.Join(dir, "SWRemote-Agent.exe")
	var warnings []string

	reportProgress(6, "Stopping SWRemote…")
	stopAgent()

	reportProgress(22, "Copying files…")
	if err := os.MkdirAll(dir, 0755); err != nil {
		finishWithError("Could not create the install folder:\n" + err.Error())
		return
	}
	if err := writeFile(target, agentExe); err != nil {
		finishWithError("Could not write the agent:\n" + err.Error())
		return
	}
	if err := writeFile(filepath.Join(dir, "SWRemote-Service.exe"), svcExe); err != nil {
		warnings = append(warnings, "wake-up service file: "+err.Error())
	}
	os.WriteFile(filepath.Join(dir, "version.txt"), []byte("v"+appVersion), 0644)

	desk, menu, start := isChecked(cChkDesk), isChecked(cChkMenu), isChecked(cChkStart)

	reportProgress(45, "Creating shortcuts…")
	if desk {
		if err := createShortcut(target, dir, "SWRemote by SWInfoSystems",
			filepath.Join(knownFolder(0x0000), "SWRemote.lnk")); err != nil {
			warnings = append(warnings, "desktop shortcut: "+err.Error())
			logf("shortcut desktop: %v", err)
		}
	}
	if menu {
		if err := createShortcut(target, dir, "SWRemote by SWInfoSystems",
			filepath.Join(knownFolder(0x0002), "SWRemote.lnk")); err != nil {
			warnings = append(warnings, "start menu shortcut: "+err.Error())
			logf("shortcut startmenu: %v", err)
		}
	}

	reportProgress(62, "Configuring startup…")
	if start {
		if err := setRunKey(target); err != nil {
			warnings = append(warnings, "startup entry: "+err.Error())
		}
	} else {
		delRunKey()
	}

	reportProgress(76, "Installing background service…")
	if w := installService(dir); w != "" {
		warnings = append(warnings, "wake-up service: "+w)
	}

	reportProgress(90, "Registering uninstaller…")
	if err := writeUninstallKey(target); err != nil {
		warnings = append(warnings, "uninstaller entry: "+err.Error())
	}

	reportProgress(100, "Done")
	finalTarget = target
	body := "SWRemote is ready. Open it from the desktop or Start Menu — a window will show your ID and PIN."
	if len(warnings) > 0 {
		body += "\n\nNotes:\n• " + strings.Join(warnings, "\n• ")
	}
	finishOK("Installed!", body, "launch")
}

func doUninstall() {
	dir := installDir()
	reportProgress(10, "Stopping SWRemote…")
	stopAgent()
	hiddenCmd("sc", "stop", "SWRemoteService")
	hiddenCmd("sc", "delete", "SWRemoteService")
	time.Sleep(500 * time.Millisecond)

	reportProgress(35, "Removing shortcuts…")
	os.Remove(filepath.Join(knownFolder(0x0000), "SWRemote.lnk"))
	os.Remove(filepath.Join(knownFolder(0x0002), "SWRemote.lnk"))

	reportProgress(55, "Cleaning registry…")
	delRunKey()
	delUninstallKey()

	reportProgress(75, "Removing files…")
	os.Remove(filepath.Join(dir, "SWRemote-Agent.exe"))
	os.Remove(filepath.Join(dir, "SWRemote-Service.exe"))
	os.Remove(filepath.Join(dir, "version.txt"))
	os.Remove(filepath.Join(dir, "swremote.json"))
	os.Remove(dir) // only succeeds if empty

	reportProgress(100, "Done")
	finishOK("Uninstalled", "SWRemote has been removed from this PC.", "close")
}

func finishOK(title, body, mode string) {
	progMu.Lock()
	progPct, progText = -1, title+"|"+body+"|"+mode
	progMu.Unlock()
	pPostMessageW.Call(gHWND, WM_APP_DONE, 0, 0)
}
func finishWithError(msg string) {
	progMu.Lock()
	progPct, progText = -2, msg
	progMu.Unlock()
	pPostMessageW.Call(gHWND, WM_APP_DONE, 0, 0)
}

// ---------------------------------------------------------------------------
// window procedure
// ---------------------------------------------------------------------------

func wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case WM_DESTROY:
		pPostQuitMessage.Call(0)
		return 0
	case WM_CLOSE:
		if installing {
			return 0 // don't close mid-install
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case WM_APP_PROGRESS:
		progMu.Lock()
		pct, txt := progPct, progText
		progMu.Unlock()
		pSendMessageW.Call(cProBar, PBM_SETPOS, uintptr(pct), 0)
		setText(cProText, txt)
		return 0
	case WM_APP_DONE:
		progMu.Lock()
		pct, txt := progPct, progText
		progMu.Unlock()
		installing = false
		if pct == -2 {
			setText(cFinMark, "!")
			setText(cFinTitle, "Something went wrong")
			setText(cFinBody, txt+"\n\nDetails were saved to:\n"+logPath)
			finished = "close"
		} else {
			parts := strings.SplitN(txt, "|", 3)
			setText(cFinMark, "✓")
			setText(cFinTitle, parts[0])
			setText(cFinBody, parts[1])
			finished = parts[2]
		}
		if finished == "launch" {
			setText(cBtnPrimary, "Launch SWRemote")
		} else {
			setText(cBtnPrimary, "Close")
		}
		showPage("finish")
		return 0
	case WM_COMMAND:
		switch wp & 0xFFFF {
		case idClose:
			pDestroyWindow.Call(hwnd)
		case idPrimary:
			onPrimary()
		}
		return 0
	case WM_PAINT:
		paintChrome(hwnd)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func onPrimary() {
	switch finished {
	case "launch":
		startDetached(finalTarget)
		pDestroyWindow.Call(gHWND)
		return
	case "close":
		pDestroyWindow.Call(gHWND)
		return
	}
	// welcome page -> start the worker
	installing = true
	showPage("progress")
	if uninstallMode {
		setText(cProTitle, "Removing…")
		go doUninstall()
	} else {
		if _, err := os.Stat(filepath.Join(installDir(), "SWRemote-Agent.exe")); err == nil {
			setText(cProTitle, "Updating…")
		} else {
			setText(cProTitle, "Installing…")
		}
		go doInstall()
	}
}

func startDetached(exe string) {
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Start()
}

func paintChrome(hwnd uintptr) {
	type paintStruct struct {
		hdc         uintptr
		erase       int32
		rc          rect
		restore     int32
		incUpdate   [32]byte
		rgbReserved [32]byte
	}
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	// blue header
	var rc rect
	rc.l, rc.t, rc.r, rc.b = 0, 0, winW, headerH
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), brBlue)
	// footer strip
	rc.l, rc.t, rc.r, rc.b = 0, footerY, winW, winH
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), brFooter)
	// header texts
	pSetBkMode.Call(hdc, TRANSPARENT)
	old, _, _ := pSelectObject.Call(hdc, fTitle)
	pSetTextColor.Call(hdc, colorRef(255, 255, 255))
	var tr rect
	tr.l, tr.t, tr.r, tr.b = 28, 20, winW-20, 58
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("SWRemote Setup"))), 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&tr)), 0)
	pSelectObject.Call(hdc, fBody)
	pSetTextColor.Call(hdc, colorRef(0xDC, 0xE9, 0xFB))
	tr.l, tr.t, tr.r, tr.b = 28, 58, winW-20, 84
	sub := "by SWInfoSystems  •  v" + appVersion
	if uninstallMode {
		sub = "by SWInfoSystems"
	}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(sub))), 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&tr)), 0)
	pSelectObject.Call(hdc, old)
	pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	for _, a := range os.Args[1:] {
		if a == "--uninstall" {
			uninstallMode = true
		}
	}
	logf("=== SWRemote Setup v%s starting (uninstall=%v) ===", appVersion, uninstallMode)

	fTitle = mkFont("Segoe UI", 22, true)
	fBody = mkFont("Segoe UI", 13, false)
	fBig = mkFont("Segoe UI", 20, true)
	fCheck = mkFont("Segoe UI", 64, true)
	brBlue, _, _ = pCreateSolidBrush.Call(colorRef(0x2F, 0x7D, 0xE1))
	brWhite, _, _ = pCreateSolidBrush.Call(colorRef(255, 255, 255))
	brFooter, _, _ = pCreateSolidBrush.Call(colorRef(0xF1, 0xF4, 0xF9))

	// progress bar class
	var icc struct{ size, icc uint32 }
	icc.size = 8
	icc.icc = 0x00000020 // ICC_PROGRESS_CLASS
	pInitCommon.Call(uintptr(unsafe.Pointer(&icc)))

	wc := wndClassEx{
		size:      uint32(unsafe.Sizeof(wndClassEx{})),
		wndProc:   windows.NewCallback(wndProc),
		bg:        brWhite,
		className: u16("SWRemoteSetupWindow"),
	}
	if r, _, _ := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		fatal("Could not start the setup window.")
	}

	// center on screen
	sw, _, _ := pGetSystemMetrics.Call(0)
	sh, _, _ := pGetSystemMetrics.Call(1)
	wx, wy := (int(sw)-winW)/2, (int(sh)-winH)/2
	if wx < 0 {
		wx = 0
	}
	if wy < 0 {
		wy = 0
	}

	title := "SWRemote Setup"
	if uninstallMode {
		title = "Uninstall SWRemote"
	}
	hwnd, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16("SWRemoteSetupWindow"))),
		uintptr(unsafe.Pointer(u16(title))),
		uintptr(0x00CA0000)|WS_VISIBLE, // overlapped+caption+sysmenu+minimize, fixed size
		uintptr(wx), uintptr(wy), winW, winH,
		0, 0, 0, 0)
	if hwnd == 0 {
		fatal("Could not start the setup window.")
	}
	gHWND = hwnd

	buildControls(hwnd)
	configureForMode()
	showPage("welcome")
	pShowWindow.Call(hwnd, SW_SHOWNORMAL)
	pUpdateWindow.Call(hwnd)

	var m struct {
		hwnd    uintptr
		message uint32
		wp      uintptr
		lp      uintptr
		time    uint32
		pt      [2]int32
	}
	for {
		if r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) == 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func buildControls(hwnd uintptr) {
	// welcome
	cWelTitle = ctl(hwnd, "STATIC", "Welcome!", SS_LEFT, 36, 130, 468, 32, 201)
	pSendMessageW.Call(cWelTitle, 48, fBig, 1)
	cWelBody = ctl(hwnd, "STATIC", "", SS_LEFT, 36, 168, 468, 210, 202)
	cChkDesk = ctl(hwnd, "BUTTON", "Desktop shortcut", BS_AUTOCHECKBOX|WS_TABSTOP, 36, 392, 468, 24, 203)
	cChkMenu = ctl(hwnd, "BUTTON", "Start Menu shortcut", BS_AUTOCHECKBOX|WS_TABSTOP, 36, 420, 468, 24, 204)
	cChkStart = ctl(hwnd, "BUTTON", "Start SWRemote with Windows", BS_AUTOCHECKBOX|WS_TABSTOP, 36, 448, 468, 24, 205)
	for _, h := range []uintptr{cChkDesk, cChkMenu, cChkStart} {
		pSendMessageW.Call(h, BM_SETCHECK, BST_CHECKED, 0)
	}
	// progress
	cProTitle = ctl(hwnd, "STATIC", "Installing…", SS_LEFT, 36, 130, 468, 32, 301)
	pSendMessageW.Call(cProTitle, 48, fBig, 1)
	cProBar = ctl(hwnd, "msctls_progress32", "", PBS_SMOOTH, 36, 190, 468, 24, 302)
	pSendMessageW.Call(cProBar, PBM_SETRANGE32, 0, 100)
	cProText = ctl(hwnd, "STATIC", "", SS_LEFT, 36, 232, 468, 60, 303)
	// finish
	cFinMark = ctl(hwnd, "STATIC", "✓", SS_CENTER, 36, 130, 468, 100, 401)
	pSendMessageW.Call(cFinMark, 48, fCheck, 1)
	cFinTitle = ctl(hwnd, "STATIC", "", SS_CENTER, 36, 240, 468, 36, 402)
	pSendMessageW.Call(cFinTitle, 48, fBig, 1)
	cFinBody = ctl(hwnd, "STATIC", "", SS_CENTER, 56, 286, 428, 220, 403)
	// footer buttons
	cBtnClose = ctl(hwnd, "BUTTON", "Close", BS_PUSHBUTTON|WS_TABSTOP, 240, 590, 110, 38, idClose)
	cBtnPrimary = ctl(hwnd, "BUTTON", "Install", BS_DEFPUSHBUTTON|WS_TABSTOP, 362, 590, 142, 38, idPrimary)
}

func configureForMode() {
	if uninstallMode {
		setText(cWelTitle, "Remove SWRemote?")
		setText(cWelBody, "This will remove the SWRemote agent, the wake-up service, shortcuts, the startup entry and settings from this PC.\r\n\r\nYour device ID is stored in the settings file and will be removed too.")
		setText(cBtnPrimary, "Uninstall")
		return
	}
	if _, err := os.Stat(filepath.Join(installDir(), "SWRemote-Agent.exe")); err == nil {
		setText(cWelTitle, "Update available")
		setText(cWelBody, "SWRemote is already installed on this PC. This will update it to v"+appVersion+" — your ID and PIN stay the same.\r\n\r\nWhat gets installed:\r\n• SWRemote agent — shares this PC's screen\r\n• Wake-up service — wake this PC from your dashboard\r\n• Start Menu & desktop shortcuts")
		setText(cBtnPrimary, "Update")
	} else {
		setText(cWelBody, "This will install SWRemote on this PC as a proper app — the agent, the wake-up service, shortcuts and an uninstaller entry. Takes less than a minute.\r\n\r\nWhat gets installed:\r\n• SWRemote agent — shares this PC's screen\r\n• Wake-up service — wake this PC from your dashboard (one admin approval)\r\n• Start Menu & desktop shortcuts\r\n• Optional: start automatically with Windows")
		setText(cBtnPrimary, "Install")
	}
}
