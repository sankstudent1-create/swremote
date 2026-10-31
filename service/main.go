// SWRemote Service — always-connected background service for a PC.
// Starts at Windows boot, holds a lightweight WebSocket to the relay, and
// launches the SWRemote agent on demand ("wake") from the owner's website.
// v8.0.
package main

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows/svc"
)

const svcName = "SWRemoteService"

var (
	serverURL = "wss://swremote-relay.onrender.com/ws"
	deviceID  string
)

func logf(f string, a ...any) {
	// service log (visible via --console, and in the log file)
	msg := fmt.Sprintf(f, a...)
	fmt.Printf("[svc] %s\n", msg)
	appendLog(msg)
}

func appendLog(msg string) {
	d, _ := os.UserCacheDir()
	if d == "" {
		return
	}
	p := filepath.Join(d, "SWRemote", "service.log")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05"), msg)
		f.Close()
	}
}

func configDir() string {
	exe, _ := os.Executable()
	return filepath.Dir(exe)
}

func readIDFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var c struct {
		DeviceID string `json:"device_id"`
	}
	if json.Unmarshal(b, &c) == nil && c.DeviceID != "" {
		return c.DeviceID
	}
	return ""
}

func loadDeviceID() string {
	// v8.9.6: prefer the agent's canonical config in
	// %LOCALAPPDATA%\SWRemote so agent + service share one ID even when
	// the install folder is customised in Setup.
	if id := readIDFile(filepath.Join(os.Getenv("LOCALAPPDATA"), "SWRemote", "swremote.json")); id != "" {
		return id
	}
	// share swremote.json with the agent (same install dir)
	p := filepath.Join(configDir(), "swremote.json")
	b, err := os.ReadFile(p)
	if err == nil {
		var c struct {
			DeviceID string `json:"device_id"`
		}
		if json.Unmarshal(b, &c) == nil && c.DeviceID != "" {
			return c.DeviceID
		}
	}
	// fresh install: create an ID now so agent + service agree
	id := randomID()
	c := map[string]any{"device_id": id, "server": serverURL}
	b, _ = json.MarshalIndent(c, "", "  ")
	_ = os.WriteFile(p, b, 0600)
	return id
}

func randomID() string {
	rb := make([]byte, 9)
	if _, err := crand.Read(rb); err != nil {
		// fallback: time-based
		t := time.Now().UnixNano()
		for i := range rb {
			rb[i] = byte(t >> (i * 7))
		}
	}
	s := ""
	for i := 0; i < 9; i++ {
		s += string(rune('0' + int(rb[i]%10)))
	}
	return s
}

// launchAgent starts the SWRemote agent exe next to the service.
func launchAgent() error {
	exe := filepath.Join(configDir(), "SWRemote-Agent.exe")
	if _, err := os.Stat(exe); err != nil {
		// try portable name
		exe = filepath.Join(configDir(), "agent.exe")
		if _, err2 := os.Stat(exe); err2 != nil {
			return fmt.Errorf("agent exe not found")
		}
	}
	// v8.9.6: a service runs in session 0, which has no interactive
	// desktop — a plain exec used to start the agent there, invisible,
	// unable to capture the screen. Launch it in the active console
	// session instead, so the window appears on the user's desktop.
	if err := launchAgentInSession(exe); err != nil {
		logf("wake: session launch failed (%v) — falling back to plain start", err)
		cmd := exec.Command(exe)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		return cmd.Start()
	}
	return nil
}

// ---- launch into the interactive user session (WTS) ----
// Stdlib syscall only, so no new module dependency.

var (
	wts32dll                      = syscall.NewLazyDLL("wtsapi32.dll")
	advapi32dll                   = syscall.NewLazyDLL("advapi32.dll")
	userenvdll                    = syscall.NewLazyDLL("userenv.dll")
	kernel32dll                   = syscall.NewLazyDLL("kernel32.dll")
	pWTSGetActiveConsoleSessionId = kernel32dll.NewProc("WTSGetActiveConsoleSessionId")
	pWTSQueryUserToken            = wts32dll.NewProc("WTSQueryUserToken")
	pDuplicateTokenEx             = advapi32dll.NewProc("DuplicateTokenEx")
	pCreateEnvironmentBlock       = userenvdll.NewProc("CreateEnvironmentBlock")
	pDestroyEnvironmentBlock      = userenvdll.NewProc("DestroyEnvironmentBlock")
	pCreateProcessAsUserW         = advapi32dll.NewProc("CreateProcessAsUserW")
	pCloseHandleProc              = kernel32dll.NewProc("CloseHandle")
)

type wtsStartupInfo struct {
	cb                                                            uint32
	reserved                                                      *uint16
	desktop                                                       *uint16
	title                                                         *uint16
	x, y, xSize, ySize, xCountChars, yCountChars, fillAttr, flags uint32
	showWindow                                                    uint16
	cbReserved2                                                   uint16
	reserved2                                                     *byte
	stdInput, stdOutput, stdError                                 uintptr
}

type wtsProcessInfo struct {
	process, thread     uintptr
	processID, threadID uint32
}

func launchAgentInSession(exe string) error {
	sid, _, _ := pWTSGetActiveConsoleSessionId.Call()
	if sid == 0xFFFFFFFF {
		return fmt.Errorf("no active console session")
	}
	var hToken uintptr
	r, _, _ := pWTSQueryUserToken.Call(sid, uintptr(unsafe.Pointer(&hToken)))
	if r == 0 || hToken == 0 {
		return fmt.Errorf("WTSQueryUserToken failed")
	}
	defer pCloseHandleProc.Call(hToken)
	var hPrimary uintptr
	// DuplicateTokenEx(hToken, MAXIMUM_ALLOWED, nil, SecurityImpersonation=2, TokenPrimary=1, &hPrimary)
	r, _, _ = pDuplicateTokenEx.Call(hToken, 0x2000000, 0, 2, 1, uintptr(unsafe.Pointer(&hPrimary)))
	if r == 0 || hPrimary == 0 {
		return fmt.Errorf("DuplicateTokenEx failed")
	}
	defer pCloseHandleProc.Call(hPrimary)
	var env uintptr
	if r, _, _ := pCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&env)), hPrimary, 0); r == 0 {
		return fmt.Errorf("CreateEnvironmentBlock failed")
	}
	defer pDestroyEnvironmentBlock.Call(env)
	desktop, _ := syscall.UTF16PtrFromString(`winsta0\default`)
	si := wtsStartupInfo{desktop: desktop}
	si.cb = uint32(unsafe.Sizeof(si))
	cmdline, _ := syscall.UTF16PtrFromString(`"` + exe + `"`)
	var pi wtsProcessInfo
	// CreateProcessAsUserW(hPrimary, nil, cmdline, nil, nil, false, CREATE_UNICODE_ENVIRONMENT=0x400, env, nil, &si, &pi)
	r, _, errno := pCreateProcessAsUserW.Call(hPrimary, 0, uintptr(unsafe.Pointer(cmdline)), 0, 0, 0, 0x400, env, 0, uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
	if r == 0 {
		return fmt.Errorf("CreateProcessAsUser failed (%v)", errno)
	}
	pCloseHandleProc.Call(pi.process)
	pCloseHandleProc.Call(pi.thread)
	return nil
}

func runLoop() {
	for {
		if err := runSession(); err != nil {
			logf("session ended: %v; retry in 15s", err)
		}
		select {
		case <-time.After(15 * time.Second):
		}
	}
}

func runSession() error {
	dialer := websocket.Dialer{HandshakeTimeout: 60 * time.Second}
	ws, _, err := dialer.Dial(serverURL, nil)
	if err != nil {
		return err
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)
	logf("connected as service for %s", deviceID)

	reg, _ := json.Marshal(map[string]any{"t": "svc-register", "id": deviceID})
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, reg); err != nil {
		return err
	}

	// heartbeat
	stopHb := make(chan struct{})
	defer close(stopHb)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopHb:
				return
			case <-t.C:
				ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"t":"svc-ping"}`))
			}
		}
	}()

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg["t"] {
		case "wake":
			logf("wake command received — launching agent")
			if err := launchAgent(); err != nil {
				logf("launch failed: %v", err)
			} else {
				logf("agent launched")
			}
		case "svc-registered":
			logf("registered with relay")
		}
	}
}

// ---------- Windows Service plumbing ----------

type handler struct{}

func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	go runLoop()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

func main() {
	deviceID = loadDeviceID()
	if len(os.Args) > 1 && os.Args[1] == "--console" {
		logf("running in console mode (device %s)", deviceID)
		runLoop()
		return
	}
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		logf("IsWindowsService: %v", err)
		return
	}
	if !isSvc {
		fmt.Println("SWRemote Service — run with --console to debug, or install as a Windows Service.")
		fmt.Println("Device ID:", deviceID)
		return
	}
	_ = svc.Run(svcName, &handler{})
}
