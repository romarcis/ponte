package main

import (
	"encoding/json"
	"net"
	"sort"
	"sync"
	"time"
)

// Every Ponte announces itself on the local network with a small UDP
// broadcast, so the others can list it without typing any address.

const (
	dataPort      = 24800
	discoveryPort = 24802
)

type beacon struct {
	App  string `json:"app"`
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
	Port int    `json:"port"`
	// Proto is the protocol version (PONTE/3 from 1.4 on; missing before),
	// so the window can say which computer needs an update.
	Proto   int    `json:"proto,omitempty"`
	Version string `json:"version,omitempty"`
}

const protoVersion = 3

type foundPeer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Addr   string `json:"addr"`
	Paired bool   `json:"paired"`
	Old    bool   `json:"old"`   // runs a Ponte too old to talk to this one
	Newer  bool   `json:"newer"` // runs a Ponte too new for this one
	Ver    string `json:"ver"`
	seen   time.Time
}

func broadcastAddrs() []string {
	out := []string{"255.255.255.255"}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := ipn.IP.To4()
			b := make(net.IP, 4)
			for i := range 4 {
				b[i] = ip[i] | ^ipn.Mask[len(ipn.Mask)-4+i]
			}
			out = append(out, b.String())
		}
	}
	return out
}

// announce broadcasts b every two seconds until stop is closed.
func announce(b func() beacon, stop <-chan struct{}) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		logf("annuncio in rete non disponibile: %v", err)
		return
	}
	defer conn.Close()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		msg, _ := json.Marshal(b())
		for _, a := range broadcastAddrs() {
			conn.WriteToUDP(msg, &net.UDPAddr{IP: net.ParseIP(a), Port: discoveryPort})
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

type discovery struct {
	mu    sync.Mutex
	found map[string]*foundPeer
	conn  *net.UDPConn
}

func startDiscovery() (*discovery, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: discoveryPort})
	if err != nil {
		return nil, err
	}
	d := &discovery{found: map[string]*foundPeer{}, conn: conn}
	go d.run()
	return d, nil
}

func (d *discovery) run() {
	buf := make([]byte, 2048)
	for {
		n, from, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var b beacon
		if json.Unmarshal(buf[:n], &b) != nil || b.App != "ponte" || b.ID == "" || b.Port == 0 {
			continue
		}
		d.mu.Lock()
		d.found[b.ID] = &foundPeer{
			ID: b.ID, Name: b.Name, OS: b.OS, Old: b.Proto < protoVersion, Newer: b.Proto > protoVersion, Ver: b.Version,
			Addr: net.JoinHostPort(from.IP.String(), itoa(b.Port)),
			seen: time.Now(),
		}
		d.mu.Unlock()
	}
}

// list returns the computers heard lately, except self.
func (d *discovery) list(self string) []foundPeer {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []foundPeer
	for id, f := range d.found {
		if time.Since(f.seen) > 7*time.Second {
			delete(d.found, id)
			continue
		}
		if id == self {
			continue
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (d *discovery) lookup(id string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.found[id]
	if !ok || time.Since(f.seen) > 7*time.Second {
		return "", false
	}
	return f.Addr, true
}

func (d *discovery) close() { d.conn.Close() }
