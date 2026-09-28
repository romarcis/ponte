package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// App holds the state shown in the window and the running node.
type App struct {
	mu  sync.Mutex
	cfg *config

	errMsg    string
	errHelp   string
	setupErr  bool // errMsg comes from starting: Riprova starts again
	code      string
	codeFails int
	codeUntil time.Time // wrong codes: no more tries until then

	node   *node
	paused bool // turned off for a while from the tray icon or the window

	startMu sync.Mutex // one start at a time
}

// peerView is a computer of the group, as the window shows it.
type peerView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Online bool   `json:"online"`
	Old    bool   `json:"old"` // runs an older Ponte: it needs the update
}

type status struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	OS          string          `json:"os"`
	Version     string          `json:"version"`
	State       string          `json:"state"`  // starting, ready, controlling, controlled, error
	Target      string          `json:"target"` // controlling: the computer's ID
	By          string          `json:"by"`     // controlled: the computer's ID
	Code        string          `json:"code"`
	EdgeSwitch  bool            `json:"edgeSwitch"`
	Error       string          `json:"error"`
	ErrorHelp   string          `json:"errorHelp"`
	Found       []foundPeer     `json:"found"` // on the network, not in the group
	Peers       []peerView      `json:"peers"`
	Layout      map[string]cell `json:"layout"`
	Clipboard   bool            `json:"clipboard"`
	ClipboardOK bool            `json:"clipboardOK"`
	Ripple      bool            `json:"ripple"`
	Color       string          `json:"color"`
	Theme       string          `json:"theme"`
	Update      string          `json:"update"` // newer version on GitHub
}

// version is set at build time (build.sh, from the release tag).
var version = "dev"

func newApp() *App {
	a := &App{cfg: loadConfig(), code: newPairingCode()}
	a.cfg.save()
	return a
}

func (a *App) status() status {
	a.mu.Lock()
	n := a.node
	s := status{
		ID: a.cfg.id(), Name: a.cfg.Name, OS: runtime.GOOS, Version: version, State: "starting",
		Code: a.code, Error: a.errMsg, ErrorHelp: a.errHelp,
		Found: []foundPeer{}, Peers: []peerView{}, Layout: a.cfg.Layout.clone().Pos,
		Clipboard: !a.cfg.NoClipboard, Ripple: !a.cfg.NoRipple, Color: a.cfg.Color, Theme: a.cfg.Theme, Update: updateAvailable(),
	}
	for id, p := range a.cfg.Peers {
		s.Peers = append(s.Peers, peerView{ID: id, Name: p.Name, OS: p.OS})
	}
	for id := range s.Layout {
		if _, ok := a.cfg.Peers[id]; !ok && id != s.ID {
			delete(s.Layout, id) // not known here (yet)
		}
	}
	paused := a.paused
	a.mu.Unlock()
	sort.Slice(s.Peers, func(i, j int) bool { return s.Peers[i].Name < s.Peers[j].Name })
	if n == nil {
		if paused {
			s.State = "paused"
		} else if s.Error != "" {
			s.State = "error"
		}
		return s
	}
	v := n.snapshot()
	s.State, s.Target, s.By = "ready", v.target, v.by
	switch {
	case v.by != "":
		s.State = "controlled"
	case v.target != "":
		s.State = "controlling"
	}
	s.EdgeSwitch, s.ClipboardOK = n.edgeOK, n.clip.Available()
	old := map[string]bool{}
	if n.disc != nil {
		for _, f := range n.disc.list(s.ID) {
			if _, paired := s.Layout[f.ID]; paired {
				old[f.ID] = f.Old
				continue
			}
			s.Found = append(s.Found, f)
		}
	}
	for i := range s.Peers {
		p := &s.Peers[i]
		p.Online = v.online[p.ID]
		p.Old = !p.Online && (old[p.ID] || v.errs[p.ID] == "old")
	}
	return s
}

func (a *App) setError(msg, help string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errMsg, a.errHelp, a.setupErr = msg, help, false
}

func (a *App) setSetupError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errMsg, a.errHelp, a.setupErr = err.Error(), "", true
	if se, ok := err.(*setupError); ok {
		a.errHelp = se.help
	}
}

func (a *App) clearError() { a.setError("", "") }

func newPairingCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

// start starts (again) the node: the connections to the other computers
// and this computer's mouse and keyboard.
func (a *App) start() error {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	a.shutdown()
	a.clearError()
	a.mu.Lock()
	wasPaused := a.paused
	a.paused = false
	a.mu.Unlock()
	if wasPaused {
		logf("Ponte riprende")
		colorChanged()
	}
	n, err := startNode(a)
	if err != nil {
		logf("avvio: %v", err)
		a.setSetupError(err)
		return err
	}
	a.mu.Lock()
	a.node = n
	a.mu.Unlock()
	return nil
}

// pause turns Ponte off until resume or the next start: this computer
// drops out of the group, and its mouse and keyboard stay its own.
func (a *App) pause() {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	a.mu.Lock()
	if a.paused {
		a.mu.Unlock()
		return
	}
	a.paused = true
	a.mu.Unlock()
	logf("Ponte in pausa")
	a.shutdown()
	a.clearError()
	colorChanged()
}

func (a *App) isPaused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.paused
}

func (a *App) clipboardOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.cfg.NoClipboard
}

func (a *App) setClipboard(on bool) {
	a.mu.Lock()
	a.cfg.NoClipboard = !on
	a.cfg.save()
	a.mu.Unlock()
}

func (a *App) rippleOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.cfg.NoRipple
}

func (a *App) setRipple(on bool) {
	a.mu.Lock()
	a.cfg.NoRipple = !on
	a.cfg.save()
	a.mu.Unlock()
}

// setColor sets the accent color of the window and of the circles; "" is
// the default.
func (a *App) setColor(c string) {
	c = strings.ToLower(c)
	if c != "" && !validColor(c) {
		return
	}
	a.mu.Lock()
	a.cfg.Color = c
	a.cfg.save()
	a.mu.Unlock()
	colorChanged()
}

func (a *App) setTheme(t string) {
	if t != "" && t != "light" && t != "dark" {
		return
	}
	a.mu.Lock()
	a.cfg.Theme = t
	a.cfg.save()
	a.mu.Unlock()
}

func validColor(c string) bool {
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	_, err := strconv.ParseUint(c[1:], 16, 32)
	return err == nil
}

// rippleRGB is the color of the circles.
func (a *App) rippleRGB() uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n, err := strconv.ParseUint(strings.TrimPrefix(a.cfg.Color, "#"), 16, 32); err == nil {
		return uint32(n)
	}
	return defaultRippleColor
}

func (a *App) setName(name string) {
	if name == "" || len(name) > 60 {
		return
	}
	a.mu.Lock()
	a.cfg.Name = name
	a.cfg.save()
	a.mu.Unlock()
}

func (a *App) name() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Name
}

func (a *App) deviceID() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.DeviceID
}

// lookupSecret authenticates a computer that connects: with the pairing
// code shown here, or with the key agreed when pairing.
func (a *App) lookupSecret(mode byte, clientID []byte) ([]byte, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if mode == authModeCode {
		if a.code == "" || time.Now().Before(a.codeUntil) {
			return nil, false
		}
		return []byte(a.code), true
	}
	p, ok := a.cfg.Peers[hex.EncodeToString(clientID)]
	if !ok {
		return nil, false
	}
	return p.Key, true
}

// codeFailed changes the pairing code after too many wrong attempts, and
// pauses the tries, so it cannot be guessed.
func (a *App) codeFailed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.codeFails++
	if a.codeFails >= 5 {
		a.code = newPairingCode()
		a.codeFails = 0
		a.codeUntil = time.Now().Add(10 * time.Second)
	}
}

// pairPeer stores a computer paired with the code (byCode: the code shown
// here was used, so it changes) or introduced by another one.
func (a *App) pairPeer(id, name, os, addr string, key []byte, byCode bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Peers[id] = &pairedPeer{Name: name, OS: os, Key: key, Addr: addr, Since: time.Now().UnixNano()}
	delete(a.cfg.Gone, id)
	a.cfg.Layout.place(id, a.cfg.id())
	a.cfg.save()
	if byCode {
		a.code = newPairingCode()
		a.codeFails = 0
	}
}

func (a *App) updatePeer(id, name, os, addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.cfg.Peers[id]; ok && (p.Name != name || p.OS != os || (addr != "" && p.Addr != addr)) {
		p.Name, p.OS = name, os
		if addr != "" {
			p.Addr = addr
		}
		a.cfg.save()
	}
}

// removePeer forgets a computer. With when, it was removed from the group
// at that time, and the others learn it; with bump, here, so the map
// changes on the others too.
func (a *App) removePeer(id string, when int64, bump bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if when != 0 {
		if a.cfg.Gone == nil {
			a.cfg.Gone = map[string]int64{}
		}
		a.cfg.Gone[id] = max(a.cfg.Gone[id], when)
	}
	if _, ok := a.cfg.Peers[id]; ok {
		delete(a.cfg.Peers, id)
		delete(a.cfg.Layout.Pos, id)
		if bump {
			a.cfg.Layout.Stamp = nextStamp(a.cfg.Layout.Stamp)
		}
	}
	a.cfg.save()
}

// nextStamp is the stamp of a change to the map: now, but always after the
// last one, in case another computer's clock is ahead.
func nextStamp(last int64) int64 { return max(time.Now().UnixNano(), last+1) }

// members lists the computers of the group and those removed from it.
func (a *App) members() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := wbuf{msgMembers}.u16(uint16(len(a.cfg.Peers)))
	for _, id := range sortedKeys(a.cfg.Peers) {
		p := a.cfg.Peers[id]
		b = b.str(id).str(p.Name).str(p.OS).str(p.Addr)
	}
	b = b.u16(uint16(len(a.cfg.Gone)))
	for _, id := range sortedKeys(a.cfg.Gone) {
		b = b.str(id).i64(a.cfg.Gone[id])
	}
	return b
}

// goneSince tells which computers of the group were removed from it after
// they were paired here.
func (a *App) goneSince(gone map[string]int64) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for id, t := range gone {
		if p, ok := a.cfg.Peers[id]; ok && p.Since < t {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (a *App) hasPeer(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.cfg.Peers[id]
	return ok
}

func (a *App) peerIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return sortedKeys(a.cfg.Peers)
}

func (a *App) peerKey(id string) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.cfg.Peers[id]; ok {
		return p.Key
	}
	return nil
}

func (a *App) peerAddr(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.cfg.Peers[id]; ok {
		return p.Addr
	}
	return ""
}

func (a *App) peerName(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.cfg.Peers[id]; ok {
		return p.Name
	}
	return id
}

// ---------- screen map ----------

func (a *App) layoutCopy() layout {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Layout.clone()
}

func (a *App) layoutNeighbor(id, dir string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Layout.neighbor(id, dir)
}

func (a *App) layoutDir(from, to string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Layout.dirTo(from, to)
}

func (a *App) layoutOrder() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Layout.order()
}

// touchLayout marks the map as changed now, so the other computers take
// it: after this one placed a computer that just paired with it.
func (a *App) touchLayout() layout {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Layout.Stamp = nextStamp(a.cfg.Layout.Stamp)
	a.cfg.save()
	return a.cfg.Layout.clone()
}

// adoptLayout takes the map of another computer.
func (a *App) adoptLayout(l layout, self string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Computers this one does not know yet stay on it: an introduction
	// may be on its way.
	l = l.clone()
	for _, id := range append(sortedKeys(a.cfg.Peers), self) {
		l.place(id, self)
	}
	a.cfg.Layout = l
	a.cfg.save()
}

// moveScreen puts a computer on another cell of the map, from the window,
// and tells the others.
func (a *App) moveScreen(id string, c cell) {
	a.mu.Lock()
	if _, ok := a.cfg.Layout.Pos[id]; !ok {
		a.mu.Unlock()
		return
	}
	a.cfg.Layout.move(id, c)
	a.cfg.Layout.Stamp = nextStamp(a.cfg.Layout.Stamp)
	a.cfg.save()
	l := a.cfg.Layout.clone()
	n := a.node
	a.mu.Unlock()
	if n != nil {
		n.broadcast(encLayout(l))
	}
}

func (a *App) forget(id string) {
	a.mu.Lock()
	n := a.node
	a.mu.Unlock()
	if n != nil {
		n.forget(id)
	} else {
		a.removePeer(id, time.Now().UnixNano(), true)
	}
}

func (a *App) connect(id, addr, code string) {
	a.mu.Lock()
	n := a.node
	a.mu.Unlock()
	if n != nil {
		n.pair(id, addr, code)
	}
}

func (a *App) shutdown() {
	a.mu.Lock()
	n := a.node
	a.node = nil
	a.mu.Unlock()
	if n != nil {
		n.Stop()
	}
}
