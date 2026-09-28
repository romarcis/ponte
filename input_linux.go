package main

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// On Linux the physical devices are read from /dev/input (evdev) and input
// is replayed through virtual devices (/dev/uinput). This works on X11 and
// Wayland alike. The pointer position, needed to switch at the screen edge,
// comes from X11.

const (
	evSyn  = 0x00
	evKeyT = 0x01
	evRelT = 0x02
	evAbsT = 0x03

	relX          = 0x00
	relY          = 0x01
	relHWheel     = 0x06
	relWheel      = 0x08
	relWheelHi    = 0x0b
	relHWheelHi   = 0x0c
	absX          = 0x00
	absY          = 0x01
	btnLeftCode   = 0x110
	btnTouch      = 0x14a
	btnToolFinger = 0x145
	btnToolDouble = 0x14d

	eviocgrab = 0x40044590
)

func eviocgbit(ev, n uintptr) uintptr { return 0x80000000 | n<<16 | 0x45<<8 | (0x20 + ev) }
func eviocgabs(abs uintptr) uintptr   { return 0x80000000 | 24<<16 | 0x45<<8 | (0x40 + abs) }
func eviocgname(n uintptr) uintptr    { return 0x80000000 | n<<16 | 0x45<<8 | 0x06 }

var inputEventSize = int(unsafe.Sizeof(syscall.Timeval{})) + 8

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

func fileIoctl(f *os.File, req, arg uintptr) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	if err := rc.Control(func(fd uintptr) { ierr = ioctl(fd, req, arg) }); err != nil {
		return err
	}
	return ierr
}

func testBit(bits []byte, n int) bool { return n/8 < len(bits) && bits[n/8]&(1<<(n%8)) != 0 }

const permHelp = "Ponte deve poter leggere e simulare mouse e tastiera. Premi «Risolvi» (ti verrà chiesta la password), oppure esegui nel terminale:\n\n" +
	"echo uinput | sudo tee /etc/modules-load.d/ponte.conf\n" +
	"printf 'KERNEL==\"uinput\", SUBSYSTEM==\"misc\", OPTIONS+=\"static_node=uinput\", TAG+=\"uaccess\"\\nSUBSYSTEM==\"input\", KERNEL==\"event*\", TAG+=\"uaccess\"\\n' | sudo tee /etc/udev/rules.d/70-ponte.rules\n" +
	"sudo modprobe uinput && sudo udevadm control --reload-rules && sudo udevadm trigger"

// ---------- X11 helper ----------

type x11 struct {
	mu   sync.Mutex
	c    *xgb.Conn
	root xproto.Window
	w, h int
}

func openX11() *x11 {
	if os.Getenv("DISPLAY") == "" {
		return nil
	}
	c, err := xgb.NewConn()
	if err != nil {
		logf("X11 non disponibile: %v", err)
		return nil
	}
	scr := xproto.Setup(c).DefaultScreen(c)
	return &x11{c: c, root: scr.Root, w: int(scr.WidthInPixels), h: int(scr.HeightInPixels)}
}

func (x *x11) refreshSize() {
	g, err := xproto.GetGeometry(x.c, xproto.Drawable(x.root)).Reply()
	if err == nil {
		x.mu.Lock()
		x.w, x.h = int(g.Width), int(g.Height)
		x.mu.Unlock()
	}
}

func (x *x11) size() (int, int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.w, x.h
}

func (x *x11) pointer() (int, int, bool) {
	r, err := xproto.QueryPointer(x.c, x.root).Reply()
	if err != nil {
		return 0, 0, false
	}
	return int(r.RootX), int(r.RootY), true
}

func (x *x11) warp(px, py int) {
	xproto.WarpPointer(x.c, 0, x.root, 0, 0, 0, 0, int16(px), int16(py))
	x.c.Sync()
}

func onWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland"
}

// ---------- capture ----------

type linuxCapture struct {
	mu     sync.Mutex
	devs   map[string]*evdev
	grab   bool
	ch     chan<- inputEvent
	stop   chan struct{}
	x      *x11
	edgeOK bool
	wg     sync.WaitGroup
}

type evdev struct {
	path    string
	f       *os.File
	hires   bool
	touch   bool
	scale   float64
	fingers int
}

func newCapture() inputCapture {
	return &linuxCapture{devs: map[string]*evdev{}, stop: make(chan struct{})}
}

func (c *linuxCapture) Start(ch chan<- inputEvent) error {
	c.ch = ch
	if n, denied := c.scan(); n == 0 {
		if denied {
			return &setupError{msg: "Ponte non ha il permesso di leggere mouse e tastiera", help: permHelp}
		}
		return &setupError{msg: "Nessun mouse o tastiera trovato", help: "Collega un mouse e una tastiera e riprova."}
	}
	c.x = openX11()
	c.edgeOK = c.x != nil && !onWayland()
	if !c.edgeOK {
		logf("posizione del puntatore non disponibile: si passa da un computer all'altro con Bloc Scorr")
	}
	c.wg.Add(1)
	go c.poll()
	return nil
}

func (c *linuxCapture) Stop() {
	close(c.stop)
	c.SetGrab(false)
	c.mu.Lock()
	for _, d := range c.devs {
		d.f.Close()
	}
	c.mu.Unlock()
	c.wg.Wait()
	if c.x != nil {
		c.x.c.Close()
	}
}

func (c *linuxCapture) EdgeSwitch() bool { return c.edgeOK }

func (c *linuxCapture) Bounds() (int, int, int, int) {
	if c.x == nil {
		return 0, 0, 1920, 1080
	}
	w, h := c.x.size()
	return 0, 0, w, h
}

func (c *linuxCapture) SetGrab(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.grab = on
	arg := uintptr(0)
	if on {
		arg = 1
	}
	for _, d := range c.devs {
		fileIoctl(d.f, eviocgrab, arg)
	}
}

func (c *linuxCapture) grabbed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.grab
}

func (c *linuxCapture) Warp(x, y int) {
	if c.x != nil {
		c.x.warp(x, y)
	}
}

func (c *linuxCapture) poll() {
	defer c.wg.Done()
	t := time.NewTicker(15 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		if i%200 == 0 {
			c.scan() // pick up devices plugged in later
			if c.x != nil {
				c.x.refreshSize()
			}
		}
		if c.edgeOK && !c.grabbed() {
			if x, y, ok := c.x.pointer(); ok {
				c.send(inputEvent{kind: evPos, x: int32(x), y: int32(y)})
			}
		}
	}
}

func (c *linuxCapture) send(ev inputEvent) {
	select {
	case c.ch <- ev:
	default:
	}
}

// scan opens new input devices. It returns how many devices are in use and
// whether some could not be opened for lack of permission.
func (c *linuxCapture) scan() (int, bool) {
	paths, _ := filepath.Glob("/dev/input/event*")
	denied := false
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range paths {
		if _, ok := c.devs[p]; ok {
			continue
		}
		f, err := os.OpenFile(p, os.O_RDONLY, 0)
		if err != nil {
			if errors.Is(err, os.ErrPermission) {
				denied = true
			}
			continue
		}
		d := &evdev{path: p, f: f}
		if !classify(d) {
			f.Close()
			continue
		}
		if c.grab {
			fileIoctl(f, eviocgrab, 1)
		}
		c.devs[p] = d
		c.wg.Add(1)
		go c.read(d)
	}
	return len(c.devs), denied
}

// classify keeps mice, keyboards and touchpads, and skips Ponte's own
// virtual devices.
func classify(d *evdev) bool {
	name := make([]byte, 256)
	fileIoctl(d.f, eviocgname(256), uintptr(unsafe.Pointer(&name[0])))
	if strings.HasPrefix(string(name), "Ponte") {
		return false
	}
	evb := make([]byte, 4)
	keyb := make([]byte, 96)
	relb := make([]byte, 4)
	absb := make([]byte, 8)
	fileIoctl(d.f, eviocgbit(0, 4), uintptr(unsafe.Pointer(&evb[0])))
	fileIoctl(d.f, eviocgbit(evKeyT, 96), uintptr(unsafe.Pointer(&keyb[0])))
	fileIoctl(d.f, eviocgbit(evRelT, 4), uintptr(unsafe.Pointer(&relb[0])))
	fileIoctl(d.f, eviocgbit(evAbsT, 8), uintptr(unsafe.Pointer(&absb[0])))
	mouse := testBit(evb, evRelT) && testBit(relb, relX) && testBit(relb, relY)
	kbd := testBit(evb, evKeyT) && testBit(keyb, 30) && testBit(keyb, 57)
	touch := testBit(evb, evAbsT) && testBit(absb, absX) && testBit(keyb, btnToolFinger) && testBit(keyb, btnTouch)
	if touch {
		d.touch = true
		var ai [6]int32
		fileIoctl(d.f, eviocgabs(absX), uintptr(unsafe.Pointer(&ai[0])))
		if ai[5] > 0 {
			d.scale = 12 / float64(ai[5]) // about 12 pixels per millimetre
		} else if ai[2] > ai[1] {
			d.scale = 1200 / float64(ai[2]-ai[1])
		} else {
			d.scale = 1
		}
	}
	return mouse || kbd || touch
}

func accel(dx, dy float64) (float64, float64) {
	v := math.Hypot(dx, dy)
	f := 1 + min(max((v-2)/8, 0), 1.5)
	return dx * f, dy * f
}

func (c *linuxCapture) read(d *evdev) {
	defer c.wg.Done()
	defer func() {
		c.mu.Lock()
		if c.devs[d.path] == d {
			delete(c.devs, d.path)
		}
		c.mu.Unlock()
		d.f.Close()
	}()
	buf := make([]byte, inputEventSize*64)
	var dx, dy float64
	var tx, ty, lastX, lastY int32
	touching, haveLast := false, false
	var scrollAcc float64
	off := inputEventSize - 8
	for {
		n, err := d.f.Read(buf)
		if err != nil {
			return
		}
		for i := 0; i+inputEventSize <= n; i += inputEventSize {
			e := buf[i+off : i+inputEventSize]
			typ := binary.LittleEndian.Uint16(e[0:])
			code := binary.LittleEndian.Uint16(e[2:])
			val := int32(binary.LittleEndian.Uint32(e[4:]))
			switch typ {
			case evKeyT:
				switch {
				case code == btnTouch:
					touching = val != 0
					haveLast = false
				case code == btnToolDouble:
					if val != 0 {
						d.fingers = 2
					} else {
						d.fingers = 1
					}
					haveLast = false
				case code >= btnLeftCode && code <= 0x117:
					if b := evdevToButton(code); b != 0 && val != 2 {
						c.send(inputEvent{kind: evButton, code: uint16(b), val: val})
					}
				case code < 0x100:
					c.send(inputEvent{kind: evKey, code: code, val: val})
				}
			case evRelT:
				switch code {
				case relX:
					dx += float64(val)
				case relY:
					dy += float64(val)
				case relWheelHi:
					d.hires = true
					c.send(inputEvent{kind: evWheel, code: 0, val: val})
				case relHWheelHi:
					d.hires = true
					c.send(inputEvent{kind: evWheel, code: 1, val: val})
				case relWheel:
					if !d.hires {
						c.send(inputEvent{kind: evWheel, code: 0, val: val * 120})
					}
				case relHWheel:
					if !d.hires {
						c.send(inputEvent{kind: evWheel, code: 1, val: val * 120})
					}
				}
			case evAbsT:
				if d.touch && code == absX {
					tx = val
				} else if d.touch && code == absY {
					ty = val
				}
			case evSyn:
				if d.touch && touching {
					if haveLast {
						mx := float64(tx-lastX) * d.scale
						my := float64(ty-lastY) * d.scale
						if d.fingers == 2 {
							scrollAcc -= my * 4
							if math.Abs(scrollAcc) >= 30 {
								c.send(inputEvent{kind: evWheel, code: 0, val: int32(scrollAcc)})
								scrollAcc = 0
							}
						} else {
							dx += mx
							dy += my
						}
					}
					lastX, lastY, haveLast = tx, ty, true
				}
				if dx != 0 || dy != 0 {
					if c.grabbed() {
						ax, ay := accel(dx, dy)
						c.send(inputEvent{kind: evRel, x: int32(math.Round(ax)), y: int32(math.Round(ay))})
					} else {
						c.send(inputEvent{kind: evMotion})
					}
					dx, dy = 0, 0
				}
			}
		}
	}
}

func evdevToButton(code uint16) uint8 {
	switch code {
	case 0x110:
		return btnLeft
	case 0x111:
		return btnRight
	case 0x112:
		return btnMiddle
	case 0x113, 0x116:
		return btnBack
	case 0x114, 0x115:
		return btnForward
	}
	return 0
}

// ---------- injection ----------

const (
	uiSetEvBit   = 0x40045564
	uiSetKeyBit  = 0x40045565
	uiSetRelBit  = 0x40045566
	uiSetAbsBit  = 0x40045567
	uiDevCreate  = 0x5501
	uiDevDestroy = 0x5502
	uiDevSetup   = 0x405c5503
	uiAbsSetup   = 0x401c5504
)

type linuxInjector struct {
	mu    sync.Mutex
	kbd   *os.File
	mouse *os.File
	w, h  int
	acc   [2]int
}

func newInjector() inputInjector { return &linuxInjector{} }

func openUinput() (*os.File, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		return nil, &setupError{msg: "Ponte non ha il permesso di controllare mouse e tastiera", help: permHelp}
	}
	return f, nil
}

func uinputCreate(f *os.File, name string) error {
	var setup [92]byte
	binary.LittleEndian.PutUint16(setup[0:], 0x06) // BUS_VIRTUAL
	binary.LittleEndian.PutUint16(setup[2:], 0x1209)
	binary.LittleEndian.PutUint16(setup[4:], 0x5047)
	binary.LittleEndian.PutUint16(setup[6:], 1)
	copy(setup[8:87], name)
	if err := fileIoctl(f, uiDevSetup, uintptr(unsafe.Pointer(&setup[0]))); err != nil {
		return err
	}
	return fileIoctl(f, uiDevCreate, 0)
}

func (l *linuxInjector) Start() error {
	l.w, l.h = 1920, 1080
	if x := openX11(); x != nil {
		l.w, l.h = x.size()
		x.c.Close()
	} else {
		logf("dimensione dello schermo non disponibile, uso %dx%d", l.w, l.h)
	}

	kbd, err := openUinput()
	if err != nil {
		return err
	}
	fileIoctl(kbd, uiSetEvBit, evKeyT)
	for k := uintptr(1); k < 0x100; k++ {
		fileIoctl(kbd, uiSetKeyBit, k)
	}
	if err := uinputCreate(kbd, "Ponte keyboard"); err != nil {
		kbd.Close()
		return err
	}

	mouse, err := openUinput()
	if err != nil {
		kbd.Close()
		return err
	}
	fileIoctl(mouse, uiSetEvBit, evKeyT)
	fileIoctl(mouse, uiSetEvBit, evRelT)
	fileIoctl(mouse, uiSetEvBit, evAbsT)
	for b := uintptr(0x110); b <= 0x114; b++ {
		fileIoctl(mouse, uiSetKeyBit, b)
	}
	for _, r := range []uintptr{relWheel, relHWheel, relWheelHi, relHWheelHi} {
		fileIoctl(mouse, uiSetRelBit, r)
	}
	for i, max := range []int{l.w - 1, l.h - 1} {
		fileIoctl(mouse, uiSetAbsBit, uintptr(i))
		var s [28]byte
		binary.LittleEndian.PutUint16(s[0:], uint16(i))
		binary.LittleEndian.PutUint32(s[12:], uint32(max)) // absinfo.maximum
		fileIoctl(mouse, uiAbsSetup, uintptr(unsafe.Pointer(&s[0])))
	}
	if err := uinputCreate(mouse, "Ponte mouse"); err != nil {
		kbd.Close()
		mouse.Close()
		return err
	}
	l.kbd, l.mouse = kbd, mouse
	return nil
}

func (l *linuxInjector) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range []*os.File{l.kbd, l.mouse} {
		if f != nil {
			fileIoctl(f, uiDevDestroy, 0)
			f.Close()
		}
	}
	l.kbd, l.mouse = nil, nil
}

func (l *linuxInjector) ScreenSize() (int, int) { return l.w, l.h }

type rawEv struct {
	typ, code uint16
	val       int32
}

func (l *linuxInjector) emit(f *os.File, evs ...rawEv) {
	if f == nil {
		return
	}
	buf := make([]byte, 0, (len(evs)+1)*inputEventSize)
	evs = append(evs, rawEv{evSyn, 0, 0})
	for _, e := range evs {
		rec := make([]byte, inputEventSize)
		o := inputEventSize - 8
		binary.LittleEndian.PutUint16(rec[o:], e.typ)
		binary.LittleEndian.PutUint16(rec[o+2:], e.code)
		binary.LittleEndian.PutUint32(rec[o+4:], uint32(e.val))
		buf = append(buf, rec...)
	}
	f.Write(buf)
}

func (l *linuxInjector) MouseAbs(x, y int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit(l.mouse, rawEv{evAbsT, absX, int32(x)}, rawEv{evAbsT, absY, int32(y)})
}

func (l *linuxInjector) Button(b uint8, down bool) {
	codes := map[uint8]uint16{btnLeft: 0x110, btnRight: 0x111, btnMiddle: 0x112, btnBack: 0x113, btnForward: 0x114}
	c, ok := codes[b]
	if !ok {
		return
	}
	v := int32(0)
	if down {
		v = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit(l.mouse, rawEv{evKeyT, c, v})
}

func (l *linuxInjector) Wheel(axis uint8, delta int) {
	if axis > 1 {
		return
	}
	hi, lo := uint16(relWheelHi), uint16(relWheel)
	if axis == 1 {
		hi, lo = relHWheelHi, relHWheel
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	evs := []rawEv{{evRelT, hi, int32(delta)}}
	l.acc[axis] += delta
	if n := l.acc[axis] / 120; n != 0 {
		evs = append(evs, rawEv{evRelT, lo, int32(n)})
		l.acc[axis] -= n * 120
	}
	l.emit(l.mouse, evs...)
}

func (l *linuxInjector) Key(code uint16, state uint8) {
	if code == 0 || code >= 0x100 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit(l.kbd, rawEv{evKeyT, code, int32(state)})
}
