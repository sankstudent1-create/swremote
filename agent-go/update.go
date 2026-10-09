// SWRemote self-update: check, download, replace, restart.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

type verInfo struct {
	Version string `json:"version"`
	Notes   string `json:"notes"`
	URL     string `json:"url"`
}

func serverHTTPBase() string {
	s := cfg.Server // e.g. wss://host/ws
	s = strings.Replace(s, "wss://", "https://", 1)
	s = strings.Replace(s, "ws://", "http://", 1)
	if i := strings.LastIndex(s, "/ws"); i > 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, "/")
}

func fetchVersion() (*verInfo, error) {
	c := &http.Client{Timeout: 20 * time.Second}
	r, err := c.Get(serverHTTPBase() + "/api/version")
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var v verInfo
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		return nil, err
	}
	if v.Version == "" || v.URL == "" {
		return nil, fmt.Errorf("bad version info")
	}
	return &v, nil
}

func verParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	out := []int{}
	for _, p := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(strings.TrimSpace(p))
		out = append(out, n)
	}
	return out
}

func isNewer(cur, latest string) bool {
	a, b := verParts(cur), verParts(latest)
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if y > x {
			return true
		}
		if x > y {
			return false
		}
	}
	return false
}

func downloadUpdate(url string) (string, error) {
	c := &http.Client{Timeout: 10 * time.Minute}
	r, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return "", fmt.Errorf("server returned %d", r.StatusCode)
	}
	tmp, err := os.CreateTemp("", "SWRemote-*.exe")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	total := r.ContentLength
	var done int64
	buf := make([]byte, 256*1024)
	for {
		n, er := r.Body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				os.Remove(tmp.Name())
				return "", werr
			}
			done += int64(n)
			if total > 0 {
				guiSetProgress(int(done * 100 / total))
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			os.Remove(tmp.Name())
			return "", er
		}
	}
	// sanity: must look like a Windows exe and be a reasonable size
	fi, _ := tmp.Stat()
	head := make([]byte, 2)
	tmp.ReadAt(head, 0)
	if fi.Size() < 1024*1024 || string(head) != "MZ" {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("downloaded file is not valid")
	}
	return tmp.Name(), nil
}

// applyUpdate swaps the running exe with the new one via a helper batch
// (Windows cannot replace a running executable directly), then restarts.
func applyUpdate(newExe string) error {
	cur, err := os.Executable()
	if err != nil {
		return err
	}
	bat := filepath.Join(os.TempDir(), "swremote-update.bat")
	script := "@echo off\r\n" +
		"set PID=" + strconv.Itoa(os.Getpid()) + "\r\n" +
		"set SRC=\"" + newExe + "\"\r\n" +
		"set DST=\"" + cur + "\"\r\n" +
		":loop\r\n" +
		"tasklist /FI \"PID eq %PID%\" 2>nul | find \"%PID%\" >nul\r\n" +
		"if not errorlevel 1 (timeout /t 1 /nobreak >nul & goto loop)\r\n" +
		"move /Y %SRC% %DST% >nul\r\n" +
		"start \"\" %DST%\r\n" +
		"del \"%~f0\"\r\n"
	if err := os.WriteFile(bat, []byte(script), 0644); err != nil {
		return err
	}
	cmd := exec.Command("cmd", "/c", bat)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}

// doUninstall removes shortcuts, autostart, registry entries and the
// installed files, then deletes itself. Invoked as: SWRemote-Agent.exe --uninstall
func doUninstall() {
	if !msgBoxYesNo("Uninstall SWRemote", "Remove SWRemote from this PC?\n\nYour ID and settings file will also be deleted.") {
		return
	}
	home, _ := os.UserHomeDir()
	appdata := os.Getenv("APPDATA")
	os.Remove(filepath.Join(home, "Desktop", "SWRemote.lnk"))
	os.Remove(filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "SWRemote.lnk"))
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE); err == nil {
		k.DeleteValue("SWRemote")
		k.Close()
	}
	registry.DeleteKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\SWRemote`)

	exe, _ := os.Executable()
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "SWRemote")
	pid := strconv.Itoa(os.Getpid())
	bat := filepath.Join(os.TempDir(), "swremote-uninstall.bat")
	script := "@echo off\r\n" +
		":loop\r\n" +
		"tasklist /FI \"PID eq " + pid + "\" 2>nul | find \"" + pid + "\" >nul\r\n" +
		"if not errorlevel 1 (timeout /t 1 /nobreak >nul & goto loop)\r\n" +
		"del /F /Q \"" + exe + "\"\r\n" +
		"del /F /Q \"" + filepath.Join(dir, "swremote.json") + "\" 2>nul\r\n" +
		"rmdir \"" + dir + "\" 2>nul\r\n" +
		"del \"%~f0\"\r\n"
	_ = os.WriteFile(bat, []byte(script), 0644)
	cmd := exec.Command("cmd", "/c", bat)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
	msgBox("SWRemote", "SWRemote has been uninstalled.", MB_ICONINFORMATION)
	os.Exit(0)
}

func runUpdateFlow() {
	guiSetUpdateBtn("Checking…", false)
	info, err := fetchVersion()
	if err != nil {
		guiSetUpdateBtn("Check for Updates", true)
		guiSetStatus("Update check failed: "+err.Error(), false)
		return
	}
	if !isNewer(appVersion, info.Version) {
		guiSetUpdateBtn("Check for Updates", true)
		msgBox("SWRemote", "You're on the latest version (v"+appVersion+").", MB_ICONINFORMATION)
		return
	}
	if !msgBoxYesNo("SWRemote Update",
		"Version v"+info.Version+" is available.\n\n"+info.Notes+"\n\nUpdate now?") {
		guiSetUpdateBtn("Check for Updates", true)
		return
	}
	guiSetUpdateBtn("Downloading…", false)
	guiSetProgress(0)
	tmp, err := downloadUpdate(info.URL)
	guiSetProgress(-1)
	if err != nil {
		guiSetUpdateBtn("Check for Updates", true)
		guiSetStatus("Download failed: "+err.Error(), false)
		return
	}
	guiSetUpdateBtn("Installing…", false)
	guiSetStatus("Installing update — SWRemote will restart…", true)
	if err := applyUpdate(tmp); err != nil {
		os.Remove(tmp)
		guiSetUpdateBtn("Check for Updates", true)
		guiSetStatus("Update failed: "+err.Error(), false)
	}
}
