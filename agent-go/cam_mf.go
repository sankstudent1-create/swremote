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
	mfplat                = windows.NewLazySystemDLL("mfplat.dll")
	mfDll                 = windows.NewLazySystemDLL("mf.dll")
	mfreadwrite           = windows.NewLazySystemDLL("mfreadwrite.dll")
	pMFStartup            = mfplat.NewProc("MFStartup")
	pMFCreateAttributes   = mfplat.NewProc("MFCreateAttributes")
	pMFCreateMediaType    = mfplat.NewProc("MFCreateMediaType")
	pMFEnumDeviceSources  = mfDll.NewProc("MFEnumDeviceSources")
	pMFCreateSourceReader = mfreadwrite.NewProc("MFCreateSourceReaderFromMediaSource")
	pMFCreateDeviceSource = mfDll.NewProc("MFCreateDeviceSource")
	pCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
)

var (
	// v8.6.7: SetupDi device-interface procs for the symbolic-link bypass
	pSetupDiEnumDeviceInterfaces      = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	pSetupDiGetDeviceInterfaceDetailW = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
)

const (
	mfVersion          = 0x00020070
	mfStartupFull      = 0
	mfFirstVideoStream = 0xFFFFFFFC
)

var (
	guidMFDevSourceAttrType = guid(0xC60AC5FE, 0x252A, 0x478F, [8]byte{0xA0, 0xEF, 0xBC, 0x8F, 0xA5, 0xF7, 0xCA, 0xD3}) // v8.9.2: was ...84 37 26 — THE 0xC00D36E6 bug
	guidMFDevSourceVidCap   = guid(0x8AC3587A, 0x4AE7, 0x42D8, [8]byte{0x99, 0xE0, 0x0A, 0x60, 0x13, 0xEE, 0xF9, 0x0F})
	guidMFMediaTypeVideo    = guid(0x73646976, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})
	guidMFVideoFormatRGB24  = guid(0x00000014, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}) // v8.9.2: MFVideoFormat_RGB24 (was the DS subtype)
	guidMFVideoFormatMJPEG  = guid(0x47504A4D, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}) // v8.9.2: 'MJPG' fourcc GUID
	guidMFVideoFormatYUY2   = guid(0x32595559, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})
	guidMFMTMajorType       = guid(0x48EBA18E, 0xF8C9, 0x4687, [8]byte{0xBF, 0x11, 0x0A, 0x74, 0xC9, 0xF9, 0x6A, 0x8F}) // v8.9.2: header-verified
	guidMFMTSubtype         = guid(0xF7E34C9A, 0x42E8, 0x4714, [8]byte{0xB7, 0x4B, 0xCB, 0x29, 0xD7, 0x2C, 0x35, 0xE5}) // v8.9.2: header-verified
	guidMFMTFrameSize       = guid(0x1652C33D, 0xD6B2, 0x4012, [8]byte{0xB8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xA3, 0x7D}) // v8.9.2: header-verified
	guidMFMTDefaultStride   = guid(0x644B4E48, 0x1E02, 0x4516, [8]byte{0xB0, 0xEB, 0xC0, 0x1C, 0xA9, 0xD4, 0x9A, 0xC6}) // v8.9.2: header-verified
	// v8.9.2: MF_SOURCE_READER_ENABLE_VIDEO_PROCESSING — lets the reader
	// convert native YUY2/MJPEG to RGB24.
	guidMFReaderEnableVideoProcessing = guid(0xFB6C4286, 0x0353, 0x11D1, [8]byte{0x90, 0x5F, 0x00, 0x00, 0xC0, 0xCC, 0x16, 0xBA})
	// v8.6.4: MFEnumDeviceSources returns IMFActivate objects, not sources.
	// v8.9.2: was IID_IMFMediaEvent — ActivateObject would E_NOINTERFACE.
	iidIMFMediaSource = guid(0x279A808D, 0xAEC7, 0x40C8, [8]byte{0x9C, 0x6B, 0xA6, 0xB4, 0x92, 0xC7, 0x8A, 0x66})
	// v8.6.7: symbolic-link bypass — device interface + source creation
	guidDevInterfaceImage = guid(0x6BDD1FC6, 0x810F, 0x11D0,
		[8]byte{0xBE, 0xC7, 0x08, 0x00, 0x2B, 0xE2, 0x09, 0x2F})
	guidDevInterfaceUSB = guid(0xA5DCBF10, 0x6530, 0x11D2,
		[8]byte{0x90, 0x1F, 0x00, 0xC0, 0x4F, 0xB9, 0x51, 0xED})
	guidMFDevSourceVidCapSymlink = guid(0x58F0AAD8, 0x22BF, 0x4F8A,
		[8]byte{0xBB, 0x3D, 0xD2, 0xC4, 0x97, 0x8C, 0x6E, 0x2F})
)

const digcfDeviceInterface = 0x00000010

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
			pMFEnumDeviceSources, pMFCreateSourceReader, pMFCreateDeviceSource,
			pCoTaskMemFree,
			pSetupDiEnumDeviceInterfaces, pSetupDiGetDeviceInterfaceDetailW,
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

// mfEnumCount enumerates MF device sources. With vidcap=true it asks for
// video-capture devices; with false it enumerates everything (used to test
// whether the enumerator itself is broken).
func mfEnumCount(vidcap bool) (int, uintptr) {
	comInit()
	var attr uintptr
	if hr, _, _ := pMFCreateAttributes.Call(uintptr(unsafe.Pointer(&attr)), 1); hr != 0 || attr == 0 {
		return 0, hr
	}
	defer comRelease(attr)
	if vidcap {
		mfAttrSetGUID(attr, guidMFDevSourceAttrType, guidMFDevSourceVidCap)
	}
	var sources uintptr
	var count uint32
	hr, _, _ := pMFEnumDeviceSources.Call(attr,
		uintptr(unsafe.Pointer(&sources)),
		uintptr(unsafe.Pointer(&count)))
	if hr != 0 {
		return 0, hr
	}
	defer pCoTaskMemFree.Call(sources)
	for i := uint32(0); i < count; i++ {
		if src := *(*uintptr)(unsafe.Pointer(sources + uintptr(i*8))); src != 0 {
			comRelease(src)
		}
	}
	return int(count), 0
}

// mfCameraCount returns how many video-capture devices MediaFoundation sees.
func mfCameraCount() int {
	if !mfEnsureStarted() {
		return 0
	}
	n, hr := mfEnumCount(true)
	if hr != 0 {
		// v8.6.4: the VIDCAP enumeration itself failed (seen: c00d36e6).
		// Try an unfiltered enumeration to learn whether the enumerator
		// is broken or there are simply no video devices.
		n2, hr2 := mfEnumCount(false)
		log("cam: MF VIDCAP enum failed (%x); unfiltered enum: %d device(s) (%x)", hr, n2, hr2)
		// v8.8.1: probe the symbolic-link bypass at startup so we know
		// immediately (without needing a call) whether direct source
		// creation works on this PC.
		mfBypassProbe()
		return 0
	}
	return n
}

// mfBypassProbe tries MFCreateDeviceSource via device symbolic links and
// logs the outcome. Lightweight: creates and releases the source only.
func mfBypassProbe() {
	links := cameraSymbolicLinks()
	if len(links) == 0 {
		log("cam: bypass probe — no image device interfaces found")
		return
	}
	for _, link := range links {
		src, hr := mfSourceFromSymlink(link)
		if hr == 0 && src != 0 {
			comRelease(src)
			log("cam: bypass probe — SOURCE CREATED OK via %s", link)
			return
		}
		log("cam: bypass probe — %s -> %x", link, hr)
	}
}

// spDeviceInterfaceData mirrors SP_DEVICE_INTERFACE_DATA (28 bytes).
type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGuid windows.GUID
	flags              uint32
	reserved           uintptr
}

// cameraSymbolicLinks returns device paths for present image-capture device
// interfaces. v8.6.7: used to bypass the broken MF device enumerator.
// v8.9.1: also tries the USB device interface (some cameras only expose that).
func cameraSymbolicLinks() []string {
	var out []string
	for _, ifGuid := range []windows.GUID{guidDevInterfaceImage, guidDevInterfaceUSB} {
		out = append(out, linksForInterface(ifGuid)...)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func linksForInterface(ifGuid windows.GUID) []string {
	var out []string
	h, _, _ := pSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&ifGuid)), 0, 0,
		digcfPresent|digcfDeviceInterface)
	if h == 0 || h == invalidHandle {
		return out
	}
	defer pSetupDiDestroyDeviceInfoList.Call(h)
	for i := 0; ; i++ {
		var ifd spDeviceInterfaceData
		ifd.cbSize = 28
		if r, _, _ := pSetupDiEnumDeviceInterfaces.Call(h, 0,
			uintptr(unsafe.Pointer(&ifGuid)), uintptr(i),
			uintptr(unsafe.Pointer(&ifd))); r == 0 {
			break
		}
		var reqSize uint32
		pSetupDiGetDeviceInterfaceDetailW.Call(h, uintptr(unsafe.Pointer(&ifd)),
			0, 0, uintptr(unsafe.Pointer(&reqSize)), 0)
		if reqSize == 0 || reqSize > 4096 {
			continue
		}
		buf := make([]byte, reqSize)
		*(*uint32)(unsafe.Pointer(&buf[0])) = 8 // cbSize on 64-bit
		if r, _, _ := pSetupDiGetDeviceInterfaceDetailW.Call(h,
			uintptr(unsafe.Pointer(&ifd)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(reqSize),
			0, 0); r == 0 {
			continue
		}
		// DevicePath is the WCHAR array right after cbSize (offset 4)
		n := (int(reqSize) - 4) / 2
		if n < 1 {
			continue
		}
		u16buf := (*[2048]uint16)(unsafe.Pointer(&buf[4]))[:n:n]
		if path := windows.UTF16ToString(u16buf); path != "" {
			out = append(out, path)
		}
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// mfSourceFromSymlink creates a media source directly from a device path,
// bypassing MFEnumDeviceSources. Returns (source, 0) on success.
func mfSourceFromSymlink(symlink string) (uintptr, uintptr) {
	var attr uintptr
	if hr, _, _ := pMFCreateAttributes.Call(uintptr(unsafe.Pointer(&attr)), 2); hr != 0 || attr == 0 {
		return 0, hr
	}
	defer comRelease(attr)
	mfAttrSetGUID(attr, guidMFDevSourceAttrType, guidMFDevSourceVidCap)
	sp, _ := windows.UTF16PtrFromString(symlink)
	// IMFAttributes::SetString (vtable 25)
	if hr := comCallN(attr, 25, uintptr(unsafe.Pointer(&guidMFDevSourceVidCapSymlink)),
		uintptr(unsafe.Pointer(sp))); hr != 0 {
		return 0, hr
	}
	var src uintptr
	hr, _, _ := pMFCreateDeviceSource.Call(attr, uintptr(unsafe.Pointer(&src)))
	if hr != 0 || src == 0 {
		return 0, hr
	}
	return src, 0
}

// mfSourceFromEnum gets the first camera via normal MF enumeration +
// ActivateObject. Returns (source, 0) on success or (0, hresult).
func mfSourceFromEnum() (uintptr, uintptr) {
	var attr uintptr
	if hr, _, _ := pMFCreateAttributes.Call(uintptr(unsafe.Pointer(&attr)), 1); hr != 0 || attr == 0 {
		return 0, hr
	}
	defer comRelease(attr)
	mfAttrSetGUID(attr, guidMFDevSourceAttrType, guidMFDevSourceVidCap)

	var sources uintptr
	var count uint32
	hr, _, _ := pMFEnumDeviceSources.Call(attr,
		uintptr(unsafe.Pointer(&sources)),
		uintptr(unsafe.Pointer(&count)))
	if hr != 0 || count == 0 {
		if sources != 0 {
			pCoTaskMemFree.Call(sources)
		}
		return 0, hr
	}
	act := *(*uintptr)(unsafe.Pointer(sources))
	for i := uint32(1); i < count; i++ {
		if s := *(*uintptr)(unsafe.Pointer(sources + uintptr(i*8))); s != 0 {
			comRelease(s)
		}
	}
	pCoTaskMemFree.Call(sources)
	if act == 0 {
		return 0, 0xC00D36D5 // MF_E_NOT_FOUND
	}
	var src uintptr
	if hr := comCallN(act, 33, // IMFActivate::ActivateObject (v8.9.2: was 3 — IMFActivate inherits IMFAttributes 0-32)
		uintptr(unsafe.Pointer(&iidIMFMediaSource)),
		uintptr(unsafe.Pointer(&src))); hr != 0 || src == 0 {
		comRelease(act)
		return 0, hr
	}
	comRelease(act)
	return src, 0
}

// mfReaderAttrs builds reader attributes with video processing enabled so
// SetCurrentMediaType(RGB24) succeeds on cameras that only expose YUY2/MJPEG.
// Returns 0 if attributes can't be created (caller proceeds without).
func mfReaderAttrs() uintptr {
	var attr uintptr
	if hr, _, _ := pMFCreateAttributes.Call(uintptr(unsafe.Pointer(&attr)), 1); hr != 0 || attr == 0 {
		return 0
	}
	// IMFAttributes::SetUINT32 — vtable index 21
	comCallN(attr, 21, uintptr(unsafe.Pointer(&guidMFReaderEnableVideoProcessing)), 1)
	return attr
}

// mfCaptureLoop captures the first camera via IMFSourceReader.
// Returns nil if it captured until stop; an error if setup failed so the
// caller can fall back to DirectShow.
func mfCaptureLoop(stop <-chan struct{}) error {
	if !mfEnsureStarted() {
		return fmt.Errorf("MediaFoundation unavailable")
	}
	comInit()

	// Attempt 1: normal MF enumeration.
	src, hr := mfSourceFromEnum()
	if hr != 0 || src == 0 {
		log("cam: MF enum failed (%x) — trying symbolic-link bypass", hr)
		// Attempt 2: build the source directly from the device path,
		// bypassing the broken enumerator entirely.
		var bhr uintptr = 1
		for _, link := range cameraSymbolicLinks() {
			log("cam: trying device path %s", link)
			if s, h := mfSourceFromSymlink(link); h == 0 && s != 0 {
				src = s
				bhr = 0
				break
			} else {
				bhr = h
			}
		}
		if bhr != 0 || src == 0 {
			return fmt.Errorf("no MF video devices (%x/%x)", hr, bhr)
		}
		log("cam: source opened via symbolic-link bypass")
	}
	defer comRelease(src)

	var reader uintptr
	// v8.9.2: ask for video processing so the reader can convert native
	// YUY2/MJPEG to RGB24; if attribute setup fails, retry without attrs.
	if attrs := mfReaderAttrs(); attrs != 0 {
		hr, _, _ := pMFCreateSourceReader.Call(src, attrs,
			uintptr(unsafe.Pointer(&reader)))
		comRelease(attrs)
		if hr != 0 || reader == 0 {
			reader = 0
			if hr2, _, _ := pMFCreateSourceReader.Call(src, 0,
				uintptr(unsafe.Pointer(&reader))); hr2 != 0 || reader == 0 {
				return fmt.Errorf("source reader failed (%x)", hr)
			}
		}
	} else if hr, _, _ := pMFCreateSourceReader.Call(src, 0,
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
	// IMFSourceReader::ReadSample — vtable index 9 (v8.9.2: was 8 = SetCurrentPosition)
	if hr := comCallN(reader, 9, mfFirstVideoStream, 0,
		uintptr(unsafe.Pointer(&streamIdx)),
		uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&ts)),
		uintptr(unsafe.Pointer(&sample))); hr != 0 || sample == 0 {
		return nil
	}
	defer comRelease(sample)
	var buf uintptr
	// IMFSample::GetBufferByIndex — vtable index 40 (v8.9.2: was 10)
	if hr := comCallN(sample, 40, 0,
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
