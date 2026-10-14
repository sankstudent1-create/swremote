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
	pGetWindowTextW   = u32.NewProc("GetWindowTextW")
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
	pCreatePen        = g32.NewProc("CreatePen")
	pCreateFontW      = g32.NewProc("CreateFontW")
	pDeleteObject     = g32.NewProc("DeleteObject")
	pLoadImageW       = u32.NewProc("LoadImageW")
	pSendMessageW     = u32.NewProc("SendMessageW")
	pInitCommon       = c32.NewProc("InitCommonControlsEx")
	pCoCreateInstance = o32.NewProc("CoCreateInstance")
	pCoInitializeEx  = o32.NewProc("CoInitializeEx")
	pSHGetFolderPathW = s32.NewProc("SHGetFolderPathW")
	pRoundRect        = g32.NewProc("RoundRect")
	pEllipse          = g32.NewProc("Ellipse")
	pInvalidateRect   = u32.NewProc("InvalidateRect")
	pGetDlgCtrlID     = u32.NewProc("GetDlgCtrlID")
	pIsWindowVisible  = u32.NewProc("IsWindowVisible")
)

const (
	WM_DESTROY = 2
	WM_CLOSE   = 16
	WM_PAINT   = 15
	WM_COMMAND = 273
	WM_APP     = 0x8000

	WM_APP_PROGRESS = WM_APP + 1
	WM_APP_DONE     = WM_APP + 2
	WM_APP_STEP     = WM_APP + 3

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

	WM_CTLCOLORSTATIC = 28
	WM_DRAWITEM       = 43
	ODS_SELECTED      = 1
	DT_CENTER         = 1
	DT_VCENTER        = 4
	DT_SINGLELINE     = 0x20
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
	fHeadTitle, fHeadSub, fTitle, fBody, fBig, fSmall, fLabel, fOpt, fBtn, fCheck uintptr
	brBlue, brWhite, brFooter, brCard, brGreen uintptr

	cWelTitle, cWelBody, cWelLoc, cWelOptLabel uintptr
	cChkDesk, cChkMenu, cChkStart               uintptr
	cDescDesk, cDescMenu, cDescStart             uintptr
	cProTitle, cProBar, cProText                uintptr
	cStepLbl                                   [5]uintptr
	cFinTitle, cFinBody                         uintptr
	cBtnPrimary, cBtnClose                      uintptr

	uninstallMode bool
	installing    bool
	finished      string // "", "launch", "close"
	finalTarget   string

	progMu    sync.Mutex
	progPct   int
	progText  string
	stepState [5]bool
	stepNames = [5]string{
		"Copying files",
		"Creating shortcuts",
		"Configuring startup",
		"Installing background service",
		"Registering uninstaller",
	}
)

const (
	idPrimary = 1001
	idClose   = 1002
)

const winW, winH = 540, 660
const headerH = 124
const footerY = 560

// option cards: (x, y, w, h) — checkbox + description live on each card
var cardRects = [3]rect{
	{28, 306, 512, 382},
	{28, 394, 512, 470},
	{28, 482, 512, 558},
}

func showPage(page string) {
	wel := page == "welcome"
	pro := page == "progress"
	fin := page == "finish"
	showCtl(cWelTitle, wel)
	showCtl(cWelBody, wel)
	showCtl(cWelLoc, wel && !uninstallMode)
	showCtl(cWelOptLabel, wel && !uninstallMode)
	for _, h := range []uintptr{cChkDesk, cChkMenu, cChkStart} {
		showCtl(h, wel && !uninstallMode)
	}
	for _, h := range []uintptr{cDescDesk, cDescMenu, cDescStart} {
		showCtl(h, wel && !uninstallMode)
	}
	showCtl(cProTitle, pro)
	showCtl(cProBar, pro)
	showCtl(cProText, pro)
	for _, h := range cStepLbl {
		showCtl(h, pro)
	}
	showCtl(cFinTitle, fin)
	showCtl(cFinBody, fin)
	showCtl(cBtnPrimary, page != "progress")
	// repaint cards / finish badge
	pInvalidateRect.Call(gHWND, 0, 1)
}

// stepDone marks a progress step complete (green check).
func stepDone(i int) {
	progMu.Lock()
	if i >= 0 && i < len(stepState) {
		stepState[i] = true
	}
	progMu.Unlock()
	pPostMessageW.Call(gHWND, WM_APP_STEP, uintptr(i), 0)
}

// setSteps resets the checklist with new labels (used by uninstall).
func setSteps(names [5]string) {
	progMu.Lock()
	stepNames = names
	for i := range stepState {
		stepState[i] = false
	}
	progMu.Unlock()
	for i, h := range cStepLbl {
		if names[i] == "" {
			showCtl(h, false)
		} else {
			showCtl(h, true)
			setText(h, "○  "+names[i])
		}
	}
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
	stepDone(0)

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
	stepDone(1)

	reportProgress(62, "Configuring startup…")
	if start {
		if err := setRunKey(target); err != nil {
			warnings = append(warnings, "startup entry: "+err.Error())
		}
	} else {
		delRunKey()
	}
	stepDone(2)

	reportProgress(76, "Installing background service…")
	if w := installService(dir); w != "" {
		warnings = append(warnings, "wake-up service: "+w)
	}
	stepDone(3)

	reportProgress(90, "Registering uninstaller…")
	if err := writeUninstallKey(target); err != nil {
		warnings = append(warnings, "uninstaller entry: "+err.Error())
	}
	stepDone(4)

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
	setSteps([5]string{
		"Stopping SWRemote",
		"Removing shortcuts",
		"Cleaning registry",
		"Removing files",
		"",
	})
	reportProgress(10, "Stopping SWRemote…")
	stopAgent()
	hiddenCmd("sc", "stop", "SWRemoteService")
	hiddenCmd("sc", "delete", "SWRemoteService")
	time.Sleep(500 * time.Millisecond)
	stepDone(0)

	reportProgress(35, "Removing shortcuts…")
	os.Remove(filepath.Join(knownFolder(0x0000), "SWRemote.lnk"))
	os.Remove(filepath.Join(knownFolder(0x0002), "SWRemote.lnk"))
	stepDone(1)

	reportProgress(55, "Cleaning registry…")
	delRunKey()
	delUninstallKey()
	stepDone(2)

	reportProgress(75, "Removing files…")
	os.Remove(filepath.Join(dir, "SWRemote-Agent.exe"))
	os.Remove(filepath.Join(dir, "SWRemote-Service.exe"))
	os.Remove(filepath.Join(dir, "version.txt"))
	os.Remove(filepath.Join(dir, "swremote.json"))
	os.Remove(dir) // only succeeds if empty
	stepDone(3)

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

func wndProc(hwnd, msg, wp, lp uintptr) (ret uintptr) {
	// v8.6.3: a panic in a GUI program dies silently — log it instead
	defer func() {
		if r := recover(); r != nil {
			logf("setup: PANIC in wndProc msg=%d: %v", msg, r)
			ret = 0
		}
	}()
	switch msg {
	case WM_DESTROY:
		logf("setup: WM_DESTROY")
		pPostQuitMessage.Call(0)
		return 0
	case WM_CLOSE:
		logf("setup: WM_CLOSE installing=%v", installing)
		if installing {
			return 0 // don't close mid-install
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case WM_APP_STEP:
		i := int(wp)
		progMu.Lock()
		done := i >= 0 && i < len(stepState) && stepState[i]
		name := ""
		if i >= 0 && i < len(stepNames) {
			name = stepNames[i]
		}
		progMu.Unlock()
		if done && name != "" {
			setText(cStepLbl[i], "✓  "+name)
		}
		pInvalidateRect.Call(cStepLbl[i], 0, 1)
		return 0
	case WM_CTLCOLORSTATIC:
		id, _, _ := pGetDlgCtrlID.Call(lp)
		pSetBkMode.Call(wp, TRANSPARENT)
		switch id {
		case 211, 212, 213, 214: // descriptions + install path: soft gray on card
			pSetTextColor.Call(wp, colorRef(0x6B, 0x7A, 0x90))
			return brCard
		case 215: // OPTIONS label
			pSetTextColor.Call(wp, colorRef(0x84, 0x94, 0xAB))
			return brWhite
		case 311, 312, 313, 314, 315: // step checklist
			progMu.Lock()
			done := stepState[int(id)-311]
			progMu.Unlock()
			if done {
				pSetTextColor.Call(wp, colorRef(0x22, 0xA3, 0x5F))
			} else {
				pSetTextColor.Call(wp, colorRef(0x84, 0x94, 0xAB))
			}
			return brWhite
		case 402: // finish title
			pSetTextColor.Call(wp, colorRef(0x16, 0x23, 0x3A))
			return brWhite
		case 403: // finish body
			pSetTextColor.Call(wp, colorRef(0x5A, 0x6B, 0x85))
			return brWhite
		default:
			pSetTextColor.Call(wp, colorRef(0x16, 0x23, 0x3A))
			return brWhite
		}
	case WM_DRAWITEM:
		// owner-drawn primary button: rounded blue pill, white bold text
		type drawItem struct {
			ctlType, ctlID, itemID, itemAction, itemState uint32
			hwndItem, hDC                                 uintptr
			rc                                          rect
			itemData                                    uintptr
		}
		di := (*drawItem)(unsafe.Pointer(lp))
		if di.ctlID == idPrimary {
			pressed := di.itemState&ODS_SELECTED != 0
			bg := colorRef(0x2F, 0x7D, 0xE1)
			if pressed {
				bg = colorRef(0x1E, 0x5B, 0xB8)
			}
			br, _, _ := pCreateSolidBrush.Call(bg)
			pen, _, _ := pCreatePen.Call(0, 1, bg)
			pSelectObject.Call(di.hDC, br)
			pSelectObject.Call(di.hDC, pen)
			pRoundRect.Call(di.hDC,
				uintptr(di.rc.l), uintptr(di.rc.t), uintptr(di.rc.r), uintptr(di.rc.b), 20, 20)
			pDeleteObject.Call(br)
			pDeleteObject.Call(pen)
			var txt [64]uint16
			pGetWindowTextW.Call(di.hwndItem, uintptr(unsafe.Pointer(&txt[0])), 64)
			pSetBkMode.Call(di.hDC, TRANSPARENT)
			pSetTextColor.Call(di.hDC, colorRef(255, 255, 255))
			pSelectObject.Call(di.hDC, fBtn)
			pDrawTextW.Call(di.hDC, uintptr(unsafe.Pointer(&txt[0])), 0xFFFFFFFF,
				uintptr(unsafe.Pointer(&di.rc)), DT_CENTER|DT_VCENTER|DT_SINGLELINE)
			return 1
		}
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
			setText(cFinTitle, "Something went wrong")
			setText(cFinBody, txt+"\n\nDetails were saved to:\n"+logPath)
			finished = "close"
		} else {
			parts := strings.SplitN(txt, "|", 3)
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
		setSteps(stepNames) // reset checklist to install steps
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

	// white canvas
	var rc rect
	rc.l, rc.t, rc.r, rc.b = 0, 0, winW, winH
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), brWhite)

	// header: vertical gradient #1E5BB8 -> #5AA2F5
	for y := int32(0); y < headerH; y++ {
		t := float64(y) / float64(headerH)
		r := byte(float64(0x1E) + t*float64(0x5A-0x1E))
		g := byte(float64(0x5B) + t*float64(0xA2-0x5B))
		b := byte(float64(0xB8) + t*float64(0xF5-0xB8))
		br, _, _ := pCreateSolidBrush.Call(colorRef(r, g, b))
		var band rect
		band.l, band.t, band.r, band.b = 0, y, winW, y+1
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&band)), br)
		pDeleteObject.Call(br)
	}

	// monogram: white rounded square + blue "S"
	penW, _, _ := pCreatePen.Call(0, 1, colorRef(255, 255, 255))
	pSelectObject.Call(hdc, brWhite)
	pSelectObject.Call(hdc, penW)
	pRoundRect.Call(hdc, 28, 30, 92, 94, 36, 36)
	pDeleteObject.Call(penW)
	pSetBkMode.Call(hdc, TRANSPARENT)
	pSetTextColor.Call(hdc, colorRef(0x2F, 0x7D, 0xE1))
	old, _, _ := pSelectObject.Call(hdc, fHeadTitle)
	var mr rect
	mr.l, mr.t, mr.r, mr.b = 28, 34, 92, 94
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("S"))), 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&mr)), DT_CENTER|DT_VCENTER|DT_SINGLELINE)

	// header texts
	pSelectObject.Call(hdc, fHeadTitle)
	pSetTextColor.Call(hdc, colorRef(255, 255, 255))
	var tr rect
	tr.l, tr.t, tr.r, tr.b = 110, 30, winW-20, 66
	title := "SWRemote Setup"
	if uninstallMode {
		title = "Uninstall SWRemote"
	}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(title))), 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&tr)), 0)
	pSelectObject.Call(hdc, fHeadSub)
	pSetTextColor.Call(hdc, colorRef(0xDC, 0xE9, 0xFB))
	tr.l, tr.t, tr.r, tr.b = 110, 68, winW-20, 94
	sub := "by SWInfoSystems  •  v" + appVersion
	if uninstallMode {
		sub = "by SWInfoSystems"
	}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(sub))), 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&tr)), 0)
	pSelectObject.Call(hdc, old)

	// footer strip
	rc.l, rc.t, rc.r, rc.b = 0, footerY, winW, winH
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), brFooter)

	// figure out which page is showing (by welcome title visibility)
	vis, _, _ := pIsWindowVisible.Call(cWelTitle)
	if vis != 0 && !uninstallMode {
		// option cards
		pen, _, _ := pCreatePen.Call(0, 1, colorRef(0xE3, 0xE9, 0xF2))
		pSelectObject.Call(hdc, brCard)
		pSelectObject.Call(hdc, pen)
		for _, c := range cardRects {
			pRoundRect.Call(hdc, uintptr(c.l), uintptr(c.t), uintptr(c.r), uintptr(c.b), 28, 28)
		}
		pDeleteObject.Call(pen)
	} else {
		visF, _, _ := pIsWindowVisible.Call(cFinTitle)
		if visF != 0 {
			// finish badge: green circle + white check
			cx, cy, rad := int32(winW/2), int32(212), int32(44)
			pen, _, _ := pCreatePen.Call(0, 1, colorRef(0x22, 0xA3, 0x5F))
			pSelectObject.Call(hdc, brGreen)
			pSelectObject.Call(hdc, pen)
			pEllipse.Call(hdc, uintptr(cx-rad), uintptr(cy-rad), uintptr(cx+rad), uintptr(cy+rad))
			pDeleteObject.Call(pen)
			pSetBkMode.Call(hdc, TRANSPARENT)
			pSetTextColor.Call(hdc, colorRef(255, 255, 255))
			o2, _, _ := pSelectObject.Call(hdc, fCheck)
			var cr rect
			cr.l, cr.t, cr.r, cr.b = cx-rad, cy-rad, cx+rad, cy+rad
			pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("✓"))), 0xFFFFFFFF,
				uintptr(unsafe.Pointer(&cr)), DT_CENTER|DT_VCENTER|DT_SINGLELINE)
			pSelectObject.Call(hdc, o2)
		}
	}
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

	fHeadTitle = mkFont("Segoe UI", 24, true)
	fHeadSub = mkFont("Segoe UI", 12, false)
	fTitle = mkFont("Segoe UI", 22, true)
	fBody = mkFont("Segoe UI", 13, false)
	fBig = mkFont("Segoe UI", 20, true)
	fSmall = mkFont("Segoe UI", 11, false)
	fLabel = mkFont("Segoe UI", 11, true)
	fOpt = mkFont("Segoe UI", 13, true)
	fBtn = mkFont("Segoe UI", 14, true)
	fCheck = mkFont("Segoe UI", 44, true)
	logf("setup: fonts ok")
	brBlue, _, _ = pCreateSolidBrush.Call(colorRef(0x2F, 0x7D, 0xE1))
	brWhite, _, _ = pCreateSolidBrush.Call(colorRef(255, 255, 255))
	brFooter, _, _ = pCreateSolidBrush.Call(colorRef(0xF1, 0xF4, 0xF9))
	brCard, _, _ = pCreateSolidBrush.Call(colorRef(0xF7, 0xF9, 0xFC))
	brGreen, _, _ = pCreateSolidBrush.Call(colorRef(0x22, 0xA3, 0x5F))
	logf("setup: brushes ok")

	// progress bar class
	var icc struct{ size, icc uint32 }
	icc.size = 8
	icc.icc = 0x00000020 // ICC_PROGRESS_CLASS
	pInitCommon.Call(uintptr(unsafe.Pointer(&icc)))
	logf("setup: initcommon ok")

	wc := wndClassEx{
		size:      uint32(unsafe.Sizeof(wndClassEx{})),
		wndProc:   windows.NewCallback(wndProc),
		bg:        brWhite,
		className: u16("SWRemoteSetupWindow"),
	}
	if r, _, _ := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		logf("setup: RegisterClassExW FAILED")
		fatal("Could not start the setup window.")
	}
	logf("setup: class registered")

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
		logf("setup: CreateWindowExW FAILED")
		fatal("Could not start the setup window.")
	}
	gHWND = hwnd
	logf("setup: window created")

	buildControls(hwnd)
	logf("setup: controls built")
	configureForMode()
	showPage("welcome")
	logf("setup: welcome shown")
	pShowWindow.Call(hwnd, SW_SHOWNORMAL)
	pUpdateWindow.Call(hwnd)
	logf("setup: entering message loop")

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
	cWelTitle = ctl(hwnd, "STATIC", "Welcome!", SS_LEFT, 36, 142, 468, 32, 201)
	pSendMessageW.Call(cWelTitle, 48, fBig, 1)
	cWelBody = ctl(hwnd, "STATIC", "", SS_LEFT, 36, 178, 468, 62, 202)
	cWelLoc = ctl(hwnd, "STATIC", "", SS_LEFT, 36, 246, 468, 20, 214)
	pSendMessageW.Call(cWelLoc, 48, fSmall, 1)
	cWelOptLabel = ctl(hwnd, "STATIC", "OPTIONS", SS_LEFT, 36, 280, 468, 20, 215)
	pSendMessageW.Call(cWelOptLabel, 48, fLabel, 1)
	chkDefs := []struct {
		id      uintptr
		label   string
		desc    string
		descID  uintptr
		cardY   int32
		chkVar  *uintptr
		descVar *uintptr
	}{
		{203, "Desktop shortcut", "Open SWRemote right from your desktop", 211, 306, &cChkDesk, &cDescDesk},
		{204, "Start Menu shortcut", "Find SWRemote in the Start Menu", 212, 394, &cChkMenu, &cDescMenu},
		{205, "Start SWRemote with Windows", "Always ready when this PC boots", 213, 482, &cChkStart, &cDescStart},
	}
	for _, d := range chkDefs {
		chk := ctl(hwnd, "BUTTON", d.label, BS_AUTOCHECKBOX|WS_TABSTOP, 46, d.cardY+12, 440, 22, d.id)
		pSendMessageW.Call(chk, 48, fOpt, 1)
		pSendMessageW.Call(chk, BM_SETCHECK, BST_CHECKED, 0)
		*d.chkVar = chk
		desc := ctl(hwnd, "STATIC", d.desc, SS_LEFT, 70, d.cardY+38, 420, 20, d.descID)
		pSendMessageW.Call(desc, 48, fSmall, 1)
		*d.descVar = desc
	}
	// progress
	cProTitle = ctl(hwnd, "STATIC", "Installing…", SS_LEFT, 36, 142, 468, 32, 301)
	pSendMessageW.Call(cProTitle, 48, fBig, 1)
	cProBar = ctl(hwnd, "msctls_progress32", "", PBS_SMOOTH, 36, 190, 468, 22, 302)
	pSendMessageW.Call(cProBar, PBM_SETRANGE32, 0, 100)
	cProText = ctl(hwnd, "STATIC", "", SS_LEFT, 36, 224, 468, 24, 303)
	for i := 0; i < 5; i++ {
		cStepLbl[i] = ctl(hwnd, "STATIC", "○  "+stepNames[i], SS_LEFT,
			36, int32(272+i*30), 468, 24, uintptr(311+i))
	}
	// finish (badge is painted; title + body are statics)
	cFinTitle = ctl(hwnd, "STATIC", "", SS_CENTER, 36, 282, 468, 36, 402)
	pSendMessageW.Call(cFinTitle, 48, fBig, 1)
	cFinBody = ctl(hwnd, "STATIC", "", SS_CENTER, 56, 326, 428, 180, 403)
	// footer buttons — primary is owner-drawn (see WM_DRAWITEM)
	cBtnClose = ctl(hwnd, "BUTTON", "Close", BS_PUSHBUTTON|WS_TABSTOP, 240, 592, 110, 38, idClose)
	cBtnPrimary = ctl(hwnd, "BUTTON", "Install", 0xB|WS_TABSTOP /*BS_OWNERDRAW*/, 362, 592, 142, 38, idPrimary)
	pSendMessageW.Call(cBtnPrimary, 48, fBtn, 1)
}

func configureForMode() {
	setText(cWelLoc, "Install location:  "+installDir())
	if uninstallMode {
		setText(cWelTitle, "Remove SWRemote?")
		setText(cWelBody, "This will remove the SWRemote agent, the wake-up service, shortcuts, the startup entry and settings from this PC.\r\n\r\nYour device ID is stored in the settings file and will be removed too.")
		setText(cBtnPrimary, "Uninstall")
		return
	}
	if _, err := os.Stat(filepath.Join(installDir(), "SWRemote-Agent.exe")); err == nil {
		setText(cWelTitle, "Update available")
		setText(cWelBody, "SWRemote is already installed on this PC. This will update it to v"+appVersion+" — your ID and PIN stay the same.")
		setText(cBtnPrimary, "Update")
	} else {
		setText(cWelTitle, "Welcome to SWRemote")
		setText(cWelBody, "Share this PC's screen and get remote help in seconds. The install takes less than a minute — pick your options below.")
		setText(cBtnPrimary, "Install")
	}
}
