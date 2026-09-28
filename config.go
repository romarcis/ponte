package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

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
	DeviceID      []byte                   `json:"device_id"`
	Name          string                   `json:"name"`
	Role          string                   `json:"role"` // "", "share", "receive"
	Edge          string                   `json:"edge"` // where the other screen sits: left, right, top, bottom
	LastServer    string                   `json:"last_server"`
	UIToken       string                   `json:"ui_token"`
	NoClipboard   bool                     `json:"no_clipboard"`
	NoRipple      bool                     `json:"no_ripple"`
	Color         string                   `json:"color"` // accent color "#rrggbb", "" = default
	TrayHintShown bool                     `json:"tray_hint_shown"`
	Clients       map[string]*pairedClient `json:"clients"`
	Servers       map[string]*pairedServer `json:"servers"`
}

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
	if c.Edge == "" {
		c.Edge = "right"
	}
	if c.UIToken == "" {
		c.UIToken = hex.EncodeToString(randomBytes(16))
	}
	if c.Clients == nil {
		c.Clients = map[string]*pairedClient{}
	}
	if c.Servers == nil {
		c.Servers = map[string]*pairedServer{}
	}
	return c
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
