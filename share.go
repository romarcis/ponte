package main

import (
	"encoding/hex"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// shareCtl runs on the computer whose mouse and keyboard are shared.
type shareCtl struct {
	app    *App
	cap    inputCapture
	clip   clipboard
	ln     net.Listener
	events chan inputEvent
	newCl  chan *peer
	gone   chan *peer
	dropID chan string
	stop   chan struct{}
	done   chan struct{}

	cl      *peer
	remote  bool
	rx, ry  float64
	local   map[uint16]time.Time // keys and buttons (1000+b) held on this computer
	held    map[uint16]bool      // keys held on the other computer
	heldBtn map[uint8]bool       // buttons held on the other computer
}

type peer struct {
	sc       *secureConn
	id       string
	name, os string
	w, h     int
	once     sync.Once
	seen     atomic.Int64 // last message from the peer (unix nanoseconds)
}

func (p *peer) close() { p.once.Do(func() { p.sc.Close() }) }

func (p *peer) send(b []byte) {
	if err := p.sc.WriteMsg(b); err != nil {
		p.close()
	}
}

func startShare(a *App) (*shareCtl, error) {
	s := &shareCtl{
		app:     a,
		cap:     makeCapture(),
		clip:    makeClipboard(),
		events:  make(chan inputEvent, 4096),
		newCl:   make(chan *peer),
		gone:    make(chan *peer),
		dropID:  make(chan string),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		local:   map[uint16]time.Time{},
		held:    map[uint16]bool{},
		heldBtn: map[uint8]bool{},
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("", itoa(dataPort)))
	if err != nil {
		return nil, &setupError{
			msg:  "La porta di rete 24800 è già in uso",
			help: "Forse è aperto un altro programma di condivisione (Synergy, Barrier, Input Leap). Chiudilo e riprova.",
		}
	}
	s.ln = ln
	if err := s.cap.Start(s.events); err != nil {
		ln.Close()
		return nil, err
	}
	a.setState("waiting", "", "")
	go s.acceptLoop()
	go s.loop()
	go announce(func() beacon {
		return beacon{App: "ponte", ID: hex.EncodeToString(a.deviceID()), Name: a.name(), OS: runtime.GOOS, Port: dataPort}
	}, s.stop)
	return s, nil
}

func (s *shareCtl) Stop() {
	close(s.stop)
	s.ln.Close()
	<-s.done
	s.cap.Stop()
}

func (s *shareCtl) drop(id string) {
	select {
	case s.dropID <- id:
	case <-s.done:
	}
}

func (s *shareCtl) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handshake(c)
	}
}

func (s *shareCtl) handshake(c net.Conn) {
	sc, cid, mode, err := serverHandshake(c, s.app.deviceID(), s.app.lookupSecret)
	if err != nil {
		c.Close()
		if errors.Is(err, errAuth) && mode == authModeCode {
			s.app.codeFailed()
		}
		logf("connessione rifiutata da %s: %v", c.RemoteAddr(), err)
		return
	}
	msg, err := sc.ReadMsg(10 * time.Second)
	if err != nil || len(msg) == 0 || msg[0] != msgHello {
		sc.Close()
		return
	}
	r := &rbuf{b: msg[1:]}
	p := &peer{sc: sc, id: hex.EncodeToString(cid)}
	p.w, p.h = int(r.i32()), int(r.i32())
	p.name, p.os = r.str(), r.str()
	if r.err != nil || p.w <= 0 || p.h <= 0 {
		sc.Close()
		return
	}
	var key []byte
	if mode == authModeCode {
		key = randomBytes(32)
		s.app.pairClient(p.id, p.name, p.os, key)
	} else {
		s.app.updateClient(p.id, p.name, p.os)
	}
	if err := sc.WriteMsg(encWelcome(s.app.name(), key)); err != nil {
		sc.Close()
		return
	}
	logf("collegato %s (%s, %dx%d)", p.name, c.RemoteAddr(), p.w, p.h)
	select {
	case s.newCl <- p:
	case <-s.stop:
		sc.Close()
		return
	}
	clipStop := make(chan struct{})
	defer close(clipStop)
	cs := startClipSync(s.app, s.clip, p.send, clipStop)
	for {
		msg, err := sc.ReadMsg(peerTimeout)
		if err == nil && len(msg) > 0 {
			p.seen.Store(time.Now().UnixNano())
			switch msg[0] {
			case msgClipboard:
				cs.received(msg)
			case msgBye:
				err = errors.New("chiuso dall'altro computer")
			}
		}
		if err != nil {
			p.close()
			select {
			case s.gone <- p:
			case <-s.stop:
			}
			return
		}
	}
}

func (s *shareCtl) loop() {
	defer close(s.done)
	ping := time.NewTicker(time.Second)
	defer ping.Stop()
	for {
		select {
		case <-s.stop:
			if s.cl != nil {
				s.leave(false, "Ponte chiuso")
				s.cl.send([]byte{msgBye})
				s.cl.close()
			}
			return
		case p := <-s.newCl:
			if s.cl != nil {
				s.leave(false, "altro computer collegato")
				s.cl.close()
			}
			s.cl = p
			p.seen.Store(time.Now().UnixNano())
			s.app.setState("connected", p.name, p.os)
		case p := <-s.gone:
			if p == s.cl {
				s.leave(false, "scollegato")
				s.cl = nil
				s.app.setState("waiting", "", "")
				logf("scollegato %s", p.name)
			}
		case id := <-s.dropID:
			if s.cl != nil && s.cl.id == id {
				s.leave(false, "computer dimenticato")
				s.cl.close()
			}
		case ev := <-s.events:
			s.handle(ev)
		case <-ping.C:
			if s.cl == nil {
				break
			}
			// Never keep this computer's mouse and keyboard captured for a
			// peer that stopped answering.
			if time.Since(time.Unix(0, s.cl.seen.Load())) > peerTimeout {
				logf("%s non risponde", s.cl.name)
				s.leave(false, "non risponde")
				s.cl.close()
				break
			}
			s.cl.send([]byte{msgPing})
		}
	}
}

func (s *shareCtl) modsHeld(keys map[uint16]bool) bool {
	return (keys[keyLeftCtrl] || keys[keyRightCtrl]) && (keys[keyLeftAlt] || keys[keyRightAlt])
}

func (s *shareCtl) heldLocally() bool {
	for k, t := range s.local {
		if time.Since(t) > 10*time.Second {
			delete(s.local, k) // a release we never saw
			continue
		}
		return true
	}
	return false
}

func (s *shareCtl) handle(ev inputEvent) {
	switch ev.kind {
	case evPos:
		if !s.remote && s.cl != nil && s.atEdge(int(ev.x), int(ev.y)) && !s.heldLocally() {
			s.enter(int(ev.x), int(ev.y), false, "bordo dello schermo")
		}
	case evRel:
		if s.remote {
			s.move(float64(ev.x), float64(ev.y))
		}
	case evButton:
		b := uint8(ev.code)
		if s.remote {
			if ev.val != 0 {
				s.heldBtn[b] = true
			} else {
				delete(s.heldBtn, b)
			}
			s.cl.send(encButton(b, ev.val != 0))
		} else if ev.val != 0 {
			s.local[1000+ev.code] = time.Now()
		} else {
			delete(s.local, 1000+ev.code)
		}
	case evWheel:
		if s.remote {
			s.cl.send(encWheel(uint8(ev.code), int(ev.val)))
		}
	case evKey:
		if ev.code == keyScrollLock {
			// Act on release, so the key is never left pressed on either side.
			if ev.val == 0 {
				if s.remote {
					s.leave(false, "Bloc Scorr")
				} else if s.cl != nil {
					s.enter(0, 0, true, "Bloc Scorr")
				}
			}
			return
		}
		if ev.code == keyEsc && ev.val == 1 && s.remote && s.modsHeld(s.held) {
			s.leave(false, "Ctrl+Alt+Esc") // Ctrl+Alt+Esc: emergency way back
			return
		}
		if s.remote {
			if ev.val == 0 {
				delete(s.held, ev.code)
			} else {
				s.held[ev.code] = true
			}
			s.cl.send(encKey(ev.code, uint8(ev.val)))
		} else if ev.val == 0 {
			delete(s.local, ev.code)
		} else {
			s.local[ev.code] = time.Now()
		}
	}
}

func (s *shareCtl) atEdge(x, y int) bool {
	bx, by, bw, bh := s.cap.Bounds()
	switch s.app.edge() {
	case "right":
		return x >= bx+bw-1
	case "left":
		return x <= bx
	case "top":
		return y <= by
	case "bottom":
		return y >= by+bh-1
	}
	return false
}

func (s *shareCtl) enter(x, y int, center bool, why string) {
	bx, by, bw, bh := s.cap.Bounds()
	w, h := float64(s.cl.w), float64(s.cl.h)
	fx := float64(x-bx) / float64(max(bw, 1))
	fy := float64(y-by) / float64(max(bh, 1))
	switch {
	case center:
		s.rx, s.ry = w/2, h/2
	case s.app.edge() == "right":
		s.rx, s.ry = 0, fy*h
	case s.app.edge() == "left":
		s.rx, s.ry = w-1, fy*h
	case s.app.edge() == "bottom":
		s.rx, s.ry = fx*w, 0
	case s.app.edge() == "top":
		s.rx, s.ry = fx*w, h-1
	}
	s.remote = true
	s.cap.SetGrab(true)
	s.cl.send(encEnter(int(s.rx), int(s.ry)))
	s.app.setState("active", s.cl.name, s.cl.os)
	logf("-> passo a %s (%s): qui %d,%d, là %d,%d", s.cl.name, why, x, y, int(s.rx), int(s.ry))
}

func (s *shareCtl) move(dx, dy float64) {
	w, h := float64(s.cl.w), float64(s.cl.h)
	s.rx += dx
	s.ry += dy
	var out bool
	switch s.app.edge() {
	case "right":
		out = s.rx < 0
	case "left":
		out = s.rx > w-1
	case "bottom":
		out = s.ry < 0
	case "top":
		out = s.ry > h-1
	}
	if out && len(s.heldBtn) == 0 && s.cap.EdgeSwitch() {
		s.leave(true, "bordo dello schermo")
		return
	}
	s.rx = min(max(s.rx, 0), w-1)
	s.ry = min(max(s.ry, 0), h-1)
	s.cl.send(encMouse(int(s.rx), int(s.ry)))
}

// leave gives control back to this computer. With warp, the local pointer
// appears on the shared edge at the height where it left the other screen.
func (s *shareCtl) leave(warp bool, why string) {
	if !s.remote {
		return
	}
	s.remote = false
	logf("<- torno a questo computer (%s)", why)
	if s.cl != nil {
		for k := range s.held {
			s.cl.send(encKey(k, 0))
		}
		for b := range s.heldBtn {
			s.cl.send(encButton(b, false))
		}
		s.cl.send([]byte{msgLeave})
		s.app.setState("connected", s.cl.name, s.cl.os)
	}
	clear(s.held)
	clear(s.heldBtn)
	s.cap.SetGrab(false)
	if warp && s.cl != nil {
		bx, by, bw, bh := s.cap.Bounds()
		fx := s.rx / float64(s.cl.w)
		fy := s.ry / float64(s.cl.h)
		x := bx + int(fx*float64(bw))
		y := by + int(fy*float64(bh))
		switch s.app.edge() {
		case "right":
			x = bx + bw - 3
		case "left":
			x = bx + 2
		case "bottom":
			y = by + bh - 3
		case "top":
			y = by + 2
		}
		s.cap.Warp(x, y)
		logf("   puntatore riportato in %d,%d", x, y)
	}
	if s.app.rippleOn() {
		showRipple(s.app.rippleRGB())
	}
}
