package main

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Copied text travels to the other computers: each one watches its own
// clipboard and sends new text to all; received text is put on the local
// clipboard. Copied files are only offered: they travel when they are pasted
// with Ctrl+V on another computer, and only to that one.

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

	// Files copied here, offered to the others.
	offer   uint32
	offered []string

	// Files copied on another computer, waiting for a paste here.
	from   string
	fromID uint32
	armed  atomic.Bool

	// onFiles is told when the files asked for arrived, or cannot come.
	onFiles func(from string, ok bool)
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
				if paths, changed := fc.Files(); changed {
					if b := c.copied(paths); b != nil {
						send(b)
					}
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
				logf("testo copiato qui (%d caratteri)", len(text))
				send(wbuf{msgClipboard}.u32(uint32(len(text))).raw(text))
			}
		}
	}()
	return c
}

// copied notes that the clipboard changed here, with paths the files now on
// it, and returns the offer for the others (nil for none).
func (c *clipSync) copied(paths []string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropOffer("") // copied here: what another computer offers is old
	had := c.offered != nil
	c.offered = nil
	if len(paths) > 0 {
		size, err := filesSize(paths)
		if err != nil {
			logf("file copiati non condivisi: più di %d MB", maxFiles>>20)
		} else {
			if c.offer++; c.offer == 0 {
				c.offer = 1
			}
			c.offered = paths
			logf("copiati %d file e cartelle (%.1f MB): partono quando si incollano su un altro computer", len(paths), float64(size)/(1<<20))
			return wbuf{msgFileOffer}.u32(c.offer)
		}
	}
	if had {
		return wbuf{msgFileOffer}.u32(0)
	}
	return nil
}

// offeredFiles returns the files of offer id, if they are still on offer.
func (c *clipSync) offeredFiles(id uint32) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == 0 || id != c.offer {
		return nil
	}
	return c.offered
}

// takeOffer returns the files another computer offers, to ask for them.
func (c *clipSync) takeOffer() (from string, id uint32, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from, id, ok = c.from, c.fromID, c.from != ""
	c.dropOffer("")
	return
}

// peerGone forgets the files a computer offered, when it disconnects: they
// cannot be fetched any more, and while they stayed on offer Ctrl+V was held
// back for nothing.
func (c *clipSync) peerGone(from string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropOffer(from)
}

// dropOffer forgets the files offered by another computer (by from, when
// not empty); c.mu held.
func (c *clipSync) dropOffer(from string) {
	if from == "" || from == c.from {
		c.from, c.fromID = "", 0
		c.armed.Store(false)
	}
}

func (c *clipSync) filesDone(from string, ok bool) {
	if c.onFiles != nil {
		c.onFiles(from, ok)
	}
}

func (c *clipSync) received(msg []byte) { c.receivedIn(&c.in, msg) }

// receivedIn handles a message from one computer; in holds the files it is
// sending.
func (c *clipSync) receivedIn(in *inbox, msg []byte) {
	if msg[0] == msgFileOffer {
		r := &rbuf{b: msg[1:]}
		id := r.u32()
		_, fc := c.cb.(fileClipboard)
		if r.err != nil || !fc || in.from == "" {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if id == 0 || !c.app.clipboardOn() {
			c.dropOffer(in.from)
			return
		}
		c.from, c.fromID = in.from, id
		c.armed.Store(true)
		return
	}
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
	// Text copied there: that is what gets pasted now.
	c.dropOffer("")
	c.last = text
	c.cb.Set(text)
}

func toLF(s string) string   { return strings.ReplaceAll(s, "\r\n", "\n") }
func toCRLF(s string) string { return strings.ReplaceAll(toLF(s), "\n", "\r\n") }
