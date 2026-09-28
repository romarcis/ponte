package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// On Windows the physical input is read with low-level hooks, which can also
// swallow it while the other computer is controlled, and it is replayed with
// SendInput.

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pSetWindowsHookEx     = user32.NewProc("SetWindowsHookExW")
	pUnhookWindowsHookEx  = user32.NewProc("UnhookWindowsHookEx")
	pCallNextHookEx       = user32.NewProc("CallNextHookEx")
	pGetMessage           = user32.NewProc("GetMessageW")
	pPostThreadMessage    = user32.NewProc("PostThreadMessageW")
	pSetCursorPos         = user32.NewProc("SetCursorPos")
	pGetSystemMetrics     = user32.NewProc("GetSystemMetrics")
	pGetCursorInfo        = user32.NewProc("GetCursorInfo")
	pGetForegroundWindow  = user32.NewProc("GetForegroundWindow")
	pGetWindowText        = user32.NewProc("GetWindowTextW")
	pGetClassName         = user32.NewProc("GetClassNameW")
	pSystemParametersInfo = user32.NewProc("SystemParametersInfoW")
	pSendInput            = user32.NewProc("SendInput")
	pSetDpiAwareCtx       = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetDPIAware          = user32.NewProc("SetProcessDPIAware")
	pGetCurrentThreadId   = kernel32.NewProc("GetCurrentThreadId")
	pGetModuleHandle      = kernel32.NewProc("GetModuleHandleW")
	pGetTickCount         = kernel32.NewProc("GetTickCount")
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

	smCXScreen  = 0
	smCYScreen  = 1
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

var modByVK = map[uint32]uint16{
	0xA0: 42, 0xA1: 54, 0x14: 58, // left Shift, right Shift, Caps Lock
	0xA2: 29, 0xA3: 97, 0xA4: 56, 0xA5: 100, // left/right Ctrl, left/right Alt
	0x5B: 125, 0x5C: 126, // left/right Windows
}

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
	// Modifiers by virtual key: their extended flag is not reliable (the
	// right Shift, for one, can come flagged as extended and was lost).
	if code := modByVK[k.vk]; code != 0 {
		return code
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
	moved    atomic.Uint32 // GetTickCount when Ponte last moved the pointer
	threadID uintptr
	done     chan struct{}
	down     map[uint32]bool // keys held, to tell repeats from presses
	unknown  map[uint32]bool // keys not sent, already logged
}

var activeCapture atomic.Pointer[winCapture]

func newCapture() inputCapture { return &winCapture{down: map[uint32]bool{}, unknown: map[uint32]bool{}} }

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
		// Park the pointer mid-screen so motion never hits an edge. The
		// middle of the main monitor is always a real place: the middle of
		// all monitors together may not be (screens of different sizes), and
		// Windows would then put the pointer elsewhere and every movement
		// would look like a jump.
		pSetCursorPos.Call(uintptr(metric(smCXScreen)/2), uintptr(metric(smCYScreen)/2))
		var pt point
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		c.cx.Store(pt.x)
		c.cy.Store(pt.y)
	}
	c.grab.Store(on)
	c.markMoved()
}

func (c *winCapture) Warp(x, y int) {
	pSetCursorPos.Call(uintptr(x), uintptr(y))
	c.markMoved()
}

func (c *winCapture) markMoved() {
	t, _, _ := pGetTickCount.Call()
	c.moved.Store(uint32(t))
}

// stale tells whether a mouse event was generated before Ponte last moved
// the pointer. Windows computed its position from the old place: right
// after the pointer is parked mid-screen, a motion made at the screen edge
// would count as a jump of half a screen and throw the pointer into a corner
// of the other computer.
func (c *winCapture) stale(m *msllhook) bool {
	d := int32(m.time - c.moved.Load())
	return d <= 0 && d > -1000
}

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
		if c.stale(m) {
			return 1
		}
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
		} else if (wParam == wmKeyDown || wParam == wmSysKeyDown) && !c.unknown[k.vk] {
			c.unknown[k.vk] = true // once per key, not at every repeat
			logf("tasto non riconosciuto, non inviato: vk %#x scan %#x flag %#x", k.vk, k.scan, k.flags)
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
	mu     sync.Mutex
	forced bool      // MouseKeys turned on by Ponte to show the pointer
	saved  mouseKeys // the user's MouseKeys settings, restored afterwards
	active bool      // this computer is being controlled
	shown  bool      // the pointer was visible at the last check

	typing      atomic.Bool  // the last input replayed was a key
	movingSince atomic.Int64 // when the pointer started moving after typing (unix ns)
}

func newInjector() inputInjector { return &winInjector{} }

func (w *winInjector) Start() error { return nil }
func (w *winInjector) Close()       { w.ShowCursor(false) }

// MOUSEKEYS
type mouseKeys struct {
	size, flags, maxSpeed, timeToMax, ctrlSpeed, res1, res2 uint32
}

const (
	spiGetMouseKeys  = 0x0036
	spiSetMouseKeys  = 0x0037
	mkfMouseKeysOn   = 0x01
	mkfAvailable     = 0x02
	smMousePresent   = 19
	cursorSuppressed = 0x02
)

// cursorInfo is CURSORINFO.
type cursorInfo struct {
	size, flags uint32
	cursor      uintptr
	pt          point
}

// pointerHidden tells whether Windows is not drawing the pointer: it does
// that when no mouse is connected (a PC whose only mouse is the one on the
// other computer) or after touch input.
func pointerHidden() bool {
	if metric(smMousePresent) == 0 {
		return true
	}
	ci := cursorInfo{}
	ci.size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r != 0 {
		return ci.flags&cursorSuppressed != 0
	}
	return false
}

// ShowCursor makes the pointer visible while this computer is controlled.
// Windows shows it only if it believes a mouse is present, so Ponte turns on
// MouseKeys (the accessibility option that moves the pointer with the
// numeric keypad) for as long as it is needed, the way Synergy and Barrier
// do, and then puts the user's setting back. The change is not saved, so a
// crash undoes it at the next sign-in.
func (w *winInjector) ShowCursor(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active, w.shown = on, true
	if on && pointerHidden() {
		w.forceCursor("puntatore nascosto da Windows (nessun mouse collegato?): lo mostro con i Tasti del mouse")
	} else if !on {
		w.restoreCursor()
	}
}

func (w *winInjector) forceCursor(why string) {
	if w.forced {
		return
	}
	{
		w.saved = mouseKeys{}
		w.saved.size = uint32(unsafe.Sizeof(w.saved))
		if r, _, _ := pSystemParametersInfo.Call(spiGetMouseKeys, uintptr(w.saved.size), uintptr(unsafe.Pointer(&w.saved)), 0); r == 0 {
			return
		}
		mk := w.saved
		mk.flags |= mkfMouseKeysOn | mkfAvailable
		if r, _, err := pSystemParametersInfo.Call(spiSetMouseKeys, uintptr(mk.size), uintptr(unsafe.Pointer(&mk)), 0); r != 0 {
			w.forced = true
			logf("%s", why)
		} else {
			logf("impossibile attivare i Tasti del mouse: %v", err)
		}
	}
}

func (w *winInjector) restoreCursor() {
	if !w.forced {
		return
	}
	pSystemParametersInfo.Call(spiSetMouseKeys, uintptr(w.saved.size), uintptr(unsafe.Pointer(&w.saved)), 0)
	w.forced = false
}

// CheckCursor watches the pointer while this computer is controlled. When
// Windows stops drawing it, the log says what was going on (which window was
// in front, whether a mouse is seen, the Tasti del mouse state) and Ponte
// turns on the Tasti del mouse even if Windows says a mouse is present.
func (w *winInjector) CheckCursor() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return
	}
	ci := cursorInfo{}
	ci.size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return
	}
	shown := ci.flags&1 != 0 // CURSOR_SHOWING
	if !shown && (w.typing.Load() || time.Since(time.Unix(0, w.movingSince.Load())) < 300*time.Millisecond) {
		// Windows hides the pointer while typing and shows it again at the
		// next move: nothing to fix. Turning on the Tasti del mouse here
		// only got in the way of the keyboard (capital letters stopped
		// working).
		return
	}
	if shown == w.shown {
		return
	}
	w.shown = shown
	if shown {
		logf("   puntatore di nuovo visibile in %d,%d", ci.pt.x, ci.pt.y)
		return
	}
	mk := mouseKeys{}
	mk.size = uint32(unsafe.Sizeof(mk))
	pSystemParametersInfo.Call(spiGetMouseKeys, uintptr(mk.size), uintptr(unsafe.Pointer(&mk)), 0)
	logf("   PUNTATORE NASCOSTO in %d,%d (flag %d); mouse collegato: %v; Tasti del mouse: %#x (attivati da Ponte: %v); in primo piano: %s",
		ci.pt.x, ci.pt.y, ci.flags, metric(smMousePresent) != 0, mk.flags, w.forced, foregroundWindow())
	if !w.forced {
		w.forceCursor("   lo mostro con i Tasti del mouse")
	}
}

// foregroundWindow describes the window in front, for the log.
func foregroundWindow() string {
	h, _, _ := pGetForegroundWindow.Call()
	if h == 0 {
		return "nessuna finestra (schermata di blocco o desktop protetto?)"
	}
	var title, class [256]uint16
	pGetWindowText.Call(h, uintptr(unsafe.Pointer(&title[0])), 256)
	pGetClassName.Call(h, uintptr(unsafe.Pointer(&class[0])), 256)
	return fmt.Sprintf("%q (%s)", syscall.UTF16ToString(title[:]), syscall.UTF16ToString(class[:]))
}

func (w *winInjector) ScreenSize() (int, int) {
	_, _, sw, sh := virtualScreen()
	return sw, sh
}

func sendMouse(in mouseInput) {
	in.typ = inputMouse
	sendInput(unsafe.Pointer(&in), unsafe.Sizeof(in))
}

func (w *winInjector) MouseAbs(x, y int) {
	if w.typing.Swap(false) || w.movingSince.Load() == 0 {
		w.movingSince.Store(time.Now().UnixNano())
	}
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

var vkByCode = map[uint16]uint16{42: 0xA0, 54: 0xA1, 58: 0x14} // left Shift, right Shift, Caps Lock

func (w *winInjector) Key(code uint16, state uint8) {
	w.typing.Store(true)
	in := keybdInput{typ: inputKeyboard}
	switch {
	case vkByCode[code] != 0:
		// Shift and Caps Lock go with their virtual key too, so the
		// accessibility options (Tasti del mouse, Tasti permanenti) that
		// watch them recognise them.
		in.vk = vkByCode[code]
		in.scan = code
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
	sendInput(unsafe.Pointer(&in), unsafe.Sizeof(in))
}

var inputBlocked atomic.Bool

// sendInput replays one event. Windows refuses it on the lock screen and
// while an administrator prompt is shown; the log tells when.
func sendInput(in unsafe.Pointer, size uintptr) {
	if n, _, _ := pSendInput.Call(1, uintptr(in), size); n == 0 {
		if !inputBlocked.Swap(true) {
			logf("Windows non accetta il mouse e la tastiera di Ponte (schermata di blocco o richiesta di amministratore?)")
		}
	} else if inputBlocked.Swap(false) {
		logf("Windows accetta di nuovo il mouse e la tastiera di Ponte")
	}
}

// logCursor writes to the log whether Windows shows the pointer, a moment
// after it arrived from the other computer.
func logCursor() {
	time.Sleep(300 * time.Millisecond)
	var ci struct {
		size, flags uint32
		cursor      uintptr
		pt          point
	}
	ci.size = uint32(unsafe.Sizeof(ci))
	pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	state := map[uint32]string{0: "nascosto", 1: "visibile", 2: "nascosto da Windows (tocco o penna)"}[ci.flags]
	mouse := "no"
	if metric(19) != 0 { // SM_MOUSEPRESENT
		mouse = "sì"
	}
	logf("   puntatore %s in %d,%d; mouse fisico collegato: %s", state, ci.pt.x, ci.pt.y, mouse)
}
