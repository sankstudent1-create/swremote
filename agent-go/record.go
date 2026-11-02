package main

// record.go — agent-side screen recording (v8.9.10).
//
// The viewer starts a recording remotely from the console. The agent
// captures its own screen at 10fps and writes an MJPEG AVI — no external
// encoder, no ffmpeg dependency, playable in VLC / Media Player /
// browsers. While recording, the viewer can compose the picture like a
// streamer: their own camera as a picture-in-picture (size, corner,
// border style), a lower-third banner (ad/sponsor-style text strip),
// cursor size/style, a 2x follow-cursor zoom, and pen / pencil / text
// annotations drawn from the console. Everything is composited into
// the recorded frames, so the AVI is the finished video — no editor
// needed. Audio is NOT in the AVI (video-only v1); the console's own
// Rec button still captures audio on the viewer side.
//
// Files land in %USERPROFILE%\Videos\SWRemote\ and the viewer pulls
// one down with {t:"record-download"} (file_meta + BIN_FILE chunks,
// the same framing the console already downloads with).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------- AVI

type aviIdx struct {
	flags  uint32
	offset uint32
	size   uint32
}

type aviWriter struct {
	f           *os.File
	w, h, fps   int
	frames      int
	index       []aviIdx
	avihPos     int64 // totalFrames field in avih
	strhPos     int64 // length field in strh
	moviSizePos int64
	moviDataPos int64
}

func putStr(b *bytes.Buffer, s string) { b.WriteString(s) }
func putU32w(b *bytes.Buffer, v uint32) {
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], v)
	b.Write(t[:])
}
func putU16w(b *bytes.Buffer, v uint16) {
	var t [2]byte
	binary.LittleEndian.PutUint16(t[:], v)
	b.Write(t[:])
}

func newAVIWriter(path string, w, h, fps int) (*aviWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	a := &aviWriter{f: f, w: w, h: h, fps: fps}
	var hdrl bytes.Buffer
	// avih
	putStr(&hdrl, "avih")
	putU32w(&hdrl, 56)
	putU32w(&hdrl, uint32(1000000/fps)) // microSecPerFrame
	putU32w(&hdrl, uint32(w*h*3*fps/8)) // maxBytesPerSec (estimate)
	putU32w(&hdrl, 0)                   // padding
	putU32w(&hdrl, 0x10)                // AVIF_HASINDEX
	a.avihPos = int64(hdrl.Len())
	putU32w(&hdrl, 0) // totalFrames (patched at close)
	putU32w(&hdrl, 0) // initialFrames
	putU32w(&hdrl, 1) // streams
	putU32w(&hdrl, uint32(w*h*3))
	putU32w(&hdrl, uint32(w))
	putU32w(&hdrl, uint32(h))
	for i := 0; i < 4; i++ {
		putU32w(&hdrl, 0)
	}
	// strl
	var strl bytes.Buffer
	putStr(&strl, "strh")
	putU32w(&strl, 56)
	putStr(&strl, "vids")
	putStr(&strl, "MJPG")
	putU32w(&strl, 0) // flags
	putU32w(&strl, 0) // priority + language
	putU32w(&strl, 0) // initialFrames
	putU32w(&strl, 1) // scale
	putU32w(&strl, uint32(fps))
	putU32w(&strl, 0) // start
	a.strhPos = int64(strl.Len())
	putU32w(&strl, 0) // length (patched)
	putU32w(&strl, uint32(w*h*3))
	putU32w(&strl, 0xFFFFFFFF) // quality
	putU32w(&strl, 0)          // sampleSize
	putU32w(&strl, 0)          // frame left
	putU32w(&strl, 0)          // frame top
	putU32w(&strl, uint32(w))  // frame right
	putU32w(&strl, uint32(h))  // frame bottom
	putStr(&strl, "strf")
	putU32w(&strl, 40)
	putU32w(&strl, 40)
	putU32w(&strl, uint32(w))
	putU32w(&strl, uint32(h))
	putU16w(&strl, 1)
	putU16w(&strl, 24)
	putStr(&strl, "MJPG")
	putU32w(&strl, uint32(w*h*3))
	for i := 0; i < 4; i++ {
		putU32w(&strl, 0)
	}
	putStr(&hdrl, "LIST")
	putU32w(&hdrl, uint32(4+strl.Len()))
	putStr(&hdrl, "strl")
	hdrl.Write(strl.Bytes())

	var riff bytes.Buffer
	putStr(&riff, "RIFF")
	putU32w(&riff, 0) // patched at close
	putStr(&riff, "AVI ")
	putStr(&riff, "LIST")
	putU32w(&riff, uint32(4+hdrl.Len()))
	putStr(&riff, "hdrl")
	hdrl.WriteTo(&riff)
	if _, err := f.Write(riff.Bytes()); err != nil {
		f.Close()
		return nil, err
	}
	// Patch positions, verified against the layout above:
	// avih data starts at 32, totalFrames is its 5th field -> 48.
	// strh data starts at 108, length is its 9th field -> 140.
	a.avihPos = 48
	a.strhPos = 140
	// movi list
	pos, _ := f.Seek(0, 1)
	a.moviSizePos = pos + 4
	var movi bytes.Buffer
	putStr(&movi, "LIST")
	putU32w(&movi, 0) // patched at close
	putStr(&movi, "movi")
	if _, err := f.Write(movi.Bytes()); err != nil {
		f.Close()
		return nil, err
	}
	a.moviDataPos, _ = f.Seek(0, 1)
	return a, nil
}

func (a *aviWriter) writeFrame(jpg []byte) error {
	pos, _ := a.f.Seek(0, 1)
	rel := uint32(pos - a.moviDataPos)
	var hdr bytes.Buffer
	putStr(&hdr, "00dc")
	putU32w(&hdr, uint32(len(jpg)))
	if _, err := a.f.Write(hdr.Bytes()); err != nil {
		return err
	}
	if _, err := a.f.Write(jpg); err != nil {
		return err
	}
	if len(jpg)%2 == 1 {
		if _, err := a.f.Write([]byte{0}); err != nil {
			return err
		}
	}
	a.index = append(a.index, aviIdx{flags: 0x10, offset: rel, size: uint32(len(jpg))})
	a.frames++
	return nil
}

func (a *aviWriter) close() error {
	idxPos, _ := a.f.Seek(0, 1)
	var idx bytes.Buffer
	putStr(&idx, "idx1")
	putU32w(&idx, uint32(16*len(a.index)))
	for _, e := range a.index {
		putStr(&idx, "00dc")
		putU32w(&idx, e.flags)
		putU32w(&idx, e.offset)
		putU32w(&idx, e.size)
	}
	if _, err := a.f.Write(idx.Bytes()); err != nil {
		return err
	}
	end, _ := a.f.Seek(0, 1)
	// patch RIFF size, movi size, frame counts
	patch := func(at int64, v uint32) {
		var t [4]byte
		binary.LittleEndian.PutUint32(t[:], v)
		a.f.WriteAt(t[:], at)
	}
	patch(4, uint32(end-8))
	patch(a.moviSizePos, uint32(idxPos-a.moviDataPos+4)) // +4 for "movi"
	patch(a.avihPos, uint32(a.frames))
	patch(a.strhPos, uint32(a.frames))
	return a.f.Close()
}

// ---------------------------------------------------------------- helpers

func parseHexColor(s string) color.RGBA {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return color.RGBA{0x7c, 0x3a, 0xed, 0xff} // brand violet default
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.RGBA{0x7c, 0x3a, 0xed, 0xff}
	}
	return color.RGBA{byte(v >> 16), byte(v >> 8), byte(v), 0xff}
}

func setPx(img *image.RGBA, x, y int, c color.RGBA) {
	if (image.Point{x, y}).In(img.Bounds()) {
		img.SetRGBA(x, y, c)
	}
}

func blendPx(img *image.RGBA, x, y int, c color.RGBA, alpha float64) {
	if !(image.Point{x, y}).In(img.Bounds()) {
		return
	}
	o := img.RGBAAt(x, y)
	img.SetRGBA(x, y, color.RGBA{
		uint8(float64(c.R)*alpha + float64(o.R)*(1-alpha)),
		uint8(float64(c.G)*alpha + float64(o.G)*(1-alpha)),
		uint8(float64(c.B)*alpha + float64(o.B)*(1-alpha)), 0xff})
}

func drawDisc(img *image.RGBA, cx, cy, r int, c color.RGBA, alpha float64) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				if alpha >= 1 {
					setPx(img, x, y, c)
				} else {
					blendPx(img, x, y, c, alpha)
				}
			}
		}
	}
}

func drawThickLine(img *image.RGBA, x0, y0, x1, y1, r int, c color.RGBA, alpha float64) {
	dx, dy := x1-x0, y1-y0
	steps := dx
	if dy < 0 {
		steps = -dy
	}
	if dx < 0 {
		steps = -dx
	}
	if dy > steps {
		steps = dy
	}
	if steps < 1 {
		steps = 1
	}
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		drawDisc(img, x0+int(float64(dx)*t), y0+int(float64(dy)*t), r, c, alpha)
	}
}

func fillRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	r := image.Rect(x, y, x+w, y+h).Intersect(img.Bounds())
	if r.Empty() {
		return
	}
	draw.Draw(img, r, &image.Uniform{c}, image.Point{}, draw.Src)
}

func drawRing(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for a := 0; a < 360; a += 2 {
		rad := float64(a) * math.Pi / 180
		x := cx + int(float64(r)*math.Cos(rad))
		y := cy + int(float64(r)*math.Sin(rad))
		drawDisc(img, x, y, 2, c, 1)
	}
	drawDisc(img, cx, cy, 3, c, 1)
}

// fillTriangle rasterizes a filled triangle (barycentric sign test).
func fillTriangle(img *image.RGBA, x0, y0, x1, y1, x2, y2 int, c color.RGBA) {
	minX, maxX := x0, x0
	minY, maxY := y0, y0
	for _, p := range [][2]int{{x1, y1}, {x2, y2}} {
		if p[0] < minX {
			minX = p[0]
		}
		if p[0] > maxX {
			maxX = p[0]
		}
		if p[1] < minY {
			minY = p[1]
		}
		if p[1] > maxY {
			maxY = p[1]
		}
	}
	sign := func(ax, ay, bx, by, px, py int) int {
		return (px-bx)*(ay-by) - (ax-bx)*(py-by)
	}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d1 := sign(x, y, x0, y0, x1, y1)
			d2 := sign(x, y, x1, y1, x2, y2)
			d3 := sign(x, y, x2, y2, x0, y0)
			neg := (d1 < 0) || (d2 < 0) || (d3 < 0)
			pos := (d1 > 0) || (d2 > 0) || (d3 > 0)
			if !(neg && pos) {
				setPx(img, x, y, c)
			}
		}
	}
}

// renderTextPill draws text on a solid pill with GDI (Segoe UI, the same
// font the agent window uses). Small bitmaps only — a banner strip or a
// caption chip — so the GDI round-trip per frame stays cheap. Pills are
// cached by content.
var pillCacheMu sync.Mutex
var pillCache = map[string]*image.RGBA{}

var pCreateDIBSection = windows.NewLazySystemDLL("gdi32.dll").NewProc("CreateDIBSection")

func renderTextPill(text string, w, h int, bg, fg color.RGBA, pts int) *image.RGBA {
	key := fmt.Sprintf("%dx%d|%02x%02x%02x|%02x%02x%02x|%d|%s", w, h, bg.R, bg.G, bg.B, fg.R, fg.G, fg.B, pts, text)
	pillCacheMu.Lock()
	if p, ok := pillCache[key]; ok {
		pillCacheMu.Unlock()
		return p
	}
	pillCacheMu.Unlock()
	pill := image.NewRGBA(image.Rect(0, 0, w, h))
	fillRect(pill, 0, 0, w, h, bg)
	hdc, _, _ := pGetDC.Call(0)
	if hdc == 0 {
		return pill
	}
	defer pReleaseDC.Call(0, hdc)
	var bi [40]byte
	binary.LittleEndian.PutUint32(bi[0:], 40)
	binary.LittleEndian.PutUint32(bi[4:], uint32(w))
	binary.LittleEndian.PutUint32(bi[8:], uint32(-h)) // top-down
	binary.LittleEndian.PutUint16(bi[12:], 1)
	binary.LittleEndian.PutUint16(bi[14:], 32)
	var bits uintptr
	hbm, _, _ := pCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bi[0])), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == 0 {
		return pill
	}
	defer pDeleteObject.Call(hbm)
	// copy pill pixels into the DIB (BGRA)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := pill.RGBAAt(x, y)
			o := (y*w + x) * 4
			*(*byte)(unsafe.Pointer(bits + uintptr(o))) = c.B
			*(*byte)(unsafe.Pointer(bits + uintptr(o) + 1)) = c.G
			*(*byte)(unsafe.Pointer(bits + uintptr(o) + 2)) = c.R
			*(*byte)(unsafe.Pointer(bits + uintptr(o) + 3)) = 0
		}
	}
	memDC, _, _ := pCreateCompatibleDC.Call(hdc)
	if memDC == 0 {
		return pill
	}
	defer pDeleteDC.Call(memDC)
	oldBmp, _, _ := pSelectObject.Call(memDC, hbm)
	font := mkFont("Segoe UI", pts, true)
	oldF, _, _ := pSelectObject.Call(memDC, font)
	pSetTextColor.Call(memDC, colorRef(int(fg.R), int(fg.G), int(fg.B)))
	pSetBkMode.Call(memDC, TRANSPARENT)
	rc := [4]int32{16, 0, int32(w - 8), int32(h)}
	pDrawTextW.Call(memDC, uintptr(unsafe.Pointer(u16(text))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&rc)), 0x24 /*DT_LEFT|DT_VCENTER|DT_SINGLELINE*/)
	pSelectObject.Call(memDC, oldF)
	pDeleteObject.Call(font)
	pSelectObject.Call(memDC, oldBmp)
	// read pixels back
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			b := *(*byte)(unsafe.Pointer(bits + uintptr(o)))
			g := *(*byte)(unsafe.Pointer(bits + uintptr(o) + 1))
			r := *(*byte)(unsafe.Pointer(bits + uintptr(o) + 2))
			pill.SetRGBA(x, y, color.RGBA{r, g, b, 0xff})
		}
	}
	pillCacheMu.Lock()
	if len(pillCache) > 64 {
		pillCache = map[string]*image.RGBA{}
	}
	pillCache[key] = pill
	pillCacheMu.Unlock()
	return pill
}

// ---------------------------------------------------------------- recorder

type recSettings struct {
	pipOn                     bool
	pipSize, pipPos, pipBorder, pipColor string
	bannerOn                  bool
	bannerText                string
	cursorSize, cursorStyle   string
	cursorZoom                bool
}

type recStroke struct {
	kind        string // pen | pencil | text
	color       string
	size        int
	pts         [][2]float64
	text        string
	x, y        float64
}

type recorder struct {
	file     *os.File
	avi      *aviWriter
	settings recSettings
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	start    time.Time
	frames   int
	path     string
}

var (
	recMu           sync.Mutex
	recActive       bool
	recCur          *recorder
	strokesMu       sync.Mutex
	recStrokes      []recStroke
	viewerImgMu     sync.Mutex
	latestViewerImg image.Image
)

func recordDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home, _ = os.UserCacheDir()
	}
	p := filepath.Join(home, "Videos", "SWRemote")
	_ = os.MkdirAll(p, 0755)
	return p
}

func getStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}
func getBool(m map[string]any, k string, def bool) bool {
	b, ok := m[k].(bool)
	if !ok {
		return def
	}
	return b
}
func parseSettings(msg map[string]any) recSettings {
	var s recSettings
	if p, ok := msg["pip"].(map[string]any); ok {
		s.pipOn = getBool(p, "on", false)
		s.pipSize = getStr(p, "size")
		s.pipPos = getStr(p, "pos")
		s.pipBorder = getStr(p, "border")
		s.pipColor = getStr(p, "color")
	}
	if b, ok := msg["banner"].(map[string]any); ok {
		s.bannerOn = getBool(b, "on", false)
		s.bannerText = getStr(b, "text")
	}
	if c, ok := msg["cursor"].(map[string]any); ok {
		s.cursorSize = getStr(c, "size")
		s.cursorStyle = getStr(c, "style")
		s.cursorZoom = getBool(c, "zoom", false)
	}
	if s.pipSize == "" {
		s.pipSize = "m"
	}
	if s.pipPos == "" {
		s.pipPos = "br"
	}
	if s.pipBorder == "" {
		s.pipBorder = "solid"
	}
	if s.cursorSize == "" {
		s.cursorSize = "m"
	}
	if s.cursorStyle == "" {
		s.cursorStyle = "ring"
	}
	return s
}

func startRecording(s recSettings) error {
	recMu.Lock()
	if recActive {
		recMu.Unlock()
		return fmt.Errorf("already recording")
	}
	first, err := captureScreen()
	if err != nil {
		recMu.Unlock()
		return err
	}
	w, h := first.Bounds().Dx(), first.Bounds().Dy()
	name := "swremote-" + time.Now().Format("20060102-150405") + ".avi"
	path := filepath.Join(recordDir(), name)
	avi, err := newAVIWriter(path, w, h, 10)
	if err != nil {
		recMu.Unlock()
		return err
	}
	r := &recorder{avi: avi, settings: s, stop: make(chan struct{}), done: make(chan struct{}), start: time.Now(), path: path}
	recActive = true
	recCur = r
	strokesMu.Lock()
	recStrokes = nil
	strokesMu.Unlock()
	recMu.Unlock()
	guiSetRec("● REC 00:00", true)
	go r.loop()
	log("record: started %s (%dx%d)", name, w, h)
	return nil
}

func (r *recorder) loop() {
	defer func() {
		if rec := recover(); rec != nil {
			log("record: panic: %v", rec)
		}
		_ = r.avi.close()
		recMu.Lock()
		recActive = false
		recCur = nil
		recMu.Unlock()
		guiSetRec("", false)
		el := time.Since(r.start)
		sendWSJSON(map[string]any{"t": "record-done", "file": filepath.Base(r.path),
			"bytes": fileSize(r.path), "seconds": int(el.Seconds()), "frames": r.frames})
		log("record: stopped, %d frames, %s", r.frames, r.path)
		close(r.done)
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-tick.C:
			frame, err := captureScreen()
			if err != nil {
				continue
			}
			r.compose(frame)
			jpg := encodeJPEG(frame, 70)
			if err := r.avi.writeFrame(jpg); err != nil {
				log("record: write failed: %v", err)
				return
			}
			r.frames++
			if r.frames%10 == 0 {
				el := time.Since(r.start)
				guiSetRec(fmt.Sprintf("● REC %02d:%02d", int(el.Minutes()), int(el.Seconds())%60), true)
			}
		}
	}
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func stopRecording() {
	recMu.Lock()
	r := recCur
	recMu.Unlock()
	if r != nil {
		r.stopOnce.Do(func() { close(r.stop) })
	}
}

func (r *recorder) compose(frame *image.RGBA) {
	W, H := frame.Bounds().Dx(), frame.Bounds().Dy()
	s := r.settings
	cx, cy, hasCursor := cursorPos()
	// 1. follow-cursor zoom (2x crop around the cursor, scaled back up)
	if s.cursorZoom && hasCursor {
		rw, rh := W/2, H/2
		x0 := cx - rw/2
		y0 := cy - rh/2
		if x0 < 0 {
			x0 = 0
		}
		if y0 < 0 {
			y0 = 0
		}
		if x0+rw > W {
			x0 = W - rw
		}
		if y0+rh > H {
			y0 = H - rh
		}
		zoomed := image.NewRGBA(image.Rect(0, 0, W, H))
		xdraw.ApproxBiLinear.Scale(zoomed, zoomed.Bounds(), frame,
			image.Rect(x0, y0, x0+rw, y0+rh), xdraw.Over, nil)
		draw.Draw(frame, frame.Bounds(), zoomed, image.Point{}, draw.Src)
	}
	// 2. cursor (drawn on top — screen capture alone has no cursor)
	if hasCursor && s.cursorStyle != "none" {
		sz := 24
		switch s.cursorSize {
		case "s":
			sz = 16
		case "l":
			sz = 36
		}
		white := color.RGBA{0xff, 0xff, 0xff, 0xff}
		black := color.RGBA{0x00, 0x00, 0x00, 0xff}
		if s.cursorStyle == "arrow" {
			// classic up-left arrow: black outline triangle + white inset
			fillTriangle(frame, cx-2, cy-2, cx-2, cy+sz+2, cx+sz+2, cy+sz*2/3, black)
			fillTriangle(frame, cx, cy, cx, cy+sz, cx+sz, cy+sz*2/3, white)
		} else {
			drawRing(frame, cx, cy, sz/2, white)
			drawRing(frame, cx, cy, sz/2+2, black)
		}
	}
	// 3. annotations (pen / pencil / text, in normalized coords)
	strokesMu.Lock()
	strokes := append([]recStroke(nil), recStrokes...)
	strokesMu.Unlock()
	for _, st := range strokes {
		c := parseHexColor(st.color)
		switch st.kind {
		case "pen", "pencil":
			alpha := 1.0
			rad := st.size / 2
			if rad < 1 {
				rad = 1
			}
			if st.kind == "pencil" {
				alpha = 0.6
				rad = 1
			}
			for i := 1; i < len(st.pts); i++ {
				x0 := int(st.pts[i-1][0] * float64(W))
				y0 := int(st.pts[i-1][1] * float64(H))
				x1 := int(st.pts[i][0] * float64(W))
				y1 := int(st.pts[i][1] * float64(H))
				drawThickLine(frame, x0, y0, x1, y1, rad, c, alpha)
			}
		case "text":
			if st.text == "" {
				continue
			}
			pts := 18
			if st.size > 0 {
				pts = st.size
			}
			pw := 28 + int(float64(len([]rune(st.text)))*float64(pts)*0.55)
			ph := pts + 22
			pill := renderTextPill(st.text, pw, ph, c, color.RGBA{0xff, 0xff, 0xff, 0xff}, pts)
			px := int(st.x * float64(W))
			py := int(st.y * float64(H))
			draw.Draw(frame, image.Rect(px, py, px+pw, py+ph), pill, image.Point{}, draw.Src)
		}
	}
	// 4. viewer camera PiP (streamer frame)
	if s.pipOn {
		viewerImgMu.Lock()
		vimg := latestViewerImg
		viewerImgMu.Unlock()
		if vimg != nil {
			frac := 0.20
			switch s.pipSize {
			case "s":
				frac = 0.15
			case "l":
				frac = 0.25
			}
			pw := int(float64(W) * frac)
			ph := pw * 3 / 4
			m := 16
			px, py := m, m
			switch s.pipPos {
			case "tr":
				px, py = W-m-pw, m
			case "bl":
				px, py = m, H-m-ph
			case "br":
				px, py = W-m-pw, H-m-ph
			}
			border := parseHexColor(s.pipColor)
			white := color.RGBA{0xff, 0xff, 0xff, 0xff}
			inset := 0
			switch s.pipBorder {
			case "solid":
				inset = 4
				fillRect(frame, px, py, pw, ph, border)
			case "glow":
				inset = 10
				fillRect(frame, px, py, pw, ph, border)
				fillRect(frame, px+6, py+6, pw-12, ph-12, white)
			}
			dst := image.NewRGBA(image.Rect(0, 0, pw-2*inset, ph-2*inset))
			xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), vimg, vimg.Bounds(), xdraw.Over, nil)
			draw.Draw(frame, image.Rect(px+inset, py+inset, px+pw-inset, py+ph-inset), dst, image.Point{}, draw.Src)
		}
	}
	// 5. lower-third banner (ad / sponsor-style strip)
	if s.bannerOn && strings.TrimSpace(s.bannerText) != "" {
		bw := W * 45 / 100
		bh := H / 12
		if bh < 48 {
			bh = 48
		}
		pill := renderTextPill(s.bannerText, bw, bh,
			color.RGBA{0x11, 0x18, 0x27, 0xff}, color.RGBA{0xff, 0xff, 0xff, 0xff}, 18)
		bx, by := 16, H-16-bh
		draw.Draw(frame, image.Rect(bx, by, bx+bw, by+bh), pill, image.Point{}, draw.Src)
	}
}

// downloadRecording streams a finished AVI to the viewer with the same
// file_meta + BIN_FILE framing the console already downloads with.
func downloadRecording(name string) {
	if name == "" || name != filepath.Base(name) {
		return
	}
	path := filepath.Join(recordDir(), name)
	f, err := os.Open(path)
	if err != nil {
		sendWSJSON(map[string]any{"t": "record-download-result", "ok": false, "error": "not found"})
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	id := name
	sendWSJSON(map[string]any{"t": "file_meta", "id": id, "name": name, "size": st.Size()})
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			pkt := make([]byte, 1+len(id)+n)
			pkt[0] = byte(len(id))
			copy(pkt[1:], id)
			copy(pkt[1+len(id):], buf[:n])
			sendBinary(0x02, pkt)
		}
		if err != nil {
			break
		}
	}
}
