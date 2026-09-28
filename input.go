package main

import "time"

// Platform-neutral input events. Key codes are Linux evdev codes on every
// platform, so both sides agree on physical keys regardless of layout.
type inputEvent struct {
	kind uint8
	x, y int32  // evPos: absolute position; evRel: delta
	code uint16 // evKey: key code; evButton: button; evWheel: axis
	val  int32  // evKey: 0 up, 1 down, 2 repeat; evButton: 0/1; evWheel: delta (120 = notch)
}

const (
	evPos    = 1 // local pointer position, reported while not grabbed
	evRel    = 2 // pointer motion, reported while grabbed
	evButton = 3
	evWheel  = 4
	evKey    = 5
)

const keyScrollLock = 70 // hotkey: jump between the two computers

const (
	keyEsc       = 1
	keyLeftCtrl  = 29
	keyRightCtrl = 97
	keyLeftAlt   = 56
	keyRightAlt  = 100
)

// peerTimeout is how long the other computer may stay silent before it is
// considered gone (it answers a ping every second).
const peerTimeout = 3 * time.Second

// inputCapture reads the physical mouse and keyboard of the sharing computer.
type inputCapture interface {
	Start(ch chan<- inputEvent) error
	Stop()
	Bounds() (x, y, w, h int)
	// EdgeSwitch reports whether the pointer position is known, so that
	// moving past the screen edge can switch computers.
	EdgeSwitch() bool
	// SetGrab stops (or restores) delivering input to the local computer.
	SetGrab(on bool)
	Warp(x, y int)
}

// inputInjector replays input on the controlled computer.
type inputInjector interface {
	Start() error
	Close()
	ScreenSize() (w, h int)
	MouseAbs(x, y int)
	Button(b uint8, down bool)
	Wheel(axis uint8, delta int)
	Key(code uint16, state uint8)
}

// cursorShower is an injector that can make the pointer visible while this
// computer is being controlled. Windows hides it when no mouse is plugged in.
type cursorShower interface {
	ShowCursor(on bool)
}

// setupError is a problem the user can fix; help explains how.
type setupError struct {
	msg  string
	help string
}

func (e *setupError) Error() string { return e.msg }

// Replaced in tests.
var (
	makeCapture  = newCapture
	makeInjector = newInjector
)
