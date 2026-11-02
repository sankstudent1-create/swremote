// power.go — remote agent restart, PC shutdown, reboot, sleep.
//
// Restart-agent reuses the existing watchdog restartSelf (releases the
// single-instance mutex, starts a fresh copy, exits). Shutdown/reboot
// enable SeShutdownPrivilege on the process token and call
// ExitWindowsEx; the caller sends its result first and then waits
// briefly, because the session dies with the action.
package main

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pGetCurrentProcess = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCurrentProcess")
	pOpenProcessToken  = windows.NewLazySystemDLL("advapi32.dll").NewProc("OpenProcessToken")
	pLookupPrivValue   = windows.NewLazySystemDLL("advapi32.dll").NewProc("LookupPrivilegeValueW")
	pAdjustPriv        = windows.NewLazySystemDLL("advapi32.dll").NewProc("AdjustTokenPrivileges")
	pExitWindowsEx     = windows.NewLazySystemDLL("user32.dll").NewProc("ExitWindowsEx")
	pSetSuspendState   = windows.NewLazySystemDLL("powrprof.dll").NewProc("SetSuspendState")
)

// doSleep suspends the PC (S3 sleep, not hibernate). It needs no
// privilege, unlike shutdown/reboot. The session dies with the sleep;
// on wake the agent resumes and its reconnect logic re-registers.
func doSleep() {
	pSetSuspendState.Call(0, 0, 0)
}

// enableShutdownPrivilege turns on SeShutdownPrivilege for this process.
// It fails fast (before any action), so the caller can report the error
// while the session is still alive.
func enableShutdownPrivilege() error {
	proc, _, _ := pGetCurrentProcess.Call()
	var tok windows.Handle
	r, _, _ := pOpenProcessToken.Call(proc, 0x0028 /* TOKEN_ADJUST_PRIVILEGES|TOKEN_QUERY */, uintptr(unsafe.Pointer(&tok)))
	if r == 0 || tok == 0 {
		return fmt.Errorf("OpenProcessToken failed")
	}
	defer windows.CloseHandle(tok)
	name, _ := syscall.UTF16PtrFromString("SeShutdownPrivilege")
	var luid [2]uint32
	r, _, _ = pLookupPrivValue.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&luid[0])))
	if r == 0 {
		return fmt.Errorf("LookupPrivilegeValue failed")
	}
	// TOKEN_PRIVILEGES: count, luid low, luid high, attributes
	var tp [4]uint32
	tp[0] = 1
	tp[1] = luid[0]
	tp[2] = luid[1]
	tp[3] = 2 // SE_PRIVILEGE_ENABLED
	r, _, _ = pAdjustPriv.Call(uintptr(tok), 0, uintptr(unsafe.Pointer(&tp[0])), 0, 0, 0)
	if r == 0 {
		return fmt.Errorf("AdjustTokenPrivileges failed")
	}
	return nil
}

// doPower performs the shutdown (reboot=false) or reboot (reboot=true).
// Call enableShutdownPrivilege first; this never returns on success.
func doPower(reboot bool) {
	flag := uintptr(0x00000001) // EWX_SHUTDOWN
	if reboot {
		flag = 0x00000002 // EWX_REBOOT
	}
	flag |= 0x00000010 // EWX_FORCEIFHUNG: hung apps don't block it
	pExitWindowsEx.Call(flag, 0)
}
