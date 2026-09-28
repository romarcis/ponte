package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// On Windows the physical input is read with low-level hooks, which can also
// swallow it while the other computer is controlled, and it is replayed with
// SendInput.

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	pUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	pCallNextHookEx      = user32.NewProc("CallNextHookEx")
	pGetMessage          = user32.NewProc("GetMessageW")
	pPostThreadMessage   = user32.NewProc("PostThreadMessageW")
	pSetCursorPos        = user32.NewProc("SetCursorPos")
	pGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	pSendInput           = user32.NewProc("SendInput")
	pSetDpiAwareCtx      = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetDPIAware         = user32.NewProc("SetProcessDPIAware")
	pGetCurrentThreadId  = kernel32.NewProc("GetCurrentThreadId")
	pGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
)

const (
	whKeyboardLL = 13
	whMouseLL    = 14
	wmQuit       = 0x0012

	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmMouseWheel  = 0x020A
	wmXButtonDown = 0x020B
	wmXButtonUp   = 0x020C
	wmMouseHWheel = 0x020E
	wmKeyDown     = 0x0100
	wmKeyUp       = 0x0101
	wmSysKeyDown  = 0x0104
	wmSysKeyUp    = 0x0105

	llkhfExtended = 0x01
	llkhfInjected = 0x10
	llmhfInjected = 0x01

	smXVirtual  = 76
	smYVirtual  = 77
	smCXVirtual = 78
	smCYVirtual = 79

	vkPause   = 0x13
	vkNumLock = 0x90
)

func init() {
	if pSetDpiAwareCtx.Find() == nil {
		pSetDpiAwareCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	} else if pSetDPIAware.Find() == nil {
		pSetDPIAware.Call()
	}
}

type point struct{ x, y int32 }

type msllhook struct {
	pt        point
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

type kbdllhook struct {
	vk, scan, flags, time uint32
	extra                 uintptr
}

func metric(i uintptr) int {
	r, _, _ := pGetSystemMetrics.Call(i)
	return int(int32(r))
}

func virtualScreen() (x, y, w, h int) {
	return metric(smXVirtual), metric(smYVirtual), metric(smCXVirtual), metric(smCYVirtual)
}

// Extended scan codes (E0 xx) and their evdev codes.
var extToEvdev = map[uint32]uint16{
	0x1C: 96, 0x1D: 97, 0x35: 98, 0x37: 99, 0x38: 100, 0x47: 102, 0x48: 103, 0x49: 104,
	0x4B: 105, 0x4D: 106, 0x4F: 107, 0x50: 108, 0x51: 109, 0x52: 110, 0x53: 111,
	0x5B: 125, 0x5C: 126, 0x5D: 127, 0x20: 113, 0x2E: 114, 0x30: 115,
	0x19: 163, 0x10: 165, 0x24: 166, 0x22: 164, 0x5E: 116, 0x5F: 142,
}

var evdevToExt = func() map[uint16]uint32 {
	m := map[uint16]uint32{}
	for k, v := range extToEvdev {
		m[v] = k
	}
	return m
}()

func winKeyToEvdev(k *kbdllhook) uint16 {
	switch k.vk {
	case vkPause:
		return 119
	case vkNumLock:
		return 69
	}
	if k.scan > 0xFF {
		return 0 // the fake Ctrl that Windows sends along with AltGr
	}
	if k.flags&llkhfExtended != 0 {
		return extToEvdev[k.scan]
	}
	if k.scan >= 1 && k.scan <= 0x58 {
		return uint16(k.scan)
	}
	return 0
}

// ---------- capture ----------

type winCapture struct {
	ch       chan<- inputEvent
	grab     atomic.Bool
	cx, cy   atomic.Int32
	threadID uintptr
	done     chan struct{}
	down     map[uint32]bool // keys held, to tell repeats from presses
}

var activeCapture atomic.Pointer[winCapture]

func newCapture() inputCapture { return &winCapture{down: map[uint32]bool{}} }

func (c *winCapture) Start(ch chan<- inputEvent) error {
	c.ch = ch
	c.done = make(chan struct{})
	activeCapture.Store(c)
	started := make(chan error)
	go c.run(started)
	return <-started
}

func (c *winCapture) run(started chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(c.done)
	tid, _, _ := pGetCurrentThreadId.Call()
	c.threadID = tid
	mod, _, _ := pGetModuleHandle.Call(0)
	mh, _, err := pSetWindowsHookEx.Call(whMouseLL, mouseHookCB, mod, 0)
	if mh == 0 {
		started <- &setupError{msg: "Impossibile leggere il mouse", help: err.Error()}
		return
	}
	kh, _, err := pSetWindowsHookEx.Call(whKeyboardLL, keyHookCB, mod, 0)
	if kh == 0 {
		pUnhookWindowsHookEx.Call(mh)
		started <- &setupError{msg: "Impossibile leggere la tastiera", help: err.Error()}
		return
	}
	started <- nil
	var msg [48]byte
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
	}
	pUnhookWindowsHookEx.Call(kh)
	pUnhookWindowsHookEx.Call(mh)
}

func (c *winCapture) Stop() {
	c.SetGrab(false)
	pPostThreadMessage.Call(c.threadID, wmQuit, 0, 0)
	<-c.done
	activeCapture.CompareAndSwap(c, nil)
}

func (c *winCapture) Bounds() (int, int, int, int) { return virtualScreen() }
func (c *winCapture) EdgeSwitch() bool             { return true }

func (c *winCapture) SetGrab(on bool) {
	if on {
		// Park the hidden pointer mid-screen so motion never hits an edge.
		x, y, w, h := virtualScreen()
		c.cx.Store(int32(x + w/2))
		c.cy.Store(int32(y + h/2))
		pSetCursorPos.Call(uintptr(x+w/2), uintptr(y+h/2))
	}
	c.grab.Store(on)
}

func (c *winCapture) Warp(x, y int) { pSetCursorPos.Call(uintptr(x), uintptr(y)) }

func (c *winCapture) send(ev inputEvent) {
	select {
	case c.ch <- ev:
	default:
	}
}

var mouseHookCB = syscall.NewCallback(func(nCode, wParam, lParam uintptr) uintptr {
	c := activeCapture.Load()
	if int32(nCode) < 0 || c == nil {
		r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
		return r
	}
	m := (*msllhook)(unsafe.Pointer(lParam))
	grab := c.grab.Load()
	switch wParam {
	case wmMouseMove:
		if grab {
			cx, cy := c.cx.Load(), c.cy.Load()
			if m.pt.x != cx || m.pt.y != cy {
				c.send(inputEvent{kind: evRel, x: m.pt.x - cx, y: m.pt.y - cy})
			}
		} else {
			c.send(inputEvent{kind: evPos, x: m.pt.x, y: m.pt.y})
		}
	case wmLButtonDown, wmLButtonUp:
		c.send(inputEvent{kind: evButton, code: btnLeft, val: b2i(wParam == wmLButtonDown)})
	case wmRButtonDown, wmRButtonUp:
		c.send(inputEvent{kind: evButton, code: btnRight, val: b2i(wParam == wmRButtonDown)})
	case wmMButtonDown, wmMButtonUp:
		c.send(inputEvent{kind: evButton, code: btnMiddle, val: b2i(wParam == wmMButtonDown)})
	case wmXButtonDown, wmXButtonUp:
		b := uint16(btnBack)
		if m.mouseData>>16 == 2 {
			b = btnForward
		}
		c.send(inputEvent{kind: evButton, code: b, val: b2i(wParam == wmXButtonDown)})
	case wmMouseWheel:
		c.send(inputEvent{kind: evWheel, code: 0, val: int32(int16(m.mouseData >> 16))})
	case wmMouseHWheel:
		c.send(inputEvent{kind: evWheel, code: 1, val: int32(int16(m.mouseData >> 16))})
	}
	if grab {
		return 1
	}
	r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
})

var keyHookCB = syscall.NewCallback(func(nCode, wParam, lParam uintptr) uintptr {
	c := activeCapture.Load()
	if int32(nCode) < 0 || c == nil {
		r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
		return r
	}
	k := (*kbdllhook)(unsafe.Pointer(lParam))
	if k.flags&llkhfInjected == 0 {
		if code := winKeyToEvdev(k); code != 0 {
			state := int32(0)
			if wParam == wmKeyDown || wParam == wmSysKeyDown {
				state = 1
				if c.down[k.vk] {
					state = 2
				}
				c.down[k.vk] = true
			} else {
				delete(c.down, k.vk)
			}
			c.send(inputEvent{kind: evKey, code: code, val: state})
		}
	}
	if c.grab.Load() {
		return 1
	}
	r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
})

func b2i(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

// ---------- injection ----------

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseMove       = 0x0001
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseXDown      = 0x0080
	mouseXUp        = 0x0100
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000
	mouseVirtDesk   = 0x4000
	mouseAbsolute   = 0x8000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyScancode = 0x0008
)

// INPUT structures as laid out on 64-bit Windows (40 bytes).
type mouseInput struct {
	typ       uint32
	_         uint32
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

type keybdInput struct {
	typ   uint32
	_     uint32
	vk    uint16
	scan  uint16
	flags uint32
	time  uint32
	extra uintptr
	_     [8]byte
}

type winInjector struct {
	mu sync.Mutex
}

func newInjector() inputInjector { return &winInjector{} }

func (w *winInjector) Start() error { return nil }
func (w *winInjector) Close()       {}

func (w *winInjector) ScreenSize() (int, int) {
	_, _, sw, sh := virtualScreen()
	return sw, sh
}

func sendMouse(in mouseInput) {
	in.typ = inputMouse
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func (w *winInjector) MouseAbs(x, y int) {
	_, _, sw, sh := virtualScreen()
	nx := int32(x * 65535 / max(sw-1, 1))
	ny := int32(y * 65535 / max(sh-1, 1))
	sendMouse(mouseInput{dx: nx, dy: ny, flags: mouseMove | mouseAbsolute | mouseVirtDesk})
}

func (w *winInjector) Button(b uint8, down bool) {
	var in mouseInput
	switch b {
	case btnLeft:
		in.flags = pick(down, mouseLeftDown, mouseLeftUp)
	case btnRight:
		in.flags = pick(down, mouseRightDown, mouseRightUp)
	case btnMiddle:
		in.flags = pick(down, mouseMiddleDown, mouseMiddleUp)
	case btnBack, btnForward:
		in.flags = pick(down, mouseXDown, mouseXUp)
		in.mouseData = 1
		if b == btnForward {
			in.mouseData = 2
		}
	default:
		return
	}
	sendMouse(in)
}

func pick(c bool, a, b uint32) uint32 {
	if c {
		return a
	}
	return b
}

func (w *winInjector) Wheel(axis uint8, delta int) {
	f := uint32(mouseWheel)
	if axis == 1 {
		f = mouseHWheel
	}
	sendMouse(mouseInput{mouseData: uint32(int32(delta)), flags: f})
}

func (w *winInjector) Key(code uint16, state uint8) {
	in := keybdInput{typ: inputKeyboard}
	switch {
	case code == 119:
		in.vk = vkPause
	case code == 69:
		in.vk = vkNumLock
		in.flags = keyExtended
	case evdevToExt[code] != 0:
		in.scan = uint16(evdevToExt[code])
		in.flags = keyScancode | keyExtended
	case code >= 1 && code <= 0x58:
		in.scan = code
		in.flags = keyScancode
	default:
		return
	}
	if state == 0 {
		in.flags |= keyUp
	}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}
