package main

// v8.3.2: waveIn microphone capture — fallback when WASAPI is unavailable
// (e.g. E_NOINTERFACE). Uses the legacy winmm waveIn API, no COM needed.

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pWaveInOpen    = winmm.NewProc("waveInOpen")
	pWaveInPrepare = winmm.NewProc("waveInPrepareHeader")
	pWaveInAddBuf  = winmm.NewProc("waveInAddBuffer")
	pWaveInStart   = winmm.NewProc("waveInStart")
	pWaveInStop    = winmm.NewProc("waveInStop")
	pWaveInUnprep  = winmm.NewProc("waveInUnprepareHeader")
	pWaveInClose   = winmm.NewProc("waveInClose")
)

const (
	WIM_OPEN  = 0x3BE
	WIM_DATA  = 0x3C0
	WIM_CLOSE = 0x3BF
	CALLBACK_FUNCTION = 0x30000
)

// waveInHdr mirrors the WAVEHDR struct.
type waveInHdr struct {
	lpData         uintptr
	dwBufferLength uint32
	dwBytesRecorded uint32
	dwUser         uintptr
	dwFlags        uint32
	dwLoops        uint32
	lpNext         uintptr
	reserved       uintptr
}

var waveInHandle uintptr

//export waveInProc
func waveInProc(hwi uintptr, msg uint32, inst uintptr, p1 uintptr, p2 uintptr) uintptr {
	// called by Windows on the waveIn thread; forward audio data
	if msg == WIM_DATA && p1 != 0 {
		hdr := (*waveInHdr)(unsafe.Pointer(p1))
		if hdr.dwBytesRecorded > 0 {
			buf := make([]byte, hdr.dwBytesRecorded)
			src := (*[1 << 30]byte)(unsafe.Pointer(hdr.lpData))[:hdr.dwBytesRecorded:hdr.dwBytesRecorded]
			copy(buf, src)
			// send as 0x07 binary frame (same as WASAPI path)
			sendBinary(0x07, buf)
		}
		// re-queue the buffer
		pWaveInAddBuf.Call(hwi, p1, uintptr(unsafe.Sizeof(waveInHdr{})))
	}
	return 0
}

func waveInCaptureLoop(stop <-chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			log("waveIn capture panic: %v", r)
		}
	}()

	wfx := waveFormatEx()
	var hwi uintptr
	// waveInOpen(&hwi, WAVE_MAPPER, &wfx, waveInProc, 0, CALLBACK_FUNCTION)
	proc := windows.NewCallback(waveInProc)
	ret, _, _ := pWaveInOpen.Call(
		uintptr(unsafe.Pointer(&hwi)),
		0xFFFFFFFF, // WAVE_MAPPER
		uintptr(unsafe.Pointer(&wfx[0])),
		proc,
		0,
		CALLBACK_FUNCTION)
	if ret != 0 || hwi == 0 {
		log("mic: waveIn open failed (%d)", ret)
		pcMicOn = false
		reportPCAV("mic", false)
		return
	}
	waveInHandle = hwi
	defer func() {
		pWaveInStop.Call(hwi)
		pWaveInClose.Call(hwi)
		waveInHandle = 0
	}()

	// allocate 4 buffers of 0.5s each (8000 samples * 2 bytes = 16000 bytes)
	const bufSize = 16000
	const numBufs = 4
	hdrs := make([]waveInHdr, numBufs)
	bufs := make([][]byte, numBufs)
	for i := 0; i < numBufs; i++ {
		bufs[i] = make([]byte, bufSize)
		hdrs[i].lpData = uintptr(unsafe.Pointer(&bufs[i][0]))
		hdrs[i].dwBufferLength = bufSize
		pWaveInPrepare.Call(hwi, uintptr(unsafe.Pointer(&hdrs[i])), uintptr(unsafe.Sizeof(waveInHdr{})))
		pWaveInAddBuf.Call(hwi, uintptr(unsafe.Pointer(&hdrs[i])), uintptr(unsafe.Sizeof(waveInHdr{})))
	}

	pWaveInStart.Call(hwi)
	log("mic: capturing via waveIn")
	pcMicOn = true
	reportPCAV("mic", true)

	<-stop
	log("mic: waveIn stopped")
	for i := 0; i < numBufs; i++ {
		pWaveInUnprep.Call(hwi, uintptr(unsafe.Pointer(&hdrs[i])), uintptr(unsafe.Sizeof(waveInHdr{})))
	}
	pcMicOn = false
	reportPCAV("mic", false)
}
