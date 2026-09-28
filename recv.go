package main

import (
	"encoding/hex"
	"errors"
	"net"
	"runtime"
	"sync"
	"time"
)

// recvCtl runs on the computer that is controlled by the other one.
type recvCtl struct {
	app  *App
	inj  inputInjector
	clip clipboard
	disc *discovery
	stop chan struct{}
	done chan struct{}
	wake chan struct{}

	mu      sync.Mutex
	pending *connectReq
	sc      *secureConn
	curID   string
}

type connectReq struct {
	id, addr, code string
}

func startRecv(a *App) (*recvCtl, error) {
	inj := makeInjector()
	if err := inj.Start(); err != nil {
		return nil, err
	}
	d, err := startDiscovery()
	if err != nil {
		logf("ricerca in rete non disponibile: %v", err)
	}
	r := &recvCtl{app: a, inj: inj, clip: makeClipboard(), disc: d, stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1)}
	a.setState("searching", "", "")
	go r.loop()
	return r, nil
}

func (r *recvCtl) Stop() {
	close(r.stop)
	r.mu.Lock()
	if r.sc != nil {
		r.sc.WriteMsg([]byte{msgBye}) // lets the other computer release its mouse at once
	}
	r.mu.Unlock()
	r.closeConn()
	<-r.done
	if r.disc != nil {
		r.disc.close()
	}
	r.inj.Close()
}

func (r *recvCtl) closeConn() {
	r.mu.Lock()
	if r.sc != nil {
		r.sc.Close()
	}
	r.mu.Unlock()
}

func (r *recvCtl) request(req connectReq) {
	r.mu.Lock()
	r.pending = &req
	r.mu.Unlock()
	r.closeConn()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *recvCtl) drop(id string) {
	r.mu.Lock()
	cur := r.curID
	r.mu.Unlock()
	if cur == id {
		r.closeConn()
	}
}

func (r *recvCtl) sleep(d time.Duration) bool {
	select {
	case <-r.stop:
		return false
	case <-r.wake:
	case <-time.After(d):
	}
	return true
}

func (r *recvCtl) loop() {
	defer close(r.done)
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		r.mu.Lock()
		req := r.pending
		r.pending = nil
		r.mu.Unlock()

		if req == nil {
			// Reconnect to the last computer we were paired with.
			r.app.mu.Lock()
			id := r.app.cfg.LastServer
			srv := r.app.cfg.Servers[id]
			r.app.mu.Unlock()
			if srv != nil {
				req = &connectReq{id: id, addr: srv.Addr}
				if r.disc != nil {
					if addr, ok := r.disc.lookup(id); ok {
						req.addr = addr
					}
				}
			}
		}
		if req == nil || req.addr == "" {
			r.app.setState("searching", "", "")
			if !r.sleep(time.Second) {
				return
			}
			continue
		}

		err := r.session(req)
		select {
		case <-r.stop:
			return
		default:
		}
		if errors.Is(err, errAuth) {
			if req.code != "" {
				r.app.setError("Codice non corretto", "Controlla il codice mostrato sull'altro computer e riprova.")
			} else {
				r.app.mu.Lock()
				delete(r.app.cfg.Servers, req.id)
				if r.app.cfg.LastServer == req.id {
					r.app.cfg.LastServer = ""
				}
				r.app.cfg.save()
				r.app.mu.Unlock()
				r.app.setError("L'abbinamento non è più valido", "Scegli di nuovo il computer e inserisci il codice che mostra.")
			}
			continue
		}
		if err != nil && req.code != "" {
			r.app.setError("Impossibile collegarsi", "Controlla che l'altro computer sia acceso, nella stessa rete, e che Ponte sia aperto in modalità Condividi.")
			continue
		}
		if !r.sleep(2 * time.Second) {
			return
		}
	}
}

func (r *recvCtl) session(req *connectReq) (err error) {
	mode, secret := authModeCode, []byte(req.code)
	if req.code == "" {
		r.app.mu.Lock()
		srv := r.app.cfg.Servers[req.id]
		r.app.mu.Unlock()
		if srv == nil {
			return errors.New("computer non abbinato")
		}
		mode, secret = authModeKey, srv.Key
	}
	r.app.setState("connecting", "", "")
	c, err := net.DialTimeout("tcp", req.addr, 5*time.Second)
	if err != nil {
		return err
	}
	sc, sid, err := clientHandshake(c, r.app.deviceID(), mode, secret)
	if err != nil {
		c.Close()
		return err
	}
	id := hex.EncodeToString(sid)
	if req.id != "" && req.id != id {
		sc.Close()
		return errors.New("computer diverso da quello atteso")
	}
	r.mu.Lock()
	r.sc, r.curID = sc, id
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.sc, r.curID = nil, ""
		r.mu.Unlock()
		sc.Close()
	}()
	select {
	case <-r.stop:
		return nil
	default:
	}

	w, h := r.inj.ScreenSize()
	if err := sc.WriteMsg(encHello(w, h, r.app.name(), runtime.GOOS)); err != nil {
		return err
	}
	msg, err := sc.ReadMsg(10 * time.Second)
	if err != nil || len(msg) == 0 || msg[0] != msgWelcome {
		return errors.New("risposta non valida")
	}
	rb := &rbuf{b: msg[1:]}
	name, key := rb.str(), rb.bytes()
	r.app.mu.Lock()
	srv := r.app.cfg.Servers[id]
	if len(key) == 32 {
		srv = &pairedServer{Key: key}
		r.app.cfg.Servers[id] = srv
	}
	peerOS := ""
	for _, f := range r.discList() {
		if f.ID == id {
			peerOS = f.OS
		}
	}
	if srv != nil {
		srv.Name, srv.Addr = name, req.addr
		if peerOS != "" {
			srv.OS = peerOS
		}
		peerOS = srv.OS
	}
	r.app.cfg.LastServer = id
	r.app.cfg.save()
	r.app.mu.Unlock()
	r.app.setState("connected", name, peerOS)
	logf("collegato a %s (%s)", name, req.addr)
	defer func() {
		if err == nil {
			logf("scollegato da %s: Ponte chiuso", name)
		} else {
			logf("scollegato da %s: %v", name, err)
		}
	}()

	clipStop := make(chan struct{})
	defer close(clipStop)
	cs := startClipSync(r.app, r.clip, func(b []byte) { sc.WriteMsg(b) }, clipStop)

	keys := map[uint16]bool{}
	btns := map[uint8]bool{}
	release := func() {
		for k := range keys {
			r.inj.Key(k, 0)
		}
		for b := range btns {
			r.inj.Button(b, false)
		}
		clear(keys)
		clear(btns)
		if cs, ok := r.inj.(cursorShower); ok {
			cs.ShowCursor(false)
		}
	}
	defer release()

	for {
		msg, err := sc.ReadMsg(peerTimeout + time.Second)
		if err != nil {
			return err
		}
		if len(msg) == 0 {
			continue
		}
		rb := &rbuf{b: msg[1:]}
		switch msg[0] {
		case msgBye:
			return nil
		case msgClipboard:
			cs.received(msg)
		case msgPing:
			if err := sc.WriteMsg([]byte{msgPing}); err != nil {
				return err
			}
		case msgEnter:
			x, y := rb.i32(), rb.i32()
			r.app.setState("active", name, peerOS)
			logf("-> %s usa questo computer", name)
			if cs, ok := r.inj.(cursorShower); ok {
				cs.ShowCursor(true)
			}
			go logCursor()
			r.inj.MouseAbs(int(x), int(y))
			if r.app.rippleOn() {
				showRipple(int(x), int(y), r.app.rippleRGB())
			}
		case msgLeave:
			release()
			r.app.setState("connected", name, peerOS)
			logf("<- %s torna al suo schermo", name)
		case msgMouse:
			r.inj.MouseAbs(int(rb.i32()), int(rb.i32()))
		case msgButton:
			b, down := rb.u8(), rb.u8() != 0
			if down {
				btns[b] = true
			} else {
				delete(btns, b)
			}
			r.inj.Button(b, down)
		case msgWheel:
			axis := rb.u8()
			r.inj.Wheel(axis, int(int16(rb.u16())))
		case msgKey:
			code, state := rb.u16(), rb.u8()
			if state == 0 {
				delete(keys, code)
			} else {
				keys[code] = true
			}
			r.inj.Key(code, state)
		}
	}
}

func (r *recvCtl) discList() []foundServer {
	if r.disc == nil {
		return nil
	}
	return r.disc.list()
}
