// SWRemote prerequisites — v8.4.0.
//
// The agent now checks everything it needs at startup (DirectShow, camera
// devices, mic/speaker devices, Windows edition) and can INSTALL what's
// missing instead of just failing: on N editions it offers the Media Feature
// Pack via an elevated DISM run (UAC consent), otherwise it re-registers
// quartz.dll/qedit.dll elevated.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	pWaveOutGetNumDevs = winmm.NewProc("waveOutGetNumDevs")
	pWaveInGetNumDevs  = winmm.NewProc("waveInGetNumDevs")
)

// windowsIsNEdition reports whether this is a Windows N edition (no media
// components preinstalled — the classic cause of camera error 80040154).
func windowsIsNEdition() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	ed, _, err := k.GetStringValue("EditionID")
	if err != nil {
		return false
	}
	// N editions end with "N": ProfessionalN, CoreN, EnterpriseN, EducationN…
	return strings.HasSuffix(strings.ToUpper(strings.TrimSpace(ed)), "N")
}

func windowsEdition() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "unknown"
	}
	defer k.Close()
	ed, _, _ := k.GetStringValue("EditionID")
	if ed == "" {
		ed, _, _ = k.GetStringValue("ProductName")
	}
	if ed == "" {
		ed = "unknown"
	}
	return ed
}

// directShowStatus checks the three things camera capture needs.
func directShowStatus() (quartzOK, builderOK bool, cams int) {
	if h, err := windows.LoadLibrary("quartz.dll"); err == nil {
		quartzOK = true
		windows.FreeLibrary(h)
	}
	comInit()
	if b, hr := coCreate(clsidCaptureGraphBuilder2, iidICaptureGraphBuilder2); hr == 0 {
		builderOK = true
		comRelease(b)
	}
	cams = countCameras()
	return quartzOK, builderOK, cams
}

// countCameras enumerates video input devices via the System Device Enumerator.
func countCameras() int {
	n, _ := countCamerasHR()
	return n
}

// countCamerasHR also returns the raw HRESULT: 0 = enumerator worked,
// 1 (S_FALSE) = enumerator fine but no video devices registered,
// anything else = the enumerator itself is broken.
func countCamerasHR() (int, uintptr) {
	comInit()
	devEnum, hr := coCreate(clsidSystemDeviceEnum, iidICreateDevEnum)
	if hr != 0 {
		log("cam: SystemDeviceEnum CoCreate failed (%x)", hr)
		return 0, hr
	}
	defer comRelease(devEnum)
	var enumMon uintptr
	// ICreateDevEnum::CreateClassEnumerator(CLSID_VideoInputDeviceCategory)
	hr = comCallN(devEnum, 3, uintptr(unsafe.Pointer(&clsidVideoInputCategory)),
		uintptr(unsafe.Pointer(&enumMon)), 0)
	if hr != 0 {
		log("cam: CreateClassEnumerator(video) failed (%x)", hr)
		return 0, hr
	}
	if enumMon == 0 {
		return 0, 1 // S_FALSE: no devices
	}
	defer comRelease(enumMon)
	n := 0
	for {
		var mon uintptr
		var fetched uint32
		// IEnumMoniker::Next(1, &mon, &fetched)
		if hr := comCallN(enumMon, 3, 1, uintptr(unsafe.Pointer(&mon)),
			uintptr(unsafe.Pointer(&fetched))); hr != 0 || fetched == 0 {
			break
		}
		comRelease(mon)
		n++
		if n > 32 {
			break
		}
	}
	return n, 0
}

func audioInOK() bool {
	r, _, _ := pWaveInGetNumDevs.Call()
	return r > 0
}

func audioOutOK() bool {
	r, _, _ := pWaveOutGetNumDevs.Call()
	return r > 0
}

// runPrereqCheck logs a full component report at startup and stores the
// one-line camera summary for the agent window.
func runPrereqCheck() {
	ed := windowsEdition()
	nEd := windowsIsNEdition()
	q, b, dsCams := directShowStatus()
	_, dsEnumHR := countCamerasHR() // v8.6.6: distinguish empty vs broken
	mfCams := mfCameraCount()       // v8.6.0: the path the Camera app uses
	cams := mfCams
	if dsCams > cams {
		cams = dsCams
	}
	mic, spk := audioInOK(), audioOutOK()
	log("prereq: Windows edition=%s (N=%v)", ed, nEd)
	log("prereq: DirectShow quartz.dll=%v captureBuilder=%v dsCams=%d dsEnumHR=%x mfCams=%d", q, b, dsCams, dsEnumHR, mfCams)
	log("prereq: mic devices=%v speaker devices=%v", mic, spk)

	// camera works if MediaFoundation sees it, or legacy DirectShow does
	camOK := mfCams > 0 || (q && b && dsCams > 0)
	var summary string
	switch {
	case cams > 0:
		summary = fmt.Sprintf("OK — %d camera(s)", cams)
	case !q || !b:
		if nEd {
			summary = "needs Media Feature Pack"
		} else {
			summary = "camera software broken — use Fix camera"
		}
	default:
		summary = "no camera found"
	}
	setCamStatus(summary)
	if !camOK {
		log("prereq: camera WILL FAIL until repaired — press 'Fix camera' in the agent window")
	}
}

// repairWindowsFlow runs the Microsoft-recommended repair for "class not
// registered" errors that survive regsvr32 (from web research): sfc /scannow
// then DISM /RestoreHealth. v8.9.0. Takes 15-30 minutes, needs admin.
func repairWindowsFlow() {
	if !msgBoxYesNo("SWRemote — Repair Windows",
		"This runs Windows' own repair tools:\n\n1. sfc /scannow (checks system files)\n2. DISM /RestoreHealth (repairs the Windows image)\n\nIt takes 15-30 minutes. Windows will ask for administrator permission twice.\n\nStart now?") {
		return
	}
	setCamStatus("repairing Windows…")
	guiSetStatus("Repairing Windows — approve the admin prompts…", false)
	log("repair-windows: starting sfc /scannow")
	if err := runElevatedWait("sfc.exe", "/scannow"); err != nil {
		log("repair-windows: sfc failed: %v", err)
		setCamStatus("sfc failed")
		guiSetStatus("System file check failed — see agent.log", false)
		msgBox("SWRemote", "Could not run sfc /scannow:\n"+err.Error(), MB_ICONERROR)
		return
	}
	log("repair-windows: sfc done, starting DISM /RestoreHealth")
	guiSetStatus("Step 2/2: repairing Windows image (DISM)…", false)
	if err := runElevatedWait("dism.exe", "/Online", "/Cleanup-Image", "/RestoreHealth"); err != nil {
		log("repair-windows: DISM failed: %v", err)
		setCamStatus("DISM failed")
		guiSetStatus("DISM repair failed — see agent.log", false)
		msgBox("SWRemote", "Could not run DISM:\n"+err.Error(), MB_ICONERROR)
		return
	}
	log("repair-windows: done — restart recommended")
	setCamStatus("repair done — restart Windows")
	guiSetStatus("Windows repair finished.", true)
	msgBox("SWRemote", "Windows repair finished.\n\nPlease restart your PC — then press Fix camera once.", MB_ICONINFORMATION)
	runPrereqCheck()
}
func runElevatedWait(exe string, args ...string) error {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", "''")+"'")
	}
	// v8.9.1: -PassThru captures the real exit code. Without it we couldn't
	// tell whether the elevated tool actually ran (sfc once "finished" in 16s).
	ps := fmt.Sprintf("$p = Start-Process -FilePath '%s' -ArgumentList %s -Verb RunAs -Wait -PassThru; exit $p.ExitCode",
		strings.ReplaceAll(exe, "'", "''"), strings.Join(quoted, ","))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var fixCamRunning = false
var fixCamMu = struct{ sync.Mutex }{}

func fixCameraFlow() {
	fixCamMu.Lock()
	if fixCamRunning {
		fixCamMu.Unlock()
		return
	}
	fixCamRunning = true
	fixCamMu.Unlock()
	defer func() {
		fixCamMu.Lock()
		fixCamRunning = false
		fixCamMu.Unlock()
	}()

	setCamStatus("checking…")
	q, b, cams := directShowStatus()
	if q && b && cams > 0 {
		setCamStatus(fmt.Sprintf("OK — %d camera(s)", cams))
		msgBox("SWRemote", fmt.Sprintf("Camera components are fine (%d camera device(s) found).\nNo repair needed.", cams), MB_ICONINFORMATION)
		return
	}
	if windowsIsNEdition() {
		if !msgBoxYesNo("SWRemote — Fix camera",
			"This PC is a Windows N edition and the Media Feature Pack is missing — the camera cannot work without it.\n\nInstall it now? Windows will ask for administrator permission.") {
			runPrereqCheck()
			return
		}
		setCamStatus("installing Media Feature Pack…")
		guiSetStatus("Installing Media Feature Pack — approve the admin prompt…", false)
		err := runElevatedWait("dism.exe", "/Online", "/Add-Capability", "/CapabilityName:Media.MediaFeaturePack~~~~0.0.1.0")
		if err != nil {
			log("fix-camera: DISM failed: %v", err)
			setCamStatus("install failed")
			guiSetStatus("Media Feature Pack install failed — see agent.log", false)
			msgBox("SWRemote", "Could not install the Media Feature Pack:\n"+err.Error(), MB_ICONERROR)
			return
		}
		log("fix-camera: Media Feature Pack installed")
		setCamStatus("installed — restart Windows")
		guiSetStatus("Media Feature Pack installed — restart Windows.", true)
		msgBox("SWRemote", "Media Feature Pack installed.\n\nPlease restart Windows — the camera will work after the restart.", MB_ICONINFORMATION)
		return
	}
	// Not an N edition: DirectShow DLLs are present but unregistered/damaged.
	if !msgBoxYesNo("SWRemote — Fix camera",
		"DirectShow looks broken on this PC.\n\nRe-register the camera components now? Windows will ask for administrator permission.") {
		runPrereqCheck()
		return
	}
	setCamStatus("repairing…")
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	for _, dll := range []string{sysRoot + `\System32\devenum.dll`, sysRoot + `\System32\quartz.dll`, sysRoot + `\System32\qedit.dll`} {
		if err := runElevatedWait("regsvr32.exe", "/s", dll); err != nil {
			log("fix-camera: regsvr32 %s failed: %v", dll, err)
		} else {
			log("fix-camera: regsvr32 %s ok", dll)
		}
	}
	q2, b2, cams2 := directShowStatus()
	if q2 && b2 && cams2 > 0 {
		setCamStatus(fmt.Sprintf("OK — %d camera(s)", cams2))
		guiSetStatus("Camera repaired.", true)
		msgBox("SWRemote", "Camera components repaired.", MB_ICONINFORMATION)
	} else {
		// v8.5.0: don't guess — read the exact fault from Device Manager's database
		runPrereqCheck()
		guiSetStatus("Camera still failing — diagnosis ready", false)
		detail := cameraDeviceReport()
		log("fix-camera diagnosis:\n%s", detail)
		if msgBoxYesNo("SWRemote — Camera diagnosis",
			"The automatic repair did not fix it. Here is exactly what Windows reports:\n\n"+detail+"\n\nOpen Device Manager now?") {
			openDeviceManager()
		}
	}
}
