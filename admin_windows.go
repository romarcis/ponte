package main

import (
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
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
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"$p = Start-Process -FilePath "+q(exe)+" -ArgumentList "+q(arg)+" -Verb RunAs -Wait -PassThru; exit $p.ExitCode")
	hideConsole(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "cancel") || strings.Contains(string(out), "annullat") {
			return errors.New("conferma di amministratore annullata")
		}
		return errors.New("conferma di amministratore non data o non riuscita")
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
