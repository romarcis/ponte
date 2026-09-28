package main

import (
	"encoding/hex"
	"errors"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Every Ponte is equal: it keeps a connection to each computer of its group,
// reads its own mouse and keyboard, and replays what another computer sends
// it. Whoever touches a computer's mouse or keyboard controls from there:
// moving past a screen edge carries the pointer to the computer next to it
// on the screen map, and from there on to the next one. If someone uses the
// mouse or keyboard of a computer being controlled, that computer takes
// them back at once.

// listenAddr is where Ponte waits for the other computers (changed in
// tests, to run several on one machine).
var listenAddr = ":" + strconv.Itoa(dataPort)

// link is the connection to one computer of the group.
type link struct {
	sc       *secureConn
	id       string
	name, os string
	w, h     int
	addr     string // where it listens
	version  string
	dialed   bool // this computer dialed it
	fresh    bool // it just paired with this computer's code
	seen     atomic.Int64
	once     sync.Once
	in       inbox // files it is sending
}

func (l *link) close() { l.once.Do(func() { l.sc.Close() }) }

func (l *link) send(b []byte) {
	if err := l.sc.WriteMsg(b); err != nil {
		l.close()
	}
}

type linkMsg struct {
	l   *link
	msg []byte
}

// nodeView is what the window shows about the links.
type nodeView struct {
	online map[string]bool
	errs   map[string]string // per computer: why it cannot be reached
	target string            // the computer this one controls
	by     string            // the computer controlling this one
}

type node struct {
	app    *App
	id     string
	cap    inputCapture  // nil when this computer's mouse cannot be read
	inj    inputInjector // nil when input cannot be replayed here
	clip   clipboard
	cs     *clipSync
	disc   *discovery
	ln     net.Listener
	port   int
	edgeOK bool

	events chan inputEvent
	msgs   chan linkMsg
	newL   chan *link
	gone   chan *link
	calls  chan func()
	stop   chan struct{}
	done   chan struct{}

	all atomic.Pointer[[]*link] // for the clipboard, which runs on its own

	viewMu sync.Mutex
	view   nodeView

	// Owned by loop.
	links    map[string]*link
	dialing  map[string]bool
	lastDial map[string]time.Time
	since    map[string]time.Time // when each computer was last linked
	errs     map[string]string

	// Controlling another computer.
	target   *link
	rx, ry   float64
	switched time.Time
	local    map[uint16]time.Time // keys and buttons (1000+b) held here
	held     map[uint16]bool      // keys held on the target
	heldBtn  map[uint8]bool       // buttons held on the target

	// Controlled by another computer.
	by        *link
	injKeys   map[uint16]bool
	injBtns   map[uint8]bool
	refused   bool
	moves     []time.Time // own mouse motions while controlled
	lastCheck time.Time
}

func startNode(a *App) (*node, error) {
	n := &node{
		app:      a,
		id:       hex.EncodeToString(a.deviceID()),
		clip:     makeClipboard(),
		events:   make(chan inputEvent, 4096),
		msgs:     make(chan linkMsg, 256),
		newL:     make(chan *link),
		gone:     make(chan *link),
		calls:    make(chan func(), 16),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		links:    map[string]*link{},
		dialing:  map[string]bool{},
		lastDial: map[string]time.Time{},
		since:    map[string]time.Time{},
		errs:     map[string]string{},
		local:    map[uint16]time.Time{},
		held:     map[uint16]bool{},
		heldBtn:  map[uint8]bool{},
		injKeys:  map[uint16]bool{},
		injBtns:  map[uint8]bool{},
	}
	n.all.Store(&[]*link{})

	var capErr, injErr error
	cap := makeCapture()
	if capErr = cap.Start(n.events); capErr == nil {
		n.cap = cap
		n.edgeOK = cap.EdgeSwitch()
	} else {
		logf("mouse e tastiera di questo computer non leggibili: %v", capErr)
	}
	inj := makeInjector()
	if injErr = inj.Start(); injErr == nil {
		n.inj = inj
	} else {
		logf("input non riproducibile su questo computer: %v", injErr)
	}
	if n.cap == nil && n.inj == nil {
		return nil, capErr
	}
	if err := firstErr(capErr, injErr); err != nil {
		a.setSetupError(err)
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		// The others can still be reached from here.
		logf("porta %s non disponibile: %v", listenAddr, err)
		a.setSetupError(&setupError{
			msg:  "La porta di rete 24800 è già in uso",
			help: "Forse è aperto un altro programma di condivisione (Synergy, Barrier, Input Leap, Input Director). Chiudilo e riavvia Ponte.",
		})
	} else {
		n.ln = ln
		n.port = ln.Addr().(*net.TCPAddr).Port
		go n.acceptLoop()
	}
	if d, err := startDiscovery(); err == nil {
		n.disc = d
	} else {
		logf("ricerca in rete non disponibile: %v", err)
	}
	now := time.Now()
	for _, id := range a.peerIDs() {
		n.since[id] = now
	}
	n.cs = startClipSync(a, n.clip, n.broadcast, n.stop)
	go n.loop()
	if n.port != 0 {
		go announce(func() beacon {
			return beacon{App: "ponte", ID: n.id, Name: a.name(), OS: runtime.GOOS, Port: n.port, Proto: protoVersion, Version: version}
		}, n.stop)
	}
	return n, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (n *node) Stop() {
	close(n.stop)
	if n.ln != nil {
		n.ln.Close()
	}
	<-n.done
	if n.disc != nil {
		n.disc.close()
	}
	if n.cap != nil {
		n.cap.Stop()
	}
	if n.inj != nil {
		n.inj.Close()
	}
}

// call runs f on the loop.
func (n *node) call(f func()) {
	select {
	case n.calls <- f:
	case <-n.done:
	}
}

func (n *node) broadcast(b []byte) {
	for _, l := range *n.all.Load() {
		l.send(b)
	}
}

func (n *node) snapshot() nodeView {
	n.viewMu.Lock()
	defer n.viewMu.Unlock()
	return n.view
}

// publish refreshes what the window and the clipboard see; loop only.
func (n *node) publish() {
	v := nodeView{online: map[string]bool{}, errs: map[string]string{}}
	all := make([]*link, 0, len(n.links))
	for id, l := range n.links {
		v.online[id] = true
		all = append(all, l)
	}
	for id, e := range n.errs {
		v.errs[id] = e
	}
	if n.target != nil {
		v.target = n.target.id
	}
	if n.by != nil {
		v.by = n.by.id
	}
	n.all.Store(&all)
	n.viewMu.Lock()
	n.view = v
	n.viewMu.Unlock()
}

// ---------- connections ----------

func (n *node) acceptLoop() {
	for {
		c, err := n.ln.Accept()
		if err != nil {
			return
		}
		go n.accept(c)
	}
}

func (n *node) screen() (int, int) {
	if n.inj != nil {
		return n.inj.ScreenSize()
	}
	_, _, w, h := n.cap.Bounds()
	return w, h
}

func (n *node) accept(c net.Conn) {
	sc, cid, mode, err := serverHandshake(c, n.app.deviceID(), n.app.lookupSecret)
	if err != nil {
		c.Close()
		if errors.Is(err, errAuth) && mode == authModeCode {
			n.app.codeFailed()
		}
		logf("collegamento rifiutato da %s: %v", c.RemoteAddr(), err)
		return
	}
	msg, err := sc.ReadMsg(10 * time.Second)
	if err != nil || len(msg) == 0 || msg[0] != msgHello {
		sc.Close()
		return
	}
	r := &rbuf{b: msg[1:]}
	l := &link{sc: sc, id: hex.EncodeToString(cid)}
	l.w, l.h = int(r.i32()), int(r.i32())
	l.name, l.os = r.str(), r.str()
	port := int(r.u16())
	l.version = r.str()
	if r.err != nil || l.w <= 0 || l.h <= 0 || l.id == n.id {
		sc.Close()
		return
	}
	if host, _, err := net.SplitHostPort(c.RemoteAddr().String()); err == nil && port != 0 {
		l.addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	var key []byte
	if mode == authModeCode {
		key = randomBytes(32)
		n.app.pairPeer(l.id, l.name, l.os, l.addr, key)
		l.fresh = true
		logf("abbinato %s", l.name)
	} else {
		n.app.updatePeer(l.id, l.name, l.os, l.addr)
	}
	w, h := n.screen()
	if err := sc.WriteMsg(encWelcome(n.app.name(), key, w, h, runtime.GOOS, n.port)); err != nil {
		sc.Close()
		return
	}
	n.run(l)
}

// dial connects to a computer: with code, to pair with it.
func (n *node) dial(id, addr, code string) {
	err := n.dialErr(id, addr, code)
	n.call(func() {
		delete(n.dialing, id)
		switch {
		case err == nil:
			delete(n.errs, id)
		case errors.Is(err, errAuth) && code != "":
			n.app.setError("Codice non corretto", "Controlla il codice mostrato sull'altro computer e riprova.")
		case errors.Is(err, errAuth):
			// The other computer forgot this one: so does this one.
			logf("%s non riconosce più questo computer: abbinamento rimosso", n.app.peerName(id))
			n.forgetLocal(id)
		case errors.Is(err, errOldPeer):
			n.errs[id] = "old"
		case code != "":
			n.app.setError("Impossibile collegarsi", "Controlla che l'altro computer sia acceso, nella stessa rete, e che Ponte sia aperto.")
		}
		n.publish()
	})
}

func (n *node) dialErr(id, addr, code string) error {
	mode, secret := authModeCode, []byte(code)
	var expect []byte
	if id != "" {
		expect, _ = hex.DecodeString(id)
	}
	if code == "" {
		key := n.app.peerKey(id)
		if key == nil {
			return errors.New("computer non abbinato")
		}
		mode, secret = authModeKey, key
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	sc, sid, err := clientHandshake(c, n.app.deviceID(), mode, secret, expect)
	if err != nil {
		c.Close()
		return err
	}
	w, h := n.screen()
	if err := sc.WriteMsg(encHello(w, h, n.app.name(), runtime.GOOS, n.port)); err != nil {
		sc.Close()
		return err
	}
	msg, err := sc.ReadMsg(10 * time.Second)
	if err != nil || len(msg) == 0 || msg[0] != msgWelcome {
		sc.Close()
		return errors.New("risposta non valida")
	}
	r := &rbuf{b: msg[1:]}
	l := &link{sc: sc, id: hex.EncodeToString(sid), dialed: true}
	l.name = r.str()
	key := r.bytes()
	l.w, l.h = int(r.i32()), int(r.i32())
	l.os = r.str()
	port := int(r.u16())
	l.version = r.str()
	if r.err != nil || l.w <= 0 || l.h <= 0 || l.id == n.id {
		sc.Close()
		return errors.New("risposta non valida")
	}
	l.addr = addr
	if host, _, err := net.SplitHostPort(addr); err == nil && port != 0 {
		l.addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	if len(key) == 32 {
		n.app.pairPeer(l.id, l.name, l.os, l.addr, key)
		logf("abbinato a %s", l.name)
	} else {
		n.app.updatePeer(l.id, l.name, l.os, l.addr)
	}
	go n.run(l)
	return nil
}

// run hands a new link to the loop and reads from it until it closes.
func (n *node) run(l *link) {
	l.seen.Store(time.Now().UnixNano())
	select {
	case n.newL <- l:
	case <-n.stop:
		l.close()
		return
	}
	for {
		msg, err := l.sc.ReadMsg(peerTimeout + time.Second)
		if err == nil && len(msg) > 0 {
			l.seen.Store(time.Now().UnixNano())
			switch msg[0] {
			case msgClipboard, msgFileStart, msgFileEntry, msgFileData, msgFileEnd:
				n.cs.receivedIn(&l.in, msg)
				continue
			case msgBye:
				err = errors.New("Ponte chiuso sull'altro computer")
			default:
				select {
				case n.msgs <- linkMsg{l, msg}:
				case <-n.stop:
					return
				}
				continue
			}
		}
		if err != nil {
			l.close()
			select {
			case n.gone <- l:
			case <-n.stop:
			}
			return
		}
	}
}

// dialer tells which computer opened l.
func (n *node) dialer(l *link) string {
	if l.dialed {
		return n.id
	}
	return l.id
}

// added takes a new link. Two computers may dial each other at the same
// time: both keep the connection opened by the one with the lower ID.
func (n *node) added(l *link) {
	if old := n.links[l.id]; old != nil {
		low := min(n.id, l.id)
		if n.dialer(old) == low && n.dialer(l) != low {
			l.close()
			return
		}
		n.dropLink(old, "collegamento sostituito")
		old.close()
	}
	n.links[l.id] = l
	delete(n.errs, l.id)
	logf("collegato %s (%s, %dx%d, Ponte %s)", l.name, l.addr, l.w, l.h, l.version)
	l.send(encLayout(n.app.layoutCopy()))
	if l.fresh {
		n.introduce(l)
	}
	n.publish()
}

// introduce pairs a computer that just joined with the others of the group
// this one is linked to, so its code is typed only once; and puts it on the
// map next to this one.
func (n *node) introduce(l *link) {
	for id, o := range n.links {
		if id == l.id {
			continue
		}
		key := randomBytes(32)
		o.send(wbuf{msgIntro}.str(l.id).str(l.name).str(l.os).str(l.addr).bytes(key))
		l.send(wbuf{msgIntro}.str(o.id).str(o.name).str(o.os).str(o.addr).bytes(key))
	}
	n.broadcast(encLayout(n.app.touchLayout()))
}

// dropLink forgets a link that closed; loop only.
func (n *node) dropLink(l *link, why string) {
	if n.links[l.id] != l {
		return
	}
	delete(n.links, l.id)
	n.since[l.id] = time.Now()
	if n.target == l {
		n.target = nil
		n.leave("", 0, why)
	}
	if n.by == l {
		n.release()
		logf("<- %s non controlla più questo computer (%s)", l.name, why)
	}
	logf("scollegato %s: %s", l.name, why)
	n.publish()
}

// maintain dials the computers of the group that are not linked. The one
// with the lower ID dials first; the other tries too after a while, in
// case a firewall lets connections through in one direction only.
func (n *node) maintain() {
	for _, id := range n.app.peerIDs() {
		if n.links[id] != nil || n.dialing[id] || time.Since(n.lastDial[id]) < 3*time.Second {
			continue
		}
		if n.id > id && time.Since(n.since[id]) < 6*time.Second {
			continue
		}
		addr := n.app.peerAddr(id)
		if n.disc != nil {
			if a, ok := n.disc.lookup(id); ok {
				addr = a
			}
		}
		if addr == "" {
			continue
		}
		n.dialing[id] = true
		n.lastDial[id] = time.Now()
		go n.dial(id, addr, "")
	}
}

// pair dials a computer with the code it shows.
func (n *node) pair(id, addr, code string) {
	n.call(func() {
		n.app.clearError()
		go n.dial(id, addr, code)
	})
}

// forget removes a computer from the group, here and on the others.
func (n *node) forget(id string) {
	n.call(func() {
		for oid, l := range n.links {
			if oid != id {
				l.send(wbuf{msgForget}.str(id))
			}
		}
		n.forgetLocal(id)
		n.broadcast(encLayout(n.app.layoutCopy()))
	})
}

func (n *node) forgetLocal(id string) {
	n.app.removePeer(id)
	delete(n.errs, id)
	if l := n.links[id]; l != nil {
		n.dropLink(l, "computer dimenticato")
		l.close()
	}
	n.publish()
}

// ---------- loop ----------

func (n *node) loop() {
	defer close(n.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	n.maintain()
	for {
		select {
		case <-n.stop:
			if n.target != nil {
				n.leave("", 0, "Ponte chiuso")
			}
			n.release()
			for _, l := range n.links {
				l.send([]byte{msgBye})
				l.close()
			}
			return
		case f := <-n.calls:
			f()
		case l := <-n.newL:
			n.added(l)
		case l := <-n.gone:
			n.dropLink(l, "scollegato")
		case m := <-n.msgs:
			n.handleMsg(m.l, m.msg)
		case ev := <-n.events:
			n.handle(ev)
		case <-tick.C:
			for _, l := range n.links {
				if time.Since(time.Unix(0, l.seen.Load())) > peerTimeout {
					// Never keep this computer's mouse and keyboard
					// captured for a computer that stopped answering.
					logf("%s non risponde", l.name)
					n.dropLink(l, "non risponde")
					l.close()
					continue
				}
				l.send([]byte{msgPing})
			}
			n.checkRefused()
			n.maintain()
		}
	}
}

func (n *node) handleMsg(l *link, msg []byte) {
	if n.links[l.id] != l {
		return
	}
	r := &rbuf{b: msg[1:]}
	switch msg[0] {
	case msgPing:
		if cs, ok := n.inj.(cursorShower); ok && n.by == l {
			cs.CheckCursor()
		}
	case msgLayout:
		n.gotLayout(l, msg)
	case msgIntro:
		id, name, os, addr, key := r.str(), r.str(), r.str(), r.str(), r.bytes()
		if r.err == nil && len(key) == 32 && id != n.id && n.app.peerKey(id) == nil {
			n.app.pairPeer(id, name, os, addr, key)
			n.since[id] = time.Time{}
			logf("%s presenta %s: abbinati", l.name, name)
			n.maintain()
		}
	case msgForget:
		if id := r.str(); r.err == nil && id != n.id && n.app.peerKey(id) != nil {
			logf("%s ha tolto %s dal gruppo", l.name, n.app.peerName(id))
			n.forgetLocal(id)
		}
	case msgBlocked:
		why := r.str()
		if r.err != nil || why == "" {
			why = "c'è una richiesta di amministratore o la schermata di blocco"
		}
		// Windows there discards what Ponte replays: give the mouse back
		// instead of leaving it stuck.
		if n.target == l {
			dir, _ := n.app.layoutDir(l.id, n.id)
			f := n.frac(dir, l)
			n.target = n.leaveTarget()
			n.leave(dir, f, l.name+" non accetta input: "+why)
			notify(l.name+" non accetta il mouse", "Su "+l.name+" "+why+": Ponte non può comandarlo. Il mouse è tornato qui.")
		}
	case msgTakeover:
		if n.target == l {
			n.target = n.leaveTarget()
			n.leave("", 0, "su "+l.name+" si usa il suo mouse")
		}
	default:
		n.replay(l, msg[0], r)
	}
}

func (n *node) gotLayout(l *link, msg []byte) {
	theirs, ok := decLayout(msg)
	if !ok {
		return
	}
	mine := n.app.layoutCopy()
	switch {
	case theirs.Stamp > mine.Stamp || (theirs.Stamp == mine.Stamp && l.id < n.id && !sameLayout(theirs, mine)):
		n.app.adoptLayout(theirs, n.id)
		n.publish()
	case theirs.Stamp < mine.Stamp:
		l.send(encLayout(mine))
	}
}

func sameLayout(a, b layout) bool {
	if len(a.Pos) != len(b.Pos) {
		return false
	}
	for id, p := range a.Pos {
		if q, ok := b.Pos[id]; !ok || p != q {
			return false
		}
	}
	return true
}

// ---------- controlled from another computer ----------

func (n *node) replay(l *link, kind byte, r *rbuf) {
	if n.inj == nil {
		return
	}
	if kind == msgEnter {
		x, y := r.i32(), r.i32()
		if n.by != nil && n.by != l {
			n.by.send([]byte{msgTakeover})
			n.release()
		}
		n.by, n.refused, n.moves = l, false, nil
		logf("-> %s usa questo computer", l.name)
		if cs, ok := n.inj.(cursorShower); ok {
			cs.ShowCursor(true)
		}
		go logCursor()
		n.inj.MouseAbs(int(x), int(y))
		if n.app.rippleOn() {
			showRipple(n.app.rippleRGB())
		}
		n.publish()
		return
	}
	if n.by != l {
		return
	}
	switch kind {
	case msgLeave:
		n.release()
		logf("<- %s torna al suo schermo", l.name)
		n.publish()
	case msgMouse:
		n.inj.MouseAbs(int(r.i32()), int(r.i32()))
		if time.Since(n.lastCheck) > 250*time.Millisecond {
			n.lastCheck = time.Now()
			if cs, ok := n.inj.(cursorShower); ok {
				cs.CheckCursor()
			}
			n.checkRefused()
		}
	case msgButton:
		b, down := r.u8(), r.u8() != 0
		if down {
			n.injBtns[b] = true
		} else {
			delete(n.injBtns, b)
		}
		n.inj.Button(b, down)
		n.checkRefused()
	case msgWheel:
		axis := r.u8()
		n.inj.Wheel(axis, int(int16(r.u16())))
	case msgKey:
		code, state := r.u16(), r.u8()
		if state == 0 {
			delete(n.injKeys, code)
		} else {
			n.injKeys[code] = true
		}
		n.inj.Key(code, state)
		if name := capsKeys[code]; name != "" && state != 2 {
			logf("   %s %s", name, map[uint8]string{0: "rilasciato", 1: "premuto"}[state])
		}
	}
}

// checkRefused tells the controlling computer when Windows refuses the
// replayed input (administrator prompt, lock screen, a program running as
// administrator in front), so it takes its mouse back.
func (n *node) checkRefused() {
	if n.by == nil || n.refused {
		return
	}
	if why := refusedReason(); why != "" {
		n.refused = true
		logf("   Windows scarta l'input di Ponte: %s; lo dico a %s, che riprende il suo mouse", why, n.by.name)
		n.by.send(wbuf{msgBlocked}.str(why))
	}
}

// release lets go of what the controlling computer held down here.
func (n *node) release() {
	if n.inj != nil {
		for k := range n.injKeys {
			n.inj.Key(k, 0)
		}
		for b := range n.injBtns {
			n.inj.Button(b, false)
		}
		if cs, ok := n.inj.(cursorShower); ok && n.by != nil {
			cs.ShowCursor(false)
		}
	}
	clear(n.injKeys)
	clear(n.injBtns)
	n.by = nil
}

// takeBack gives this computer back to whoever uses its own mouse or
// keyboard, and tells the computer that was controlling it.
func (n *node) takeBack() {
	l := n.by
	l.send([]byte{msgTakeover})
	n.release()
	logf("<- si usa il mouse di questo computer: %s lo lascia", l.name)
	n.publish()
}

// ---------- this computer's mouse and keyboard ----------

func (n *node) handle(ev inputEvent) {
	if n.by != nil {
		// Someone is using this computer's own mouse or keyboard while
		// another computer controls it: one motion may be the desk
		// shaking, a few in a row are a hand.
		own := false
		switch ev.kind {
		case evMotion:
			now := time.Now()
			keep := n.moves[:0]
			for _, t := range n.moves {
				if now.Sub(t) < 300*time.Millisecond {
					keep = append(keep, t)
				}
			}
			n.moves = append(keep, now)
			own = len(n.moves) >= 4
		case evButton, evKey:
			own = ev.val == 1
		case evWheel:
			own = true
		}
		if !own {
			return
		}
		n.takeBack()
	}
	switch ev.kind {
	case evPos:
		if n.target == nil && n.settled() && !n.heldLocally() {
			n.atEdge(int(ev.x), int(ev.y))
		}
	case evRel:
		if n.target != nil {
			n.move(float64(ev.x), float64(ev.y))
		}
	case evButton:
		b := uint8(ev.code)
		if n.target != nil {
			if ev.val != 0 {
				n.heldBtn[b] = true
			} else {
				delete(n.heldBtn, b)
			}
			n.target.send(encButton(b, ev.val != 0))
		} else if ev.val != 0 {
			n.local[1000+ev.code] = time.Now()
		} else {
			delete(n.local, 1000+ev.code)
		}
	case evWheel:
		if n.target != nil {
			n.target.send(encWheel(uint8(ev.code), int(ev.val)))
		}
	case evKey:
		if ev.code == keyScrollLock {
			// Act on release, so the key is never left pressed anywhere.
			if ev.val == 0 {
				n.next()
			}
			return
		}
		if ev.code == keyEsc && ev.val == 1 && n.target != nil && modsHeld(n.held) {
			n.target = n.leaveTarget()
			n.leave("", 0, "Ctrl+Alt+Esc") // the emergency way back
			return
		}
		if n.target != nil {
			if ev.val == 0 {
				delete(n.held, ev.code)
			} else {
				n.held[ev.code] = true
			}
			n.target.send(encKey(ev.code, uint8(ev.val)))
			if name := capsKeys[ev.code]; name != "" && ev.val != 2 {
				logf("   %s %s, inviato", name, map[int32]string{0: "rilasciato", 1: "premuto"}[ev.val])
			}
		} else if ev.val == 0 {
			delete(n.local, ev.code)
		} else {
			n.local[ev.code] = time.Now()
		}
	}
}

func modsHeld(keys map[uint16]bool) bool {
	return (keys[keyLeftCtrl] || keys[keyRightCtrl]) && (keys[keyLeftAlt] || keys[keyRightAlt])
}

func (n *node) heldLocally() bool {
	for k, t := range n.local {
		if time.Since(t) > 10*time.Second {
			delete(n.local, k) // a release we never saw
			continue
		}
		return true
	}
	return false
}

// switchGuard is how long after a switch the screen edge cannot switch
// again. Without it a pointer pushed against the edge bounced back and forth
// between the computers several times a second.
var switchGuard = 300 * time.Millisecond

func (n *node) settled() bool { return time.Since(n.switched) >= switchGuard }

// atEdge moves over to the computer next to the screen edge the pointer
// reached, if there is one.
func (n *node) atEdge(x, y int) {
	bx, by, bw, bh := n.cap.Bounds()
	fx := float64(x-bx) / float64(max(bw, 1))
	fy := float64(y-by) / float64(max(bh, 1))
	for _, e := range []struct {
		dir string
		hit bool
		f   float64
	}{
		{"right", x >= bx+bw-1, fy}, {"left", x <= bx, fy},
		{"bottom", y >= by+bh-1, fx}, {"top", y <= by, fx},
	} {
		if !e.hit {
			continue
		}
		if id, ok := n.app.layoutNeighbor(n.id, e.dir); ok {
			if l := n.links[id]; l != nil {
				n.enter(l, e.dir, e.f, false, "bordo dello schermo")
				return
			}
		}
	}
}

// cursorRoom keeps the pointer this far from the bottom and right sides
// when it changes screen: arriving in a corner left the arrow off screen, as
// if it had vanished.
// ponytail: fixed pixels, scale with the other screen's DPI if arrows still get clipped.
const cursorRoom = 40

// enter moves the pointer onto l's screen, arriving from its side opposite
// to dir at fraction f along that edge; center puts it in the middle.
func (n *node) enter(l *link, dir string, f float64, center bool, why string) {
	w, h := float64(l.w), float64(l.h)
	switch {
	case center:
		n.rx, n.ry = w/2, h/2
	case dir == "right":
		n.rx, n.ry = 0, awayFromCorners(f*h, h)
	case dir == "left":
		n.rx, n.ry = w-1, awayFromCorners(f*h, h)
	case dir == "bottom":
		n.rx, n.ry = awayFromCorners(f*w, w), 0
	case dir == "top":
		n.rx, n.ry = awayFromCorners(f*w, w), h-1
	}
	if n.target != nil {
		n.leaveTarget()
	} else {
		n.cap.SetGrab(true)
	}
	n.target = l
	n.switched = time.Now()
	l.send(encEnter(int(n.rx), int(n.ry)))
	logf("-> passo a %s (%s): là %d,%d", l.name, why, int(n.rx), int(n.ry))
	n.publish()
}

// leaveTarget lets go of the computer being controlled, releasing what was
// held on it; it returns nil, for n.target.
func (n *node) leaveTarget() *link {
	if t := n.target; t != nil {
		for k := range n.held {
			t.send(encKey(k, 0))
		}
		for b := range n.heldBtn {
			t.send(encButton(b, false))
		}
		t.send([]byte{msgLeave})
	}
	clear(n.held)
	clear(n.heldBtn)
	return nil
}

// frac is where the pointer is along the edge of l's screen it leaves by
// going in dir.
func (n *node) frac(dir string, l *link) float64 {
	if dir == "left" || dir == "right" {
		return n.ry / float64(max(l.h, 1))
	}
	return n.rx / float64(max(l.w, 1))
}

func (n *node) move(dx, dy float64) {
	t := n.target
	w, h := float64(t.w), float64(t.h)
	n.rx += dx
	n.ry += dy
	dir := ""
	switch {
	case n.rx < 0:
		dir = "left"
	case n.rx > w-1:
		dir = "right"
	case n.ry < 0:
		dir = "top"
	case n.ry > h-1:
		dir = "bottom"
	}
	if dir != "" && len(n.heldBtn) == 0 && n.settled() {
		if id, ok := n.app.layoutNeighbor(t.id, dir); ok {
			f := n.frac(dir, t)
			if id == n.id && n.edgeOK {
				n.target = n.leaveTarget()
				n.leave(dir, f, "bordo dello schermo")
				return
			}
			if l := n.links[id]; l != nil {
				n.enter(l, dir, f, false, "bordo dello schermo")
				return
			}
		}
	}
	n.rx = min(max(n.rx, 0), w-1)
	n.ry = min(max(n.ry, 0), h-1)
	t.send(encMouse(int(n.rx), int(n.ry)))
}

// leave gives the pointer back to this computer; the caller already let go
// of the target. With dir, the pointer comes in from the edge crossed going
// in dir, at fraction f along it; without, it stays where it was parked.
func (n *node) leave(dir string, f float64, why string) {
	if n.cap == nil {
		return
	}
	n.switched = time.Now()
	logf("<- torno a questo computer (%s)", why)
	clear(n.held)
	clear(n.heldBtn)
	n.cap.SetGrab(false)
	if dir != "" && n.edgeOK {
		bx, by, bw, bh := n.cap.Bounds()
		x := bx + int(awayFromCorners(f*float64(bw), float64(bw)))
		y := by + int(awayFromCorners(f*float64(bh), float64(bh)))
		switch dir {
		case "left": // came in by the right edge
			x, y = bx+bw-3, min(y, by+bh-cursorRoom)
		case "right":
			x, y = bx+2, min(y, by+bh-cursorRoom)
		case "top":
			x, y = min(x, bx+bw-cursorRoom), by+bh-3
		case "bottom":
			x, y = min(x, bx+bw-cursorRoom), by+2
		}
		n.cap.Warp(x, y)
		logf("   puntatore riportato in %d,%d", x, y)
	}
	if n.app.rippleOn() {
		showRipple(n.app.rippleRGB())
	}
	n.publish()
}

// next is Scroll Lock: on to the next computer of the map that is on, and
// back here after the last.
func (n *node) next() {
	if n.cap == nil {
		return
	}
	var ring []string
	for _, id := range n.app.layoutOrder() {
		if id == n.id || n.links[id] != nil {
			ring = append(ring, id)
		}
	}
	cur := n.id
	if n.target != nil {
		cur = n.target.id
	}
	i := 0
	for j, id := range ring {
		if id == cur {
			i = j
		}
	}
	nxt := ring[(i+1)%len(ring)]
	if nxt == n.id {
		if n.target != nil {
			n.target = n.leaveTarget()
			n.leave("", 0, "Bloc Scorr")
		}
		return
	}
	n.enter(n.links[nxt], "", 0, true, "Bloc Scorr")
}

// awayFromCorners keeps a coordinate along the shared edge a little inside
// the screen, so the pointer never lands exactly in a corner, where it is
// hard to see (at the bottom only its tip is on screen) and where the two
// screens, of different sizes, meet their ends. The margin is at least
// cursorRoom, so the arrow is never drawn off screen.
func awayFromCorners(v, size float64) float64 {
	m := max(cursorRoom, size*0.03)
	if size <= 2*m {
		return size / 2
	}
	return min(max(v, m), size-1-m)
}

// capsKeys are logged on the controlled computer, to follow capital letters.
var capsKeys = map[uint16]string{42: "Shift sinistro", 54: "Shift destro", 58: "Bloc Maiusc"}
