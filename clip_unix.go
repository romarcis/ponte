//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// On Linux and macOS the clipboard is reached through the usual command-line
// tools: wl-clipboard (Wayland), xclip or xsel (X11), pbcopy (macOS).

type cmdClipboard struct {
	get, set []string
}

func newClipboard() clipboard {
	var cands [][2][]string
	switch {
	case runtime.GOOS == "darwin":
		cands = [][2][]string{{{"pbpaste"}, {"pbcopy"}}}
	case os.Getenv("WAYLAND_DISPLAY") != "":
		cands = [][2][]string{
			{{"wl-paste", "--no-newline", "--type", "text"}, {"wl-copy", "--type", "text/plain"}},
			{{"xclip", "-selection", "clipboard", "-o"}, {"xclip", "-selection", "clipboard", "-i"}},
			{{"xsel", "-b", "-o"}, {"xsel", "-b", "-i"}},
		}
	default:
		cands = [][2][]string{
			{{"xclip", "-selection", "clipboard", "-o"}, {"xclip", "-selection", "clipboard", "-i"}},
			{{"xsel", "-b", "-o"}, {"xsel", "-b", "-i"}},
		}
	}
	for _, c := range cands {
		if _, err := exec.LookPath(c[0][0]); err == nil {
			return &cmdClipboard{get: c[0], set: c[1]}
		}
	}
	logf("appunti non disponibili: installa xclip (X11) o wl-clipboard (Wayland)")
	return &cmdClipboard{}
}

func (c *cmdClipboard) Available() bool { return c.get != nil }

func (c *cmdClipboard) Get() (string, bool) {
	if c.get == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, c.get[0], c.get[1:]...).Output()
	if err != nil {
		return "", false
	}
	return toLF(string(out)), true
}

func (c *cmdClipboard) Set(text string) {
	if c.set == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.set[0], c.set[1:]...)
	cmd.Stdin = strings.NewReader(text)
	cmd.Run()
}
