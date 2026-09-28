package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ponte looks for a newer release on GitHub and, when asked from the window,
// replaces its own executable and restarts.

const releasesURL = "https://api.github.com/repos/romarcis/ponte/releases/latest"

var available struct {
	sync.Mutex
	version, url string // newer release and its download for this system
}

// assetName is the release file for this system, as built by build.sh.
func assetName() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return "Ponte.exe"
	case "windows/arm64":
		return "Ponte-arm64.exe"
	case "linux/amd64":
		return "ponte-linux-x64"
	case "linux/arm64":
		return "ponte-linux-arm64"
	}
	return ""
}

// newer tells whether version a (like "1.2.10") is after b.
func newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// watchUpdates checks GitHub soon after start and then every few hours.
func watchUpdates() {
	if version == "dev" || assetName() == "" {
		return
	}
	time.Sleep(10 * time.Second)
	for {
		if err := checkUpdate(); err != nil {
			logf("controllo aggiornamenti: %v", err)
		}
		time.Sleep(6 * time.Hour)
	}
}

func checkUpdate() error {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(releasesURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return err
	}
	v := strings.TrimPrefix(rel.Tag, "v")
	if !newer(v, version) {
		return nil
	}
	for _, a := range rel.Assets {
		if a.Name == assetName() {
			available.Lock()
			if available.version != v {
				logf("disponibile Ponte %s", v)
			}
			available.version, available.url = v, a.URL
			available.Unlock()
		}
	}
	return nil
}

func updateAvailable() string {
	available.Lock()
	defer available.Unlock()
	return available.version
}

// applyUpdate downloads the new executable, puts it in place of this one
// and starts it; the caller then quits.
// ponytail: trusts HTTPS from GitHub, no checksum or signature check.
func applyUpdate() error {
	available.Lock()
	v, url := available.version, available.url
	available.Unlock()
	if url == "" {
		return errors.New("nessun aggiornamento disponibile")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf("scarico Ponte %s", v)
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	f, err := os.OpenFile(exe+".new", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || n < 1<<20 {
		os.Remove(exe + ".new")
		return fmt.Errorf("download incompleto (%d byte): %v", n, err)
	}
	// Windows lets a running executable be renamed, not overwritten.
	os.Remove(exe + ".old")
	if err := os.Rename(exe, exe+".old"); err != nil {
		os.Remove(exe + ".new")
		return err
	}
	if err := os.Rename(exe+".new", exe); err != nil {
		os.Rename(exe+".old", exe)
		return err
	}
	cmd := exec.Command(exe, "--after-update")
	if err := cmd.Start(); err != nil {
		os.Rename(exe, exe+".new")
		os.Rename(exe+".old", exe)
		return err
	}
	logf("aggiornato a Ponte %s, riavvio", v)
	return nil
}

// cleanupUpdate removes the executable left behind by the last update.
func cleanupUpdate() {
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}
