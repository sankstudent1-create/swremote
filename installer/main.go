// SWRemote Setup — single-file Windows installer.
// Embeds the agent .exe and the WPF wizard (setup.ps1), extracts both to a
// temp folder and runs the wizard. No admin rights needed.
package main

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed setup.ps1
var setupPS1 string

//go:embed SWRemote-Agent.exe
var agentExe []byte

func main() {
	tmp, err := os.MkdirTemp("", "SWRemoteSetup")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmp)

	if err := os.WriteFile(filepath.Join(tmp, "setup.ps1"), []byte(setupPS1), 0644); err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(tmp, "SWRemote-Agent.exe"), agentExe, 0644); err != nil {
		return
	}

	args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(tmp, "setup.ps1")}
	if len(os.Args) > 1 && os.Args[1] == "--uninstall" {
		args = append(args, "-Uninstall")
	}
	ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.Command(ps, args...)
	_ = cmd.Run()
}
