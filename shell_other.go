//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Outside Windows the window is an app-style browser window for now.

var shellURL string

func runShell(app *App, url string, show bool, serve func()) {
	shellURL = url
	if show {
		go openWindow(url)
	}
	serve()
}

func showExisting(url, token string) { openWindow(url) }

func showWindow() { openWindow(shellURL) }

func colorChanged() {}

func quitApp(app *App) {
	app.shutdown()
	os.Exit(0)
}

// openWindow shows the interface as a standalone app window when Chrome or
// Chromium is installed, otherwise in the default browser.
func openWindow(url string) {
	if runtime.GOOS != "darwin" {
		for _, b := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "brave-browser"} {
			if p, err := exec.LookPath(b); err == nil {
				cmd := exec.Command(p, "--app="+url, "--window-size=1000,740")
				if cmd.Start() == nil {
					go cmd.Wait()
					return
				}
			}
		}
	}
	cmd := exec.Command("xdg-open", url)
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("Apri questo indirizzo nel browser:", url)
		return
	}
	go cmd.Wait()
}
