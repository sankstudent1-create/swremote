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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
	"golang.org/x/sys/windows"
)

// appVersion is overridden at build time: -ldflags "-X main.appVersion=3.1.0"
var appVersion = "8.0.0"

var (
	cfg   *Config
	cfgMu sync.Mutex
)

// ---------------- WinAPI ----------------

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	pSendInput       = user32.NewProc("SendInput")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pMessageBoxW     = user32.NewProc("MessageBoxW")
	pLockWorkStation = user32.NewProc("LockWorkStation")
	pGetCursorPos    = user32.NewProc("GetCursorPos")
)

// needKeyframe is set when a viewer joins or asks for a full frame.
var needKeyframe atomic.Bool

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
	Server    string `json:"server"`
	DeviceID  string `json:"device_id"`
	Name      string `json:"name"`
	PIN       string `json:"pin"`
	FPS       int    `json:"fps"`
	Quality   int    `json:"quality"`
	Scale     float64 `json:"scale"`
	ClaimCode string `json:"claim_code"` // Phase 1: shown in the window, entered once in the dashboard
	AutoRunSet bool   `json:"autorun_set"` // true once the user (or first-run default) decided about auto-start
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

func randomPIN() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	var sb strings.Builder
	for _, v := range b {
		sb.WriteByte(byte('0' + int(v)%10))
	}
	return sb.String()
}

func randomClaimCode() string {
	const chars = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no confusing 0/O/1/I/L
	var b [6]byte
	_, _ = rand.Read(b[:])
	var sb strings.Builder
	for _, v := range b {
		sb.WriteByte(chars[int(v)%len(chars)])
	}
	return sb.String()
}

func loadConfig() *Config {
	c := &Config{Server: "wss://swremote-relay.onrender.com/ws", FPS: 30, Quality: 70, Scale: 1.0, PIN: ""}
	if hn, err := os.Hostname(); err == nil {
		c.Name = hn
	}
	if data, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(data, c)
	}
	if c.DeviceID == "" {
		c.DeviceID = randomID()
	}
	if c.PIN == "" {
		c.PIN = randomPIN()
	}
	if c.ClaimCode == "" {
		c.ClaimCode = randomClaimCode()
	}
	if c.FPS < 1 || c.FPS > 30 {
		c.FPS = 30
	}
	if c.Quality < 10 || c.Quality > 90 {
		c.Quality = 70
	}
	if c.Scale < 0.25 || c.Scale > 1 {
		c.Scale = 1.0
	}
	_ = os.WriteFile(configPath(), mustJSON(c), 0600)
	return c
}

func saveConfig() {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	_ = os.WriteFile(configPath(), mustJSON(cfg), 0600)
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

// ---------------- tiled streaming (v3.0) ----------------
// Instead of JPEG-encoding the whole screen every frame (5-8 fps), the
// screen is split into 128x128 tiles; only tiles that changed since the
// previous frame are encoded and sent (0x03 packets). Full keyframes (0x01)
// go out on viewer join, on request, on resolution change, and every 4 s.

const tileSize = 128

func tileGrid(w, h int) (cols, rows int) {
	cols = (w + tileSize - 1) / tileSize
	rows = (h + tileSize - 1) / tileSize
	return cols, rows
}

func tileRect(idx, cols, w, h int) image.Rectangle {
	tx, ty := idx%cols, idx/cols
	x0, y0 := tx*tileSize, ty*tileSize
	x1, y1 := x0+tileSize, y0+tileSize
	if x1 > w {
		x1 = w
	}
	if y1 > h {
		y1 = h
	}
	return image.Rect(x0, y0, x1, y1)
}

// tileChanged compares one tile region row by row (fast memcmp).
func tileChanged(cur, prev *image.RGBA, r image.Rectangle) bool {
	cs, ps := cur.Stride, prev.Stride
	for y := r.Min.Y; y < r.Max.Y; y++ {
		w := (r.Max.X - r.Min.X) * 4
		co := y*cs + r.Min.X*4
		po := y*ps + r.Min.X*4
		if !bytes.Equal(cur.Pix[co:co+w], prev.Pix[po:po+w]) {
			return true
		}
	}
	return false
}

func encodeJPEG(img image.Image, quality int) []byte {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: clampInt(quality, 10, 95)})
	return buf.Bytes()
}

func captureScreen() (*image.RGBA, error) {
	bounds := screenshot.GetDisplayBounds(0)
	return screenshot.CaptureRect(bounds)
}

// cursorPos returns the current mouse position in screen pixels.
func cursorPos() (int, int, bool) {
	var pt [2]int32
	r, _, _ := pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt[0])))
	if r == 0 {
		return 0, 0, false
	}
	return int(pt[0]), int(pt[1]), true
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
	if len(os.Args) > 1 && os.Args[1] == "--uninstall" {
		initLog()
		doUninstall()
		return
	}
	initLog()
	cfg = loadConfig()
	log("ID %s, server %s", cfg.DeviceID, cfg.Server)

	// auto-start with Windows by default from now on; the in-window checkbox opts out
	if !cfg.AutoRunSet {
		setAutoRun(true)
		cfg.AutoRunSet = true
		saveConfig()
	}

	// network loop runs in the background; the GUI owns the main thread
	safeGo("netloop", func() {
		// keep-alive: periodic HTTPS hits stop the free relay from sleeping
		safeGo("keepalive", func() {
			c := &http.Client{Timeout: 20 * time.Second}
			for {
				time.Sleep(10 * time.Minute)
				if r, err := c.Get(serverHTTPBase() + "/api/health"); err != nil {
					log("keepalive failed: %v", err)
				} else {
					r.Body.Close()
				}
			}
		})
		backoff := 2 * time.Second
		for {
			guiSetStatus("Connecting to relay…", false)
			if err := runSession(cfg); err != nil {
				log("session ended: %v", err)
				guiSetStatus("Connection lost — retrying…", false)
			}
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	})

	runGUI() // blocks until the window is closed
	log("window closed, exiting")
}

func runSession(cfg *Config) error {
	dialer := websocket.Dialer{HandshakeTimeout: 60 * time.Second} // free relay may be waking up
	ws, _, err := dialer.Dial(cfg.Server, nil)
	if err != nil {
		return err
	}
	defer ws.Close()
	wsConn = ws
	defer func() { wsConn = nil }()
	ws.SetReadLimit(16 << 20)
	fmt.Println("Connected to", cfg.Server)

	reg, _ := json.Marshal(map[string]any{
		"t": "register", "id": cfg.DeviceID, "name": cfg.Name,
		"pin": cfg.PIN, "platform": "windows", "claimCode": cfg.ClaimCode,
	})
	if err := wsWrite(websocket.TextMessage, reg); err != nil {
		return err
	}

	stop := make(chan struct{})
	defer close(stop)

	// screen sender — tiled dirty-region streaming (v3.0).
	// Captures at cfg.FPS, diffs 128x128 tiles vs the previous frame and
	// sends only changed tiles (0x03). Full keyframes (0x01) go out on
	// viewer join / request, on resolution change, and every 4 seconds.
	safeGo("sender", func() {
		interval := time.Second / time.Duration(cfg.FPS)
		maxQ := clampInt(cfg.Quality, 30, 90)
		q := maxQ
		if q > 70 {
			q = 70 // start balanced, adapt up from here
		}
		var prev *image.RGBA
		var winStart = time.Now()
		var winTotal time.Duration
		var winCount int
		lastKeyframe := time.Now().Add(-time.Hour) // force keyframe first
		needKeyframe.Store(true)
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			img, err := captureScreen()
			if err != nil || img == nil {
				select {
				case <-stop:
					return
				case <-time.After(interval):
				}
				continue
			}
			w, h := img.Bounds().Dx(), img.Bounds().Dy()
			if prev != nil && (prev.Bounds().Dx() != w || prev.Bounds().Dy() != h) {
				prev = nil // resolution changed — start over
			}
			cols, rows := tileGrid(w, h)
			nTiles := cols * rows
			key := needKeyframe.Swap(false) || prev == nil || time.Since(lastKeyframe) > 4*time.Second
			if key {
				data := encodeJPEG(img, q)
				pkt := make([]byte, 0, len(data)+1)
				pkt = append(pkt, 0x01)
				pkt = append(pkt, data...)
				if err := wsWrite(websocket.BinaryMessage, pkt); err != nil {
					return
				}
				lastKeyframe = time.Now()
			} else {
				var changed []int
				for idx := 0; idx < nTiles; idx++ {
					if tileChanged(img, prev, tileRect(idx, cols, w, h)) {
						changed = append(changed, idx)
					}
				}
				if len(changed) > nTiles*40/100 {
					// too much changed — a full keyframe is cheaper
					needKeyframe.Store(true)
				} else {
					for _, idx := range changed {
						tile := encodeJPEG(img.SubImage(tileRect(idx, cols, w, h)), q)
						pkt := make([]byte, 0, len(tile)+3)
						pkt = append(pkt, 0x03)
						var ib [2]byte
						binary.BigEndian.PutUint16(ib[:], uint16(idx))
						pkt = append(pkt, ib[:]...)
						pkt = append(pkt, tile...)
								if err := wsWrite(websocket.BinaryMessage, pkt); err != nil {
							return
						}
					}
				}
			}
			prev = img
			elapsed := time.Since(start)
			winTotal += elapsed
			winCount++
			// re-tune every 2 seconds based on average tick cost
			if time.Since(winStart) > 2*time.Second && winCount > 0 {
				avg := winTotal / time.Duration(winCount)
				if avg < 25*time.Millisecond && q < maxQ {
					q += 5
					if q > maxQ {
						q = maxQ
					}
				} else if avg > 60*time.Millisecond && q > 30 {
					q -= 10
					if q < 30 {
						q = 30
					}
				}
				winStart, winTotal, winCount = time.Now(), 0, 0
			}
			// if the tick took longer than the interval, skip sleeping (drop lag)
			if elapsed < interval {
				select {
				case <-stop:
					return
				case <-time.After(interval - elapsed):
				}
			}
		}
	})

	// cursor reporter — sends remote mouse position when it moves (v3.0)
	safeGo("cursor", func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		lx, ly := -1, -1
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			x, y, ok := cursorPos()
			if !ok || (x == lx && y == ly) {
				continue
			}
			lx, ly = x, y
			_ = wsWrite(websocket.TextMessage,
				[]byte(fmt.Sprintf(`{"t":"cursor","x":%d,"y":%d}`, x, y)))
		}
	})

	// heartbeat
	safeGo("heartbeat", func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = wsWrite(websocket.TextMessage, []byte(`{"t":"ping"}`))
			}
		}
	})

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if mt == websocket.BinaryMessage {
			if len(data) > 0 && data[0] == 0x02 {
				handleFileChunk(data[1:])
			} else if len(data) > 1 && data[0] == 0x04 {
				handleViewerFrame(data[1:]) // viewer camera JPEG
			} else if len(data) > 1 && data[0] == 0x05 {
				handleViewerPCM(data[1:]) // viewer mic PCM
			}
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg["t"] {
		case "registered":
			guiSetStatus("Online — waiting for viewers…", true)
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
		case "keyframe":
			needKeyframe.Store(true)
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
			needKeyframe.Store(true) // new viewer gets a full frame immediately
			if n, ok := msg["viewers"].(float64); ok {
				guiSetViewers(int(n))
			}
			guiSetStatus("Viewer connected — sharing screen", true)
		case "viewer_left":
			if n, ok := msg["viewers"].(float64); ok {
				guiSetViewers(int(n))
				if n == 0 {
					guiSetStatus("Online — waiting for viewers…", true)
				}
			}
		case "viewers":
			// full viewer identity list from the relay
			var list []viewerInfo
			if arr, ok := msg["viewers"].([]any); ok {
				for _, it := range arr {
					if m, ok := it.(map[string]any); ok {
						vid, _ := m["vid"].(string)
						name, _ := m["name"].(string)
						avatar, _ := m["avatar"].(string)
						if vid != "" {
							list = append(list, viewerInfo{vid: vid, name: name, avatarURL: avatar})
						}
					}
				}
			}
			setViewers(list)
			if len(list) == 0 {
				guiSetStatus("Online — waiting for viewers…", true)
			} else {
				guiSetStatus("Viewer connected — sharing screen", true)
			}
		case "call-start":
			onCallStart(msg)
		case "call-end":
			onCallEnd()
		case "av-state":
			onPeerAV(msg)
		case "av-ctrl":
			onAVCtrl(msg)
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
