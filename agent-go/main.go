// SWRemote Windows Agent — native .exe build (pure Go, no cgo).
// Cross-compiled: GOOS=windows GOARCH=amd64 go build -o SWRemote-Agent.exe .
//
// - Registers with the relay server (9-digit ID + PIN)
// - Streams the screen as JPEG frames over WebSocket
// - Applies remote mouse/keyboard via WinAPI SendInput
// - Receives files, chat popups, remote lock, quality changes
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"
	"golang.org/x/sys/windows"
)

// ---------------- WinAPI ----------------

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	pSendInput       = user32.NewProc("SendInput")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pMessageBoxW     = user32.NewProc("MessageBoxW")
	pLockWorkStation = user32.NewProc("LockWorkStation")
)

const (
	SM_CXSCREEN = 0
	SM_CYSCREEN = 1

	INPUT_MOUSE    = 0
	INPUT_KEYBOARD = 1

	MOUSEEVENTF_MOVE     = 0x0001
	MOUSEEVENTF_LEFTDOWN = 0x0002
	MOUSEEVENTF_LEFTUP   = 0x0004
	MOUSEEVENTF_RIGHTDOWN = 0x0008
	MOUSEEVENTF_RIGHTUP   = 0x0010
	MOUSEEVENTF_MIDDLEDOWN = 0x0020
	MOUSEEVENTF_MIDDLEUP   = 0x0040
	MOUSEEVENTF_WHEEL    = 0x0800
	MOUSEEVENTF_ABSOLUTE = 0x8000

	KEYEVENTF_KEYUP   = 0x0002
	KEYEVENTF_UNICODE = 0x0004
)

func getSystemMetrics(n int) int {
	r, _, _ := pGetSystemMetrics.Call(uintptr(n))
	return int(r)
}

func sendInput(buf []byte) {
	pSendInput.Call(1, uintptr(unsafe.Pointer(&buf[0])), 40)
}

func mouseInput(dx, dy int32, data, flags uint32) {
	var b [40]byte
	binary.LittleEndian.PutUint32(b[0:4], INPUT_MOUSE)
	binary.LittleEndian.PutUint32(b[8:12], uint32(dx))
	binary.LittleEndian.PutUint32(b[12:16], uint32(dy))
	binary.LittleEndian.PutUint32(b[16:20], data)
	binary.LittleEndian.PutUint32(b[20:24], flags)
	sendInput(b[:])
}

func keyInput(vk, scan uint16, flags uint32) {
	var b [40]byte
	binary.LittleEndian.PutUint32(b[0:4], INPUT_KEYBOARD)
	binary.LittleEndian.PutUint16(b[8:10], vk)
	binary.LittleEndian.PutUint16(b[10:12], scan)
	binary.LittleEndian.PutUint32(b[12:16], flags)
	sendInput(b[:])
}

func mouseMoveAbs(xRatio, yRatio float64) {
	w := getSystemMetrics(SM_CXSCREEN)
	h := getSystemMetrics(SM_CYSCREEN)
	dx := int32(xRatio * 65535)
	dy := int32(yRatio * 65535)
	_ = w; _ = h
	mouseInput(dx, dy, 0, MOUSEEVENTF_MOVE|MOUSEEVENTF_ABSOLUTE)
}

func mouseButton(which string, down bool) {
	var flag uint32
	switch which {
	case "right":
		if down { flag = MOUSEEVENTF_RIGHTDOWN } else { flag = MOUSEEVENTF_RIGHTUP }
	case "middle":
		if down { flag = MOUSEEVENTF_MIDDLEDOWN } else { flag = MOUSEEVENTF_MIDDLEUP }
	default:
		if down { flag = MOUSEEVENTF_LEFTDOWN } else { flag = MOUSEEVENTF_LEFTUP }
	}
	mouseInput(0, 0, 0, flag)
}

func mouseWheel(dy int) {
	mouseInput(0, 0, uint32(int32(dy)*120), MOUSEEVENTF_WHEEL)
}

var specialVK = map[string]uint16{
	"enter": 0x0D, "tab": 0x09, "backspace": 0x08, "delete": 0x2E,
	"esc": 0x1B, "escape": 0x1B, "space": 0x20, "shift": 0x10,
	"ctrl": 0x11, "alt": 0x12, "up": 0x26, "down": 0x28,
	"left": 0x25, "right": 0x27, "home": 0x24, "end": 0x23,
	"pageup": 0x21, "pagedown": 0x22, "win": 0x5B, "cmd": 0x5B,
	"caps": 0x14,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75,
	"f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
}

func keyPress(name string, down bool) {
	vk, ok := specialVK[strings.ToLower(name)]
	if !ok {
		if len(name) == 1 {
			typeUnicode(name, down)
		}
		return
	}
	var flags uint32
	if !down {
		flags = KEYEVENTF_KEYUP
	}
	keyInput(vk, 0, flags)
}

func typeUnicode(s string, down bool) {
	for _, r := range s {
		var flags uint32 = KEYEVENTF_UNICODE
		if !down {
			flags |= KEYEVENTF_KEYUP
		}
		// UTF-16 code units
		if r < 0x10000 {
			keyInput(0, uint16(r), flags)
		} else {
			r -= 0x10000
			keyInput(0, uint16(0xD800+(r>>10)), flags)
			keyInput(0, uint16(0xDC00+(r&0x3FF)), flags)
		}
	}
}

func notify(title, text string) {
	go func() {
		t, _ := windows.UTF16PtrFromString(title)
		m, _ := windows.UTF16PtrFromString(text)
		pMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x40)
	}()
}

func lockWorkstation() {
	pLockWorkStation.Call()
}

// ---------------- config ----------------

type Config struct {
	Server   string `json:"server"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	PIN      string `json:"pin"`
	FPS      int    `json:"fps"`
	Quality  int    `json:"quality"`
	Scale    float64 `json:"scale"`
}

func configPath() string {
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "swremote.json")
}

func randomID() string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	var sb strings.Builder
	for _, v := range b {
		sb.WriteByte(byte('1' + int(v) % 9))
	}
	return sb.String()
}

func loadConfig() *Config {
	cfg := &Config{Server: "wss://localhost:8080/ws", FPS: 10, Quality: 60, Scale: 0.75, PIN: "123456"}
	if hn, err := os.Hostname(); err == nil {
		cfg.Name = hn
	}
	if data, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(data, cfg)
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = randomID()
	}
	if cfg.FPS < 1 || cfg.FPS > 25 {
		cfg.FPS = 10
	}
	if cfg.Quality < 10 || cfg.Quality > 90 {
		cfg.Quality = 60
	}
	if cfg.Scale < 0.25 || cfg.Scale > 1 {
		cfg.Scale = 0.75
	}
	_ = os.WriteFile(configPath(), mustJSON(cfg), 0600)
	return cfg
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

// ---------------- capture ----------------

func grabFrame(cfg *Config) ([]byte, int, int) {
	bounds := screenshot.GetDisplayBounds(0)
	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return nil, 0, 0
	}
	sw := int(float64(bounds.Dx()) * cfg.Scale)
	sh := int(float64(bounds.Dy()) * cfg.Scale)
	var src image.Image = img
	if sw != bounds.Dx() || sh != bounds.Dy() {
		dst := image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
		src = dst
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, src, &jpeg.Options{Quality: cfg.Quality})
	return buf.Bytes(), sw, sh
}

// ---------------- input dispatch ----------------

func applyInput(msg map[string]any) {
	k, _ := msg["k"].(string)
	switch k {
	case "move":
		x, _ := msg["x"].(float64)
		y, _ := msg["y"].(float64)
		mouseMoveAbs(x, y)
	case "down", "up":
		b, _ := msg["b"].(string)
		mouseButton(b, k == "down")
	case "scroll":
		dy, _ := msg["dy"].(float64)
		dx, _ := msg["dx"].(float64)
		mouseWheel(int(dy))
		if dx != 0 {
			mouseInput(0, 0, uint32(int32(dx)*120), 0x1000) // MOUSEEVENTF_HWHEEL
		}
	case "key":
		name, _ := msg["key"].(string)
		down, _ := msg["down"].(bool)
		keyPress(name, down)
	case "type":
		text, _ := msg["text"].(string)
		if len(text) > 200 {
			text = text[:200]
		}
		typeUnicode(text, true)
		typeUnicode(text, false)
	}
}

// ---------------- file receive ----------------

type incomingFile struct {
	f    *os.File
	path string
	left int64
}

var incoming = map[string]*incomingFile{}

func downloadDir() string {
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, "Downloads", "SWRemote")
	_ = os.MkdirAll(d, 0755)
	return d
}

func handleFileMeta(msg map[string]any) {
	id, _ := msg["id"].(string)
	name, _ := msg["name"].(string)
	size, _ := msg["size"].(float64)
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == ' ' {
			return r
		}
		return -1
	}, name)
	if safe == "" {
		safe = "file.bin"
	}
	path := filepath.Join(downloadDir(), safe)
	f, err := os.Create(path)
	if err != nil {
		fmt.Println("file create error:", err)
		return
	}
	incoming[id] = &incomingFile{f: f, path: path, left: int64(size)}
	fmt.Printf("Receiving %s (%d bytes)\n", safe, int64(size))
}

func handleFileChunk(data []byte) {
	if len(data) < 2 {
		return
	}
	idLen := int(data[0])
	if len(data) < 1+idLen {
		return
	}
	id := string(data[1 : 1+idLen])
	payload := data[1+idLen:]
	rec, ok := incoming[id]
	if !ok {
		return
	}
	_, _ = rec.f.Write(payload)
	rec.left -= int64(len(payload))
	if rec.left <= 0 {
		rec.f.Close()
		fmt.Println("File saved:", rec.path)
		notify("SWRemote", "File received: "+filepath.Base(rec.path))
		delete(incoming, id)
	}
}

// ---------------- main ----------------

func main() {
	cfg := loadConfig()
	fmt.Println("============================================")
	fmt.Println("  SWRemote Agent  (by SWInfoSystems)")
	fmt.Println("============================================")
	fmt.Println("Your SWRemote ID :", cfg.DeviceID)
	fmt.Println("Your PIN         :", cfg.PIN)
	fmt.Println("Edit swremote.json next to this .exe to change server/PIN.")
	fmt.Println()

	backoff := 2 * time.Second
	for {
		if err := runSession(cfg); err != nil {
			fmt.Println("Connection lost:", err)
		}
		fmt.Printf("Retrying in %v...\n", backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func runSession(cfg *Config) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.Dial(cfg.Server, nil)
	if err != nil {
		return err
	}
	defer ws.Close()
	ws.SetReadLimit(16 << 20)
	fmt.Println("Connected to", cfg.Server)

	reg, _ := json.Marshal(map[string]any{
		"t": "register", "id": cfg.DeviceID, "name": cfg.Name,
		"pin": cfg.PIN, "platform": "windows",
	})
	if err := ws.WriteMessage(websocket.TextMessage, reg); err != nil {
		return err
	}

	stop := make(chan struct{})
	defer close(stop)

	// screen sender
	go func() {
		interval := time.Second / time.Duration(cfg.FPS)
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			data, _, _ := grabFrame(cfg)
			if data != nil {
				pkt := make([]byte, 0, len(data)+1)
				pkt = append(pkt, 0x01)
				pkt = append(pkt, data...)
				ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := ws.WriteMessage(websocket.BinaryMessage, pkt); err != nil {
					return
				}
			}
			// adaptive: if sending took longer than the interval, skip sleeping (drop lag)
			if elapsed := time.Since(start); elapsed < interval {
				select {
				case <-stop:
					return
				case <-time.After(interval - elapsed):
				}
			}
		}
	}()

	// heartbeat
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"t":"ping"}`))
			}
		}
	}()

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if mt == websocket.BinaryMessage {
			if len(data) > 0 && data[0] == 0x02 {
				handleFileChunk(data[1:])
			}
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg["t"] {
		case "registered":
			fmt.Println("Registered. Waiting for viewers...")
		case "input":
			applyInput(msg)
		case "quality":
			if q, ok := msg["q"].(float64); ok {
				cfg.Quality = clampInt(int(q), 10, 90)
			}
			if s, ok := msg["scale"].(float64); ok {
				cfg.Scale = clampFloat(s, 0.25, 1.0)
			}
			fmt.Println("quality ->", cfg.Quality, "scale ->", cfg.Scale)
		case "chat":
			text, _ := msg["text"].(string)
			fmt.Println("CHAT:", text)
			if len(text) > 200 {
				text = text[:200]
			}
			notify("SWRemote message", text)
		case "file_meta":
			handleFileMeta(msg)
		case "lock":
			fmt.Println("Remote lock requested")
			lockWorkstation()
		case "viewer_joined":
			notify("SWRemote", "Someone connected to this PC")
		}
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
