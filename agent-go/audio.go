package main

// v6.0: plays the caller's voice — 8kHz mono 16-bit PCM via waveOut.

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winmm           = windows.NewLazySystemDLL("winmm.dll")
	pWaveOutOpen    = winmm.NewProc("waveOutOpen")
	pWaveOutPrepare = winmm.NewProc("waveOutPrepareHeader")
	pWaveOutWrite   = winmm.NewProc("waveOutWrite")
	pWaveOutUnprep  = winmm.NewProc("waveOutUnprepareHeader")
	pWaveOutClose   = winmm.NewProc("waveOutClose")
	pWaveOutReset   = winmm.NewProc("waveOutReset")
)

// waveOutHdr mirrors the Win32 WAVEHDR struct. On 64-bit Windows this is
// 48 bytes — v8.4.0 fixed a crash bug where playPCM passed a 32-byte buffer
// and let winmm read 16 bytes past it on every audio chunk.
type waveOutHdr struct {
	lpData          uintptr
	dwBufferLength  uint32
	dwBytesRecorded uint32
	dwUser          uintptr
	dwFlags         uint32
	dwLoops         uint32
	lpNext          uintptr
	reserved        uintptr
}

var (
	audioMu   sync.Mutex
	audioOpen uintptr // HWAVEOUT handle, 0 = closed

	// pcmCh queues incoming voice chunks to a single playback goroutine.
	// v8.4.0: previously every chunk spawned its own goroutine with a
	// short-lived header — unbounded growth and unsafe lifetimes.
	pcmCh     = make(chan []byte, 64)
	pcmPumpOn int32
)

// waveFormatEx builds a WAVEFORMATEX struct for 8kHz mono 16-bit PCM.
func waveFormatEx() [18]byte {
	var w [18]byte
	putU16(w[0:], 1)     // wFormatTag = PCM
	putU16(w[2:], 1)     // nChannels = mono
	putU32(w[4:], 8000)  // nSamplesPerSec
	putU32(w[8:], 16000) // nAvgBytesPerSec
	putU16(w[12:], 2)    // nBlockAlign
	putU16(w[14:], 16)   // wBitsPerSample
	putU16(w[16:], 0)    // cbSize
	return w
}

func openAudio() bool {
	audioMu.Lock()
	defer audioMu.Unlock()
	if audioOpen != 0 {
		return true
	}
	wfx := waveFormatEx()
	var h uintptr
	r, _, _ := pWaveOutOpen.Call(
		uintptr(unsafe.Pointer(&h)),
		0xFFFFFFFF, // WAVE_MAPPER
		uintptr(unsafe.Pointer(&wfx[0])),
		0, 0, 0x00000008, // CALLBACK_NULL
	)
	if r != 0 {
		return false
	}
	audioOpen = h
	return true
}

// ensurePCMPump starts the single playback goroutine (once per process).
func ensurePCMPump() {
	if atomic.CompareAndSwapInt32(&pcmPumpOn, 0, 1) {
		go pcmPump()
	}
}

// pcmPump plays queued voice chunks in order on one goroutine, with a
// correctly-sized header and proper prepare/write/unprepare lifecycle.
func pcmPump() {
	defer func() {
		atomic.StoreInt32(&pcmPumpOn, 0)
		if r := recover(); r != nil {
			log("pcm pump panic: %v", r)
		}
	}()
	hdrSize := uintptr(unsafe.Sizeof(waveOutHdr{}))
	for buf := range pcmCh {
		audioMu.Lock()
		h := audioOpen
		audioMu.Unlock()
		if h == 0 || len(buf) == 0 {
			continue
		}
		var hdr waveOutHdr
		hdr.lpData = uintptr(unsafe.Pointer(&buf[0]))
		hdr.dwBufferLength = uint32(len(buf))
		hdr.dwBytesRecorded = uint32(len(buf))
		if r, _, _ := pWaveOutPrepare.Call(h, uintptr(unsafe.Pointer(&hdr)), hdrSize); r != 0 {
			continue
		}
		pWaveOutWrite.Call(h, uintptr(unsafe.Pointer(&hdr)), hdrSize)
		// hold the header until this chunk has played, then release it
		ms := len(buf) / 16 // 8kHz 16-bit mono = 16 bytes/ms
		time.Sleep(time.Duration(ms)*time.Millisecond + 30*time.Millisecond)
		pWaveOutUnprep.Call(h, uintptr(unsafe.Pointer(&hdr)), hdrSize)
	}
}

// playPCM queues one chunk of 8kHz mono 16-bit PCM for playback.
func playPCM(pcm []byte) {
	if !openAudio() {
		return
	}
	ensurePCMPump()
	// copy the chunk (the caller's buffer may be reused)
	buf := make([]byte, len(pcm))
	copy(buf, pcm)
	select {
	case pcmCh <- buf:
	default:
		// playback is backed up — drop, never grow without bound
	}
}

func stopAudio() {
	audioMu.Lock()
	h := audioOpen
	audioOpen = 0
	audioMu.Unlock()
	if h != 0 {
		pWaveOutReset.Call(h)
		pWaveOutClose.Call(h)
	}
}
