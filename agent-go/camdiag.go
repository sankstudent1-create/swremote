// SWRemote camera diagnostics — v8.5.0.
//
// When the automatic camera repair fails, the agent now reads the exact
// fault straight from Windows' device manager database (SetupDi) instead
// of guessing: which camera hardware is present, its problem code, and what
// to do about it. It can also open Device Manager in one click.

package main

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	setupapi = windows.NewLazySystemDLL("setupapi.dll")
	pSetupDiGetClassDevsW = setupapi.NewProc("SetupDiGetClassDevsW")
	pSetupDiEnumDeviceInfo = setupapi.NewProc("SetupDiEnumDeviceInfo")
	pSetupDiGetDeviceRegistryProperty = setupapi.NewProc("SetupDiGetDeviceRegistryPropertyW")
	pSetupDiDestroyDeviceInfoList = setupapi.NewProc("SetupDiDestroyDeviceInfoList")

	cfgmgr32 = windows.NewLazySystemDLL("cfgmgr32.dll")
	pCMGetDevNodeStatus = cfgmgr32.NewProc("CM_Get_DevNode_Status")
)

var guidDevClassImage = guid(0x6BDD1FC6, 0x810F, 0x11D0,
	[8]byte{0xBE, 0xC7, 0x08, 0x00, 0x2B, 0xE2, 0x09, 0x2F})

// guidDevClassCamera is the "Camera" device setup class (Windows 10+).
// v8.6.0: modern laptop webcams live here, NOT under the still-image class —
// checking only IMAGE wrongly reported "no camera hardware".
var guidDevClassCamera = guid(0xCA3E7AB9, 0xB4C3, 0x4AE6,
	[8]byte{0x8B, 0x17, 0x8E, 0xAF, 0xD3, 0xBB, 0x2F, 0xBD})

const (
	digcfPresent      = 0x00000002
	spdrpFriendlyName = 12
	spdrpDeviceDesc   = 0
	dnHasProblem      = 0x00000400
	invalidHandle     = ^uintptr(0)
)

// spDevInfoData mirrors SP_DEVINFO_DATA (28 bytes).
type spDevInfoData struct {
	cbSize    uint32
	classGuid windows.GUID
	devInst   uint32
	reserved  uintptr
}

// cmProbText translates Device Manager problem codes to plain words.
var cmProbText = map[uint32]string{
	1:  "not configured correctly (Code 1)",
	10: "could not start (Code 10)",
	12: "not enough free resources (Code 12)",
	14: "needs a restart to work (Code 14)",
	16: "not fully set up (Code 16)",
	18: "reinstall its driver (Code 18)",
	19: "registry information damaged (Code 19)",
	21: "Windows is removing the device (Code 21)",
	22: "disabled (Code 22)",
	24: "not present or not working (Code 24)",
	28: "driver not installed (Code 28)",
	29: "disabled in BIOS/UEFI (Code 29)",
	31: "driver failed to load (Code 31)",
	32: "driver disabled (Code 32)",
	33: "resource conflict (Code 33)",
	37: "could not initialize its driver (Code 37)",
	38: "an old driver is stuck in memory (Code 38)",
	39: "driver missing or corrupt (Code 39)",
	40: "registry entry invalid (Code 40)",
	41: "driver loaded but device not found (Code 41)",
	43: "stopped by Windows after reporting problems (Code 43)",
	44: "stopped by an application (Code 44)",
	45: "not connected to the PC (Code 45)",
	47: "prepared for safe removal (Code 47)",
	48: "driver blocked from starting (Code 48)",
	52: "driver is not digitally signed (Code 52)",
	54: "device failure (Code 54)",
}

type camDevice struct {
	name    string
	problem uint32 // 0 = no problem reported
}

// enumCameraDevices lists present imaging/camera devices with problem codes.
// v8.6.0: checks BOTH the still-image class and the Camera class.
func enumCameraDevices() []camDevice {
	var out []camDevice
	seen := map[string]bool{}
	for _, classGuid := range []windows.GUID{guidDevClassImage, guidDevClassCamera} {
		h, _, _ := pSetupDiGetClassDevsW.Call(
			uintptr(unsafe.Pointer(&classGuid)), 0, 0, digcfPresent)
		if h == 0 || h == invalidHandle {
			continue
		}
		for i := 0; ; i++ {
			var did spDevInfoData
			did.cbSize = uint32(unsafe.Sizeof(did))
			r, _, _ := pSetupDiEnumDeviceInfo.Call(h, uintptr(i), uintptr(unsafe.Pointer(&did)))
			if r == 0 {
				break
			}
			name := devProp(h, &did, spdrpFriendlyName)
			if name == "" {
				name = devProp(h, &did, spdrpDeviceDesc)
			}
			if name == "" {
				name = "Unknown camera device"
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			var status, problem uint32
			var pc uint32
			if cr, _, _ := pCMGetDevNodeStatus.Call(
				uintptr(unsafe.Pointer(&status)),
				uintptr(unsafe.Pointer(&problem)),
				uintptr(did.devInst), 0); cr == 0 && status&dnHasProblem != 0 {
				pc = problem
			}
			out = append(out, camDevice{name: name, problem: pc})
			if len(out) >= 16 {
				break
			}
		}
		pSetupDiDestroyDeviceInfoList.Call(h)
	}
	return out
}

func devProp(h uintptr, did *spDevInfoData, prop uint32) string {
	var buf [512]uint16
	var reqSize, dataType uint32
	r, _, _ := pSetupDiGetDeviceRegistryProperty.Call(
		h, uintptr(unsafe.Pointer(did)), uintptr(prop),
		uintptr(unsafe.Pointer(&dataType)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2),
		uintptr(unsafe.Pointer(&reqSize)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:])
}

// cameraDeviceReport builds a precise, human-readable diagnosis of what
// Windows reports about cameras.
func cameraDeviceReport() string {
	q, b, _ := directShowStatus()
	devs := enumCameraDevices()
	var sb strings.Builder
	if q && b {
		sb.WriteString("Camera software (DirectShow): OK\n")
	} else {
		sb.WriteString("Camera software (DirectShow): BROKEN\n")
	}
	if len(devs) == 0 {
		sb.WriteString("Camera hardware: none detected by Windows.\n")
		sb.WriteString("\nIf this PC has a camera, it may be turned off in BIOS/UEFI, " +
			"by a function key (look for a camera icon on the F-keys), " +
			"or by a physical privacy switch/shutter.")
		return sb.String()
	}
	fmt.Fprintf(&sb, "Camera hardware: %d device(s)\n", len(devs))
	for _, d := range devs {
		if d.problem == 0 {
			fmt.Fprintf(&sb, "• %s — working\n", d.name)
		} else if txt, ok := cmProbText[d.problem]; ok {
			fmt.Fprintf(&sb, "• %s — %s\n", d.name, txt)
		} else {
			fmt.Fprintf(&sb, "• %s — fault (Code %d)\n", d.name, d.problem)
		}
	}
	advice := ""
	for _, d := range devs {
		if advice != "" {
			break
		}
		switch d.problem {
		case 28, 39, 31, 1, 18, 37, 41:
			advice = "The camera driver is missing or broken. Download the camera driver " +
				"from your laptop/PC maker's support site (search your model name + \"camera driver\"), " +
				"install it, then restart Windows."
		case 22:
			advice = "The camera is disabled. Open Device Manager, right-click the camera → Enable device."
		case 29:
			advice = "The camera is disabled in BIOS/UEFI. Restart, enter BIOS/UEFI setup, and enable the camera."
		case 45:
			advice = "Windows thinks the camera is unplugged. For a USB camera, unplug it and plug it back in."
		case 52:
			advice = "Windows blocked the camera driver because it is not digitally signed. " +
				"Get a signed driver from the PC maker's support site."
		}
	}
	if advice != "" {
		sb.WriteString("\n" + advice)
	} else if allWorking(devs) {
		sb.WriteString("\nWindows says the camera is working — the problem is the camera " +
			"software. Restart Windows and try again; if it persists, send agent.log.")
	}
	return sb.String()
}

func allWorking(devs []camDevice) bool {
	for _, d := range devs {
		if d.problem != 0 {
			return false
		}
	}
	return len(devs) > 0
}

// openDeviceManager opens Device Manager in one click (no admin needed to view).
func openDeviceManager() {
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))),
		uintptr(unsafe.Pointer(u16("devmgmt.msc"))), 0, 0, 1)
}
