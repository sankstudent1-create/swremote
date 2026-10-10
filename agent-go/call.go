package main

// v6.0: video call — the remote viewer streams camera (JPEG frames, binary
// 0x04) and mic (8kHz mono 16-bit PCM, binary 0x05) to this PC. This PC shows
// the video in a small window and plays the audio via waveOut.
// This PC's own camera/mic capture is Phase 4 (see docs/ROADMAP-V3.md).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"sync"
	"unsafe"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows"
)

var (
	callMu       sync.Mutex
	callActive   bool
	callPeerName string
	callPeerCam  = true
	callPeerMic  = true
	playMuted    bool // this PC muted the peer's audio/video

	videoHWND uintptr
	videoImg  image.Image
	videoMu   sync.Mutex

	// PC-side capture state (v7.0) — both on by default during a call
	pcCamOn bool
	pcMicOn bool

	wsWriteMu sync.Mutex
	wsConn    *websocket.Conn
)

func sendWSJSON(v any) {
	b, _ := json.Marshal(v)
	wsWriteMu.Lock()
	defer wsWriteMu.Unlock()
	if wsConn != nil {
		_ = wsConn.WriteMessage(websocket.TextMessage, b)
	}
}

// ---------- incoming signaling (called from the netloop) ----------

func onCallStart(msg map[string]any) {
	name, _ := msg["name"].(string)
	if name == "" {
		name = "Someone"
	}
	callMu.Lock()
	if callActive {
		callMu.Unlock()
		sendWSJSON(map[string]any{"t": "call-decline"})
		return
	}
	callMu.Unlock()
	// v7.0: auto-connect — no Accept/Decline prompt on the PC
	callMu.Lock()
	callActive = true
	callPeerName = name
	callPeerCam, callPeerMic = true, true
	playMuted = false
	// PC camera+mic are ON by default when a call connects
	pcCamOn, pcMicOn = true, true
	callMu.Unlock()
	sendWSJSON(map[string]any{"t": "call-accept"})
	sendWSJSON(map[string]any{"t": "av-state", "side": "agent", "cam": true, "mic": true})
	guiSetStatus("On a call with "+name, true)
	openVideoWindow()
	startPCCapture()
	notify("SWRemote call", "Video call with "+name)
}

func onCallEnd() {
	callMu.Lock()
	was := callActive
	callActive = false
	callMu.Unlock()
	if was {
		closeVideoWindow()
		stopAudio()
		stopPCCapture()
		guiSetStatus("Online — waiting for viewers…", true)
		notify("SWRemote call", "Call ended")
	}
}

// av-state: the peer muted/unmuted their cam/mic
func onPeerAV(msg map[string]any) {
	callMu.Lock()
	if v, ok := msg["cam"].(bool); ok {
		callPeerCam = v
	}
	if v, ok := msg["mic"].(bool); ok {
		callPeerMic = v
	}
	callMu.Unlock()
	if videoHWND != 0 {
		pInvalidateRect.Call(videoHWND, 0, 1)
	}
}

// av-ctrl: a logged-in viewer controls this PC's camera/mic capture.
// Both are ON by default; the viewer can turn either off (or back on).
func onAVCtrl(msg map[string]any) {
	callMu.Lock()
	changed := false
	if v, ok := msg["cam"].(bool); ok {
		pcCamOn = v
		changed = true
	}
	if v, ok := msg["mic"].(bool); ok {
		pcMicOn = v
		changed = true
	}
	cam, mic := pcCamOn, pcMicOn
	callMu.Unlock()
	if changed {
		sendWSJSON(map[string]any{"t": "av-state", "side": "agent", "cam": cam, "mic": mic})
		guiSetStatus(fmt.Sprintf("PC camera %s, mic %s (by viewer)", onOff(cam), onOff(mic)), true)
	}
	// legacy: mic-only form mutes this PC's playback of the peer
	if _, hasCam := msg["cam"]; !hasCam {
		if v, ok := msg["mic"].(bool); ok {
			callMu.Lock()
			playMuted = !v
			callMu.Unlock()
			if !v {
				stopAudio()
			}
		}
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ---------- incoming media from the peer ----------

func handleViewerFrame(jpeg []byte) {
	callMu.Lock()
	active, peerCam, muted := callActive, callPeerCam, playMuted
	callMu.Unlock()
	if !active || !peerCam || muted {
		return
	}
	img, _, err := image.Decode(bytes.NewReader(jpeg))
	if err != nil {
		return
	}
	videoMu.Lock()
	videoImg = img
	videoMu.Unlock()
	if videoHWND != 0 {
		pInvalidateRect.Call(videoHWND, 0, 1)
	}
}

func handleViewerPCM(pcm []byte) {
	callMu.Lock()
	active, peerMic, muted := callActive, callPeerMic, playMuted
	callMu.Unlock()
	if !active || !peerMic || muted || len(pcm) < 2 {
		return
	}
	playPCM(pcm)
}

// ---------- video window ----------

var videoWndProcCb = windows.NewCallback(videoWndProc)

func openVideoWindow() {
	if videoHWND != 0 {
		return
	}
	go func() {
		cls := u16("SWRemoteVideo")
		cursor, _, _ := pLoadCursorW.Call(0, IDC_ARROW)
		var wc [72]byte // WNDCLASSW
		putU32(wc[0:], 0x0003) // style: CS_HREDRAW|CS_VREDRAW
		*(*uintptr)(unsafe.Pointer(&wc[8])) = videoWndProcCb
		*(*uintptr)(unsafe.Pointer(&wc[40])) = cursor
		*(*uintptr)(unsafe.Pointer(&wc[48])) = gWhiteBr
		*(*uintptr)(unsafe.Pointer(&wc[64])) = uintptr(unsafe.Pointer(cls))
		if r, _, _ := pRegisterClassW.Call(uintptr(unsafe.Pointer(&wc[0]))); r == 0 {
			return
		}
		// PiP: always-on-top, bottom-right corner of the primary screen
		sw, _, _ := pGetSystemMetrics.Call(0) // SM_CXSCREEN
		sh, _, _ := pGetSystemMetrics.Call(1) // SM_CYSCREEN
		ww, wh := 256, 208
		x, y := int(sw)-ww-16, int(sh)-wh-64
		hwnd, _, _ := pCreateWindowExW.Call(
			0x00000008, // WS_EX_TOPMOST
			uintptr(unsafe.Pointer(cls)),
			uintptr(unsafe.Pointer(u16("Video call — SWRemote"))),
			0x00CF0000, uintptr(x), uintptr(y), uintptr(ww), uintptr(wh), 0, 0, 0, 0)
		videoHWND = hwnd
		pShowWindow.Call(hwnd, 1)
		pUpdateWindow.Call(hwnd)
		var m [32]byte
		for {
			r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m[0])), 0, 0, 0)
			if r == 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m[0])))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m[0])))
		}
		videoHWND = 0
	}()
}

func closeVideoWindow() {
	if videoHWND != 0 {
		pPostMessageW.Call(videoHWND, 0x0010 /*WM_CLOSE*/, 0, 0)
	}
	videoMu.Lock()
	videoImg = nil
	videoMu.Unlock()
}

func videoWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch uint32(msg) {
	case 0x0002: // WM_DESTROY
		pPostQuitMessage.Call(0)
		return 0
	case 0x000F: // WM_PAINT
		var ps paintStruct
		hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		blk, _, _ := pCreateSolidBrush.Call(0)
		var rc = [4]int32{0, 0, 240, 192}
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), blk)
		pDeleteObject.Call(blk)
		videoMu.Lock()
		img := videoImg
		videoMu.Unlock()
		callMu.Lock()
		name, peerCam := callPeerName, callPeerCam
		callMu.Unlock()
		if img != nil && peerCam {
			drawVideoFrame(hdc, img, 240, 192)
		} else {
			pSetBkMode.Call(hdc, TRANSPARENT)
			pSetTextColor.Call(hdc, colorRef(0xaa, 0xaa, 0xaa))
			oldF, _, _ := pSelectObject.Call(hdc, gFonts["norm"])
			txt := name
			if txt == "" {
				txt = "Video call"
			}
			if !peerCam {
				txt += " — camera off"
			}
			var trc = [4]int32{0, 80, 240, 120}
			pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(txt))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&trc)), 1 /*DT_CENTER*/)
			pSelectObject.Call(hdc, oldF)
		}
		pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// drawVideoFrame renders img scaled-to-fit into a w×h area via a DIB.
func drawVideoFrame(hdc uintptr, img image.Image, w, h int) {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw == 0 || sh == 0 {
		return
	}
	scale := float64(w) / float64(sw)
	if float64(h)/float64(sh) < scale {
		scale = float64(h) / float64(sh)
	}
	dw, dh := int(float64(sw)*scale), int(float64(sh)*scale)
	if dw < 1 || dh < 1 {
		return
	}
	dx, dy := (w-dw)/2, (h-dh)/2
	stride := (dw*3 + 3) &^ 3
	px := make([]byte, stride*dh)
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + int(float64(y)/scale)
		if sy >= b.Max.Y {
			sy = b.Max.Y - 1
		}
		for x := 0; x < dw; x++ {
			sx := b.Min.X + int(float64(x)/scale)
			if sx >= b.Max.X {
				sx = b.Max.X - 1
			}
			r, g, bl, _ := img.At(sx, sy).RGBA()
			o := y*stride + x*3
			px[o], px[o+1], px[o+2] = byte(bl>>8), byte(g>>8), byte(r>>8)
		}
	}
	var bi [40]byte
	putU32(bi[0:], 40)
	putU32(bi[4:], uint32(dw))
	putU32(bi[8:], uint32(-dh))
	putU16(bi[12:], 1)
	putU16(bi[14:], 24)
	bmp, _, _ := pCreateDIBitmap.Call(hdc,
		uintptr(unsafe.Pointer(&bi[0])), 4,
		uintptr(unsafe.Pointer(&px[0])), uintptr(unsafe.Pointer(&bi[0])), 0)
	if bmp == 0 {
		return
	}
	memDC, _, _ := pCreateCompatibleDC.Call(hdc)
	pSelectObject.Call(memDC, bmp)
	pBitBlt.Call(hdc, uintptr(dx), uintptr(dy), uintptr(dw), uintptr(dh), memDC, 0, 0, 0x00CC0020)
	pDeleteDC.Call(memDC)
	pDeleteObject.Call(bmp)
}
