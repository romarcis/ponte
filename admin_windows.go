package main

import (
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
// as administrator too. Ponte can start that way at sign-in through a
// scheduled task, which Windows runs without asking each time; creating it
// asks for confirmation once.

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

// setAdminStart creates or removes the task, through a copy of Ponte run as
// administrator unless this one already is.
func setAdminStart(on bool) error {
	defer forgetTaskState()
	arg := "--admin-start=off"
	if on {
		arg = "--admin-start=on"
	}
	if isElevated() {
		return adminStartCommand(arg)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return runAsAdmin(exe, arg)
}

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
			return errors.New("conferma di amministratore annullata")
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

// adminStartCommand runs in the copy of Ponte started as administrator.
func adminStartCommand(arg string) error {
	defer forgetTaskState()
	switch arg {
	case "--admin-start=off":
		if !adminStartEnabled() {
			return nil
		}
		if err := schtasks("/Delete", "/F", "/TN", adminTask).Run(); err != nil {
			return err
		}
		logf("avvio come amministratore disattivato")
	case "--admin-start=on":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		f := filepath.Join(os.TempDir(), "ponte-task.xml")
		if err := os.WriteFile(f, taskXML(exe), 0o600); err != nil {
			return err
		}
		defer os.Remove(f)
		if out, err := schtasks("/Create", "/F", "/TN", adminTask, "/XML", f).CombinedOutput(); err != nil {
			return errors.New(strings.TrimSpace(string(out)))
		}
		regCmd("delete", runKey, "/v", "Ponte", "/f").Run() // the task replaces it
		logf("avvio come amministratore attivato")
	}
	return nil
}

// taskXML describes the task: at this user's sign-in, run Ponte with the
// highest rights, on battery too and with no time limit (the defaults of
// schtasks stop it after 3 days and on battery).
func taskXML(exe string) []byte {
	user := os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME")
	esc := func(s string) string {
		var b strings.Builder
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	s := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Avvia Ponte come amministratore all'accesso</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + esc(user) + `</UserId></LogonTrigger></Triggers>
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

// relaunchAsAdmin starts Ponte again through the task when the option is on
// but this Ponte was started normally (by hand, after an update...): it
// could then not control programs run as administrator. A stamp keeps it
// from trying in a loop when the task cannot give administrator rights.
func relaunchAsAdmin() bool {
	if isElevated() || !adminStartEnabled() {
		return false
	}
	stamp := filepath.Join(configDir(), "riavvio-amministratore")
	if st, err := os.Stat(stamp); err == nil && time.Since(st.ModTime()) < time.Minute {
		return false
	}
	os.MkdirAll(configDir(), 0o700)
	os.WriteFile(stamp, nil, 0o600)
	return restartAsAdmin() == nil
}
