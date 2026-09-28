// Ponte shares one mouse and keyboard between two computers on the same
// network. Move the pointer past the edge of the screen and it continues on
// the other computer.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

const uiPort = 24801

func itoa(n int) string { return strconv.Itoa(n) }

func logf(format string, args ...any) { log.Printf(format, args...) }

func setupLog() {
	os.MkdirAll(configDir(), 0o700)
	f, err := os.OpenFile(filepath.Join(configDir(), "ponte.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	if os.Stderr != nil {
		log.SetOutput(io.MultiWriter(f, os.Stderr))
	} else {
		log.SetOutput(f)
	}
}

func main() {
	if serviceCommand(os.Args[1:]) {
		return
	}
	background, afterUpdate := false, false
	for _, a := range os.Args[1:] {
		switch a {
		case "--background", "-b":
			background = true
		case "--after-update":
			afterUpdate = true
		case "--setup", "--remove":
			// Started as administrator by grantAdmin or removeAdmin.
			setupLog()
			if err := adminCommand(a); err != nil {
				logf("amministratore (%s): %v", a, err)
				os.Exit(1)
			}
			return
		case "--version", "-v":
			fmt.Println("Ponte", version)
			return
		}
	}
	app := newApp()
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", uiPort, app.cfg.UIToken)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", uiPort))
	// After an update the old Ponte is still closing: wait for it.
	for i := 0; err != nil && afterUpdate && i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", uiPort))
	}
	if err != nil {
		// Ponte is already running: just show its window.
		showExisting(url, app.cfg.UIToken)
		return
	}
	// Started normally: hand over to a Ponte run as administrator, which
	// waits for this port to be free.
	if startAsAdmin(app, background) {
		ln.Close()
		return
	}
	// Only now: a second Ponte started by mistake must not wipe the log of
	// the one already running.
	setupLog()
	cleanupUpdate()
	if setupAsAdmin() {
		background = false
	}
	go watchUpdates()
	if app.cfg.Role != "" {
		app.setRole(app.cfg.Role)
	}
	logf("Ponte %s avviato (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
	runShell(app, url, !background, func() { serveUI(ln, app) })
}
