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
	"sync"
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
// v8.0: the batch VERIFIES the move succeeded before restarting — previously
// a failed move silently restarted the OLD exe ("closes but opens older version").
func applyUpdate(newExe string) error {
	cur, err := os.Executable()
	if err != nil {
		return err
	}
	// pre-flight: make sure we can write to the install folder.
	// v8.6.1: the old check opened the RUNNING exe for writing to test this —
	// Windows locks a running process's image file, so that test ALWAYS
	// failed and no update could ever install ("being used by another
	// process"). Testing the folder is the correct check.
	dir := filepath.Dir(cur)
	tf, err := os.CreateTemp(dir, "swremote-writetest-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot write to install folder (need admin?): %v", err)
	}
	tf.Close()
	os.Remove(tf.Name())
	bat := filepath.Join(os.TempDir(), "swremote-update.bat")
	script := "@echo off\r\n" +
		"set PID=" + strconv.Itoa(os.Getpid()) + "\r\n" +
		"set SRC=\"" + newExe + "\"\r\n" +
		"set DST=\"" + cur + "\"\r\n" +
		":loop\r\n" +
		"tasklist /FI \"PID eq %PID%\" 2>nul | find \"%PID%\" >nul\r\n" +
		"if not errorlevel 1 (timeout /t 1 /nobreak >nul & goto loop)\r\n" +
		"move /Y %SRC% %DST% >nul\r\n" +
		"if errorlevel 1 (\r\n" +
		"  powershell -NoProfile -Command \"Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.MessageBox]::Show('Could not replace SWRemote.exe — please run as administrator and try again.','SWRemote Update','OK','Error')\"\r\n" +
		"  del \"%~f0\"\r\n" +
		"  exit /b 1\r\n" +
		")\r\n" +
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

// stopDeleteService removes the wake-up service (used by uninstall).
func stopDeleteService() {
	hide := func(args ...string) string {
		cmd := exec.Command("sc", args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	hide("stop", "SWRemoteService")
	time.Sleep(500 * time.Millisecond)
	out := hide("delete", "SWRemoteService")
	if strings.Contains(out, "Access") || strings.Contains(out, "denied") {
		// needs admin — one UAC prompt during uninstall is expected
		_ = runElevated("sc", "stop", "SWRemoteService")
		time.Sleep(500 * time.Millisecond)
		_ = runElevated("sc", "delete", "SWRemoteService")
	}
}

// doUninstall removes shortcuts, autostart, registry entries and the
// installed files, then deletes itself. Invoked as: SWRemote-Agent.exe --uninstall
func doUninstall() {
	if !msgBoxYesNo("Uninstall SWRemote", "Remove SWRemote from this PC?\n\nYour ID and settings file will also be deleted.") {
		return
	}
	// v8.5.0: remove the wake-up service as well
	stopDeleteService()
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

// autoUpdateCheck runs silently in the background: shortly after startup
// (and every 6h after), it checks for a newer version and installs it
// automatically — no prompts, no manual downloads. v8.5.0 made updates
// fully silent: a live call is never dropped for an update (it installs
// when the call ends) and failures are logged and retried later.
func autoUpdateCheck() {
	time.Sleep(60 * time.Second)
	for {
		checkAndUpdateSilently(false)
		time.Sleep(6 * time.Hour)
	}
}

// pendingUpdate holds a downloaded update waiting for a call to end.
var (
	pendingUpdate   string
	pendingUpdateMu sync.Mutex
)

func checkAndUpdateSilently(manual bool) {
	if updateRunning.Swap(true) {
		return
	}
	defer updateRunning.Store(false)
	info, err := fetchVersion()
	if err != nil {
		log("update check failed: %v", err)
		if manual {
			guiSetStatus("Update check failed: "+err.Error(), false)
		}
		return
	}
	if !isNewer(appVersion, info.Version) {
		if manual {
			msgBox("SWRemote", "You're on the latest version (v"+appVersion+").", MB_ICONINFORMATION)
		}
		return
	}
	log("update: v%s available — downloading silently", info.Version)
	guiSetUpdateBtn("Downloading v"+info.Version+"…", false)
	guiSetProgress(0)
	tmp, err := downloadUpdate(info.URL)
	guiSetProgress(-1)
	if err != nil {
		log("update download failed: %v", err)
		guiSetUpdateBtn("Check for Updates", true)
		guiSetStatus("Update download failed — will retry later", false)
		return
	}
	log("update: v%s downloaded", info.Version)
	// never drop a live call for an update — install when the call ends
	callMu.Lock()
	active := callActive
	callMu.Unlock()
	if active {
		pendingUpdateMu.Lock()
		if pendingUpdate != "" {
			os.Remove(pendingUpdate)
		}
		pendingUpdate = tmp
		pendingUpdateMu.Unlock()
		guiSetUpdateBtn("Update ready — installs after call", false)
		guiSetStatus("Update v"+info.Version+" downloaded — installs when the call ends.", true)
		log("update: call active, installing after the call")
		return
	}
	installUpdate(tmp, info.Version)
}

func installUpdate(tmp, ver string) {
	guiSetUpdateBtn("Installing v"+ver+"…", false)
	guiSetStatus("Installing update v"+ver+" — SWRemote will restart…", true)
	log("update: installing v%s", ver)
	if err := applyUpdate(tmp); err != nil {
		os.Remove(tmp)
		guiSetUpdateBtn("Check for Updates", true)
		guiSetStatus("Update failed: "+err.Error(), false)
		log("update: install failed: %v", err)
	}
	// applyUpdate exits the process on success — we never return from it
}

// runUpdateFlow is the manual "Check for Updates" button — same silent flow.
func runUpdateFlow() {
	checkAndUpdateSilently(true)
}

// installPendingUpdate installs an update that was deferred until a call ended.
func installPendingUpdate() {
	pendingUpdateMu.Lock()
	pu := pendingUpdate
	pendingUpdate = ""
	pendingUpdateMu.Unlock()
	if pu == "" {
		return
	}
	go func() {
		ver := ""
		if info, err := fetchVersion(); err == nil {
			ver = info.Version
		}
		installUpdate(pu, ver)
	}()
}
