package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// pairedPeer is a computer of the group: the key was agreed when pairing.
type pairedPeer struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	Key  []byte `json:"key"`
	Addr string `json:"addr,omitempty"` // last address it answered on
}

// Before version 1.4 a computer either shared or received: these are read
// only to move the pairings over.
type pairedClient struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	Key  []byte `json:"key"`
}

type pairedServer struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	Key  []byte `json:"key"`
	Addr string `json:"addr"`
}

type config struct {
	DeviceID      []byte                 `json:"device_id"`
	Name          string                 `json:"name"`
	UIToken       string                 `json:"ui_token"`
	NoClipboard   bool                   `json:"no_clipboard"`
	NoRipple      bool                   `json:"no_ripple"`
	Color         string                 `json:"color"` // accent color "#rrggbb", "" = default
	TrayHintShown bool                   `json:"tray_hint_shown"`
	AdminDeclined bool                   `json:"admin_declined"` // said no to administrator rights
	Peers         map[string]*pairedPeer `json:"peers"`
	Layout        layout                 `json:"layout"`

	Role    string                   `json:"role,omitempty"` // before 1.4: "", "share", "receive"
	Edge    string                   `json:"edge,omitempty"` // before 1.4: where the other screen sat
	Clients map[string]*pairedClient `json:"clients,omitempty"`
	Servers map[string]*pairedServer `json:"servers,omitempty"`
}

func (c *config) id() string { return hex.EncodeToString(c.DeviceID) }

func configDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "Ponte")
}

func loadConfig() *config {
	c := &config{}
	if b, err := os.ReadFile(filepath.Join(configDir(), "config.json")); err == nil {
		json.Unmarshal(b, c)
	}
	if len(c.DeviceID) != 16 {
		c.DeviceID = randomBytes(16)
	}
	if c.Name == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "Il mio computer"
		}
		c.Name = strings.TrimSuffix(h, ".local")
	}
	if c.UIToken == "" {
		c.UIToken = hex.EncodeToString(randomBytes(16))
	}
	if c.Peers == nil {
		c.Peers = map[string]*pairedPeer{}
	}
	c.migrate()
	if c.Layout.Pos == nil {
		c.Layout.Pos = map[string]cell{}
	}
	c.Layout.keep(append(sortedKeys(c.Peers), c.id()), c.id())
	return c
}

// migrate moves the pairings of Ponte 1.3 and earlier over: the key is the
// same on both computers. The sharing computer knew where the other screen
// was, so its map wins over the default one of the receiving computer.
func (c *config) migrate() {
	if len(c.Clients) == 0 && len(c.Servers) == 0 && c.Role == "" {
		return
	}
	for id, p := range c.Clients {
		c.Peers[id] = &pairedPeer{Name: p.Name, OS: p.OS, Key: p.Key}
	}
	for id, p := range c.Servers {
		c.Peers[id] = &pairedPeer{Name: p.Name, OS: p.OS, Key: p.Key, Addr: p.Addr}
	}
	c.Layout = layout{Pos: map[string]cell{c.id(): {}}}
	if c.Role == "share" {
		d, ok := dirs[c.Edge]
		if !ok {
			d = dirs["right"]
		}
		for _, id := range sortedKeys(c.Clients) {
			if _, used := c.Layout.at(d); !used {
				c.Layout.Pos[id] = d
			}
		}
		c.Layout.Stamp = 1
	}
	c.Role, c.Edge, c.Clients, c.Servers = "", "", nil, nil
}

func (c *config) save() error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := filepath.Join(configDir(), "config.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(configDir(), "config.json"))
}
