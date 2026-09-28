package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeCapture struct {
	mu   sync.Mutex
	ch   chan<- inputEvent
	grab bool
	warp [2]int
}

func (f *fakeCapture) Start(ch chan<- inputEvent) error { f.ch = ch; return nil }
func (f *fakeCapture) Stop()                            {}
func (f *fakeCapture) Bounds() (int, int, int, int)     { return 0, 0, 1000, 800 }
func (f *fakeCapture) EdgeSwitch() bool                 { return true }
func (f *fakeCapture) SetGrab(on bool)                  { f.mu.Lock(); f.grab = on; f.mu.Unlock() }
func (f *fakeCapture) Warp(x, y int)                    { f.mu.Lock(); f.warp = [2]int{x, y}; f.mu.Unlock() }
func (f *fakeCapture) grabbed() bool                    { f.mu.Lock(); defer f.mu.Unlock(); return f.grab }

type fakeInjector struct {
	mu   sync.Mutex
	log  []string
	w, h int
}

func (f *fakeInjector) add(s string)           { f.mu.Lock(); f.log = append(f.log, s); f.mu.Unlock() }
func (f *fakeInjector) Start() error           { return nil }
func (f *fakeInjector) Close()                 {}
func (f *fakeInjector) ScreenSize() (int, int) { return f.w, f.h }
func (f *fakeInjector) MouseAbs(x, y int)      { f.add(fmt.Sprintf("mouse %d %d", x, y)) }
func (f *fakeInjector) Button(b uint8, d bool) { f.add(fmt.Sprintf("button %d %v", b, d)) }
func (f *fakeInjector) Wheel(a uint8, d int)   { f.add(fmt.Sprintf("wheel %d %d", a, d)) }
func (f *fakeInjector) Key(c uint16, s uint8)  { f.add(fmt.Sprintf("key %d %d", c, s)) }
func (f *fakeInjector) ShowCursor(on bool)     { f.add(fmt.Sprintf("cursor %v", on)) }
func (f *fakeInjector) CheckCursor()           {}
func (f *fakeInjector) has(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.log {
		if l == s {
			return true
		}
	}
	return false
}
func (f *fakeInjector) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.log) == 0 {
		return ""
	}
	return f.log[len(f.log)-1]
}

type fakeClip struct {
	mu      sync.Mutex
	text    string
	files   []string
	changed bool
}

func (f *fakeClip) Files() ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.changed
	f.changed = false
	return f.files, c
}
func (f *fakeClip) SetFiles(p []string)      { f.mu.Lock(); f.files = p; f.mu.Unlock() }
func (f *fakeClip) copyFiles(p []string)     { f.mu.Lock(); f.files, f.changed = p, true; f.mu.Unlock() }
func (f *fakeClip) pasted() (files []string) { f.mu.Lock(); defer f.mu.Unlock(); return f.files }

func (f *fakeClip) Get() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text, f.text != ""
}
func (f *fakeClip) Set(t string)    { f.mu.Lock(); f.text = t; f.mu.Unlock() }
func (f *fakeClip) Available() bool { return true }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitLong(t, what, 6*time.Second, cond)
}

func waitLong(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for i := 0; i < int(d/(20*time.Millisecond)); i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

type testPC struct {
	app  *App
	cap  *fakeCapture
	inj  *fakeInjector
	clip *fakeClip
}

// newTestPCs starts Ponte several times on this machine, each with its own
// fake mouse, screen and clipboard.
func newTestPCs(t *testing.T, names ...string) []*testPC {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	listenAddr = "127.0.0.1:0"
	switchGuard = 0
	t.Cleanup(func() { listenAddr = ":24800"; switchGuard = 300 * time.Millisecond })
	var pcs []*testPC
	var cur *testPC
	makeCapture = func() inputCapture { return cur.cap }
	makeInjector = func() inputInjector { return cur.inj }
	makeClipboard = func() clipboard { return cur.clip }
	for i, name := range names {
		cur = &testPC{cap: &fakeCapture{}, inj: &fakeInjector{w: 1600 + 100*i, h: 900}, clip: &fakeClip{}}
		cur.app = &App{cfg: loadConfig(), code: newPairingCode()}
		cur.app.cfg.DeviceID = []byte{byte(i + 1), 15: 0}
		cur.app.cfg.Name = name
		cur.app.cfg.Peers = map[string]*pairedPeer{}
		cur.app.cfg.Layout = layout{Pos: map[string]cell{cur.app.cfg.id(): {}}}
		if err := cur.app.start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cur.app.shutdown)
		pcs = append(pcs, cur)
	}
	return pcs
}

func (p *testPC) addr() string { return fmt.Sprintf("127.0.0.1:%d", p.app.node.port) }
func (p *testPC) id() string   { return p.app.cfg.id() }

func (p *testPC) online(q *testPC) bool {
	for _, v := range p.app.status().Peers {
		if v.ID == q.id() {
			return v.Online
		}
	}
	return false
}

func (p *testPC) pos(q *testPC) cell { return p.app.layoutCopy().Pos[q.id()] }

func TestGroup(t *testing.T) {
	pcs := newTestPCs(t, "Fisso", "Portatile", "Salotto")
	a, b, c := pcs[0], pcs[1], pcs[2]

	// Wrong code is refused.
	b.app.connect(a.id(), a.addr(), "000000")
	waitFor(t, "wrong code error", func() bool { return b.app.status().Error == "Codice non corretto" })

	code := a.app.status().Code
	b.app.connect(a.id(), a.addr(), code)
	waitFor(t, "a and b linked", func() bool { return a.online(b) && b.online(a) })
	if a.app.status().Code == code {
		t.Fatal("pairing code should change after pairing")
	}
	// b sits right of a, on both maps.
	waitFor(t, "b placed right of a", func() bool { return a.pos(b) == cell{1, 0} && b.pos(b) == cell{1, 0} && b.pos(a) == cell{0, 0} })

	// c pairs with a only, and a introduces it to b.
	c.app.connect(a.id(), a.addr(), a.app.status().Code)
	waitFor(t, "c linked to a and b", func() bool { return c.online(a) && c.online(b) && b.online(c) })
	waitFor(t, "c on every map", func() bool { return a.pos(c) == cell{-1, 0} && b.pos(c) == cell{-1, 0} && c.pos(b) == cell{1, 0} })

	// Copied text reaches every computer.
	a.clip.Set("ciao a tutti")
	waitFor(t, "clipboard to b and c", func() bool {
		tb, _ := b.clip.Get()
		tc, _ := c.clip.Get()
		return tb == "ciao a tutti" && tc == "ciao a tutti"
	})

	// Move c to the right of b, from a: every map follows.
	a.app.moveScreen(c.id(), cell{2, 0})
	waitFor(t, "c moved on b's map", func() bool { return b.pos(c) == cell{2, 0} && c.pos(c) == cell{2, 0} })

	// a's pointer reaches its right edge: b.
	a.cap.ch <- inputEvent{kind: evPos, x: 999, y: 400} // moved by another computer
	time.Sleep(50 * time.Millisecond)
	if a.cap.grabbed() {
		t.Fatal("a pointer on the edge but not moved by a's own mouse switched")
	}
	a.cap.ch <- inputEvent{kind: evMotion}
	a.cap.ch <- inputEvent{kind: evPos, x: 999, y: 400}
	waitFor(t, "enter b", func() bool { return b.inj.last() == "mouse 0 450" })
	if !a.cap.grabbed() {
		t.Fatal("a should grab its mouse")
	}
	waitFor(t, "a controlling b", func() bool {
		s := a.app.status()
		return s.State == "controlling" && s.Target == b.id() && b.app.status().State == "controlled"
	})
	a.cap.ch <- inputEvent{kind: evKey, code: 30, val: 1}
	a.cap.ch <- inputEvent{kind: evKey, code: 30, val: 0}
	waitFor(t, "key on b", func() bool { return b.inj.has("key 30 1") && b.inj.last() == "key 30 0" })

	// On past b's right edge, straight to c.
	a.cap.ch <- inputEvent{kind: evRel, x: 1700, y: 0}
	waitFor(t, "enter c from b", func() bool { return c.inj.last() == "mouse 0 450" })
	waitFor(t, "b released", func() bool { return b.app.status().State == "ready" && b.inj.has("cursor false") })

	// Back left through b, then home to a.
	a.cap.ch <- inputEvent{kind: evRel, x: -100, y: 0}
	waitFor(t, "back on b", func() bool { return b.inj.last() == "mouse 1699 450" })
	a.cap.ch <- inputEvent{kind: evRel, x: -1700, y: 0}
	waitFor(t, "home", func() bool { return !a.cap.grabbed() })
	a.cap.mu.Lock()
	warp := a.cap.warp
	a.cap.mu.Unlock()
	if warp != [2]int{997, 400} {
		t.Fatalf("warp = %v", warp)
	}

	// Scroll Lock goes through the computers in map order.
	a.cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "hotkey to b", func() bool { return a.app.status().Target == b.id() })
	a.cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "hotkey to c", func() bool { return a.app.status().Target == c.id() })
	a.cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "hotkey home", func() bool { return !a.cap.grabbed() && a.app.status().State == "ready" })

	// Someone moves b's own mouse while a controls b: b takes it back, and
	// a's mouse stays on a.
	a.cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "a on b again", func() bool { return b.app.status().By == a.id() })
	b.cap.ch <- inputEvent{kind: evMotion}
	time.Sleep(50 * time.Millisecond)
	if b.app.status().By == "" {
		t.Fatal("one motion should not take the computer back")
	}
	for range 4 {
		b.cap.ch <- inputEvent{kind: evMotion}
	}
	waitFor(t, "b takes over", func() bool { return b.app.status().State == "ready" && !a.cap.grabbed() })

	// b now controls: its right edge leads to c.
	// Its pointer was left on the edge towards a: it must move off the
	// edge before an edge counts again.
	b.cap.ch <- inputEvent{kind: evMotion}
	b.cap.ch <- inputEvent{kind: evPos, x: 0, y: 300}
	time.Sleep(50 * time.Millisecond)
	if b.cap.grabbed() {
		t.Fatal("bounced back to a right after taking over")
	}
	b.cap.ch <- inputEvent{kind: evMotion}
	b.cap.ch <- inputEvent{kind: evPos, x: 500, y: 300}
	b.cap.ch <- inputEvent{kind: evMotion}
	b.cap.ch <- inputEvent{kind: evPos, x: 999, y: 0}
	waitFor(t, "b controls c", func() bool { return c.app.status().By == b.id() && b.cap.grabbed() })
	for _, k := range []uint16{keyLeftCtrl, keyLeftAlt, keyEsc} {
		b.cap.ch <- inputEvent{kind: evKey, code: k, val: 1}
	}
	waitFor(t, "emergency way back", func() bool { return !b.cap.grabbed() && c.app.status().State == "ready" })
	for _, k := range []uint16{keyLeftCtrl, keyLeftAlt, keyEsc} {
		b.cap.ch <- inputEvent{kind: evKey, code: k, val: 0}
	}

	// Closing Ponte on a computer being controlled gives the mouse back.
	a.cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "a on b before closing", func() bool { return b.app.status().By == a.id() })
	start := time.Now()
	b.app.shutdown()
	waitFor(t, "released on bye", func() bool { return !a.cap.grabbed() })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("mouse released after %v", d)
	}

	// Started again, b reconnects with the stored keys, no code.
	makeCapture = func() inputCapture { return b.cap }
	makeInjector = func() inputInjector { return b.inj }
	makeClipboard = func() clipboard { return b.clip }
	if err := b.app.start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b back", func() bool { return b.online(a) && b.online(c) && a.online(b) })

	// Forgetting c on a removes it from the whole group.
	introGrace = 0
	defer func() { introGrace = time.Minute }()
	a.app.forget(c.id())
	waitFor(t, "c gone everywhere", func() bool {
		a.app.mu.Lock()
		_, onA := a.app.cfg.Peers[c.id()]
		a.app.mu.Unlock()
		b.app.mu.Lock()
		_, onB := b.app.cfg.Peers[c.id()]
		b.app.mu.Unlock()
		return !onA && !onB && !b.online(c)
	})
	// c tries again after a while, is refused, and drops its pairings.
	waitLong(t, "c forgets the group", 15*time.Second, func() bool {
		c.app.mu.Lock()
		defer c.app.mu.Unlock()
		return len(c.app.cfg.Peers) == 0
	})
}

func TestMigrateOldPairings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(configDir(), 0o700)
	os.WriteFile(filepath.Join(configDir(), "config.json"), []byte(`{"device_id":"AAAAAAAAAAAAAAAAAAAAAA==","role":"share","edge":"left",
		"clients":{"bb":{"name":"CASA","os":"windows","key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}}`), 0o600)
	c := loadConfig()
	if p := c.Peers["bb"]; p == nil || p.Name != "CASA" || len(p.Key) != 32 {
		t.Fatalf("peer not moved over: %+v", c.Peers)
	}
	if c.Layout.Pos["bb"] != (cell{-1, 0}) || c.Layout.Pos[c.id()] != (cell{}) || c.Layout.Stamp == 0 {
		t.Fatalf("layout: %+v", c.Layout)
	}
	if c.Role != "" || c.Clients != nil {
		t.Fatal("old fields kept")
	}
}

func TestRippleFrame(t *testing.T) {
	const size = 100
	buf := make([]uint32, size*size)
	lit := func() (n int) {
		for _, p := range buf {
			if p>>24 != 0 {
				n++
			}
		}
		return
	}
	rippleFrame(buf, size, 0.3, 4, defaultRippleColor)
	if lit() == 0 || buf[0] != 0 || buf[size/2*size+size/2] != 0 {
		t.Fatal("expected a ring, clear corners and center mid-animation")
	}
	rippleFrame(buf, size, 1, 4, defaultRippleColor)
	if lit() != 0 {
		t.Fatal("circles should be gone at the end")
	}
}

func TestTintPNG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	copy(img.Pix, []uint8{0x5b, 0x5b, 0xf7, 255, 255, 255, 255, 128})
	var b bytes.Buffer
	png.Encode(&b, img)
	out, _ := png.Decode(bytes.NewReader(tintPNG(b.Bytes(), 0xc2410c)))
	got := out.(*image.NRGBA).Pix
	if want := []uint8{0xc2, 0x41, 0x0c, 255, 255, 255, 255, 128}; !bytes.Equal(got, want) {
		t.Fatalf("tint: got %v, want %v", got, want)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.2.4", "1.2.3", true}, {"1.2.10", "1.2.9", true}, {"1.2.3", "1.2.3", false}, {"1.2", "1.2.1", false}, {"2.0.0", "1.9.9", true}} {
		if newer(c.a, c.b) != c.want {
			t.Errorf("newer(%q, %q) != %v", c.a, c.b, c.want)
		}
	}
}

type testFileClip struct{ files []string }

func (c *testFileClip) Get() (string, bool)     { return "", false }
func (c *testFileClip) Set(string)              {}
func (c *testFileClip) Available() bool         { return true }
func (c *testFileClip) Files() ([]string, bool) { return nil, false }
func (c *testFileClip) SetFiles(paths []string) { c.files = paths }

func TestSendFiles(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "cartella", "sotto"), 0o700)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("ciao"), 0o600)
	big := bytes.Repeat([]byte("x"), fileChunk+10) // more than one chunk
	os.WriteFile(filepath.Join(src, "cartella", "sotto", "b.bin"), big, 0o600)

	cb := &testFileClip{}
	c := &clipSync{app: &App{cfg: &config{}}, cb: cb}
	sendFiles([]string{filepath.Join(src, "a.txt"), filepath.Join(src, "cartella")}, c.received)
	if len(cb.files) != 2 {
		t.Fatalf("clipboard files: %v", cb.files)
	}
	if b, _ := os.ReadFile(filepath.Join(inboxDir(), "a.txt")); string(b) != "ciao" {
		t.Fatalf("a.txt: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(inboxDir(), "cartella", "sotto", "b.bin")); !bytes.Equal(b, big) {
		t.Fatalf("b.bin: %d bytes", len(b))
	}

	// A path leading out of the temporary folder is refused.
	cb.files = nil
	c.received([]byte{msgFileStart})
	c.received(wbuf{msgFileEntry}.u8(0).str("../fuori.txt"))
	c.received(append(wbuf{msgFileData}, "no"...))
	c.received(wbuf{msgFileEnd}.u16(1).str("fuori.txt"))
	if cb.files != nil {
		t.Fatal("files outside the temporary folder were accepted")
	}
	if _, err := os.Stat(filepath.Join(inboxDir(), "..", "fuori.txt")); err == nil {
		t.Fatal("file written outside the temporary folder")
	}
}

// A connection that drops during the handshake is not a refusal: only a
// refusal may make Ponte forget a pairing.
func TestHandshakeDropIsNotRefusal(t *testing.T) {
	key := randomBytes(32)
	for _, refuse := range []bool{false, true} {
		cli, srv := net.Pipe()
		go func() {
			serverHandshake(srv, randomBytes(16), func(byte, []byte) ([]byte, bool) {
				if !refuse {
					srv.Close() // gone before answering
				}
				return key, !refuse
			})
			srv.Close()
		}()
		_, _, err := clientHandshake(cli, randomBytes(16), authModeKey, key, nil)
		if refuse != errors.Is(err, errAuth) {
			t.Fatalf("refuse=%v: err = %v", refuse, err)
		}
		cli.Close()
	}
}

// Copied files travel only when they are pasted with Ctrl+V on another
// computer, typed there or from the computer controlling it.
func TestPasteFetchesFiles(t *testing.T) {
	pcs := newTestPCs(t, "Fisso", "Portatile")
	a, b := pcs[0], pcs[1]
	b.app.connect(a.id(), a.addr(), a.app.status().Code)
	waitFor(t, "a and b linked", func() bool { return a.online(b) && b.online(a) })

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "uno.txt"), []byte("uno"), 0o600)
	os.WriteFile(filepath.Join(src, "due.txt"), []byte("due"), 0o600)

	// Copied on a: b only hears of it.
	a.clip.copyFiles([]string{filepath.Join(src, "uno.txt")})
	waitFor(t, "offer on b", func() bool { return b.app.node.cs.armed.Load() })
	time.Sleep(100 * time.Millisecond)
	if b.clip.pasted() != nil {
		t.Fatal("files sent before the paste")
	}

	// Ctrl+V on b's own keyboard: the files come, then the paste.
	b.cap.ch <- inputEvent{kind: evKey, code: keyLeftCtrl, val: 1}
	b.cap.ch <- inputEvent{kind: evKey, code: keyV, val: 1}
	b.cap.ch <- inputEvent{kind: evPaste}
	waitFor(t, "pasted on b", func() bool { return b.inj.last() == "key 47 0" })
	if p := b.clip.pasted(); len(p) != 1 || filepath.Base(p[0]) != "uno.txt" {
		t.Fatalf("b clipboard: %v", p)
	}
	if b.inj.has("key 29 1") {
		t.Fatal("Ctrl pressed again while held")
	}
	if b.app.node.cs.armed.Load() {
		t.Fatal("offer still pending after the paste")
	}
	b.cap.ch <- inputEvent{kind: evKey, code: keyV, val: 0}
	b.cap.ch <- inputEvent{kind: evKey, code: keyLeftCtrl, val: 0}

	// Copied again on a, pasted on b from a's keyboard while a controls b.
	a.clip.copyFiles([]string{filepath.Join(src, "due.txt")})
	waitFor(t, "second offer on b", func() bool { return b.app.node.cs.armed.Load() })
	a.cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "a controlling b", func() bool { return b.app.status().State == "controlled" })
	a.cap.ch <- inputEvent{kind: evKey, code: keyLeftCtrl, val: 1}
	a.cap.ch <- inputEvent{kind: evKey, code: keyV, val: 1}
	waitFor(t, "second paste on b", func() bool {
		p := b.clip.pasted()
		return len(p) == 1 && filepath.Base(p[0]) == "due.txt" && b.inj.last() == "key 47 0"
	})
	b.inj.mu.Lock()
	vs := 0
	for _, e := range b.inj.log {
		if e == "key 47 1" {
			vs++
		}
	}
	b.inj.mu.Unlock()
	if vs != 2 {
		t.Fatalf("V pressed %d times on b, want 2 (one per paste)", vs)
	}
	a.cap.ch <- inputEvent{kind: evKey, code: keyV, val: 0}
	a.cap.ch <- inputEvent{kind: evKey, code: keyLeftCtrl, val: 0}
	waitFor(t, "keys released on b", func() bool { return b.inj.last() == "key 29 0" })

	// With nothing on offer, Ctrl+V goes through as it is.
	a.cap.ch <- inputEvent{kind: evKey, code: keyLeftCtrl, val: 1}
	a.cap.ch <- inputEvent{kind: evKey, code: keyV, val: 1}
	waitFor(t, "plain paste", func() bool { return b.inj.last() == "key 47 1" })
}
