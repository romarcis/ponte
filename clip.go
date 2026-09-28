package main

import (
	"strings"
	"sync"
	"time"
)

// Copied text travels to the other computers: each one watches its own
// clipboard and sends new text to all; received text is put on the local
// clipboard.

const maxClipboard = 1 << 20

type clipboard interface {
	// Get returns the clipboard text; ok is false when it holds no text.
	Get() (text string, ok bool)
	Set(text string)
	Available() bool
}

var makeClipboard = newClipboard

type clipSync struct {
	app  *App
	cb   clipboard
	mu   sync.Mutex
	last string
	in   inbox // files being received
}

// startClipSync watches the clipboard until stop is closed, calling send with
// each new text.
func startClipSync(app *App, cb clipboard, send func([]byte), stop <-chan struct{}) *clipSync {
	c := &clipSync{app: app, cb: cb}
	c.last, _ = cb.Get() // what is already there stays local
	fc, _ := cb.(fileClipboard)
	if fc != nil {
		fc.Files()
	}
	go func() {
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			if !app.clipboardOn() {
				continue
			}
			if fc != nil {
				if paths, changed := fc.Files(); changed && len(paths) > 0 {
					sendFiles(paths, send)
				}
			}
			text, ok := cb.Get()
			if !ok || text == "" || len(text) > maxClipboard {
				continue
			}
			c.mu.Lock()
			changed := text != c.last
			c.last = text
			c.mu.Unlock()
			if changed {
				send(wbuf{msgClipboard}.u32(uint32(len(text))).raw(text))
			}
		}
	}()
	return c
}

func (c *clipSync) received(msg []byte) { c.receivedIn(&c.in, msg) }

// receivedIn handles a message from one computer; in holds the files it is
// sending.
func (c *clipSync) receivedIn(in *inbox, msg []byte) {
	if msg[0] != msgClipboard {
		c.receivedFiles(in, msg)
		return
	}
	r := &rbuf{b: msg[1:]}
	n := r.u32()
	if n > maxClipboard {
		return
	}
	text := string(r.take(int(n)))
	if r.err != nil || !c.app.clipboardOn() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock() // several computers may send at once
	c.last = text
	c.cb.Set(text)
}

func toLF(s string) string   { return strings.ReplaceAll(s, "\r\n", "\n") }
func toCRLF(s string) string { return strings.ReplaceAll(toLF(s), "\n", "\r\n") }
