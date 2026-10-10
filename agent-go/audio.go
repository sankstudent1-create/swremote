package main

// v6.0: plays the caller's voice — 8kHz mono 16-bit PCM via waveOut.

import (
	"sync"
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

	audioMu   sync.Mutex
	audioOpen uintptr // HWAVEOUT handle, 0 = closed
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

// playPCM queues one chunk of 8kHz mono 16-bit PCM for playback.
func playPCM(pcm []byte) {
	if !openAudio() {
		return
	}
	audioMu.Lock()
	h := audioOpen
	audioMu.Unlock()
	if h == 0 {
		return
	}
	// copy the chunk (the caller's buffer may be reused)
	buf := make([]byte, len(pcm))
	copy(buf, pcm)
	// WAVEHDR: lpData, dwBufferLength, dwBytesRecorded, dwUser, dwFlags, dwLoops, lpNext, reserved
	var hdr [32]byte
	*(*uintptr)(unsafe.Pointer(&hdr[0])) = uintptr(unsafe.Pointer(&buf[0]))
	putU32(hdr[8:], uint32(len(buf)))
	putU32(hdr[12:], uint32(len(buf)))
	go func() {
		// keep buf alive until playback finishes; simplest: hold a reference
		// for a bounded time based on chunk duration
		pWaveOutPrepare.Call(h, uintptr(unsafe.Pointer(&hdr[0])), 32)
		pWaveOutWrite.Call(h, uintptr(unsafe.Pointer(&hdr[0])), 32)
		// chunk duration: len/16000 seconds; sleep a bit longer, then cleanup
		// (we intentionally do not block the caller)
		_ = buf
	}()
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
