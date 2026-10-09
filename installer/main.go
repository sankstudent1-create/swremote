// SWRemote Setup — single-file Windows installer.
// Embeds the agent .exe, the WPF wizard (setup.ps1) and artwork, extracts
// everything to a temp folder and runs the wizard. No admin rights needed.
package main

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

//go:embed setup.ps1
var setupPS1 string

//go:embed SWRemote-Agent.exe
var agentExe []byte

//go:embed install-art.png
var installArt []byte

//go:embed logo-mark.png
var logoMark []byte

var (
	user32       = syscall.NewLazyDLL("user32.dll")
	pMessageBoxW = user32.NewProc("MessageBoxW")
)

func fatal(title, msg string) {
	// keep the UTF-16 buffers alive in locals for the whole modal call
	t, _ := syscall.UTF16FromString(title)
	m, _ := syscall.UTF16FromString(msg)
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(&m[0])), uintptr(unsafe.Pointer(&t[0])), 0x10 /*MB_ICONERROR*/)
	os.Exit(1)
}

func main() {
	uninstall := len(os.Args) > 1 && os.Args[1] == "--uninstall"

	tmp, err := os.MkdirTemp("", "SWRemoteSetup")
	if err != nil {
		fatal("SWRemote Setup", "Could not create temp folder:\n"+err.Error())
	}
	defer os.RemoveAll(tmp)

	files := map[string][]byte{
		"setup.ps1":          []byte(setupPS1),
		"SWRemote-Agent.exe": agentExe,
		"install-art.png":    installArt,
		"logo-mark.png":      logoMark,
	}
	for name, data := range files {
		if name == "setup.ps1" {
			// UTF-8 BOM: without it, Windows PowerShell 5.1 reads the script
			// as ANSI, mangles every non-ASCII char (—, •, →, ✓, emoji) and
			// the whole wizard fails to parse.
			data = append([]byte{0xEF, 0xBB, 0xBF}, data...)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), data, 0644); err != nil {
			fatal("SWRemote Setup", "Could not unpack installer files:\n"+err.Error())
		}
	}

	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	ps := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(ps); err != nil {
		fatal("SWRemote Setup", "Windows PowerShell was not found on this PC.\nSWRemote Setup needs it to run.")
	}

	args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(tmp, "setup.ps1")}
	if uninstall {
		args = append(args, "-Uninstall")
	}
	cmd := exec.Command(ps, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fatal("SWRemote Setup",
			"The setup wizard ran into a problem:\n\n"+err.Error()+"\n\nDetails:\n"+truncate(string(out), 1500))
	}
	_ = out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
