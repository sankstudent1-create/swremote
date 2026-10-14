// SWRemote camera capture via MediaFoundation — v8.6.0.
//
// The modern capture path, the same one the Windows Camera app uses
// (IMFSourceReader). This works with Frame Server cameras and does NOT
// depend on the legacy DirectShow COM registration, which is damaged on
// some PCs (CLSID_CaptureGraphBuilder2 -> 80040154 REGDB_E_CLASSNOTREG).
// DirectShow remains as a fallback.

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mfplat       = windows.NewLazySystemDLL("mfplat.dll")
	mfDll        = windows.NewLazySystemDLL("mf.dll")
	mfreadwrite  = windows.NewLazySystemDLL("mfreadwrite.dll")
	pMFStartup   = mfplat.NewProc("MFStartup")
	pMFCreateAttributes = mfplat.NewProc("MFCreateAttributes")
	pMFCreateMediaType  = mfplat.NewProc("MFCreateMediaType")
	pMFEnumDeviceSources = mfDll.NewProc("MFEnumDeviceSources")
	pMFCreateSourceReader = mfreadwrite.NewProc("MFCreateSourceReaderFromMediaSource")
	pCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
)

const (
	mfVersion                    = 0x00020070
	mfStartupFull                = 0
	mfFirstVideoStream           = 0xFFFFFFFC
)

var (
	guidMFDevSourceAttrType = guid(0xC60AC5FE, 0x252A, 0x478F, [8]byte{0xA0, 0xEF, 0xBC, 0x8F, 0xA5, 0x84, 0x37, 0x26})
	guidMFDevSourceVidCap   = guid(0x8AC3587A, 0x4AE7, 0x42D8, [8]byte{0x99, 0xE0, 0x0A, 0x60, 0x13, 0xEE, 0xF9, 0x0F})
	guidMFMediaTypeVideo    = guid(0x73646976, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})
	guidMFVideoFormatRGB24  = guid(0xE436EB7D, 0x524F, 0x11CE, [8]byte{0x9F, 0x53, 0x00, 0x20, 0xAF, 0x0B, 0xA7, 0x70})
	guidMFVideoFormatMJPEG  = guid(0xCB10D434, 0x905F, 0x42AA, [8]byte{0xBC, 0xC4, 0x65, 0x7E, 0x2A, 0xC0, 0x9E, 0x7E})
	guidMFVideoFormatYUY2   = guid(0x32595559, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})
	guidMFMTMajorType       = guid(0x48EBA18E, 0xF8C9, 0x461A, [8]byte{0xB6, 0x64, 0x96, 0x2B, 0x02, 0x42, 0x8F, 0x33})
	guidMFMTSubtype         = guid(0xF7E34C9A, 0x42E8, 0x4714, [8]byte{0xB7, 0x4B, 0xAA, 0x6B, 0x67, 0x33, 0x6E, 0xCC})
	guidMFMTFrameSize       = guid(0x1652C33D, 0xD6B2, 0x4019, [8]byte{0xB8, 0x34, 0xF4, 0x75, 0x84, 0x1E, 0xDA, 0xA9})
	guidMFMTDefaultStride   = guid(0x6446461B, 0x8DA0, 0x4538, [8]byte{0xAD, 0x1C, 0xDB, 0x59, 0x46, 0xCE, 0xD9, 0x98})
)

var (
	mfOnce    sync.Once
	mfStarted bool
)

// mfEnsureStarted initializes MediaFoundation once per process and verifies
// every entry point we need actually exists.
func mfEnsureStarted() bool {
	mfOnce.Do(func() {
		comInit()
		for _, p := range []*windows.LazyProc{
			pMFStartup, pMFCreateAttributes, pMFCreateMediaType,
			pMFEnumDeviceSources, pMFCreateSourceReader, pCoTaskMemFree,
		} {
			if err := p.Find(); err != nil {
				log("cam: MediaFoundation unavailable (%v)", err)
				return
			}
		}
		if hr, _, _ := pMFStartup.Call(mfVersion, mfStartupFull); hr == 0 {
			mfStarted = true
		} else {
			log("cam: MFStartup failed (%x)", hr)
		}
	})
	return mfStarted
}

// mfAttrSetGUID wraps IMFAttributes::SetGUID (vtable index 24).
func mfAttrSetGUID(attr uintptr, key, val windows.GUID) uintptr {
	return comCallN(attr, 24, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&val)))
}

// mfCameraCount returns how many video-capture devices MediaFoundation sees.
func mfCameraCount() int {
	if !mfEnsureStarted() {
		return 0
	}
	comInit()
	var attr uintptr
	if hr, _, _ := pMFCreateAttributes.Call(uintptr(unsafe.Pointer(&attr)), 1); hr != 0 || attr == 0 {
		return 0
	}
	defer comRelease(attr)
	mfAttrSetGUID(attr, guidMFDevSourceAttrType, guidMFDevSourceVidCap)
	var sources uintptr
	var count uint32
	if hr, _, _ := pMFEnumDeviceSources.Call(attr,
		uintptr(unsafe.Pointer(&sources)),
		uintptr(unsafe.Pointer(&count))); hr != 0 {
		return 0
	}
	defer pCoTaskMemFree.Call(sources)
	for i := uint32(0); i < count; i++ {
		if src := *(*uintptr)(unsafe.Pointer(sources + uintptr(i*8))); src != 0 {
			comRelease(src)
		}
	}
	return int(count)
}

// mfCaptureLoop captures the first camera via IMFSourceReader.
// Returns nil if it captured until stop; an error if setup failed so the
// caller can fall back to DirectShow.
func mfCaptureLoop(stop <-chan struct{}) error {
	if !mfEnsureStarted() {
		return fmt.Errorf("MediaFoundation unavailable")
	}
	comInit()

	var attr uintptr
	if hr, _, _ := pMFCreateAttributes.Call(uintptr(unsafe.Pointer(&attr)), 1); hr != 0 || attr == 0 {
		return fmt.Errorf("MFCreateAttributes failed (%x)", hr)
	}
	defer comRelease(attr)
	mfAttrSetGUID(attr, guidMFDevSourceAttrType, guidMFDevSourceVidCap)

	var sources uintptr
	var count uint32
	if hr, _, _ := pMFEnumDeviceSources.Call(attr,
		uintptr(unsafe.Pointer(&sources)),
		uintptr(unsafe.Pointer(&count))); hr != 0 || count == 0 {
		if sources != 0 {
			pCoTaskMemFree.Call(sources)
		}
		return fmt.Errorf("no MF video devices (%x)", hr)
	}
	// take the first source, release the rest
	src := *(*uintptr)(unsafe.Pointer(sources))
	for i := uint32(1); i < count; i++ {
		if s := *(*uintptr)(unsafe.Pointer(sources + uintptr(i*8))); s != 0 {
			comRelease(s)
		}
	}
	pCoTaskMemFree.Call(sources)
	if src == 0 {
		return fmt.Errorf("no MF video device")
	}
	defer comRelease(src)

	var reader uintptr
	if hr, _, _ := pMFCreateSourceReader.Call(src, 0,
		uintptr(unsafe.Pointer(&reader))); hr != 0 || reader == 0 {
		return fmt.Errorf("source reader failed (%x)", hr)
	}
	defer comRelease(reader)

	// negotiate a format we can turn into JPEG
	subtype, ok := mfNegotiate(reader)
	if !ok {
		return fmt.Errorf("no usable video format")
	}
	log("cam: capturing via MediaFoundation (%s)", subtype)

	w, h, stride := mfFrameSize(reader)
	if w == 0 || h == 0 {
		w, h, stride = 640, 480, 640*3
	}
	log("cam: frame %dx%d", w, h)
	reportPCAV("cam", true)

	tick := time.NewTicker(200 * time.Millisecond) // ~5fps
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-tick.C:
			if !pcWants("cam") {
				continue
			}
			if j := mfGrabFrame(reader, subtype, w, h, stride); len(j) > 0 {
				sendBinary(0x06, j)
			}
		}
	}
}

// mfNegotiate picks the first workable subtype; returns its short name.
func mfNegotiate(reader uintptr) (string, bool) {
	try := []struct {
		name string
		g    windows.GUID
	}{
		{"RGB24", guidMFVideoFormatRGB24},
		{"MJPEG", guidMFVideoFormatMJPEG},
		{"YUY2", guidMFVideoFormatYUY2},
	}
	for _, t := range try {
		var mt uintptr
		if hr, _, _ := pMFCreateMediaType.Call(uintptr(unsafe.Pointer(&mt))); hr != 0 || mt == 0 {
			continue
		}
		mfAttrSetGUID(mt, guidMFMTMajorType, guidMFMediaTypeVideo)
		mfAttrSetGUID(mt, guidMFMTSubtype, t.g)
		// IMFSourceReader::SetCurrentMediaType — vtable index 7
		hr := comCallN(reader, 7, mfFirstVideoStream, 0, mt)
		comRelease(mt)
		if hr == 0 {
			return t.name, true
		}
		log("cam: MF subtype %s rejected (%x)", t.name, hr)
	}
	return "", false
}

// mfFrameSize reads width/height/stride from the negotiated media type.
func mfFrameSize(reader uintptr) (w, h, stride int) {
	var mt uintptr
	// IMFSourceReader::GetCurrentMediaType — vtable index 6
	if hr := comCallN(reader, 6, mfFirstVideoStream,
		uintptr(unsafe.Pointer(&mt))); hr != 0 || mt == 0 {
		return 0, 0, 0
	}
	defer comRelease(mt)
	var fs uint64
	// IMFAttributes::GetUINT64 — vtable index 8
	if hr := comCallN(mt, 8, uintptr(unsafe.Pointer(&guidMFMTFrameSize)),
		uintptr(unsafe.Pointer(&fs))); hr == 0 {
		w, h = int(fs>>32), int(fs&0xFFFFFFFF)
	}
	var st int32
	// IMFAttributes::GetUINT32 — vtable index 7
	if hr := comCallN(mt, 7, uintptr(unsafe.Pointer(&guidMFMTDefaultStride)),
		uintptr(unsafe.Pointer(&st))); hr == 0 {
		if st < 0 {
			st = -st
		}
		stride = int(st)
	}
	if stride == 0 {
		stride = w * 3
	}
	return w, h, stride
}

// mfGrabFrame reads one sample and converts it to JPEG.
func mfGrabFrame(reader uintptr, subtype string, w, h, stride int) []byte {
	var streamIdx, flags uint32
	var ts int64
	var sample uintptr
	// IMFSourceReader::ReadSample — vtable index 8
	if hr := comCallN(reader, 8, mfFirstVideoStream, 0,
		uintptr(unsafe.Pointer(&streamIdx)),
		uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&ts)),
		uintptr(unsafe.Pointer(&sample))); hr != 0 || sample == 0 {
		return nil
	}
	defer comRelease(sample)
	var buf uintptr
	// IMFSample::GetBufferByIndex — vtable index 10
	if hr := comCallN(sample, 10, 0,
		uintptr(unsafe.Pointer(&buf))); hr != 0 || buf == 0 {
		return nil
	}
	defer comRelease(buf)
	var data uintptr
	var maxLen, curLen uint32
	// IMFMediaBuffer::Lock — vtable index 3
	if hr := comCallN(buf, 3, uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(&maxLen)),
		uintptr(unsafe.Pointer(&curLen))); hr != 0 || data == 0 {
		return nil
	}
	defer comCallN(buf, 4) // Unlock — vtable index 4
	raw := (*[1 << 30]byte)(unsafe.Pointer(data))[:curLen:curLen]
	switch subtype {
	case "MJPEG":
		// already a JPEG — validate and pass through
		if curLen > 2 && raw[0] == 0xFF && raw[1] == 0xD8 {
			out := make([]byte, curLen)
			copy(out, raw)
			return out
		}
		return nil
	case "YUY2":
		return yuy2ToJPEG(raw, w, h)
	default: // RGB24, top-down from MediaFoundation
		return rgb24TopDownToJPEG(raw, w, h, stride)
	}
}

// rgb24TopDownToJPEG converts a top-down RGB24 buffer to JPEG.
func rgb24TopDownToJPEG(buf []byte, w, h, stride int) []byte {
	if stride < w*3 {
		stride = w * 3
	}
	if len(buf) < stride*(h-1)+w*3 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			si := y*stride + x*3
			di := img.PixOffset(x, y)
			img.Pix[di], img.Pix[di+1], img.Pix[di+2] = buf[si+2], buf[si+1], buf[si]
			img.Pix[di+3] = 255
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 60}); err != nil {
		return nil
	}
	return out.Bytes()
}

// yuy2ToJPEG converts a YUY2 (YUV 4:2:2) buffer to JPEG.
func yuy2ToJPEG(buf []byte, w, h int) []byte {
	if len(buf) < w*h*2 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x += 2 {
			si := (y*w + x) * 2
			y0, u, y1, v := buf[si], buf[si+1], buf[si+2], buf[si+3]
			r0, g0, b0 := yuvToRGB(y0, u, v)
			r1, g1, b1 := yuvToRGB(y1, u, v)
			di := img.PixOffset(x, y)
			img.Pix[di], img.Pix[di+1], img.Pix[di+2], img.Pix[di+3] = r0, g0, b0, 255
			di = img.PixOffset(x+1, y)
			img.Pix[di], img.Pix[di+1], img.Pix[di+2], img.Pix[di+3] = r1, g1, b1, 255
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 60}); err != nil {
		return nil
	}
	return out.Bytes()
}

func yuvToRGB(y, u, v byte) (byte, byte, byte) {
	c := int(y) - 16
	d := int(u) - 128
	e := int(v) - 128
	r := (298*c + 409*e + 128) >> 8
	g := (298*c - 100*d - 208*e + 128) >> 8
	b := (298*c + 516*d + 128) >> 8
	return clamp8(r), clamp8(g), clamp8(b)
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
