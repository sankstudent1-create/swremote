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

func loadDeviceID() string {
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
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
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
