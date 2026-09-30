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
	// evMotion: the physical mouse moved while not grabbed. A computer
	// being controlled from another one takes its mouse back on it.
	evMotion = 6
	// evPaste: Ctrl+V pressed here while files copied on another computer
	// are on offer. The capture held the V back; the files come first.
	evPaste = 7
)

const keyScrollLock = 70 // hotkey: jump to the next computer

const keyV = 47

const (
	keyEsc       = 1
	keyLeftCtrl  = 29
	keyRightCtrl = 97
	keyLeftAlt   = 56
	keyRightAlt  = 100
)

// peerTimeout is how long another computer may stay silent before it is
// considered gone (each side pings every second).
const peerTimeout = 3 * time.Second

// inputCapture reads this computer's physical mouse and keyboard; what
// Ponte itself replays is not reported.
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

// pasteHolder is a capture that can hold back Ctrl+V while armed reports
// that clipboard content from another computer is on offer, sending evPaste
// instead (Windows).
type pasteHolder interface {
	HoldPaste(armed func() bool)
}

// inputInjector replays input when another computer controls this one.
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
	// CheckCursor is called often while controlled: it notices when the
	// pointer becomes hidden, logs why, and shows it again.
	CheckCursor()
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
