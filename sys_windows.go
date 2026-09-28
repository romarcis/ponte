package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func init() {
	if unsafe.Sizeof(mouseInput{}) != 40 || unsafe.Sizeof(keybdInput{}) != 40 {
		panic("Ponte supporta solo Windows a 64 bit")
	}
}

func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

func regCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("reg", args...)
	hideConsole(cmd)
	return cmd
}

func autostartEnabled() bool {
	return regCmd("query", runKey, "/v", "Ponte").Run() == nil || adminStartEnabled()
}

func setAutostart(on bool) error {
	if !on {
		regCmd("delete", runKey, "/v", "Ponte", "/f").Run()
		if adminStartEnabled() {
			return setAdminStart(false)
		}
		return nil
	}
	if adminStartEnabled() {
		return nil // the administrator task already starts Ponte
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return regCmd("add", runKey, "/v", "Ponte", "/t", "REG_SZ", "/d", `"`+exe+`" --background`, "/f").Run()
}

func canFixPermissions() bool { return false }

func fixPermissions() error { return errors.New("non necessario su Windows") }
