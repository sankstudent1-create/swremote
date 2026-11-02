// loopback.go — PC system-audio capture (WASAPI loopback) → viewers (v8.9.9).
//
// Mirrors micCaptureLoop, but taps the RENDER endpoint with
// AUDCLNT_STREAMFLAGS_LOOPBACK, so what the PC itself plays (videos,
// music, call audio) reaches the viewer as binary 0x08 in the same 16kHz
// mono PCM format as the mic. Session-scoped: it runs while the relay
// session is live and sends only while the PC-sound toggle is on;
// when off it drains silently, exactly like a muted mic.
//
// v8.9.12: the LOOPBACK flag was written as 0x2 — the real constant is
// 0x00020000, so every Initialize failed with AUDCLNT_E_INVALID_STREAM_FLAG
// (88890021) and PC sound never worked on any PC, even though the toggle
// and the drain loop ran.
package main

import (
	"sync/atomic"
	"time"
	"unsafe"
)

var loopbackOn int32 = 1 // default on; console toggles via {t:"pc-audio"}

func setLoopback(on bool) {
	v := int32(0)
	if on {
		v = 1
	}
	atomic.StoreInt32(&loopbackOn, v)
	log("loopback: %v", on)
}

func loopbackLoop(stop <-chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			log("loopback panic: %v", r)
		}
	}()
	comInit()
	enum, hr := coCreate(clsidMMDeviceEnumerator, iidIMMDeviceEnumerator)
	if hr != 0 {
		log("loopback: no device enumerator (%x)", hr)
		return
	}
	defer comRelease(enum)

	var device uintptr
	// GetDefaultAudioEndpoint(eRender=0, eConsole=0)
	if hr := comCallN(enum, 4, 0, 0, uintptr(unsafe.Pointer(&device))); hr != 0 || device == 0 {
		log("loopback: no render device (%x)", hr)
		return
	}
	defer comRelease(device)

	var ac uintptr
	// Activate(IID_IAudioClient, CLSCTX_ALL=23)
	if hr := comCallN(device, 3, uintptr(unsafe.Pointer(&iidIAudioClient)), 23, 0, uintptr(unsafe.Pointer(&ac))); hr != 0 || ac == 0 {
		log("loopback: activate failed (%x)", hr)
		return
	}
	defer comRelease(ac)

	var pwfx uintptr
	if hr := comCallN(ac, 8, uintptr(unsafe.Pointer(&pwfx))); hr != 0 || pwfx == 0 {
		log("loopback: GetMixFormat failed (%x)", hr)
		return
	}
	fmt := (*[18]byte)(unsafe.Pointer(pwfx))
	ch := int(fmt[2]) | int(fmt[3])<<8
	rate := int(fmt[4]) | int(fmt[5])<<8 | int(fmt[6])<<16 | int(fmt[7])<<24
	bits := int(fmt[14]) | int(fmt[15])<<8
	tag := int(fmt[0]) | int(fmt[1])<<8
	isFloat := tag == 3
	log("loopback: format %dch %dHz %dbit float=%v", ch, rate, bits, isFloat)

	// Initialize(SHARED, LOOPBACK=0x00020000, 1s buffer, 0, pwfx, NULL)
	if hr := comCallN(ac, 3, 0, 0x00020000, 10000000, 0, pwfx, 0); hr != 0 {
		log("loopback: initialize failed (%x)", hr)
		return
	}
	var cc uintptr
	if hr := comCallN(ac, 14, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&cc))); hr != 0 || cc == 0 {
		log("loopback: GetService failed (%x)", hr)
		return
	}
	defer comRelease(cc)
	if hr := comCallN(ac, 10); hr != 0 { // Start
		log("loopback: start failed (%x)", hr)
		return
	}
	defer comCallN(ac, 11) // Stop
	log("loopback: capturing")

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if atomic.LoadInt32(&loopbackOn) == 0 {
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
					pcm := micToPCM(data, int(nFrames), ch, bits, isFloat, rate)
					if len(pcm) > 0 {
						sendBinary(0x08, pcm)
					}
				}
				comCallN(cc, 4, uintptr(nFrames)) // ReleaseBuffer
				comCallN(cc, 5, uintptr(unsafe.Pointer(&nPackets)))
			}
		}
	}
}
