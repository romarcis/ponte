package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// Windows ignores the input Ponte replays while a program run as
// administrator (FanControl, PC Manager...) is in front, unless Ponte runs
// as administrator too. The first time Ponte is opened it asks for
// administrator rights once, and with them creates a scheduled task that
// Windows runs as administrator without asking again, and installs the
// Ponte service (service_windows.go). From then on a Ponte started
// normally hands over to the task. If the user says no, Ponte keeps
// working without them and the window says what does not work.

const adminTask = "Ponte"

func schtasks(args ...string) *exec.Cmd {
	cmd := exec.Command("schtasks", args...)
	hideConsole(cmd)
	return cmd
}

var taskCache struct {
	sync.Mutex
	on   bool
	when time.Time
}

// adminStartEnabled tells whether the task exists; the window asks often,
// so the answer is kept for a few seconds.
func adminStartEnabled() bool {
	c := &taskCache
	c.Lock()
	defer c.Unlock()
	if time.Since(c.when) > 5*time.Second {
		c.on = schtasks("/Query", "/TN", adminTask).Run() == nil
		c.when = time.Now()
	}
	return c.on
}

func forgetTaskState() {
	taskCache.Lock()
	taskCache.when = time.Time{}
	taskCache.Unlock()
}

// isElevated tells whether this Ponte runs as administrator.
func isElevated() bool { return selfElevated() }

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size                   uint32
	mask                   uint32
	hwnd                   uintptr
	verb, file, params     *uint16
	dir                    *uint16
	show                   int32
	instApp, idList        uintptr
	class                  *uint16
	keyClass               uintptr
	hotKey                 uint32
	iconOrMonitor, process uintptr
}

var (
	pShellExecuteEx      = shell32.NewProc("ShellExecuteExW")
	pCoInitializeEx      = syscall.NewLazyDLL("ole32.dll").NewProc("CoInitializeEx")
	pWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	pGetExitCodeProcess  = kernel32.NewProc("GetExitCodeProcess")
)

var errAdminCancelled = errors.New("conferma di amministratore annullata")

// runAsAdmin runs exe with arg as administrator and waits for it. The
// request is tied to Ponte's window, so that Windows shows its confirmation
// in front instead of only flashing it in the taskbar.
func runAsAdmin(exe, arg string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pCoInitializeEx.Call(0, 0x2|0x4) // COINIT_APARTMENTTHREADED | COINIT_DISABLE_OLE1DDE
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	params, _ := syscall.UTF16PtrFromString(arg)
	in := shellExecuteInfo{mask: 0x40 | 0x100, hwnd: gui.mainHwnd, verb: verb, file: file, params: params} // SEE_MASK_NOCLOSEPROCESS | SEE_MASK_NOASYNC
	in.size = uint32(unsafe.Sizeof(in))
	if r, _, err := pShellExecuteEx.Call(uintptr(unsafe.Pointer(&in))); r == 0 {
		if err == syscall.Errno(1223) { // ERROR_CANCELLED
			return errAdminCancelled
		}
		return err
	}
	defer pCloseHandle.Call(in.process)
	pWaitForSingleObject.Call(in.process, 0xFFFFFFFF)
	var code uint32
	pGetExitCodeProcess.Call(in.process, uintptr(unsafe.Pointer(&code)))
	if code != 0 {
		return errors.New("non riuscito, i dettagli sono nel log")
	}
	return nil
}

// adminCommand runs the modes of the copy of Ponte started as
// administrator: --setup prepares the task and the service, --remove takes
// them away.
func adminCommand(arg string) error {
	if arg == "--remove" {
		defer forgetTaskState()
		if adminStartEnabled() {
			if out, err := schtasks("/Delete", "/F", "/TN", adminTask).CombinedOutput(); err != nil {
				return errors.New(strings.TrimSpace(string(out)))
			}
		}
		return uninstallService()
	}
	if err := ensureTask(); err != nil {
		return err
	}
	return ensureService()
}

// ensureTask creates the task, or recreates it when it starts another copy
// of Ponte or comes from an older Ponte, whose task also started it at
// sign-in: that is now the job of "Avvia con il computer" alone.
func ensureTask() error {
	defer forgetTaskState()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := schtasks("/Query", "/TN", adminTask, "/XML").Output()
	old := err == nil && contains(out, "LogonTrigger")
	if err == nil && !old && contains(out, xmlEscape(exe)) {
		return nil
	}
	f := filepath.Join(os.TempDir(), "ponte-task.xml")
	if err := os.WriteFile(f, taskXML(exe), 0o600); err != nil {
		return err
	}
	defer os.Remove(f)
	if out, err := schtasks("/Create", "/F", "/TN", adminTask, "/XML", f).CombinedOutput(); err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	if old {
		setAutostart(true) // it started with the computer through the old task
	}
	logf("attività per l'avvio come amministratore pronta")
	return nil
}

// contains looks for s in the output of schtasks, which may be UTF-16.
func contains(out []byte, s string) bool {
	if strings.Contains(string(out), s) {
		return true
	}
	var w []byte
	for _, c := range utf16.Encode([]rune(s)) {
		w = append(w, byte(c), byte(c>>8))
	}
	return bytes.Contains(out, w)
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// taskXML describes the task: run on request Ponte with the highest
// rights, on battery too and with no time limit (the defaults of schtasks
// stop it after 3 days and on battery).
func taskXML(exe string) []byte {
	user := os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME")
	esc := xmlEscape
	s := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Avvia Ponte come amministratore</Description></RegistrationInfo>
  <Principals><Principal id="Author"><UserId>` + esc(user) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>HighestAvailable</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>4</Priority>
  </Settings>
  <Actions Context="Author"><Exec><Command>` + esc(exe) + `</Command><Arguments>--background --after-update</Arguments></Exec></Actions>
</Task>
`
	u := utf16.Encode([]rune(s))
	b := []byte{0xFF, 0xFE}
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// restartAsAdmin starts Ponte again through the task, as administrator; the
// caller then quits.
func restartAsAdmin() error { return schtasks("/Run", "/TN", adminTask).Run() }

func relaunchStamp() string { return filepath.Join(configDir(), "riavvio-amministratore") }

// relaunchAsAdmin starts Ponte again through the task, as administrator;
// the caller then quits. show asks the new Ponte to open its window, which
// the task starts hidden. The stamp also keeps Ponte from trying in a loop
// when the task cannot give administrator rights: a Ponte run as
// administrator removes it.
func relaunchAsAdmin(show bool) error {
	body := []byte{}
	if show {
		body = []byte("mostra")
	}
	os.MkdirAll(configDir(), 0o700)
	os.WriteFile(relaunchStamp(), body, 0o600)
	return restartAsAdmin()
}

// startAsAdmin runs before a Ponte started normally goes on; true means it
// handed over to a Ponte run as administrator and must quit. It asks for
// administrator rights only the first time Ponte is opened by hand, not at
// sign-in, and not again once the user said no: the window then offers it.
func startAsAdmin(app *App, background bool) bool {
	if isElevated() {
		return false
	}
	if adminStartEnabled() {
		if st, err := os.Stat(relaunchStamp()); err == nil && time.Since(st.ModTime()) < time.Minute {
			return false // the task started this Ponte, still without rights
		}
		return relaunchAsAdmin(!background) == nil
	}
	if background || app.cfg.AdminDeclined {
		return false
	}
	if err := grantAdmin(); err != nil {
		if errors.Is(err, errAdminCancelled) {
			app.cfg.AdminDeclined = true
			app.cfg.save()
		}
		return false
	}
	return relaunchAsAdmin(true) == nil
}

// grantAdmin asks Windows for administrator rights and uses them to prepare
// the task and the service.
func grantAdmin() error {
	defer forgetTaskState()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return runAsAdmin(exe, "--setup")
}

// removeAdmin takes the task and the service away, and the start with the
// computer: what "Rimuovi Ponte da questo PC" does before Ponte quits.
func removeAdmin() error {
	var err error
	if isElevated() {
		err = adminCommand("--remove")
	} else if adminStartEnabled() || queryService() != "off" {
		var exe string
		if exe, err = os.Executable(); err == nil {
			err = runAsAdmin(exe, "--remove")
		}
	}
	if err != nil {
		return err
	}
	return setAutostart(false)
}

// setupAsAdmin runs in a Ponte started as administrator: it keeps the task
// and the service up to date, after an update too. When the task started
// it for a window opened by hand, it tells the caller to show the window.
func setupAsAdmin() (show bool) {
	if !isElevated() {
		return false
	}
	if b, err := os.ReadFile(relaunchStamp()); err == nil && string(b) == "mostra" {
		if st, err := os.Stat(relaunchStamp()); err == nil && time.Since(st.ModTime()) < time.Minute {
			show = true
		}
	}
	// Gone, so that a Ponte opened by hand soon after hands over again.
	os.Remove(relaunchStamp())
	go func() {
		if err := adminCommand("--setup"); err != nil {
			logf("preparazione come amministratore: %v", err)
		}
	}()
	return show
}
