// webview.go — the agent window as a WebView2 page (v8.9.13).
//
// Why: the classic window is painted pixel-by-pixel with GDI, which has
// no CSS, no layout engine, no rounded cards — it can never look like
// the web console. WebView2 is real Chromium (Edge's engine, shipped
// with Windows 11 and most Windows 10 installs), so the agent window
// can run the same calm HTML/CSS design as the console.
//
// Safety: everything here is best-effort. If the WebView2 runtime is
// missing, or environment/controller creation times out, setup returns
// an error and runGUI falls back to the classic GDI window — the user
// always gets a window.
//
// COM notes (pure Go, no cgo): WebView2 handlers are COM objects with
// a 4-entry vtable (QueryInterface, AddRef, Release, Invoke). Objects
// and vtables are kept alive in package-level slices because Go's GC
// never moves heap objects but would collect unreferenced ones. Page
// messages are never handled on the WebView2 thread directly: they are
// queued and posted to the GUI thread (WM_APP_WVMSG), so clipboard and
// control actions run exactly where button clicks run today.

package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed agent.html
var agentHTML string

var (
	wvController uintptr
	wvCore       uintptr

	wvMsgMu    sync.Mutex
	wvMsgNext  uintptr = 1
	wvMsgQueue         = map[uintptr]string{}

	comKeepAlive []any
	comHandlerMu sync.Mutex
	comHandlerFn = map[uintptr]func(a1, a2 uintptr){}
)

var (
	wvQI = windows.NewCallback(func(this, riid, ppv uintptr) uintptr {
		// Accept any interface: WebView2 only needs a valid pointer back.
		*(*uintptr)(unsafe.Pointer(ppv)) = this
		return 0 // S_OK
	})
	wvAddRef  = windows.NewCallback(func(this uintptr) uintptr { return 1 })
	wvRelease = windows.NewCallback(func(this uintptr) uintptr { return 1 })
	wvInvoke  = windows.NewCallback(func(this, a1, a2 uintptr) uintptr {
		comHandlerMu.Lock()
		fn := comHandlerFn[this]
		comHandlerMu.Unlock()
		if fn != nil {
			fn(a1, a2)
		}
		return 0
	})
)

// newComHandler builds a COM handler object whose Invoke calls fn.
func newComHandler(fn func(a1, a2 uintptr)) uintptr {
	vtbl := []uintptr{wvQI, wvAddRef, wvRelease, wvInvoke}
	type comObj struct {
		vtbl *uintptr
	}
	obj := &comObj{vtbl: &vtbl[0]}
	ptr := uintptr(unsafe.Pointer(obj))
	comHandlerMu.Lock()
	comHandlerFn[ptr] = fn
	comKeepAlive = append(comKeepAlive, obj, vtbl)
	comHandlerMu.Unlock()
	return ptr
}

type envRes struct {
	hr  uintptr
	env uintptr
}

type ctrlRes struct {
	hr   uintptr
	ctrl uintptr
}

// setupWebView2 creates the environment, controller, message handler
// and navigates to the embedded agent page. Each async stage has a
// 10s deadline so a broken runtime degrades to the GDI window fast.
func setupWebView2(hwnd uintptr) error {
	loader, err := windows.LoadLibrary("WebView2Loader.dll")
	if err != nil {
		return fmt.Errorf("WebView2Loader missing: %v", err)
	}
	proc, err := windows.GetProcAddress(loader, "CreateCoreWebView2EnvironmentWithOptions")
	if err != nil {
		return fmt.Errorf("CreateCoreWebView2EnvironmentWithOptions missing: %v", err)
	}
	// Per-user data folder (WebView2 requires a writable one).
	udf := os.Getenv("LOCALAPPDATA")
	if udf == "" {
		udf = os.TempDir()
	}
	udf = filepath.Join(udf, "SWRemote", "wv2")
	if err := os.MkdirAll(udf, 0o755); err != nil {
		return fmt.Errorf("user data folder: %v", err)
	}
	udfPtr, err := windows.UTF16PtrFromString(udf)
	if err != nil {
		return err
	}

	envDone := make(chan envRes, 1)
	envHandler := newComHandler(func(a1, a2 uintptr) {
		envDone <- envRes{hr: a1, env: a2}
	})
	r, _, _ := syscall.SyscallN(proc, 0, uintptr(unsafe.Pointer(udfPtr)), 0, envHandler)
	if r != 0 {
		return fmt.Errorf("CreateCoreWebView2Environment failed (%x)", r)
	}
	var env uintptr
	select {
	case res := <-envDone:
		if res.hr != 0 {
			return fmt.Errorf("environment failed (%x)", res.hr)
		}
		env = res.env
	case <-time.After(10 * time.Second):
		return fmt.Errorf("environment timed out")
	}

	ctrlDone := make(chan ctrlRes, 1)
	ctrlHandler := newComHandler(func(a1, a2 uintptr) {
		ctrlDone <- ctrlRes{hr: a1, ctrl: a2}
	})
	// ICoreWebView2Environment::CreateCoreWebView2Controller (vtable 3)
	if hr := comCallN(env, 3, hwnd, ctrlHandler); hr != 0 {
		return fmt.Errorf("CreateCoreWebView2Controller failed (%x)", hr)
	}
	var ctrl uintptr
	select {
	case res := <-ctrlDone:
		if res.hr != 0 {
			return fmt.Errorf("controller failed (%x)", res.hr)
		}
		ctrl = res.ctrl
	case <-time.After(10 * time.Second):
		return fmt.Errorf("controller timed out")
	}

	// ICoreWebView2Controller::get_CoreWebView2 (vtable 18)
	var core uintptr
	if hr := comCallN(ctrl, 18, uintptr(unsafe.Pointer(&core))); hr != 0 || core == 0 {
		return fmt.Errorf("get_CoreWebView2 failed (%x)", hr)
	}
	// ICoreWebView2::add_WebMessageReceived (vtable 22)
	msgHandler := newComHandler(func(a1, a2 uintptr) {
		onWebViewMessage(a2)
	})
	var token int64
	if hr := comCallN(core, 22, msgHandler, uintptr(unsafe.Pointer(&token))); hr != 0 {
		return fmt.Errorf("add_WebMessageReceived failed (%x)", hr)
	}
	// ICoreWebView2::NavigateToString (vtable 5)
	htmlPtr, err := windows.UTF16PtrFromString(agentHTML)
	if err != nil {
		return err
	}
	if hr := comCallN(core, 5, uintptr(unsafe.Pointer(htmlPtr))); hr != 0 {
		return fmt.Errorf("NavigateToString failed (%x)", hr)
	}

	wvController = ctrl
	wvCore = core
	wvResize()
	log("webview: agent UI running in WebView2")
	return nil
}

// wvResize keeps the WebView2 child filling the host window's client area.
func wvResize() {
	if wvController == 0 {
		return
	}
	var rc [4]int32
	pGetClientRect.Call(gHWND, uintptr(unsafe.Pointer(&rc)))
	// ICoreWebView2Controller::put_Bounds (vtable 6, RECT by reference on x64)
	comCallN(wvController, 6, uintptr(unsafe.Pointer(&rc)))
}

// runWebViewGUI runs the WebView2 agent window. A nil return means the
// window ran and closed normally; any error means "fall back to GDI".
func runWebViewGUI() error {
	hwnd, err := createHostWindow()
	if err != nil {
		return err
	}
	if err := setupWebView2(hwnd); err != nil {
		pDestroyWindow.Call(hwnd)
		gHWND = 0
		wvController, wvCore = 0, 0
		return err
	}
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
		// NOTE: no IsDialogMessage here — it would eat the web page's
		// Tab and Enter keys. The page handles its own keyboard input.
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		heartbeatTick()
	}
	return nil
}

type wvViewer struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

type wvState struct {
	T             string     `json:"t"`
	ID            string     `json:"id"`
	PIN           string     `json:"pin"`
	Status        string     `json:"status"`
	StatusOK      bool       `json:"statusOk"`
	Pill          string     `json:"pill"`
	PillOK        bool       `json:"pillOk"`
	Link          string     `json:"link"`
	Claim         string     `json:"claim"`
	Viewers       []wvViewer `json:"viewers"`
	ViewCount     int        `json:"viewCount"`
	Wake          string     `json:"wake"`
	WakeInstalled bool       `json:"wakeInstalled"`
	Cam           string     `json:"cam"`
	Chat          []string   `json:"chat"`
	RecText       string     `json:"recText"`
	RecShow       bool       `json:"recShow"`
	Progress      int        `json:"progress"`
	UpdText       string     `json:"updText"`
	UpdOn         bool       `json:"updOn"`
	AutoRun       bool       `json:"autoRun"`
	Uptime        string     `json:"uptime"`
	Version       string     `json:"version"`
}

// pushWebViewState sends the current agent snapshot to the page.
// No-op in GDI mode (wvCore == 0).
func pushWebViewState() {
	if wvCore == 0 {
		return
	}
	gState.mu.Lock()
	st := wvState{
		T: "state", Status: gState.statusText, StatusOK: gState.statusOK,
		Pill: gState.pillText, PillOK: gState.pillOK,
		Wake: gState.wakeStatus, WakeInstalled: gState.wakeInstalled,
		Cam: gState.camStatus, Chat: append([]string(nil), gState.chatLines...),
		RecText: gState.recText, RecShow: gState.recShow,
		Progress: gState.progress, UpdText: gState.updBtnText, UpdOn: gState.updBtnOn,
		Uptime: fmtUptime(time.Since(gState.started)),
	}
	st.ViewCount = gState.viewers
	gState.mu.Unlock()

	cfgMu.Lock()
	st.ID, st.PIN, st.Claim = cfg.DeviceID, cfg.PIN, cfg.ClaimCode
	cfgMu.Unlock()
	st.Link = inviteLink()
	st.AutoRun = getAutoRun()
	st.Version = appVersion

	viewersMu.Lock()
	for _, v := range viewers {
		st.Viewers = append(st.Viewers, wvViewer{Name: v.name, Avatar: v.avatarURL})
	}
	viewersMu.Unlock()

	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	p, err := windows.UTF16PtrFromString(string(b))
	if err != nil {
		return
	}
	// ICoreWebView2::PostWebMessageAsString (vtable 38)
	comCallN(wvCore, 38, uintptr(unsafe.Pointer(p)))
}

// (onWebViewMessage frees the message string with pCoTaskMemFree from cam_mf.go.)

// onWebViewMessage runs on a WebView2 thread: it only reads the message
// and queues it for the GUI thread — never touches controls directly.
func onWebViewMessage(args uintptr) {
	// ICoreWebView2WebMessageReceivedEventArgs::TryGetWebMessageAsString (vtable 5)
	var strPtr uintptr
	if hr := comCallN(args, 5, uintptr(unsafe.Pointer(&strPtr))); hr != 0 || strPtr == 0 {
		return
	}
	js := windows.UTF16ToString((*[1 << 20]uint16)(unsafe.Pointer(strPtr))[:])
	pCoTaskMemFree.Call(strPtr)
	if strings.TrimSpace(js) == "" {
		return
	}
	wvMsgMu.Lock()
	id := wvMsgNext
	wvMsgNext++
	wvMsgQueue[id] = js
	wvMsgMu.Unlock()
	if gHWND != 0 {
		pPostMessageW.Call(gHWND, WM_APP_WVMSG, id, 0)
	}
}

type wvMsg struct {
	T    string `json:"t"`
	A    string `json:"a"`
	Text string `json:"text"`
	On   bool   `json:"on"`
}

// dispatchWebViewAction runs on the GUI thread — the same actions the
// classic window's buttons run, so behavior matches exactly.
func dispatchWebViewAction(js string) {
	var m wvMsg
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		return
	}
	if m.T == "ready" {
		pushWebViewState()
		return
	}
	if m.T != "action" {
		return
	}
	switch m.A {
	case "copy-id":
		copyToClipboard(cfg.DeviceID)
		guiSetStatus("ID copied to clipboard.", true)
	case "copy-link":
		copyToClipboard(inviteLink())
		guiSetStatus("Invite link copied — send it to them.", true)
	case "copy-claim":
		copyToClipboard(cfg.ClaimCode)
		guiSetStatus("Link code copied — enter it in your dashboard.", true)
	case "new-pin":
		newPIN := randomPIN()
		cfgMu.Lock()
		cfg.PIN = newPIN
		cfgMu.Unlock()
		saveConfig()
		guiSetStatus("New PIN generated.", true)
	case "update":
		if updateRunning.Swap(true) {
			return
		}
		go runUpdateFlow()
	case "wake-install":
		go func() {
			msg := reinstallWakeService()
			gState.mu.Lock()
			gState.wakeStatus = msg
			gState.mu.Unlock()
			if gHWND != 0 {
				pPostMessageW.Call(gHWND, WM_APP_META, 0, 0)
			}
		}()
	case "fix-cam":
		go fixCameraFlow()
	case "repair-win":
		go repairWindowsFlow()
	case "autorun":
		setAutoRun(m.On)
		cfgMu.Lock()
		cfg.AutoRunSet = true
		cfgMu.Unlock()
		saveConfig()
	case "chat-send":
		text := strings.TrimSpace(m.Text)
		if len(text) > 200 {
			text = text[:200]
		}
		if text != "" {
			sendWSJSON(map[string]any{"t": "chat", "text": text})
			guiAddChat("You", text)
		}
	case "quit":
		markQuitting()
		pPostMessageW.Call(gHWND, WM_CLOSE, 0, 0)
	}
}
