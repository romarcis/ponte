//go:build !linux && !windows

package main

import (
	"errors"
	"os/exec"
)

// macOS and other systems are not supported yet: the window opens, but
// sharing or receiving reports that the system is not supported.

var errUnsupported = &setupError{
	msg:  "Questo sistema operativo non è ancora supportato",
	help: "Ponte funziona per ora su Windows e Linux.",
}

func newCapture() inputCapture   { return unsupportedCapture{} }
func newInjector() inputInjector { return unsupportedInjector{} }

type unsupportedCapture struct{}

func (unsupportedCapture) Start(chan<- inputEvent) error { return errUnsupported }
func (unsupportedCapture) Stop()                         {}
func (unsupportedCapture) Bounds() (int, int, int, int)  { return 0, 0, 1, 1 }
func (unsupportedCapture) EdgeSwitch() bool              { return false }
func (unsupportedCapture) SetGrab(bool)                  {}
func (unsupportedCapture) Warp(int, int)                 {}

type unsupportedInjector struct{}

func (unsupportedInjector) Start() error           { return errUnsupported }
func (unsupportedInjector) Close()                 {}
func (unsupportedInjector) ScreenSize() (int, int) { return 1, 1 }
func (unsupportedInjector) MouseAbs(int, int)      {}
func (unsupportedInjector) Button(uint8, bool)     {}
func (unsupportedInjector) Wheel(uint8, int)       {}
func (unsupportedInjector) Key(uint16, uint8)      {}

func hideConsole(*exec.Cmd)   {}
func autostartEnabled() bool  { return false }
func setAutostart(bool) error { return errors.New("non supportato") }
func canFixPermissions() bool { return false }
func fixPermissions() error   { return errors.New("non supportato") }

func logCursor() {}

func refusedReason() string { return "" }
