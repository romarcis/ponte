package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"runtime"
	"sort"
	"sync"
)

// App holds the state shown in the window and the running role.
type App struct {
	mu  sync.Mutex
	cfg *config

	state      string // see status.State
	peerName   string
	peerOS     string
	errMsg     string
	errHelp    string
	code       string
	codeFails  int
	edgeSwitch bool
	clipOK     bool

	share *shareCtl
	recv  *recvCtl
}

type peerView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
}

type status struct {
	Name        string        `json:"name"`
	OS          string        `json:"os"`
	Version     string        `json:"version"`
	Role        string        `json:"role"`
	State       string        `json:"state"` // share: waiting, connected, active; receive: searching, connecting, connected, active; both: error, idle
	Code        string        `json:"code"`
	Peer        string        `json:"peer"`
	PeerOS      string        `json:"peerOS"`
	Edge        string        `json:"edge"`
	EdgeSwitch  bool          `json:"edgeSwitch"`
	Error       string        `json:"error"`
	ErrorHelp   string        `json:"errorHelp"`
	Found       []foundServer `json:"found"`
	Clients     []peerView    `json:"clients"`
	Servers     []peerView    `json:"servers"`
	Target      string        `json:"target"`
	Clipboard   bool          `json:"clipboard"`
	ClipboardOK bool          `json:"clipboardOK"`
	Ripple      bool          `json:"ripple"`
}

// version is set at build time (build.sh, from the release tag).
var version = "dev"

func newApp() *App {
	a := &App{cfg: loadConfig(), state: "idle"}
	a.cfg.save()
	return a
}

func (a *App) status() status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := status{
		Name: a.cfg.Name, OS: runtime.GOOS, Version: version,
		Role: a.cfg.Role, State: a.state, Code: a.code,
		Peer: a.peerName, PeerOS: a.peerOS, Edge: a.cfg.Edge, EdgeSwitch: a.edgeSwitch,
		Error: a.errMsg, ErrorHelp: a.errHelp, Target: a.cfg.LastServer,
		Found: []foundServer{}, Clients: []peerView{}, Servers: []peerView{},
		Clipboard: !a.cfg.NoClipboard, ClipboardOK: a.clipOK, Ripple: !a.cfg.NoRipple,
	}
	for id, c := range a.cfg.Clients {
		s.Clients = append(s.Clients, peerView{id, c.Name, c.OS})
	}
	for id, c := range a.cfg.Servers {
		s.Servers = append(s.Servers, peerView{id, c.Name, c.OS})
	}
	sort.Slice(s.Clients, func(i, j int) bool { return s.Clients[i].Name < s.Clients[j].Name })
	sort.Slice(s.Servers, func(i, j int) bool { return s.Servers[i].Name < s.Servers[j].Name })
	if a.recv != nil && a.recv.disc != nil {
		for _, f := range a.recv.disc.list() {
			_, f.Paired = a.cfg.Servers[f.ID]
			s.Found = append(s.Found, f)
		}
	}
	return s
}

func (a *App) setState(state, peer, peerOS string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = state
	a.peerName = peer
	a.peerOS = peerOS
	if state == "connected" || state == "active" {
		a.errMsg, a.errHelp = "", ""
	}
}

func (a *App) setError(msg, help string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errMsg, a.errHelp = msg, help
}

func (a *App) clearError() { a.setError("", "") }

func newPairingCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

// setRole stops whatever runs now and starts the new role.
func (a *App) setRole(role string) error {
	a.mu.Lock()
	share, recv := a.share, a.recv
	a.share, a.recv = nil, nil
	a.mu.Unlock()
	if share != nil {
		share.Stop()
	}
	if recv != nil {
		recv.Stop()
	}

	a.mu.Lock()
	a.cfg.Role = role
	a.cfg.save()
	a.state, a.peerName, a.peerOS, a.errMsg, a.errHelp = "idle", "", "", "", ""
	a.code = ""
	a.mu.Unlock()

	var err error
	switch role {
	case "share":
		a.mu.Lock()
		a.code = newPairingCode()
		a.codeFails = 0
		a.mu.Unlock()
		var s *shareCtl
		if s, err = startShare(a); err == nil {
			a.mu.Lock()
			a.share = s
			a.edgeSwitch = s.cap.EdgeSwitch()
			a.clipOK = s.clip.Available()
			a.mu.Unlock()
		}
	case "receive":
		var r *recvCtl
		if r, err = startRecv(a); err == nil {
			a.mu.Lock()
			a.recv = r
			a.clipOK = r.clip.Available()
			a.mu.Unlock()
		}
	}
	if err != nil {
		logf("avvio ruolo %s: %v", role, err)
		a.mu.Lock()
		a.state = "error"
		a.errMsg = err.Error()
		if se, ok := err.(*setupError); ok {
			a.errHelp = se.help
		}
		a.mu.Unlock()
	}
	return err
}

func (a *App) setEdge(edge string) {
	switch edge {
	case "left", "right", "top", "bottom":
	default:
		return
	}
	a.mu.Lock()
	a.cfg.Edge = edge
	a.cfg.save()
	a.mu.Unlock()
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

func (a *App) edge() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Edge
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

// lookupSecret is used by the sharing side to authenticate a client.
func (a *App) lookupSecret(mode byte, clientID []byte) ([]byte, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if mode == authModeCode {
		if a.code == "" {
			return nil, false
		}
		return []byte(a.code), true
	}
	c, ok := a.cfg.Clients[hex.EncodeToString(clientID)]
	if !ok {
		return nil, false
	}
	return c.Key, true
}

// codeFailed changes the pairing code after too many wrong attempts, so it
// cannot be guessed.
func (a *App) codeFailed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.codeFails++
	if a.codeFails >= 5 {
		a.code = newPairingCode()
		a.codeFails = 0
	}
}

func (a *App) pairClient(id, name, os string, key []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Clients[id] = &pairedClient{Name: name, OS: os, Key: key}
	a.cfg.save()
	a.code = newPairingCode()
	a.codeFails = 0
}

func (a *App) updateClient(id, name, os string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.cfg.Clients[id]; ok && (c.Name != name || c.OS != os) {
		c.Name, c.OS = name, os
		a.cfg.save()
	}
}

func (a *App) forget(id string) {
	a.mu.Lock()
	delete(a.cfg.Clients, id)
	delete(a.cfg.Servers, id)
	if a.cfg.LastServer == id {
		a.cfg.LastServer = ""
	}
	a.cfg.save()
	share, recv := a.share, a.recv
	a.mu.Unlock()
	if share != nil {
		share.drop(id)
	}
	if recv != nil {
		recv.drop(id)
	}
}

func (a *App) connect(id, addr, code string) {
	a.mu.Lock()
	r := a.recv
	a.mu.Unlock()
	if r != nil {
		a.clearError()
		r.request(connectReq{id: id, addr: addr, code: code})
	}
}

func (a *App) shutdown() {
	a.mu.Lock()
	share, recv := a.share, a.recv
	a.share, a.recv = nil, nil
	a.mu.Unlock()
	if share != nil {
		share.Stop()
	}
	if recv != nil {
		recv.Stop()
	}
}
