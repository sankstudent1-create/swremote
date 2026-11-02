// SWRemote agent GUI — native Win32 window, pure Go (no cgo).
package main

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
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

	pRegisterClassExW    = u32.NewProc("RegisterClassExW")
	pCreateWindowExW     = u32.NewProc("CreateWindowExW")
	pRegisterClassW      = u32.NewProc("RegisterClassW")
	pDefWindowProcW      = u32.NewProc("DefWindowProcW")
	pShowWindow          = u32.NewProc("ShowWindow")
	pDestroyWindow       = u32.NewProc("DestroyWindow")       // v8.9.13: webview fallback cleanup
	pGetClientRect       = u32.NewProc("GetClientRect")       // v8.9.11: paint to the real client size
	pSetFocus            = u32.NewProc("SetFocus")            // v8.9.10: chat input
	pFlashWindow         = u32.NewProc("FlashWindow")         // v8.9.10: chat attention
	pGetForegroundWindow = u32.NewProc("GetForegroundWindow") // v8.9.10: chat attention
	pIsDialogMessage     = u32.NewProc("IsDialogMessageW")    // v8.9.10: Tab + Enter in chat input
	pUpdateWindow        = u32.NewProc("UpdateWindow")
	pGetMessageW         = u32.NewProc("GetMessageW")
	pTranslateMessage    = u32.NewProc("TranslateMessage")
	pDispatchMessageW    = u32.NewProc("DispatchMessageW")
	pPostQuitMessage     = u32.NewProc("PostQuitMessage")
	pPostMessageW        = u32.NewProc("PostMessageW")
	pSendMessageW        = u32.NewProc("SendMessageW")
	pSetWindowTextW      = u32.NewProc("SetWindowTextW")
	pLoadCursorW         = u32.NewProc("LoadCursorW")
	pLoadImageW          = u32.NewProc("LoadImageW")
	pBeginPaint          = u32.NewProc("BeginPaint")
	pEndPaint            = u32.NewProc("EndPaint")
	pOpenClipboard       = u32.NewProc("OpenClipboard")
	pEmptyClipboard      = u32.NewProc("EmptyClipboard")
	pSetClipboardData    = u32.NewProc("SetClipboardData")
	pCloseClipboard      = u32.NewProc("CloseClipboard")
	pCreateMutexW        = k32.NewProc("CreateMutexW")
	pCreateFontW         = g32.NewProc("CreateFontW")
	pCreateSolidBrush    = g32.NewProc("CreateSolidBrush")
	pCreatePen           = g32.NewProc("CreatePen")
	pRoundRect           = g32.NewProc("RoundRect")
	pEllipse             = g32.NewProc("Ellipse")
	pGetDC               = u32.NewProc("GetDC")
	pReleaseDC           = u32.NewProc("ReleaseDC")
	pCreateDIBitmap      = g32.NewProc("CreateDIBitmap")
	pDeleteObject        = g32.NewProc("DeleteObject")
	pCreateCompatibleDC  = g32.NewProc("CreateCompatibleDC")
	pDeleteDC            = g32.NewProc("DeleteDC")
	pBitBlt              = g32.NewProc("BitBlt")
	pSetTextColor        = g32.NewProc("SetTextColor")
	pSetBkMode           = g32.NewProc("SetBkMode")
	pSelectObject        = g32.NewProc("SelectObject")
	pFillRect            = u32.NewProc("FillRect")
	pDrawTextW           = u32.NewProc("DrawTextW")
	pGradientFill        = msimg.NewProc("GradientFill")
	pInitCommon          = comc.NewProc("InitCommonControlsEx")

	pGlobalAlloc  = k32.NewProc("GlobalAlloc")
	pGlobalLock   = k32.NewProc("GlobalLock")
	pGlobalUnlock = k32.NewProc("GlobalUnlock")
	pGlobalFree   = k32.NewProc("GlobalFree") // v8.9.2: clipboard failure path
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
	ES_MULTILINE        = 0x00000004 // v8.9.10: chat log
	ES_AUTOVSCROLL      = 0x00000040 // v8.9.10: chat log
	WS_VSCROLL          = 0x00200000 // v8.9.10: chat log
	BS_DEFPUSHBUTTON    = 0x00000001 // v8.9.10: chat Send (Enter key)
	SW_SHOW             = 5
	WM_DESTROY          = 2
	WM_PAINT            = 15
	WM_CLOSE            = 16
	WM_SIZE             = 5
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
	MB_ICONERROR        = 16
	MB_ICONQUESTION     = 32
	MB_ICONINFORMATION  = 64
	MB_TASKMODAL        = 0x2000 // v8.9.2: worker-thread dialogs stay on top without owning the main window
	MB_SETFOREGROUND    = 0x10000
	MB_TOPMOST          = 0x40000
	IDYES               = 6
)

const (
	WM_APP_STATUS   = WM_APP + 1
	WM_APP_PROGRESS = WM_APP + 2
	WM_APP_UPDBTN   = WM_APP + 3
	WM_APP_CAMSTAT  = WM_APP + 4 // v8.9.2: worker-thread camera status
	WM_APP_META     = WM_APP + 5 // v8.9.2: worker-thread viewers/uptime text
	WM_APP_CHAT     = WM_APP + 6 // v8.9.10: a chat message arrived — drain into the chat log
	WM_APP_REC      = WM_APP + 7 // v8.9.10: recording indicator changed
	WM_APP_WVMSG    = WM_APP + 8 // v8.9.13: a WebView2 page message — dispatch on the GUI thread
)

const (
	ctlIDLabel     = 101
	ctlIDValue     = 102
	ctlPINLabel    = 103
	ctlPINValue    = 104
	ctlNewPIN      = 105
	ctlDot         = 106
	ctlStatus      = 107
	ctlLinkLbl     = 108
	ctlLinkEdit    = 109
	ctlCopy        = 110
	ctlUpdate      = 111
	ctlAutoRun     = 112
	ctlProg        = 113
	ctlQuit        = 114
	ctlVer         = 115
	ctlCopyID      = 116
	ctlMeta        = 117
	ctlClaimLbl    = 118
	ctlClaimVal    = 119
	ctlCopyClaim   = 120
	ctlViewLbl     = 121
	ctlWakeLbl     = 122
	ctlWakeVal     = 123
	ctlWakeInstall = 124
	ctlCamLbl      = 125
	ctlCamVal      = 126
	ctlFixCam      = 127
	ctlRepairWin   = 128 // v8.9.0: sfc /scannow + DISM one-click repair
	ctlChatLbl     = 129 // v8.9.10: chat card label
	ctlChatLog     = 130 // v8.9.10: read-only chat log (multiline edit)
	ctlChatInput   = 131 // v8.9.10: chat reply input
	ctlChatSend    = 132 // v8.9.10: send chat reply
	ctlRec         = 133 // v8.9.10: recording indicator
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
	hdc        uintptr
	fErase     uint32
	left       int32
	top        int32
	right      int32
	bottom     int32
	fRestore   uint32
	fIncUpdate uint32
	reserved   [32]byte
}

type triVertex struct {
	x, y       int32
	r, g, b, a uint16
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
	mu            sync.Mutex
	statusText    string
	statusOK      bool // green vs gray dot
	pillText      string
	pillOK        bool
	progress      int // -1 = hidden
	updBtnText    string
	updBtnOn      bool
	camStatus     string // v8.4.0: one-line camera health from runPrereqCheck
	viewers       int
	chatPending   []string // v8.9.10: chat lines waiting for the UI thread
	chatLines     []string // v8.9.13: last chat lines, shared with the webview UI
	wakeStatus    string   // v8.9.13: wake-up service line, shared with the webview UI
	wakeInstalled bool
	recText       string // v8.9.10: recording indicator text ("" = hidden)
	recShow       bool
	started       time.Time
}

var (
	gHWND  uintptr
	gState = &guiState{statusText: "Starting…", progress: -1, updBtnText: "Check for Updates", updBtnOn: true, started: time.Now()}
	gCtl   = map[int]uintptr{}
	// v8.9.11: adaptive layout — compact when the screen is short, and
	// the chat card rect the paint handler draws behind the chat controls.
	gCompact   bool
	gChatRich  bool
	gChatCardT int32
	gChatCardB int32
	gWhiteBr   uintptr
	gBgBr      uintptr
	gBlueBr    uintptr
	gCardBr    uintptr
	gCardPen   uintptr
	gGreenBr   uintptr
	gGrayBr    uintptr
	gFonts     = map[string]uintptr{}
	gLogoBmp   uintptr
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
	pBitBlt.Call(hdc, 24, 18, 64, 64, memDC, 0, 0, 0x00CC0020 /*SRCCOPY*/)
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

// guiAddChat queues a chat line for the in-window chat card (v8.9.10 —
// chat no longer pops a MessageBox). Safe from any goroutine.
func guiAddChat(who, text string) {
	line := time.Now().Format("15:04") + " " + who + ": " + text
	gState.mu.Lock()
	gState.chatPending = append(gState.chatPending, line)
	gState.chatLines = append(gState.chatLines, line)
	if len(gState.chatLines) > 100 {
		gState.chatLines = gState.chatLines[len(gState.chatLines)-100:]
	}
	gState.mu.Unlock()
	if gHWND != 0 {
		pPostMessageW.Call(gHWND, WM_APP_CHAT, 0, 0)
		// attention cue only when the window is not in front
		if fg, _, _ := pGetForegroundWindow.Call(); fg != gHWND {
			pFlashWindow.Call(gHWND, 1)
		}
	}
}

// guiSetRec shows or hides the recording indicator (v8.9.10).
func guiSetRec(text string, show bool) {
	gState.mu.Lock()
	gState.recText, gState.recShow = text, show
	gState.mu.Unlock()
	if gHWND != 0 {
		pPostMessageW.Call(gHWND, WM_APP_REC, 0, 0)
	}
}

func drainChat() {
	gState.mu.Lock()
	lines := gState.chatPending
	gState.chatPending = nil
	gState.mu.Unlock()
	h, ok := gCtl[ctlChatLog]
	if !ok || len(lines) == 0 {
		return
	}
	for _, ln := range lines {
		if gChatRich {
			appendChatRich(h, ln)
		} else {
			b, _ := windows.UTF16FromString(ln + "\r\n")
			pSendMessageW.Call(h, 0x00B1 /*EM_SETSEL*/, 0xFFFFFFFF, 0xFFFFFFFF)
			pSendMessageW.Call(h, 0x00C2 /*EM_REPLACESEL*/, 0, uintptr(unsafe.Pointer(&b[0])))
		}
	}
}

// mkChatLog creates the chat log as a RichEdit control when available
// (v8.9.11: colored names and times, like a modern chat app) and falls
// back to a plain read-only edit on systems without Msftedit.
func mkChatLog(x, y, w, h int32) {
	if _, err := windows.LoadLibrary("Msftedit.dll"); err == nil {
		hwnd, _, _ := pCreateWindowExW.Call(0,
			uintptr(unsafe.Pointer(u16("RICHEDIT50W"))),
			uintptr(unsafe.Pointer(u16(""))),
			WS_CHILD|WS_VISIBLE|WS_BORDER|ES_MULTILINE|ES_READONLY|WS_VSCROLL|ES_AUTOVSCROLL,
			uintptr(x), uintptr(y), uintptr(w), uintptr(h),
			gHWND, uintptr(ctlChatLog), 0, 0)
		if hwnd != 0 {
			pSendMessageW.Call(hwnd, 48 /*WM_SETFONT*/, gFonts["norm"], 1)
			gCtl[ctlChatLog] = hwnd
			gChatRich = true
			return
		}
	}
	mkCtl("EDIT", "", WS_BORDER|ES_MULTILINE|ES_READONLY|WS_VSCROLL|ES_AUTOVSCROLL, x, y, w, h, ctlChatLog, gFonts["norm"])
}

func appendRichPiece(h uintptr, s string, rgb uint32, bold bool) {
	var cf [60]byte
	binary.LittleEndian.PutUint32(cf[0:], 60)
	binary.LittleEndian.PutUint32(cf[4:], 0x40000000|0x00000001) // CFM_COLOR|CFM_BOLD
	if bold {
		binary.LittleEndian.PutUint32(cf[8:], 1) // CFE_BOLD
	}
	binary.LittleEndian.PutUint32(cf[20:], rgb) // crTextColor
	pSendMessageW.Call(h, 0x00B1 /*EM_SETSEL*/, 0xFFFFFFFF, 0xFFFFFFFF)
	pSendMessageW.Call(h, 0x0444 /*EM_SETCHARFORMAT*/, 0x0001 /*SCF_SELECTION*/, uintptr(unsafe.Pointer(&cf[0])))
	b, _ := windows.UTF16FromString(s)
	pSendMessageW.Call(h, 0x00C2 /*EM_REPLACESEL*/, 0, uintptr(unsafe.Pointer(&b[0])))
}

// appendChatRich renders one "HH:MM Name: text" line as gray time +
// violet bold name + dark text.
func appendChatRich(h uintptr, line string) {
	timePart, rest := line, ""
	if len(line) > 6 {
		timePart, rest = line[:5], line[6:]
	}
	name, text := rest, ""
	if i := strings.Index(rest, ": "); i >= 0 {
		name, text = rest[:i], rest[i+2:]
	}
	appendRichPiece(h, timePart+" ", 0x808080, false)
	appendRichPiece(h, name+": ", 0x7C3AED, true)
	appendRichPiece(h, text+"\r\n", 0x181C1F, false)
}

func sendChatFromInput() {
	h, ok := gCtl[ctlChatInput]
	if !ok {
		return
	}
	n, _, _ := pSendMessageW.Call(h, 0x000E /*WM_GETTEXTLENGTH*/, 0, 0)
	if n <= 0 {
		return
	}
	buf := make([]uint16, n+1)
	pSendMessageW.Call(h, 0x000D /*WM_GETTEXT*/, uintptr(n+1), uintptr(unsafe.Pointer(&buf[0])))
	text := strings.TrimSpace(windows.UTF16ToString(buf))
	if text == "" {
		return
	}
	if len(text) > 200 {
		text = text[:200]
	}
	sendWSJSON(map[string]any{"t": "chat", "text": text})
	guiAddChat("You", text)
	pSetWindowTextW.Call(h, uintptr(unsafe.Pointer(u16(""))))
	pSetFocus.Call(h)
}

func applyStatus() {
	gState.mu.Lock()
	t := gState.statusText
	// derive the header pill from connection-related statuses; transient
	// messages (copied etc.) leave the last connection state in place
	switch {
	case strings.HasPrefix(t, "Online"), strings.HasPrefix(t, "Viewer connected"):
		gState.pillText, gState.pillOK = "Online", true
	case strings.HasPrefix(t, "Connecting"), strings.HasPrefix(t, "Connection lost"):
		gState.pillText, gState.pillOK = "Connecting…", false
	}
	gState.mu.Unlock()
	setText(ctlStatus, t)
	// repaint the dot so WM_CTLCOLORSTATIC recolors it, and repaint only
	// the header band (v8.9.4: was a full-window repaint on every status
	// change, which re-drew logo/profile art for no reason).
	if h, ok := gCtl[ctlDot]; ok {
		pInvalidateRect.Call(h, 0, 1)
	}
	var hdr = [4]int32{0, 0, 2000, 110}
	pInvalidateRect.Call(gHWND, uintptr(unsafe.Pointer(&hdr)), 1)
}

// drawProfile paints the device identity card: monogram avatar, device name,
// ID and status. v8.8: the "user profile" given a proper, premium shape.
func drawProfile(hdc uintptr) {
	// card
	oldPen, _, _ := pSelectObject.Call(hdc, gCardPen)
	oldBr, _, _ := pSelectObject.Call(hdc, gCardBr)
	pRoundRect.Call(hdc, 20, 112, 840, 196, 24, 24)
	pSelectObject.Call(hdc, oldPen)
	pSelectObject.Call(hdc, oldBr)

	// avatar circle (violet) with device initials
	name := cfg.Name
	if name == "" {
		name = "This PC"
	}
	initials := profileInitials(name)
	avBr, _, _ := pCreateSolidBrush.Call(colorRef(0x7c, 0x3a, 0xed))
	oldPen2, _, _ := pSelectObject.Call(hdc, gCardPen)
	oldBr2, _, _ := pSelectObject.Call(hdc, avBr)
	pEllipse.Call(hdc, 44, 126, 108, 190)
	pSelectObject.Call(hdc, oldPen2)
	pSelectObject.Call(hdc, oldBr2)
	pDeleteObject.Call(avBr)

	pSetBkMode.Call(hdc, TRANSPARENT)
	pSetTextColor.Call(hdc, colorRef(255, 255, 255))
	oldF, _, _ := pSelectObject.Call(hdc, gFonts["avatar"])
	var rcA = [4]int32{44, 134, 108, 182}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(initials))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&rcA)), 0x00000001|0x00000004 /*CENTER|VCENTER*/)
	// device name
	pSetTextColor.Call(hdc, colorRef(0x18, 0x1c, 0x1f))
	pSelectObject.Call(hdc, gFonts["profilename"])
	var rcN = [4]int32{124, 126, 700, 156}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(name))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&rcN)), 0)
	// ID + status line
	pSetTextColor.Call(hdc, colorRef(0x84, 0x94, 0xab))
	pSelectObject.Call(hdc, gFonts["norm"])
	gState.mu.Lock()
	st := gState.statusText
	gState.mu.Unlock()
	var rcS = [4]int32{124, 156, 700, 184}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("ID "+cfg.DeviceID+"   •   "+st))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&rcS)), 0)
	pSelectObject.Call(hdc, oldF)
}

// profileInitials returns up to 2 initials from a device/user name.
func profileInitials(name string) string {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return "?"
	}
	init := ""
	for i, p := range parts {
		if i >= 2 {
			break
		}
		for _, r := range p {
			init += strings.ToUpper(string(r))
			break
		}
	}
	if init == "" {
		return "?"
	}
	return init
}
func drawPill(hdc uintptr) {
	gState.mu.Lock()
	text, ok := gState.pillText, gState.pillOK
	gState.mu.Unlock()
	if text == "" {
		text, ok = "Starting…", false
	}
	// white pill (header right)
	oldPen, _, _ := pSelectObject.Call(hdc, gCardPen)
	oldBr, _, _ := pSelectObject.Call(hdc, gWhiteBr)
	pRoundRect.Call(hdc, 620, 30, 836, 64, 34, 34)
	pSelectObject.Call(hdc, oldPen)
	pSelectObject.Call(hdc, oldBr)
	// dot
	dotBr := gGrayBr
	if ok {
		dotBr = gGreenBr
	}
	oldPen2, _, _ := pSelectObject.Call(hdc, gCardPen)
	oldBr2, _, _ := pSelectObject.Call(hdc, dotBr)
	pEllipse.Call(hdc, 636, 41, 648, 53)
	pSelectObject.Call(hdc, oldPen2)
	pSelectObject.Call(hdc, oldBr2)
	// label
	pSetBkMode.Call(hdc, TRANSPARENT)
	pSetTextColor.Call(hdc, colorRef(0x1e, 0x29, 0x3b))
	oldF, _, _ := pSelectObject.Call(hdc, gFonts["norm"])
	var rc = [4]int32{656, 34, 828, 60}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(text))), 0xFFFFFFFF /*cchText=-1, null-terminated*/, uintptr(unsafe.Pointer(&rc)), 0)
	pSelectObject.Call(hdc, oldF)
}

var grayLabels = map[int]bool{ctlIDLabel: true, ctlPINLabel: true, ctlLinkLbl: true, ctlClaimLbl: true, ctlViewLbl: true, ctlWakeLbl: true, ctlCamLbl: true, ctlVer: true, ctlMeta: true}

var primaryBtns = map[int]bool{ctlCopyID: true, ctlNewPIN: true, ctlCopy: true, ctlCopyClaim: true, ctlWakeInstall: true, ctlUpdate: true, ctlFixCam: true, ctlRepairWin: true}

const WM_CTLCOLORBTN = 309

func wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch uint32(msg) {
	case WM_DESTROY:
		pPostQuitMessage.Call(0)
		return 0
	case WM_CLOSE:
		// v8.9.14: the X button is a clean quit too — previously only
		// the Quit button marked it, so every X-close was reported as
		// "did not quit cleanly" on the next start (a false alarm).
		markQuitting()
		pPostQuitMessage.Call(0)
		return 0
	case WM_CTLCOLORBTN:
		hdc := wp
		ctl := lp
		for id, h := range gCtl {
			if h == ctl {
				if id == ctlQuit {
					pSetTextColor.Call(hdc, colorRef(0x33, 0x44, 0x5c))
					pSetBkMode.Call(hdc, TRANSPARENT)
					return gWhiteBr
				}
				if primaryBtns[id] {
					pSetTextColor.Call(hdc, colorRef(255, 255, 255))
					pSetBkMode.Call(hdc, TRANSPARENT)
					return gBlueBr
				}
			}
		}
		pSetBkMode.Call(hdc, TRANSPARENT)
		return gWhiteBr
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
	case WM_SIZE:
		// v8.9.13: keep the WebView2 child filling the host window.
		wvResize()
		return 0
	case WM_PAINT:
		// v8.9.13: in webview mode the child covers the client area, so
		// the GDI cards underneath are never seen — skip painting them.
		if wvController != 0 {
			r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
			return r
		}
		var ps paintStruct
		hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		// light background (v8.8: website surface) — fill the real
		// client size (v8.9.11: was hardcoded 860x640, leaving the
		// taller window's bottom strip unpainted).
		var crc [4]int32
		pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&crc)))
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&crc)), gBgBr)
		// section cards — v8.8 layout with profile card on top
		oldPen, _, _ := pSelectObject.Call(hdc, gCardPen)
		oldBr, _, _ := pSelectObject.Call(hdc, gCardBr)
		pRoundRect.Call(hdc, 20, 208, 418, 306, 24, 24)  // your ID
		pRoundRect.Call(hdc, 20, 318, 418, 416, 24, 24)  // PIN & status
		pRoundRect.Call(hdc, 20, 428, 418, 526, 24, 24)  // link this device
		pRoundRect.Call(hdc, 434, 208, 840, 306, 24, 24) // invite link
		pRoundRect.Call(hdc, 434, 318, 840, 416, 24, 24) // viewers
		pRoundRect.Call(hdc, 434, 428, 840, 526, 24, 24) // wake-up & camera
		if gChatCardB > 0 {
			pRoundRect.Call(hdc, 20, uintptr(gChatCardT), 840, uintptr(gChatCardB), 24, 24) // chat
		}
		pSelectObject.Call(hdc, oldPen)
		pSelectObject.Call(hdc, oldBr)
		drawViewers(hdc)
		drawProfile(hdc) // v8.8: device identity card
		// header gradient (v8.8: violet, matching the website)
		v := [2]triVertex{
			{0, 0, 0x7c * 257, 0x3a * 257, 0xed * 257, 0},
			{860, 100, 0xa7 * 257, 0x8b * 257, 0xfa * 257, 0},
		}
		gr := gradientRect{0, 1}
		pGradientFill.Call(hdc, uintptr(unsafe.Pointer(&v[0])), 2, uintptr(unsafe.Pointer(&gr)), 1, 0)
		// header text
		pSetBkMode.Call(hdc, TRANSPARENT)
		pSetTextColor.Call(hdc, colorRef(255, 255, 255))
		old, _, _ := pSelectObject.Call(hdc, gFonts["title"])
		var rc = [4]int32{100, 12, 420, 52}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("SWRemote"))), 8, uintptr(unsafe.Pointer(&rc)), 0)
		pSelectObject.Call(hdc, gFonts["sub"])
		rc = [4]int32{102, 52, 320, 76}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("by SWInfoSystems"))), 16, uintptr(unsafe.Pointer(&rc)), 0)
		// version badge (v8.0: always visible in the header)
		pSelectObject.Call(hdc, gFonts["small"])
		rc = [4]int32{102, 74, 320, 94}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("v"+appVersion))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&rc)), 0)
		pSelectObject.Call(hdc, old)
		drawLogo(hdc)
		drawPill(hdc)
		pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case WM_APP_STATUS:
		applyStatus()
		pushWebViewState()
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
		pushWebViewState()
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
	case WM_APP_CAMSTAT:
		gState.mu.Lock()
		cs := gState.camStatus
		gState.mu.Unlock()
		setText(ctlCamVal, cs)
		pushWebViewState()
		return 0
	case WM_APP_META:
		refreshMeta()
		pushWebViewState()
		return 0
	case WM_APP_CHAT:
		drainChat()
		pushWebViewState()
		return 0
	case WM_APP_REC:
		gState.mu.Lock()
		t, show := gState.recText, gState.recShow
		gState.mu.Unlock()
		setText(ctlRec, t)
		if h, ok := gCtl[ctlRec]; ok {
			var vis uintptr = 0
			if show {
				vis = SW_SHOW
			}
			pShowWindow.Call(h, vis)
		}
		pushWebViewState()
		return 0
	case WM_APP_WVMSG:
		// v8.9.13: a WebView2 page message, dispatched on the GUI thread
		// (clipboard and control actions must run here, like clicks).
		wvMsgMu.Lock()
		js, ok := wvMsgQueue[wp]
		if ok {
			delete(wvMsgQueue, wp)
		}
		wvMsgMu.Unlock()
		if ok {
			dispatchWebViewAction(js)
		}
		return 0
	case WM_TIMER:
		heartbeatTick()    // v8.9.4: the timer ONLY ticks the heartbeat now.
		pushWebViewState() // v8.9.13: 1s refresh (uptime, viewer count)
		// The per-second label refresh goes out as its own message instead
		// of running inside the timer handler: a slow refresh (blocked
		// label update, stalled paint) used to stall the heartbeat itself,
		// so the watchdog mistook a busy UI for a dead one and restarted
		// mid-call. WM_APP_META already runs refreshMeta().
		pPostMessageW.Call(gHWND, WM_APP_META, 0, 0)
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
	// v8.9.2: owner 0 + taskmodal/topmost — these are often called from
	// worker goroutines (fix-camera, repair flows); owning the main window
	// from another thread disables it, and a hidden box made the app look
	// dead while clicks did nothing.
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(&m[0])), uintptr(unsafe.Pointer(&t[0])), flags|MB_TASKMODAL|MB_SETFOREGROUND|MB_TOPMOST)
}

func msgBoxYesNo(title, text string) bool {
	t, _ := windows.UTF16FromString(title)
	m, _ := windows.UTF16FromString(text)
	r, _, _ := pMessageBoxW.Call(0, uintptr(unsafe.Pointer(&m[0])), uintptr(unsafe.Pointer(&t[0])), MB_YESNO|MB_ICONQUESTION|MB_TASKMODAL|MB_SETFOREGROUND|MB_TOPMOST)
	return r == IDYES
}

func copyToClipboard(s string) {
	// v8.9.2: retry OpenClipboard (another app may hold it briefly) and
	// check results — the old code ignored both return values, could show
	// "ID copied" when the copy failed, and leaked the handle on failure.
	opened := false
	for i := 0; i < 10 && !opened; i++ {
		if r, _, _ := pOpenClipboard.Call(gHWND); r != 0 {
			opened = true
		} else {
			time.Sleep(15 * time.Millisecond)
		}
	}
	if !opened {
		return
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call(0)
	b, _ := windows.UTF16FromString(s)
	size := uintptr(len(b)) * 2
	h, _, _ := pGlobalAlloc.Call(GMEM_MOVEABLE, size)
	if h == 0 {
		return
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return
	}
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(p)), len(b))
	copy(dst, b)
	pGlobalUnlock.Call(h)
	if r, _, _ := pSetClipboardData.Call(CF_UNICODETEXT, h); r == 0 {
		pGlobalFree.Call(h) // clipboard didn't take ownership
	}
	// on success the clipboard owns h — don't free it
}

// updateRunning guards against stacked update flows from repeated clicks.
var updateRunning atomic.Bool

func onClick(id int) {
	switch id {
	case ctlChatSend: // v8.9.10: reply to viewers from the chat card
		sendChatFromInput()
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
	case ctlWakeInstall:
		// v8.9.0: fresh reinstall — clears the stuck "waiting for admin" state
		go func() {
			setText(ctlWakeVal, "Reinstalling…")
			msg := reinstallWakeService()
			if gCtl[ctlWakeVal] != 0 {
				pSetWindowTextW.Call(gCtl[ctlWakeVal], uintptr(unsafe.Pointer(u16(msg))))
			}
		}()
	case ctlFixCam:
		// v8.4.0: repair/install the camera components on demand
		go fixCameraFlow()
	case ctlRepairWin:
		// v8.9.0: sfc + DISM from web research — fixes broken system files
		// when regsvr32 alone doesn't stick
		go repairWindowsFlow()
	case ctlQuit:
		markQuitting() // v8.9.0: clean quit — no auto-restart
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
		cfgMu.Lock()
		cfg.AutoRunSet = true // explicit user choice — never auto-change again
		cfgMu.Unlock()
		saveConfig()
	}
}

func inviteLink() string {
	return "https://swremote-relay.onrender.com/?id=" + cfg.DeviceID
}

// setCamStatus stores the camera health line and updates the window if shown.
// v8.9.2: callable from worker goroutines — it used to call setText directly,
// a synchronous cross-thread SendMessage that stalls the worker when the UI
// isn't pumping. Now it stores state and posts; the UI thread applies it.
func setCamStatus(s string) {
	gState.mu.Lock()
	gState.camStatus = s
	gState.mu.Unlock()
	if gHWND != 0 {
		pPostMessageW.Call(gHWND, WM_APP_CAMSTAT, 0, 0)
	}
}

func guiSetViewers(n int) {
	gState.mu.Lock()
	gState.viewers = n
	gState.mu.Unlock()
	if gHWND != 0 {
		pPostMessageW.Call(gHWND, WM_APP_META, 0, 0)
	}
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
	// v8.9.10: the VIEWERS label carries the true count — only 2 profile
	// rows fit in the card, so the count is the honest total.
	lbl := "VIEWERS"
	if v > 0 {
		lbl = fmt.Sprintf("VIEWERS (%d)", v)
	}
	setText(ctlViewLbl, lbl)
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

// singleInstanceMutex is held for the life of the process.
var singleInstanceMutex uintptr

// acquireSingleInstance returns false (after telling the user) if another
// copy is already running. v8.6.1: called at startup, BEFORE the network
// loop and updater start — previously a duplicate instance connected with
// the same device ID and downloaded updates before noticing it was a dup.
func acquireSingleInstance() bool {
	// v8.9.2: use openAgentMutex (windows.CreateMutex returns the real
	// ERROR_ALREADY_EXISTS) — the old lazy-proc + separate GetLastError
	// check raced and let duplicates through (see watchdog.go).
	h, exists := openAgentMutex()
	if exists {
		if h != 0 {
			windows.CloseHandle(h)
		}
		t, _ := windows.UTF16PtrFromString("SWRemote")
		m, _ := windows.UTF16PtrFromString("SWRemote is already running.\nLook for its window on the taskbar.")
		pMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 64)
		return false
	}
	singleInstanceMutex = uintptr(h)
	_ = singleInstanceMutex
	return true
}

// build and run the main window; blocks until quit
func runGUI() {
	icc := iccEx{size: 8, icc: 0x00000002 /* ICC_PROGRESS_CLASS */}
	pInitCommon.Call(uintptr(unsafe.Pointer(&icc)))

	gWhiteBr, _, _ = pCreateSolidBrush.Call(colorRef(255, 255, 255))
	gBgBr, _, _ = pCreateSolidBrush.Call(colorRef(0xf7, 0xfa, 0xfe))   // v8.8: website surface
	gBlueBr, _, _ = pCreateSolidBrush.Call(colorRef(0x7c, 0x3a, 0xed)) // v8.8: violet primary
	gCardBr, _, _ = pCreateSolidBrush.Call(colorRef(255, 255, 255))
	gCardPen, _, _ = pCreatePen.Call(0 /*PS_SOLID*/, 1, colorRef(0xe7, 0xed, 0xf5))
	gGreenBr, _, _ = pCreateSolidBrush.Call(colorRef(0x22, 0xa3, 0x5f))
	gGrayBr, _, _ = pCreateSolidBrush.Call(colorRef(0x9a, 0xa7, 0xbb))
	gFonts["title"] = mkFont("Segoe UI", 24, true)
	gFonts["sub"] = mkFont("Segoe UI", 11, false)
	gFonts["lbl"] = mkFont("Segoe UI", 10, false)
	gFonts["id"] = mkFont("Consolas", 30, true)
	gFonts["pin"] = mkFont("Consolas", 20, true)
	gFonts["norm"] = mkFont("Segoe UI", 12, false)
	gFonts["profilename"] = mkFont("Segoe UI", 16, true) // v8.8: profile
	gFonts["avatar"] = mkFont("Segoe UI", 20, true)      // v8.8: avatar initials
	gFonts["small"] = mkFont("Segoe UI", 9, false)
	gLogoBmp = loadLogoBitmap()

	// v8.9.13: the agent window is now a WebView2 page — the same calm
	// HTML/CSS design as the web console — when the Edge WebView2
	// runtime is present. Otherwise it falls back to the classic GDI
	// window, so a missing runtime can never leave the user with no
	// window at all.
	// v8.9.14: both paths share ONE host window. The old code created
	// the host, destroyed it when WebView2 setup failed (which queued
	// a WM_QUIT), then created a second window whose message loop died
	// instantly on that queued WM_QUIT — the window flashed and the
	// process exited. That is what the 21:26 log shows (three 0–2s
	// runs, no webview line). Now the fallback reuses the same window
	// and the failure reason is logged, so the next log proves which
	// path ran.
	hwnd, err := createHostWindow()
	if err != nil {
		return
	}
	if err := setupWebView2(hwnd); err == nil {
		applyStatus()
		refreshMeta()
		refreshWakeStatus()
		pSetTimer.Call(hwnd, 1, 1000, 0)
		pShowWindow.Call(hwnd, SW_SHOW)
		pUpdateWindow.Call(hwnd)
		var m msgT
		for {
			r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			// NOTE: no IsDialogMessage here — it would eat the web
			// page's Tab and Enter keys.
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
			heartbeatTick()
		}
		return
	} else {
		log("webview unavailable (%v) — using classic window", err)
	}
	runGDI(hwnd)
}

// createHostWindow registers the window class (tolerating an existing
// registration on the fallback path) and creates the centered,
// screen-size-aware host window shared by both UI paths.
func createHostWindow() (uintptr, error) {
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
	// Ignore the result: ERROR_CLASS_ALREADY_EXISTS is fine (fallback
	// path re-registers); CreateWindowExW below is the real check.
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	// v8.9.11: honor the screen size — a 780px window overflows a 768p
	// laptop screen and hides the chat card and Quit button, and a
	// fixed (200,80) origin wastes big screens. Center the window and
	// drop to the compact layout when the screen is short.
	sw, _, _ := pGetSystemMetrics.Call(0)
	sh, _, _ := pGetSystemMetrics.Call(1)
	winH := int32(780)
	gCompact = sh > 0 && sh < 860
	if gCompact {
		winH = 700
	}
	wx := (int32(sw) - 876) / 2
	wy := (int32(sh) - winH) / 2
	if wx < 0 {
		wx = 0
	}
	if wy < 0 {
		wy = 0
	}

	hwnd, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(u16("SWRemote"))),
		WS_OVERLAPPEDWINDOW&^0x00040000&^0x00080000, // no maximize box, no resize border
		uintptr(wx), uintptr(wy), 876, uintptr(winH),
		0, 0, 0, 0)
	if hwnd == 0 {
		return 0, fmt.Errorf("create window failed")
	}
	gHWND = hwnd
	return hwnd, nil
}

func runGDI(hwnd uintptr) {

	// controls (client coords) — v8.8 layout: profile card + 2x3 grid
	// left column
	mkCtl("STATIC", "YOUR ID", 0, 40, 220, 220, 18, ctlIDLabel, gFonts["lbl"])
	mkCtl("STATIC", cfg.DeviceID, SS_LEFT, 40, 240, 250, 38, ctlIDValue, gFonts["id"])
	mkCtl("BUTTON", "Copy ID", BS_PUSHBUTTON|BS_FLAT, 304, 242, 96, 34, ctlCopyID, gFonts["norm"])
	mkCtl("STATIC", "PIN", 0, 40, 330, 200, 18, ctlPINLabel, gFonts["lbl"])
	mkCtl("STATIC", cfg.PIN, SS_LEFT, 40, 350, 180, 30, ctlPINValue, gFonts["pin"])
	mkCtl("BUTTON", "New PIN", BS_PUSHBUTTON|BS_FLAT, 304, 350, 96, 30, ctlNewPIN, gFonts["norm"])
	mkCtl("STATIC", "●", SS_LEFT, 40, 384, 18, 18, ctlDot, gFonts["norm"])
	mkCtl("STATIC", "Starting…", SS_LEFT, 62, 384, 330, 18, ctlStatus, gFonts["norm"])
	mkCtl("STATIC", "LINK THIS DEVICE", 0, 40, 440, 220, 18, ctlClaimLbl, gFonts["lbl"])
	mkCtl("STATIC", cfg.ClaimCode, SS_LEFT, 40, 460, 180, 30, ctlClaimVal, gFonts["pin"])
	mkCtl("BUTTON", "Copy code", BS_PUSHBUTTON|BS_FLAT, 304, 460, 96, 30, ctlCopyClaim, gFonts["norm"])
	// right column
	mkCtl("STATIC", "INVITE LINK", 0, 454, 220, 220, 18, ctlLinkLbl, gFonts["lbl"])
	mkCtl("EDIT", inviteLink(), WS_BORDER|ES_READONLY|ES_AUTOHSCROLL, 454, 242, 268, 30, ctlLinkEdit, gFonts["norm"])
	mkCtl("BUTTON", "Copy", BS_PUSHBUTTON|BS_FLAT, 732, 240, 88, 34, ctlCopy, gFonts["norm"])
	mkCtl("STATIC", "VIEWERS", 0, 454, 330, 200, 18, ctlViewLbl, gFonts["lbl"])
	mkCtl("STATIC", "", SS_LEFT, 454, 352, 366, 18, ctlMeta, gFonts["small"])
	mkCtl("STATIC", "WAKE-UP", 0, 454, 440, 200, 18, ctlWakeLbl, gFonts["lbl"])
	mkCtl("STATIC", "Checking…", SS_LEFT, 454, 460, 220, 18, ctlWakeVal, gFonts["norm"])
	mkCtl("BUTTON", "Install", BS_PUSHBUTTON|BS_FLAT, 680, 458, 90, 30, ctlWakeInstall, gFonts["norm"])
	// v8.4.0: camera health + one-click repair
	mkCtl("STATIC", "CAMERA", 0, 454, 492, 200, 16, ctlCamLbl, gFonts["lbl"])
	mkCtl("STATIC", "checking…", SS_LEFT, 540, 492, 280, 16, ctlCamVal, gFonts["small"])
	// v8.9.11: chat + bottom-bar rows move up on short screens so
	// nothing falls off the bottom; the section cards above never move.
	chatLblY, chatLogY, chatLogH, chatInputY, bottomY := int32(528), int32(550), int32(100), int32(658), int32(700)
	if gCompact {
		chatLblY, chatLogY, chatLogH, chatInputY, bottomY = 520, 540, 60, 606, 640
	}
	gChatCardT, gChatCardB = chatLblY-8, chatInputY+34
	// bottom bar
	mkCtl("BUTTON", "Check for Updates", BS_PUSHBUTTON|BS_FLAT, 20, bottomY, 150, 38, ctlUpdate, gFonts["norm"])
	mkCtl("BUTTON", "Fix camera", BS_PUSHBUTTON|BS_FLAT, 178, bottomY, 130, 38, ctlFixCam, gFonts["norm"])
	mkCtl("BUTTON", "Repair Windows", BS_PUSHBUTTON|BS_FLAT, 316, bottomY, 140, 38, ctlRepairWin, gFonts["norm"])
	mkCtl("BUTTON", "Start SWRemote with Windows", BS_AUTOCHECKBOX, 434, bottomY+8, 180, 24, ctlAutoRun, gFonts["norm"])
	mkCtl("msctls_progress32", "", 0, 620, bottomY+12, 92, 14, ctlProg, 0)
	pSendMessageW.Call(gCtl[ctlProg], PBM_SETRANGE32, 0, 100)
	pShowWindow.Call(gCtl[ctlProg], 0)
	mkCtl("BUTTON", "Quit", BS_PUSHBUTTON|BS_FLAT, 720, bottomY, 120, 38, ctlQuit, gFonts["norm"])
	// v8.9.10: chat card — messages from viewers land here, never in a
	// popup; the PC user replies from the input box (Enter = Send).
	mkCtl("STATIC", "CHAT — messages from viewers", 0, 20, chatLblY, 300, 18, ctlChatLbl, gFonts["lbl"])
	mkChatLog(20, chatLogY, 640, chatLogH)
	mkCtl("EDIT", "", WS_BORDER|ES_AUTOHSCROLL, 20, chatInputY, 540, 28, ctlChatInput, gFonts["norm"])
	mkCtl("BUTTON", "Send", BS_DEFPUSHBUTTON|BS_FLAT, 570, chatInputY, 90, 28, ctlChatSend, gFonts["norm"])

	// v8.9.10: recording indicator (hidden until a recording runs)
	mkCtl("STATIC", "", SS_LEFT, 680, chatInputY, 176, 28, ctlRec, gFonts["norm"])
	pShowWindow.Call(gCtl[ctlRec], 0)
	if getAutoRun() {
		pSendMessageW.Call(gCtl[ctlAutoRun], BM_SETCHECK, BST_CHECKED, 0)
	}

	applyStatus()
	refreshMeta()
	refreshWakeStatus() // v8.1: show wake-up service status
	// v8.4.0: show the camera health found by runPrereqCheck at startup
	gState.mu.Lock()
	cs := gState.camStatus
	gState.mu.Unlock()
	if cs != "" {
		setText(ctlCamVal, cs)
	}
	pSetTimer.Call(hwnd, 1, 1000, 0) // v8.9.2: 1s heartbeat (was 5s) + uptime refresh
	pShowWindow.Call(hwnd, SW_SHOW)
	pUpdateWindow.Call(hwnd)

	var m msgT
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		// v8.9.10: dialog messages give Tab navigation + Enter = default
		// button (chat Send) without a dialog template.
		if d, _, _ := pIsDialogMessage.Call(hwnd, uintptr(unsafe.Pointer(&m))); d == 0 {
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		heartbeatTick() // v8.9.4: the pump itself proves the GUI thread alive
	}
}
