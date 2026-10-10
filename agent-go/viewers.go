package main

// v6.0: viewer identity — who is watching this PC (name + avatar from their account)

import (
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/image/draw"
)

type viewerInfo struct {
	vid       string
	name      string
	avatarURL string
	avatarBMP uintptr // cached GDI bitmap handle (0 = none)
}

var (
	viewersMu sync.Mutex
	viewers   []viewerInfo
)

func avatarDir() string {
	d, _ := os.UserCacheDir()
	if d == "" {
		d, _ = os.UserHomeDir()
	}
	p := filepath.Join(d, "SWRemote", "avatars")
	_ = os.MkdirAll(p, 0755)
	return p
}

// setViewers replaces the viewer list (from relay {"t":"viewers"}), downloads
// missing avatars in the background, then repaints the window.
func setViewers(list []viewerInfo) {
	viewersMu.Lock()
	// keep existing bitmap handles for vids we already have
	old := map[string]uintptr{}
	for _, v := range viewers {
		if v.avatarBMP != 0 {
			old[v.vid] = v.avatarBMP
		}
	}
	viewers = list
	for i := range viewers {
		if h, ok := old[viewers[i].vid]; ok {
			viewers[i].avatarBMP = h
		}
	}
	n := len(viewers)
	viewersMu.Unlock()

	guiSetViewers(n)
	for _, v := range list {
		if v.avatarURL != "" && v.avatarBMP == 0 {
			go fetchAvatar(v.vid, v.avatarURL)
		}
	}
	if gHWND != 0 {
		pInvalidateRect.Call(gHWND, 0, 1)
	}
}

func fetchAvatar(vid, url string) {
	path := filepath.Join(avatarDir(), vid+".img")
	if _, err := os.Stat(path); err != nil {
		c := &http.Client{Timeout: 15 * time.Second}
		r, err := c.Get(url)
		if err != nil {
			return
		}
		defer r.Body.Close()
		f, err := os.Create(path)
		if err != nil {
			return
		}
		_, _ = io.Copy(f, io.LimitReader(r.Body, 2<<20))
		f.Close()
	}
	h := avatarToBitmap(path, 36)
	if h == 0 {
		return
	}
	viewersMu.Lock()
	for i := range viewers {
		if viewers[i].vid == vid {
			viewers[i].avatarBMP = h
			break
		}
	}
	viewersMu.Unlock()
	if gHWND != 0 {
		pInvalidateRect.Call(gHWND, 0, 1)
	}
}

// avatarToBitmap decodes an image file and returns a GDI bitmap handle (caller draws it).
func avatarToBitmap(path string, size int) uintptr {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return 0
	}
	img = resizeAvatar(img, size)
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// build a 24-bit DIB
	stride := (w*3 + 3) &^ 3
	px := make([]byte, stride*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			o := y*stride + x*3
			px[o], px[o+1], px[o+2] = byte(bl>>8), byte(g>>8), byte(r>>8)
		}
	}
	var bi [40]byte
	putU32(bi[0:], 40)
	putU32(bi[4:], uint32(w))
	putU32(bi[8:], uint32(-h)) // top-down
	putU16(bi[12:], 1)
	putU16(bi[14:], 24)
	hdc, _, _ := pGetDC.Call(0)
	if hdc == 0 {
		return 0
	}
	defer pReleaseDC.Call(0, hdc)
	bmp, _, _ := pCreateDIBitmap.Call(hdc,
		uintptr(unsafe.Pointer(&bi[0])), 4 /*CBM_INIT*/,
		uintptr(unsafe.Pointer(&px[0])), uintptr(unsafe.Pointer(&bi[0])), 0 /*DIB_RGB_COLORS*/)
	return bmp
}

func putU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}
func putU16(b []byte, v uint16) {
	b[0], b[1] = byte(v), byte(v>>8)
}

// resizeAvatar scales img to size×size using x/image/draw (no extra deps).
func resizeAvatar(img image.Image, size int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	return dst
}

// drawViewers paints viewer rows (avatar + name) inside the VIEWERS card.
// Card: (12,536)-(418,680). Label at y=548. Rows start at y=576, 40px each.
func drawViewers(hdc uintptr) {
	viewersMu.Lock()
	list := make([]viewerInfo, len(viewers))
	copy(list, viewers)
	viewersMu.Unlock()

	pSetBkMode.Call(hdc, TRANSPARENT)
	oldF, _, _ := pSelectObject.Call(hdc, gFonts["norm"])

	if len(list) == 0 {
		pSetTextColor.Call(hdc, colorRef(0x84, 0x94, 0xab))
		var rc = [4]int32{28, 412, 400, 440}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16("No one is watching right now."))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&rc)), 0)
		pSelectObject.Call(hdc, oldF)
		return
	}
	y := int32(408)
	for i, v := range list {
		if i >= 2 {
			break
		}
		// avatar (36×36) or initial tile
		if v.avatarBMP != 0 {
			memDC, _, _ := pCreateCompatibleDC.Call(hdc)
			oldBmp, _, _ := pSelectObject.Call(memDC, v.avatarBMP)
			pBitBlt.Call(hdc, 28, uintptr(y), 36, 36, memDC, 0, 0, 0x00CC0020)
			pSelectObject.Call(memDC, oldBmp)
			pDeleteDC.Call(memDC)
		} else {
			// violet initial tile
			tileBr, _, _ := pCreateSolidBrush.Call(colorRef(0x7c, 0x3a, 0xed))
			oldBr, _, _ := pSelectObject.Call(hdc, tileBr)
			oldPen, _, _ := pSelectObject.Call(hdc, gCardPen)
			pRoundRect.Call(hdc, 28, uintptr(y), 64, uintptr(y+36), 18, 18)
			pSelectObject.Call(hdc, oldBr)
			pSelectObject.Call(hdc, oldPen)
			pDeleteObject.Call(tileBr)
			pSetTextColor.Call(hdc, colorRef(0xff, 0xff, 0xff))
			ch := "?"
			if v.name != "" {
				r := []rune(v.name)
				ch = string(r[0])
			}
			var irc = [4]int32{28, y, 64, y + 36}
			pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(ch))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&irc)), 0x0001 /*DT_CENTER*/)
		}
		// name
		pSetTextColor.Call(hdc, colorRef(0x18, 0x1c, 0x1f))
		name := v.name
		if name == "" {
			name = "Guest"
		}
		var nrc = [4]int32{74, y + 6, 400, y + 30}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(name))), 0xFFFFFFFF, uintptr(unsafe.Pointer(&nrc)), 0)
		y += 44
	}
	pSelectObject.Call(hdc, oldF)
}
