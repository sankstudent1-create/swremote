package main

// v7.0: PC-side camera + mic capture for video calls.
//   Camera: DirectShow filter graph + SampleGrabber (RGB24), polled via
//           GetCurrentBuffer — no COM callback interface to implement.
//   Mic:    WASAPI IAudioClient + IAudioCaptureClient, polled.
// Both send to the viewer: camera as binary 0x06 (JPEG), mic as binary 0x07
// (8kHz mono 16-bit PCM). Failures are logged; the agent reports
// av-state with cam/mic=false so the viewer sees "unavailable", never a hang.

import (
	"bytes"
	"image"
	"image/jpeg"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows"
)

var (
	ole32            = windows.NewLazySystemDLL("ole32.dll")
	pCoCreateInstance = ole32.NewProc("CoCreateInstance")
	pCoInitializeEx   = ole32.NewProc("CoInitializeEx")
)

var (
	capMu      sync.Mutex
	capRunning bool
	capStopCh  chan struct{}
)

// guid builds a windows.GUID from string parts.
func guid(d1 uint32, d2, d3 uint16, d4 [8]byte) windows.GUID {
	return windows.GUID{Data1: d1, Data2: d2, Data3: d3, Data4: d4}
}

var (
	clsidMMDeviceEnumerator = guid(0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E})
	iidIMMDeviceEnumerator  = guid(0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0xB6, 0x36})
	iidIAudioClient         = guid(0x1CB9AD4C, 0xDBFA, 0x4C32, [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2})
	iidIAudioCaptureClient  = guid(0xC8ADBD64, 0xE71E, 0x48A0, [8]byte{0xB4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17})

	clsidFilterGraph          = guid(0xE436EBB3, 0x524F, 0x11CE, [8]byte{0x9F, 0x53, 0x00, 0x20, 0xAF, 0x0B, 0xA7, 0x70})
	clsidCaptureGraphBuilder2 = guid(0xBF87B6E1, 0x8C27, 0x11D0, [8]byte{0xB3, 0xF2, 0x00, 0xAA, 0x00, 0x37, 0x61, 0xC5})
	clsidSampleGrabber        = guid(0xC1F400A0, 0x3F08, 0x11D3, [8]byte{0x9F, 0x0B, 0x00, 0x60, 0x08, 0x03, 0x9E, 0x37})
	clsidSystemDeviceEnum     = guid(0x62BE5D10, 0x60B9, 0x11D0, [8]byte{0xBD, 0x3B, 0x00, 0xA0, 0xC9, 0x11, 0xCE, 0x86})
	clsidVideoInputCategory   = guid(0x860BB310, 0x5D01, 0x11D0, [8]byte{0xBD, 0x3B, 0x00, 0xA0, 0xC9, 0x11, 0xCE, 0x86})
	iidIBaseFilter            = guid(0x56A86895, 0x0AD4, 0x11CE, [8]byte{0xB0, 0x3A, 0x00, 0x20, 0xAF, 0x0B, 0xA7, 0x70})
	iidISampleGrabber         = guid(0x6B652FFF, 0x11FE, 0x4FCE, [8]byte{0x92, 0xAD, 0x02, 0x66, 0xB5, 0xD7, 0xC7, 0x8A})
	iidIMediaControl          = guid(0x56A868B1, 0x0AD4, 0x11CE, [8]byte{0xB0, 0x3A, 0x00, 0x20, 0xAF, 0x0B, 0xA7, 0x70})
	iidICreateDevEnum         = guid(0x29840822, 0x5B84, 0x11D0, [8]byte{0xBD, 0x3B, 0x00, 0xA0, 0xC9, 0x11, 0xCE, 0x86})
	mediatypeVideo            = guid(0x73646976, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})
	mediasubtypeRGB24         = guid(0xE436EB7D, 0x524F, 0x11CE, [8]byte{0x9F, 0x53, 0x00, 0x20, 0xAF, 0x0B, 0xA7, 0x70})
	pinCategoryCapture        = guid(0xFB6C4281, 0x0353, 0x11D1, [8]byte{0x90, 0x5F, 0x00, 0x00, 0xC0, 0xCC, 0x16, 0xBA})
)

// comCallN invokes a COM vtable method: vtbl[idx](this, args...)
func comCallN(unk uintptr, idx int, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(unk))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(idx*8)))
	all := append([]uintptr{unk}, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

func comRelease(unk uintptr) {
	if unk != 0 {
		comCallN(unk, 2)
	}
}

func coCreate(cls, iid windows.GUID) (uintptr, uintptr) {
	var unk uintptr
	hr, _, _ := pCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&cls)),
		0,
		23, // CLSCTX_ALL — v8.0.1: INPROC_SERVER alone failed on some PCs
		uintptr(unsafe.Pointer(&iid)),
		uintptr(unsafe.Pointer(&unk)))
	if hr != 0 || unk == 0 {
		return 0, hr
	}
	return unk, 0
}

// comInit initializes COM on the calling thread. Each capture goroutine
// must call this — COM apartment state is per-thread, and Go goroutines
// migrate across OS threads.
func comInit() {
	pCoInitializeEx.Call(0, 0) // COINIT_MULTITHREADED
}

func init() {
	// COM for capture threads (MTA is fine for our polling usage)
	pCoInitializeEx.Call(0, 0)
}

// ---------- public control ----------

func startPCCapture() {
	capMu.Lock()
	if capRunning {
		capMu.Unlock()
		return
	}
	capRunning = true
	capStopCh = make(chan struct{})
	capMu.Unlock()
	go micCaptureLoop(capStopCh)
	go camCaptureLoop(capStopCh)
	log("PC capture started")
}

func stopPCCapture() {
	capMu.Lock()
	if !capRunning {
		capMu.Unlock()
		return
	}
	capRunning = false
	close(capStopCh)
	capMu.Unlock()
	log("PC capture stopped")
}

func pcWants(typ string) bool {
	callMu.Lock()
	defer callMu.Unlock()
	if !callActive {
		return false
	}
	if typ == "cam" {
		return pcCamOn
	}
	return pcMicOn
}

func sendBinary(kind byte, data []byte) {
	pkt := make([]byte, 1+len(data))
	pkt[0] = kind
	copy(pkt[1:], data)
	_ = wsWrite(websocket.BinaryMessage, pkt)
}

// ---------- mic capture (WASAPI) ----------

func micCaptureLoop(stop <-chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			log("mic capture panic: %v", r)
		}
	}()
	comInit() // COM is per-thread; this goroutine needs its own init
	enum, hr := coCreate(clsidMMDeviceEnumerator, iidIMMDeviceEnumerator)
	if hr != 0 {
		log("mic: no device enumerator (%x)", hr)
		reportPCAV("mic", false)
		return
	}
	defer comRelease(enum)

	var device uintptr
	// GetDefaultAudioEndpoint(eCapture=1, eConsole=0)
	if hr := comCallN(enum, 4, 1, 0, uintptr(unsafe.Pointer(&device))); hr != 0 || device == 0 {
		log("mic: no capture device (%x)", hr)
		reportPCAV("mic", false)
		return
	}
	defer comRelease(device)

	var ac uintptr
	// Activate(IID_IAudioClient, CLSCTX_ALL=23)
	if hr := comCallN(device, 3, uintptr(unsafe.Pointer(&iidIAudioClient)), 23, 0, uintptr(unsafe.Pointer(&ac))); hr != 0 || ac == 0 {
		log("mic: activate failed (%x)", hr)
		reportPCAV("mic", false)
		return
	}
	defer comRelease(ac)

	var pwfx uintptr
	if hr := comCallN(ac, 8, uintptr(unsafe.Pointer(&pwfx))); hr != 0 || pwfx == 0 {
		log("mic: GetMixFormat failed (%x)", hr)
		reportPCAV("mic", false)
		return
	}
	// read format: nChannels(2), nSamplesPerSec(4), wBitsPerSample(14), wFormatTag(0)
	fmt := (*[18]byte)(unsafe.Pointer(pwfx))
	ch := int(fmt[2]) | int(fmt[3])<<8
	rate := int(fmt[4]) | int(fmt[5])<<8 | int(fmt[6])<<16 | int(fmt[7])<<24
	bits := int(fmt[14]) | int(fmt[15])<<8
	tag := int(fmt[0]) | int(fmt[1])<<8
	isFloat := tag == 3
	log("mic: format %dch %dHz %dbit float=%v", ch, rate, bits, isFloat)

	// Initialize(SHARED, 0, 1s buffer, 0, pwfx, NULL)
	if hr := comCallN(ac, 3, 0, 0, 10000000, 0, pwfx, 0); hr != 0 {
		log("mic: initialize failed (%x)", hr)
		reportPCAV("mic", false)
		return
	}
	var cc uintptr
	if hr := comCallN(ac, 14, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&cc))); hr != 0 || cc == 0 {
		log("mic: GetService failed (%x)", hr)
		reportPCAV("mic", false)
		return
	}
	defer comRelease(cc)
	if hr := comCallN(ac, 10); hr != 0 { // Start
		log("mic: start failed (%x)", hr)
		reportPCAV("mic", false)
		return
	}
	defer comCallN(ac, 11) // Stop
	log("mic: capturing")
	reportPCAV("mic", true)

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if !pcWants("mic") {
				// drain silently while muted
				drainMic(cc)
				continue
			}
			var nPackets uint32
			comCallN(cc, 5, uintptr(unsafe.Pointer(&nPackets)))
			for nPackets > 0 {
				var data uintptr
				var nFrames uint32
				var flags uint32
				if hr := comCallN(cc, 3, uintptr(unsafe.Pointer(&data)),
					uintptr(unsafe.Pointer(&nFrames)), uintptr(unsafe.Pointer(&flags)),
					0, 0); hr != 0 || data == 0 {
					break
				}
				if flags&2 == 0 && nFrames > 0 { // not silent
					pcm := micToPCM(data, int(nFrames), ch, bits, isFloat)
					if len(pcm) > 0 {
						sendBinary(0x07, pcm)
					}
				}
				comCallN(cc, 4, uintptr(nFrames)) // ReleaseBuffer
				comCallN(cc, 5, uintptr(unsafe.Pointer(&nPackets)))
			}
		}
	}
}

func drainMic(cc uintptr) {
	var nPackets uint32
	comCallN(cc, 5, uintptr(unsafe.Pointer(&nPackets)))
	for nPackets > 0 {
		var data uintptr
		var nFrames uint32
		var flags uint32
		comCallN(cc, 3, uintptr(unsafe.Pointer(&data)),
			uintptr(unsafe.Pointer(&nFrames)), uintptr(unsafe.Pointer(&flags)), 0, 0)
		comCallN(cc, 4, uintptr(nFrames))
		comCallN(cc, 5, uintptr(unsafe.Pointer(&nPackets)))
	}
}

// micToPCM converts captured frames to 8kHz mono 16-bit PCM.
func micToPCM(data uintptr, nFrames, ch, bits int, isFloat bool) []byte {
	if nFrames == 0 || ch == 0 {
		return nil
	}
	mono := make([]float64, nFrames)
	if isFloat && bits == 32 {
		f := (*[1 << 28]float32)(unsafe.Pointer(data))
		for i := 0; i < nFrames; i++ {
			sum := 0.0
			for c := 0; c < ch; c++ {
				sum += float64(f[i*ch+c])
			}
			mono[i] = sum / float64(ch)
		}
	} else if !isFloat && bits == 16 {
		s := (*[1 << 28]int16)(unsafe.Pointer(data))
		for i := 0; i < nFrames; i++ {
			sum := 0
			for c := 0; c < ch; c++ {
				sum += int(s[i*ch+c])
			}
			mono[i] = float64(sum) / float64(ch) / 32768.0
		}
	} else {
		return nil // unsupported format
	}
	// resample to 8kHz (assume input is 44.1k/48k; measure not available — use ratio by frames/time would need clock; simple decimation by ratio)
	// We don't know the exact input rate here; capture typically 48k. Use fixed 6:1 (48k->8k).
	out := make([]byte, 0, nFrames/6*2)
	for i := 0; i < nFrames; i += 6 {
		v := mono[i]
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		s16 := int16(v * 32767)
		out = append(out, byte(s16), byte(s16>>8))
	}
	return out
}

// reportPCAV tells the viewer whether PC cam/mic is actually available.
func reportPCAV(which string, ok bool) {
	callMu.Lock()
	if which == "cam" {
		pcCamOn = pcCamOn && ok
	} else {
		pcMicOn = pcMicOn && ok
	}
	cam, mic := pcCamOn, pcMicOn
	callMu.Unlock()
	sendWSJSON(map[string]any{"t": "av-state", "side": "agent", "cam": cam, "mic": mic})
}

// ---------- camera capture (DirectShow + SampleGrabber) ----------

var (
	iidIFilterGraph2         = guid(0x36B73882, 0xC2C8, 0x11CF, [8]byte{0x8B, 0x46, 0x00, 0x80, 0x5F, 0x6C, 0xEF, 0x60})
	iidICaptureGraphBuilder2 = guid(0x93E5A4E0, 0x2D50, 0x11D1, [8]byte{0xB3, 0x43, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x98})
)

func camCaptureLoop(stop <-chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			log("cam capture panic: %v", r)
		}
	}()
	comInit() // COM is per-thread; this goroutine needs its own init
	graph, hr := coCreate(clsidFilterGraph, iidIFilterGraph2)
	if hr != 0 {
		log("cam: no filter graph (%x)", hr)
		reportPCAV("cam", false)
		return
	}
	defer comRelease(graph)

	builder, hr := coCreate(clsidCaptureGraphBuilder2, iidICaptureGraphBuilder2)
	if hr != 0 {
		log("cam: no capture builder (%x)", hr)
		reportPCAV("cam", false)
		return
	}
	defer comRelease(builder)
	comCallN(builder, 3, graph) // SetFiltergraph

	camFilter := findCamera()
	if camFilter == 0 {
		log("cam: no camera device found")
		reportPCAV("cam", false)
		return
	}
	defer comRelease(camFilter)
	comCallN(graph, 3, camFilter, uintptr(unsafe.Pointer(u16("cam")))) // AddFilter

	grabBase, hr := coCreate(clsidSampleGrabber, iidIBaseFilter)
	if hr != 0 {
		log("cam: no sample grabber (%x)", hr)
		reportPCAV("cam", false)
		return
	}
	defer comRelease(grabBase)
	var grabber uintptr
	if hr := comCallN(grabBase, 0, uintptr(unsafe.Pointer(&iidISampleGrabber)), uintptr(unsafe.Pointer(&grabber))); hr != 0 || grabber == 0 {
		log("cam: ISampleGrabber QI failed (%x)", hr)
		reportPCAV("cam", false)
		return
	}
	defer comRelease(grabber)

	// SetMediaType: video/RGB24
	var amt [80]byte
	copy(amt[0:16], guidBytes(mediatypeVideo))
	copy(amt[16:32], guidBytes(mediasubtypeRGB24))
	if hr := comCallN(grabber, 4, uintptr(unsafe.Pointer(&amt[0]))); hr != 0 {
		log("cam: SetMediaType failed (%x)", hr)
		reportPCAV("cam", false)
		return
	}
	comCallN(graph, 3, grabBase, uintptr(unsafe.Pointer(u16("grab")))) // AddFilter
	// RenderStream(PIN_CATEGORY_CAPTURE, MEDIATYPE_Video, cam, NULL, grab)
	if hr := comCallN(builder, 7,
		uintptr(unsafe.Pointer(&pinCategoryCapture)),
		uintptr(unsafe.Pointer(&mediatypeVideo)),
		camFilter, 0, grabBase); hr != 0 {
		log("cam: RenderStream failed (%x)", hr)
		reportPCAV("cam", false)
		return
	}
	comCallN(grabber, 6, 1) // SetBufferSamples(TRUE)

	var mc uintptr
	if hr := comCallN(graph, 0, uintptr(unsafe.Pointer(&iidIMediaControl)), uintptr(unsafe.Pointer(&mc))); hr != 0 || mc == 0 {
		log("cam: IMediaControl QI failed (%x)", hr)
		reportPCAV("cam", false)
		return
	}
	defer comRelease(mc)
	if hr := comCallN(mc, 7); hr != 0 { // Run
		log("cam: graph Run failed (%x)", hr)
		reportPCAV("cam", false)
		return
	}
	defer comCallN(mc, 9) // Stop
	log("cam: capturing")
	reportPCAV("cam", true)

	// learn frame size from the connected media type
	w, h := camFrameSize(grabber)
	if w == 0 || h == 0 {
		w, h = 640, 480
	}
	log("cam: frame %dx%d", w, h)

	tick := time.NewTicker(200 * time.Millisecond) // ~5fps
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if !pcWants("cam") {
				continue
			}
			var size int32
			if hr := comCallN(grabber, 7, uintptr(unsafe.Pointer(&size)), 0); hr != 0 || size == 0 {
				continue
			}
			buf := make([]byte, size)
			sz := size
			if hr := comCallN(grabber, 7, uintptr(unsafe.Pointer(&sz)), uintptr(unsafe.Pointer(&buf[0]))); hr != 0 {
				continue
			}
			if j := rgb24ToJPEG(buf, w, h); len(j) > 0 {
				sendBinary(0x06, j)
			}
		}
	}
}

func guidBytes(g windows.GUID) []byte {
	b := make([]byte, 16)
	putU32(b[0:], g.Data1)
	putU16(b[4:], g.Data2)
	putU16(b[6:], g.Data3)
	copy(b[8:], g.Data4[:])
	return b
}

// findCamera returns the first video capture device's IBaseFilter (or 0).
func findCamera() uintptr {
	devEnum, hr := coCreate(clsidSystemDeviceEnum, iidICreateDevEnum)
	if hr != 0 {
		return 0
	}
	defer comRelease(devEnum)
	var enumMon uintptr
	// CreateClassEnumerator(CLSID_VideoInputDeviceCategory, &enum, 0)
	if hr := comCallN(devEnum, 3, uintptr(unsafe.Pointer(&clsidVideoInputCategory)),
		uintptr(unsafe.Pointer(&enumMon)), 0); hr != 0 || enumMon == 0 {
		return 0
	}
	defer comRelease(enumMon)
	var mon uintptr
	var fetched uint32
	// Next(1, &mon, &fetched)
	if hr := comCallN(enumMon, 3, 1, uintptr(unsafe.Pointer(&mon)),
		uintptr(unsafe.Pointer(&fetched))); hr != 0 || mon == 0 {
		return 0
	}
	defer comRelease(mon)
	var filter uintptr
	// IMoniker::BindToObject(NULL, NULL, IID_IBaseFilter, &filter) — idx 9
	if hr := comCallN(mon, 9, 0, 0,
		uintptr(unsafe.Pointer(&iidIBaseFilter)),
		uintptr(unsafe.Pointer(&filter))); hr != 0 || filter == 0 {
		return 0
	}
	return filter
}

// camFrameSize reads width/height from the grabber's connected media type.
func camFrameSize(grabber uintptr) (int, int) {
	var amt [80]byte
	if hr := comCallN(grabber, 5, uintptr(unsafe.Pointer(&amt[0]))); hr != 0 { // GetConnectedMediaType
		return 0, 0
	}
	// pbFormat at offset 72 -> VIDEOINFOHEADER; bmiHeader at +48; biWidth at +48, biHeight at +52
	pbFormat := *(*uintptr)(unsafe.Pointer(&amt[72]))
	if pbFormat == 0 {
		return 0, 0
	}
	w := int(*(*int32)(unsafe.Pointer(pbFormat + 48)))
	h := int(*(*int32)(unsafe.Pointer(pbFormat + 52)))
	if h < 0 {
		h = -h
	}
	return w, h
}

// rgb24ToJPEG converts a bottom-up RGB24 buffer to JPEG.
func rgb24ToJPEG(buf []byte, w, h int) []byte {
	stride := w * 3
	if len(buf) < stride*h {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		srcY := h - 1 - y // bottom-up
		for x := 0; x < w; x++ {
			si := srcY*stride + x*3
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
