// SWRemote agent GUI — native Win32 window, pure Go (no cgo).
package main

import (
	_ "embed"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed logo.bmp
var logoBMP []byte

// ---------- Win32 bindings ----------
var (
	u32   = windows.NewLazySystemDLL("user32.dll")
	g32   = windows.NewLazySystemDLL("gdi32.dll")
	k32   = windows.NewLazySystemDLL("kernel32.dll")
	msimg = windows.NewLazySystemDLL("msimg32.dll")
	comc  = windows.NewLazySystemDLL("comctl32.dll")

	pRegisterClassExW = u32.NewProc("RegisterClassExW")
	pCreateWindowExW   = u32.NewProc("CreateWindowExW")
	pDefWindowProcW    = u32.NewProc("DefWindowProcW")
	pShowWindow        = u32.NewProc("ShowWindow")
	pUpdateWindow      = u32.NewProc("UpdateWindow")
	pGetMessageW       = u32.NewProc("GetMessageW")
	pTranslateMessage  = u32.NewProc("TranslateMessage")
	pDispatchMessageW  = u32.NewProc("DispatchMessageW")
	pPostQuitMessage   = u32.NewProc("PostQuitMessage")
	pPostMessageW      = u32.NewProc("PostMessageW")
	pSendMessageW      = u32.NewProc("SendMessageW")
	pSetWindowTextW    = u32.NewProc("SetWindowTextW")
	pLoadCursorW       = u32.NewProc("LoadCursorW")
	pLoadImageW        = u32.NewProc("LoadImageW")
	pBeginPaint        = u32.NewProc("BeginPaint")
	pEndPaint          = u32.NewProc("EndPaint")
	pOpenClipboard     = u32.NewProc("OpenClipboard")
	pEmptyClipboard    = u32.NewProc("EmptyClipboard")
	pSetClipboardData  = u32.NewProc("SetClipboardData")
	pCloseClipboard    = u32.NewProc("CloseClipboard")
	pCreateMutexW      = k32.NewProc("CreateMutexW")
	pCreateFontW      = g32.NewProc("CreateFontW")
	pCreateSolidBrush = g32.NewProc("CreateSolidBrush")
	pCreatePen        = g32.NewProc("CreatePen")
	pRoundRect        = g32.NewProc("RoundRect")
	pCreateCompatibleDC = g32.NewProc("CreateCompatibleDC")
	pDeleteDC         = g32.NewProc("DeleteDC")
	pBitBlt           = g32.NewProc("BitBlt")
	pSetTextColor     = g32.NewProc("SetTextColor")
	pSetBkMode        = g32.NewProc("SetBkMode")
	pSelectObject     = g32.NewProc("SelectObject")
	pFillRect         = u32.NewProc("FillRect")
	pDrawTextW        = u32.NewProc("DrawTextW")
	pGradientFill     = msimg.NewProc("GradientFill")
	pInitCommon       = comc.NewProc("InitCommonControlsEx")

	pGlobalAlloc = k32.NewProc("GlobalAlloc")
	pGlobalLock  = k32.NewProc("GlobalLock")
	pGlobalUnlock = k32.NewProc("GlobalUnlock")
)

const (
	WS_OVERLAPPEDWINDOW = 0x00CF0000
	WS_VISIBLE          = 0x10000000
	WS_CHILD            = 0x40000000
	WS_TABSTOP          = 0x00010000
	WS_BORDER           = 0x00800000
	BS_PUSHBUTTON       = 0x00000000
	BS_AUTOCHECKBOX     = 0x00000003
	BS_FLAT             = 0x00008000
	SS_LEFT             = 0x00000000
	ES_READONLY         = 0x00000800
	ES_AUTOHSCROLL      = 0x00000080
	SW_SHOW             = 5
	WM_DESTROY          = 2
	WM_PAINT            = 15
	WM_CLOSE            = 16
	WM_COMMAND          = 273
	WM_CTLCOLORSTATIC   = 312
	WM_TIMER            = 0x0113
	WM_APP              = 0x8000
	BN_CLICKED          = 0
	TRANSPARENT         = 1
	CF_UNICODETEXT      = 13
	GMEM_MOVEABLE       = 2
	IDC_ARROW           = 32512
	COLOR_WINDOW        = 5
	BM_GETCHECK         = 240
	BM_SETCHECK         = 241
	BST_CHECKED         = 1
	PBM_SETRANGE32      = 1030
	PBM_SETPOS          = 1026
	MB_YESNO            = 4
	MB_ICONQUESTION     = 32
	MB_ICONINFORMATION  = 64
	IDYES               = 6
)

const (
	WM_APP_STATUS   = WM_APP + 1
	WM_APP_PROGRESS = WM_APP + 2
	WM_APP_UPDBTN   = WM_APP + 3
)

const (
	ctlIDLabel = 101
	ctlIDValue = 102
	ctlPINLabel = 103
	ctlPINValue = 104
	ctlNewPIN   = 105
	ctlDot      = 106
	ctlStatus   = 107
	ctlLinkLbl  = 108
	ctlLinkEdit = 109
	ctlCopy     = 110
	ctlUpdate   = 111
	ctlAutoRun  = 112
	ctlProg     = 113
	ctlQuit     = 114
	ctlVer      = 115
	ctlCopyID   = 116
	ctlMeta     = 117
	ctlClaimLbl = 118
	ctlClaimVal = 119
	ctlCopyClaim = 120
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type msgT struct {
	hwnd    uintptr
	message uint32
	_       uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
	lPriv   uint32
}

type paintStruct struct {
	hdc         uintptr
	fErase      uint32
	left        int32
	top         int32
	right       int32
	bottom      int32
	fRestore    uint32
	fIncUpdate  uint32
	reserved    [32]byte
}

type triVertex struct {
	x, y          int32
	r, g, b, a    uint16
}

type iccEx struct {
	size uint32
	icc  uint32
}

type gradientRect struct {
	upperLeft  uint32
	lowerRight uint32
}

// keep the window-procedure callback alive for the life of the process
var wndProcCb = windows.NewCallback(wndProc)

// ---------- state ----------
type guiState struct {
	mu         sync.Mutex
	statusText string
	statusOK   bool // green vs gray dot
	progress   int  // -1 = hidden
	updBtnText string
	updBtnOn   bool
	viewers    int
	started    time.Time
}

var (
	gHWND    uintptr
	gState   = &guiState{statusText: "Starting…", progress: -1, updBtnText: "Check for Updates", updBtnOn: true, started: time.Now()}
	gCtl     = map[int]uintptr{}
	gWhiteBr uintptr
	gCardBr  uintptr
	gCardPen uintptr
	gFonts   = map[string]uintptr{}
	gLogoBmp uintptr
)

// loadLogoBitmap writes the embedded AI logo to a temp file and loads it as
// an HBITMAP for the header. Returns 0 on failure (header draws without it).
func loadLogoBitmap() uintptr {
	if len(logoBMP) == 0 {
		return 0
	}
	tmp, err := os.CreateTemp("", "swr-logo-*.bmp")
	if err != nil {
		return 0
	}
	path := tmp.Name()
	if _, err := tmp.Write(logoBMP); err != nil {
		tmp.Close()
		os.Remove(path)
		return 0
	}
	tmp.Close()
	defer os.Remove(path) // bitmap lives in memory after LoadImageW
	r, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(u16(path))),
		0 /*IMAGE_BITMAP*/, 0, 0, 0x10 /*LR_LOADFROMFILE*/)
	return r
}

func drawLogo(hdc uintptr) {
	if gLogoBmp == 0 {
		return
	}
	memDC, _, _ := pCreateCompatibleDC.Call(hdc)
	if memDC == 0 {
		return
	}
	oldBmp, _, _ := pSelectObject.Call(memDC, gLogoBmp)
	pBitBlt.Call(hdc, 20, 20, 64, 64, memDC, 0, 0, 0x00CC0020 /*SRCCOPY*/)
	pSelectObject.Call(memDC, oldBmp)
	pDeleteDC.Call(memDC)
}

func u16(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }

func mkFont(name string, pts int, bold bool) uintptr {
	w := int32(400)
	if bold {
		w = 700
	}
	h, _, _ := pCreateFontW.Call(
		uintptr(int32(-pts*96/72)), 0, 0, 0, uintptr(w), 0, 0, 0,
		1, 4, 0, 4, 0, uintptr(unsafe.Pointer(u16(name))))
	return h
}

func colorRef(r, g, b int) uintptr { return uintptr(r | g<<8 | b<<16) }

func mkCtl(class string, text string, style uintptr, x, y, w, h int32, id int, font uintptr) uintptr {
	hwnd, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16(class))),
		uintptr(unsafe.Pointer(u16(text))),
		WS_CHILD|WS_VISIBLE|style,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		gHWND, uintptr(id), 0, 0)
	if font != 0 {
		pSendMessageW.Call(hwnd, 48 /*WM_SETFONT*/, font, 1)
	}
	gCtl[id] = hwnd
	return hwnd
}

func setText(id int, s string) {
	if h, ok := gCtl[id]; ok {
		pSetWindowTextW.Call(h, uintptr(unsafe.Pointer(u16(s))))
	}
}

// thread-safe UI updates from worker goroutines
func guiSetStatus(text string, ok bool) {
	gState.mu.Lock()
	gState.statusText = text
	gState.statusOK = ok
	gState.mu.Unlock()
	if gHWND != 0 {
		pPostMessageW.Call(gHWND, WM_APP_STATUS, 0, 0)
	}
}

func guiSetProgress(pct int) {
	gState.mu.Lock()
	gState.progress = pct
	gState.mu.Unlock()
	if gHWND != 0 {
		pPostMessageW.Call(gHWND, WM_APP_PROGRESS, 0, 0)
	}
}

func guiSetUpdateBtn(text string, enabled bool) {
	gState.mu.Lock()
	gState.updBtnText = text
	gState.updBtnOn = enabled
	gState.mu.Unlock()
	if gHWND != 0 {
		pPostMessageW.Call(gHWND, WM_APP_UPDBTN, 0, 0)
	}
}

func applyStatus() {
	gState.mu.Lock()
	t := gState.statusText
	gState.mu.Unlock()
	setText(ctlStatus, t)
	// repaint the dot so WM_CTLCOLORSTATIC recolors it
	if h, ok := gCtl[ctlDot]; ok {
		pInvalidateRect.Call(h, 0, 1)
	}
}

var grayLabels = map[int]bool{ctlIDLabel: true, ctlPINLabel: true, ctlLinkLbl: true, ctlClaimLbl: true, ctlVer: true, ctlMeta: true}

func wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch uint32(msg) {
	case WM_DESTROY:
		pPostQuitMessage.Call(0)
		return 0
	case WM_CLOSE:
		pPostQuitMessage.Call(0)
		return 0
	case WM_CTLCOLORSTATIC:
		hdc := wp
		ctl := lp
		if ctl == gCtl[ctlDot] {
			gState.mu.Lock()
			ok := gState.statusOK
			gState.mu.Unlock()
			if ok {
				pSetTextColor.Call(hdc, colorRef(0x22, 0xa3, 0x5f))
			} else {
				pSetTextColor.Call(hdc, colorRef(0x9a, 0xa7, 0xbb))
			}
			pSetBkMode.Call(hdc, TRANSPARENT)
			return gWhiteBr
		}
		for id, h := range gCtl {
			if h == ctl && grayLabels[id] {
				pSetTextColor.Call(hdc, colorRef(0x84, 0x94, 0xab))
				break
			}
		}
		pSetBkMode.Call(hdc, TRANSPARENT)
		return gWhiteBr
	case WM_PAINT:
		var ps paintStruct
		hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		// white background
		var rcFull = [4]int32{0, 0, 430, 706}
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rcFull)), gWhiteBr)
		// section cards (v4 design)
		oldPen, _, _ := pSelectObject.Call(hdc, gCardPen)
		oldBr, _, _ := pSelectObject.Call(hdc, gCardBr)
		pRoundRect.Call(hdc, 12, 112, 418, 322, 28, 28) // ID + PIN + status
		pRoundRect.Call(hdc, 12, 312, 418, 384, 28, 28) // invite link
		pRoundRect.Call(hdc, 12, 380, 418, 452, 28, 28) // link this device
		pSelectObject.Call(hdc, oldPen)
		pSelectObject.Call(hdc, oldBr)
		// header gradient
		v := [2]triVertex{
			{0, 0, 0x2f * 257, 0x7d * 257, 0xe1 * 257, 0},
			{430, 104, 0x5a * 257, 0xa2 * 257, 0xf5 * 257, 0},
		}
		gr := gradientRect{0, 1}
		pGradientFill.Call(hdc, uintptr(unsafe.Pointer(&v[0])), 2, uintptr(unsafe.Pointer(&gr)), 1, 0)
		// header text
		pSetBkMode.Call(hdc, TRANSPARENT)
		pSetTextColor.Call(hdc, colorRef(255, 255, 255))
		old, _, _ := pSelectObject.Call(hdc, gFonts["title"])
		var rc = [4]int32{100, 14, 420, 60}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("SWRemote"))), 8, uintptr(unsafe.Pointer(&rc)), 0)
		pSelectObject.Call(hdc, gFonts["sub"])
		rc = [4]int32{100, 58, 420, 84}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("by SWInfoSystems"))), 16, uintptr(unsafe.Pointer(&rc)), 0)
		pSelectObject.Call(hdc, old)
		drawLogo(hdc)
		pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case WM_APP_STATUS:
		applyStatus()
		return 0
	case WM_APP_PROGRESS:
		gState.mu.Lock()
		pct := gState.progress
		gState.mu.Unlock()
		if h, ok := gCtl[ctlProg]; ok {
			if pct < 0 {
				pShowWindow.Call(h, 0 /*SW_HIDE*/)
			} else {
				pShowWindow.Call(h, SW_SHOW)
				pSendMessageW.Call(h, PBM_SETPOS, uintptr(pct), 0)
			}
		}
		return 0
	case WM_APP_UPDBTN:
		gState.mu.Lock()
		t, on := gState.updBtnText, gState.updBtnOn
		gState.mu.Unlock()
		setText(ctlUpdate, t)
		if h, ok := gCtl[ctlUpdate]; ok {
			var en uintptr = 1
			if !on {
				en = 0
			}
			pSendMessageW.Call(h, 0x00F5 /*BM_SETSTATE*/, 0, 0)
			// enable/disable
			pEnableWindow.Call(h, en)
		}
		return 0
	case WM_TIMER:
		refreshMeta()
		return 0
	case WM_COMMAND:
		id := int(wp & 0xFFFF)
		code := int((wp >> 16) & 0xFFFF)
		if code == BN_CLICKED {
			onClick(id)
		}
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

var pEnableWindow = u32.NewProc("EnableWindow")
var pInvalidateRect = u32.NewProc("InvalidateRect")
var pSetTimer = u32.NewProc("SetTimer")

func msgBox(title, text string, flags uintptr) {
	// keep the UTF-16 buffers alive in locals for the whole modal call
	t, _ := windows.UTF16FromString(title)
	m, _ := windows.UTF16FromString(text)
	pMessageBoxW.Call(gHWND, uintptr(unsafe.Pointer(&m[0])), uintptr(unsafe.Pointer(&t[0])), flags)
}

func msgBoxYesNo(title, text string) bool {
	t, _ := windows.UTF16FromString(title)
	m, _ := windows.UTF16FromString(text)
	r, _, _ := pMessageBoxW.Call(gHWND, uintptr(unsafe.Pointer(&m[0])), uintptr(unsafe.Pointer(&t[0])), MB_YESNO|MB_ICONQUESTION)
	return r == IDYES
}

func copyToClipboard(s string) {
	pOpenClipboard.Call(gHWND)
	pEmptyClipboard.Call(0)
	b, _ := windows.UTF16FromString(s)
	size := uintptr(len(b)) * 2
	h, _, _ := pGlobalAlloc.Call(GMEM_MOVEABLE, size)
	if h != 0 {
		p, _, _ := pGlobalLock.Call(h)
		if p != 0 {
			dst := unsafe.Slice((*uint16)(unsafe.Pointer(p)), len(b))
			copy(dst, b)
			pGlobalUnlock.Call(h)
			pSetClipboardData.Call(CF_UNICODETEXT, h)
		}
	}
	pCloseClipboard.Call()
}

// updateRunning guards against stacked update flows from repeated clicks.
var updateRunning atomic.Bool

func onClick(id int) {
	switch id {
	case ctlCopyID:
		copyToClipboard(cfg.DeviceID)
		guiSetStatus("ID copied to clipboard.", true)
	case ctlCopy:
		copyToClipboard(inviteLink())
		guiSetStatus("Invite link copied — send it to them.", true)
	case ctlCopyClaim:
		copyToClipboard(cfg.ClaimCode)
		guiSetStatus("Link code copied — enter it in your dashboard.", true)
	case ctlNewPIN:
		newPIN := randomPIN()
		cfgMu.Lock()
		cfg.PIN = newPIN
		cfgMu.Unlock()
		saveConfig() // takes cfgMu itself — must not hold the lock here
		setText(ctlPINValue, newPIN)
		guiSetStatus("New PIN generated.", true)
	case ctlUpdate:
		if updateRunning.Swap(true) {
			return // an update check is already in progress
		}
		go runUpdateFlow()
	case ctlQuit:
		pPostMessageW.Call(gHWND, WM_CLOSE, 0, 0)
	case ctlAutoRun:
		var chk uintptr = 0
		if h, ok := gCtl[ctlAutoRun]; ok {
			r, _, _ := pSendMessageW.Call(h, BM_GETCHECK, 0, 0)
			if r == BST_CHECKED {
				chk = 1
			}
		}
		setAutoRun(chk == 1)
	}
}

func inviteLink() string {
	return "https://swremote-relay.onrender.com/?id=" + cfg.DeviceID
}

func guiSetViewers(n int) {
	gState.mu.Lock()
	gState.viewers = n
	gState.mu.Unlock()
	refreshMeta()
}

func fmtUptime(d time.Duration) string {
	m := int(d.Minutes())
	if m < 1 {
		return "just started"
	}
	if m < 60 {
		return fmt.Sprintf("up %dm", m)
	}
	h := m / 60
	if h < 24 {
		return fmt.Sprintf("up %dh %dm", h, m%60)
	}
	return fmt.Sprintf("up %dd %dh", h/24, h%24)
}

func refreshMeta() {
	gState.mu.Lock()
	v, st := gState.viewers, gState.started
	gState.mu.Unlock()
	vw := "no viewers"
	if v == 1 {
		vw = "1 viewer"
	} else if v > 1 {
		vw = fmt.Sprintf("%d viewers", v)
	}
	setText(ctlMeta, vw+"   •   "+fmtUptime(time.Since(st)))
}

func setAutoRun(on bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()
	if on {
		if p, err := os.Executable(); err == nil {
			key.SetStringValue("SWRemote", `"`+p+`"`)
		}
	} else {
		key.DeleteValue("SWRemote")
	}
}

func getAutoRun() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	_, _, err = key.GetStringValue("SWRemote")
	return err == nil
}

// build and run the main window; blocks until quit
func runGUI() {
	// single instance
	mname := u16("SWRemote-Agent-Mutex")
	h, _, _ := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(mname)))
	if h != 0 {
		if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
			t, _ := windows.UTF16PtrFromString("SWRemote")
			m, _ := windows.UTF16PtrFromString("SWRemote is already running.\nLook for its window on the taskbar.")
			pMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 64)
			return
		}
	}

	icc := iccEx{size: 8, icc: 0x00000002 /* ICC_PROGRESS_CLASS */}
	pInitCommon.Call(uintptr(unsafe.Pointer(&icc)))

	gWhiteBr, _, _ = pCreateSolidBrush.Call(colorRef(255, 255, 255))
	gCardBr, _, _ = pCreateSolidBrush.Call(colorRef(0xf7, 0xfa, 0xfd))
	gCardPen, _, _ = pCreatePen.Call(0 /*PS_SOLID*/, 1, colorRef(0xe3, 0xea, 0xf3))
	gFonts["title"] = mkFont("Segoe UI", 24, true)
	gFonts["sub"] = mkFont("Segoe UI", 11, false)
	gFonts["lbl"] = mkFont("Segoe UI", 10, false)
	gFonts["id"] = mkFont("Consolas", 30, true)
	gFonts["pin"] = mkFont("Consolas", 20, true)
	gFonts["norm"] = mkFont("Segoe UI", 12, false)
	gFonts["small"] = mkFont("Segoe UI", 9, false)
	gLogoBmp = loadLogoBitmap()

	cursor, _, _ := pLoadCursorW.Call(0, IDC_ARROW)
	clsName := u16("SWRemoteWindow")
	wc := wndClassExW{
		style:         3, // CS_HREDRAW|CS_VREDRAW
		lpfnWndProc:   wndProcCb,
		hCursor:       cursor,
		hbrBackground: gWhiteBr,
		lpszClassName: clsName,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, _ := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return
	}

	hwnd, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(u16("SWRemote"))),
		WS_OVERLAPPEDWINDOW&^0x00040000, // no maximize box
		200, 120, 446, 745,
		0, 0, 0, 0)
	if hwnd == 0 {
		return
	}
	gHWND = hwnd

	// controls (client coords)
	mkCtl("STATIC", "YOUR ID", 0, 24, 122, 200, 20, ctlIDLabel, gFonts["lbl"])
	mkCtl("STATIC", cfg.DeviceID, SS_LEFT, 24, 144, 272, 44, ctlIDValue, gFonts["id"])
	mkCtl("BUTTON", "Copy ID", BS_PUSHBUTTON, 304, 148, 100, 34, ctlCopyID, gFonts["norm"])
	mkCtl("STATIC", "PIN", 0, 200, 200, 60, 20, ctlPINLabel, gFonts["lbl"])
	mkCtl("STATIC", cfg.PIN, SS_LEFT, 24, 222, 220, 34, ctlPINValue, gFonts["pin"])
	mkCtl("BUTTON", "New PIN", BS_PUSHBUTTON, 300, 220, 104, 32, ctlNewPIN, gFonts["norm"])
	mkCtl("STATIC", "●", SS_LEFT, 24, 268, 22, 24, ctlDot, gFonts["norm"])
	mkCtl("STATIC", "Starting…", SS_LEFT, 50, 270, 354, 22, ctlStatus, gFonts["norm"])
	mkCtl("STATIC", "no viewers   •   just started", SS_LEFT, 24, 294, 380, 20, ctlMeta, gFonts["small"])
	mkCtl("STATIC", "INVITE LINK", 0, 320, 200, 200, 20, ctlLinkLbl, gFonts["lbl"])
	mkCtl("EDIT", inviteLink(), WS_BORDER|ES_READONLY|ES_AUTOHSCROLL, 24, 342, 290, 30, ctlLinkEdit, gFonts["norm"])
	mkCtl("BUTTON", "Copy", BS_PUSHBUTTON, 322, 340, 82, 34, ctlCopy, gFonts["norm"])
	mkCtl("STATIC", "LINK THIS DEVICE", 0, 24, 388, 220, 20, ctlClaimLbl, gFonts["lbl"])
	mkCtl("STATIC", cfg.ClaimCode, SS_LEFT, 24, 410, 220, 34, ctlClaimVal, gFonts["pin"])
	mkCtl("BUTTON", "Copy code", BS_PUSHBUTTON, 300, 408, 104, 32, ctlCopyClaim, gFonts["norm"])
	mkCtl("BUTTON", "Check for Updates", BS_PUSHBUTTON, 24, 458, 380, 40, ctlUpdate, gFonts["norm"])
	mkCtl("BUTTON", "Start SWRemote with Windows", BS_AUTOCHECKBOX, 24, 512, 380, 24, ctlAutoRun, gFonts["norm"])
	mkCtl("msctls_progress32", "", 0, 24, 544, 380, 18, ctlProg, 0)
	pSendMessageW.Call(gCtl[ctlProg], PBM_SETRANGE32, 0, 100)
	pShowWindow.Call(gCtl[ctlProg], 0)
	mkCtl("BUTTON", "Quit", BS_PUSHBUTTON, 24, 580, 380, 38, ctlQuit, gFonts["norm"])
	mkCtl("STATIC", "v"+appVersion+"   •   swremote-relay.onrender.com", SS_LEFT, 24, 632, 380, 18, ctlVer, gFonts["small"])

	// gray labels are colored via WM_CTLCOLORSTATIC (grayLabels set)
	if getAutoRun() {
		pSendMessageW.Call(gCtl[ctlAutoRun], BM_SETCHECK, BST_CHECKED, 0)
	}

	applyStatus()
	refreshMeta()
	pSetTimer.Call(hwnd, 1, 30000, 0) // refresh uptime every 30s
	pShowWindow.Call(hwnd, SW_SHOW)
	pUpdateWindow.Call(hwnd)

	var m msgT
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
