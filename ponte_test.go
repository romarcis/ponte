package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
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
	mu  sync.Mutex
	log []string
}

func (f *fakeInjector) add(s string)           { f.mu.Lock(); f.log = append(f.log, s); f.mu.Unlock() }
func (f *fakeInjector) Start() error           { return nil }
func (f *fakeInjector) Close()                 {}
func (f *fakeInjector) ScreenSize() (int, int) { return 1600, 900 }
func (f *fakeInjector) MouseAbs(x, y int)      { f.add(fmt.Sprintf("mouse %d %d", x, y)) }
func (f *fakeInjector) Button(b uint8, d bool) { f.add(fmt.Sprintf("button %d %v", b, d)) }
func (f *fakeInjector) Wheel(a uint8, d int)   { f.add(fmt.Sprintf("wheel %d %d", a, d)) }
func (f *fakeInjector) Key(c uint16, s uint8)  { f.add(fmt.Sprintf("key %d %d", c, s)) }
func (f *fakeInjector) ShowCursor(on bool)     { f.add(fmt.Sprintf("cursor %v", on)) }
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
	mu   sync.Mutex
	text string
}

func (f *fakeClip) Get() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text, f.text != ""
}
func (f *fakeClip) Set(t string)    { f.mu.Lock(); f.text = t; f.mu.Unlock() }
func (f *fakeClip) Available() bool { return true }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestEndToEnd(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cap := &fakeCapture{}
	inj := &fakeInjector{}
	makeCapture = func() inputCapture { return cap }
	makeInjector = func() inputInjector { return inj }
	srvClip, cliClip := &fakeClip{text: "vecchio"}, &fakeClip{}
	clips := 0
	makeClipboard = func() clipboard {
		clips++
		if clips == 1 {
			return srvClip
		}
		return cliClip
	}

	srv := &App{cfg: loadConfig(), state: "idle"}
	srv.cfg.Name = "Fisso"
	cli := &App{cfg: loadConfig(), state: "idle"}
	cli.cfg.DeviceID = randomBytes(16)
	cli.cfg.Name = "Portatile"

	if err := srv.setRole("share"); err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown()
	if err := cli.setRole("receive"); err != nil {
		t.Fatal(err)
	}

	// Wrong code is refused.
	cli.connect("", "127.0.0.1:24800", "000000")
	waitFor(t, "wrong code error", func() bool { return cli.status().Error == "Codice non corretto" })

	code := srv.status().Code
	cli.connect("", "127.0.0.1:24800", code)
	waitFor(t, "client connected", func() bool { return cli.status().State == "connected" })
	waitFor(t, "server connected", func() bool { return srv.status().State == "connected" })
	if cli.status().Peer != "Fisso" || srv.status().Peer != "Portatile" {
		t.Fatalf("peer names: %q %q", cli.status().Peer, srv.status().Peer)
	}
	if srv.status().Code == code {
		t.Fatal("pairing code should change after pairing")
	}
	if len(srv.cfg.Clients) != 1 || len(cli.cfg.Servers) != 1 {
		t.Fatal("pairing not stored")
	}

	// Copied text goes both ways; what was copied before connecting stays local.
	time.Sleep(600 * time.Millisecond)
	if c, _ := cliClip.Get(); c != "" {
		t.Fatalf("old clipboard leaked: %q", c)
	}
	srvClip.Set("ciao dal fisso")
	waitFor(t, "clipboard to client", func() bool { t, _ := cliClip.Get(); return t == "ciao dal fisso" })
	cliClip.Set("risposta\nriga 2 è ok")
	waitFor(t, "clipboard to server", func() bool { t, _ := srvClip.Get(); return t == "risposta\nriga 2 è ok" })
	srv.setClipboard(false)
	cliClip.Set("privato")
	time.Sleep(1200 * time.Millisecond)
	if c, _ := srvClip.Get(); c == "privato" {
		t.Fatal("clipboard sharing is off but text arrived")
	}
	srv.setClipboard(true)

	// Pointer reaches the right edge at mid height: control moves over.
	cap.ch <- inputEvent{kind: evPos, x: 999, y: 400}
	waitFor(t, "enter", func() bool { return inj.last() == "mouse 0 450" })
	if !cap.grabbed() || srv.status().State != "active" {
		t.Fatal("server should grab input")
	}
	waitFor(t, "client active", func() bool { return cli.status().State == "active" })
	if !inj.has("cursor true") {
		t.Fatal("pointer not made visible on the controlled computer")
	}

	cap.ch <- inputEvent{kind: evRel, x: 100, y: -50}
	waitFor(t, "move", func() bool { return inj.last() == "mouse 100 400" })
	cap.ch <- inputEvent{kind: evKey, code: 30, val: 1}
	waitFor(t, "key down", func() bool { return inj.last() == "key 30 1" })
	cap.ch <- inputEvent{kind: evKey, code: 30, val: 0}
	cap.ch <- inputEvent{kind: evButton, code: btnLeft, val: 1}
	cap.ch <- inputEvent{kind: evButton, code: btnLeft, val: 0}
	cap.ch <- inputEvent{kind: evWheel, code: 0, val: -120}
	waitFor(t, "wheel", func() bool { return inj.last() == "wheel 0 -120" })

	// A held key is released on the other computer when control comes back.
	cap.ch <- inputEvent{kind: evKey, code: 42, val: 1}
	cap.ch <- inputEvent{kind: evRel, x: -200, y: 0}
	waitFor(t, "leave", func() bool { return !cap.grabbed() })
	waitFor(t, "shift released", func() bool {
		inj.mu.Lock()
		defer inj.mu.Unlock()
		for _, l := range inj.log {
			if l == "key 42 0" {
				return true
			}
		}
		return false
	})
	if cap.warp != [2]int{997, 355} {
		t.Fatalf("warp = %v", cap.warp)
	}
	waitFor(t, "client back to connected", func() bool { return cli.status().State == "connected" })
	if inj.last() != "cursor false" {
		t.Fatalf("pointer visibility not given back: %q", inj.last())
	}

	// Ctrl+Alt+Esc is the emergency way back.
	cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "enter for emergency", func() bool { return cap.grabbed() })
	for _, k := range []uint16{keyLeftCtrl, keyLeftAlt, keyEsc} {
		cap.ch <- inputEvent{kind: evKey, code: k, val: 1}
	}
	waitFor(t, "emergency leave", func() bool { return !cap.grabbed() })
	for _, k := range []uint16{keyLeftCtrl, keyLeftAlt, keyEsc} {
		cap.ch <- inputEvent{kind: evKey, code: k, val: 0}
	}

	// Scroll Lock jumps over and back.
	cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 1}
	cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "hotkey enter", func() bool { return cap.grabbed() && inj.last() == "mouse 800 450" })
	cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 1}
	cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "hotkey leave", func() bool { return !cap.grabbed() })

	// Closing Ponte on the controlled computer while it is in use gives the
	// mouse back at once.
	cap.ch <- inputEvent{kind: evKey, code: keyScrollLock, val: 0}
	waitFor(t, "enter again", func() bool { return cap.grabbed() })
	start := time.Now()
	cli.setRole("")
	waitFor(t, "released on bye", func() bool { return !cap.grabbed() })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("mouse released after %v", d)
	}

	// Restarting the receiving side reconnects with the stored key, no code.
	waitFor(t, "server sees disconnect", func() bool { return srv.status().State == "waiting" })
	cli.setRole("receive")
	defer cli.shutdown()
	waitFor(t, "reconnected", func() bool { return cli.status().State == "connected" })

	// Forgetting the client on the server makes the stored key useless.
	for id := range srv.cfg.Clients {
		srv.forget(id)
	}
	waitFor(t, "stale pairing reported", func() bool { return cli.status().Error == "L'abbinamento non è più valido" })
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
