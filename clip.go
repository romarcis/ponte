package main

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Copies are offered to the group without changing anyone else's clipboard.
// Text is fetched when entering another computer (or with Ctrl+V there);
// files travel only when requested by Ctrl+V.

const maxClipboard = 1 << 20

type clipboard interface {
	// Get returns the clipboard text; ok is false when it holds no text.
	Get() (text string, ok bool)
	Set(text string)
	Available() bool
}

var makeClipboard = newClipboard

type clipSync struct {
	app         *App
	cb          clipboard
	mu          sync.Mutex
	pollMu      sync.Mutex // preserve announcement order across the timer and input loop
	last        string
	in          inbox // files being received
	send        func([]byte)
	revision    uint64 // a local copy invalidates an outstanding paste
	textID      uint32
	offeredText string

	// Files copied here, offered to the others.
	offer   uint32
	offered []string

	// Files copied on another computer, waiting for a paste here.
	from     string
	fromID   uint32
	fromText bool
	armed    atomic.Bool

	// onFiles is told when the files asked for arrived, or cannot come.
	onFiles func(from string, ok bool)
}

// startClipSync watches the clipboard until stop is closed, calling send with
// clipboard offer.
func startClipSync(app *App, cb clipboard, send func([]byte), stop <-chan struct{}) *clipSync {
	c := &clipSync{app: app, cb: cb, send: send}
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
			c.poll()
		}
	}()
	return c
}

// poll also runs immediately before a paste/request, so a new local copy
// wins even if Ctrl+V arrives before the next 400ms clipboard check.
func (c *clipSync) poll() {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()
	if !c.app.clipboardOn() {
		return
	}
	var messages [][]byte
	c.mu.Lock()
	var paths []string
	var fileChanged bool
	if fc, ok := c.cb.(fileClipboard); ok {
		paths, fileChanged = fc.Files()
	}
	text, ok := c.cb.Get()
	if !ok {
		text = ""
	}
	textChanged := text != c.last || (fileChanged && len(paths) == 0 && text != "")
	if fileChanged || textChanged {
		c.revision++
		c.dropOffer("")
	}
	if fileChanged {
		if b := c.copied(paths); b != nil {
			messages = append(messages, b)
		}
	}
	if textChanged || (fileChanged && len(paths) > 0) {
		hadText := c.offeredText != ""
		c.offeredText = ""
		c.last = text
		if textChanged && ok && text != "" && len(text) <= maxClipboard && len(paths) == 0 {
			c.textID++
			if c.textID == 0 {
				c.textID = 1
			}
			c.offeredText = text
			logf("testo copiato qui (%d byte): parte quando si incolla su un altro computer", len(text))
			messages = append(messages, wbuf{msgTextOffer}.u32(c.textID))
		} else if hadText {
			messages = append(messages, wbuf{msgTextOffer}.u32(0))
		}
	}
	c.mu.Unlock()
	for _, b := range messages {
		if c.send != nil {
			c.send(b)
		}
	}
}

// copied notes that the clipboard changed here, with paths the files now on
// it, and returns the offer for the others (nil for none).
func (c *clipSync) copied(paths []string) []byte {
	// c.mu held by poll.
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

func (c *clipSync) textOfOffer(id uint32) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offeredText, id != 0 && id == c.textID && c.offeredText != ""
}

func (c *clipSync) announcements() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out [][]byte
	if c.offeredText != "" {
		out = append(out, wbuf{msgTextOffer}.u32(c.textID))
	}
	if len(c.offered) > 0 {
		out = append(out, wbuf{msgFileOffer}.u32(c.offer))
	}
	return out
}

// applyText runs only for the response to a pending paste. poll has already
// checked for new local copies; their revision must still match the request.
func (c *clipSync) applyText(text string, offer clipboardOffer) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revision != offer.revision || c.from != offer.from || c.fromID != offer.id || !c.fromText || !c.app.clipboardOn() {
		return false
	}
	c.cb.Set(text)
	c.last = text
	c.offeredText = ""
	c.offered = nil
	c.dropOffer("")
	if fc, ok := c.cb.(fileClipboard); ok {
		fc.Files()
	} // don't offer the received copy back
	return true
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
type clipboardOffer struct {
	from     string
	id       uint32
	text     bool
	revision uint64
}

func (c *clipSync) takeOffer(textOnly bool) (clipboardOffer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	offer := clipboardOffer{c.from, c.fromID, c.fromText, c.revision}
	ok := c.from != "" && (!textOnly || c.fromText)
	if ok && !offer.text {
		c.dropOffer("")
	}
	return offer, ok
}

// peerGone forgets the files a computer offered, when it disconnects: they
// cannot be fetched any more, and while they stayed on offer Ctrl+V was held
// back for nothing.
func (c *clipSync) peerGone(from string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropOffer(from)
}

func (c *clipSync) unavailable(from string, id uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.from == from && c.fromID == id && c.fromText {
		c.dropOffer(from)
	}
}

func (c *clipSync) resetOffers() {
	c.mu.Lock()
	c.dropOffer("")
	c.offered, c.offeredText = nil, ""
	c.revision++
	c.mu.Unlock()
	if c.send != nil {
		c.send(wbuf{msgTextOffer}.u32(0))
		c.send(wbuf{msgFileOffer}.u32(0))
	}
}

// dropOffer forgets the files offered by another computer (by from, when
// not empty); c.mu held.
func (c *clipSync) dropOffer(from string) {
	if from == "" || from == c.from {
		c.from, c.fromID, c.fromText = "", 0, false
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
	if msg[0] == msgFileOffer || msg[0] == msgTextOffer {
		r := &rbuf{b: msg[1:]}
		id := r.u32()
		_, fc := c.cb.(fileClipboard)
		text := msg[0] == msgTextOffer
		if r.err != nil || (!text && !fc) || in.from == "" {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if id == 0 || !c.app.clipboardOn() {
			if c.fromText == text {
				c.dropOffer(in.from)
			}
			return
		}
		c.from, c.fromID = in.from, id
		c.fromText = text
		c.armed.Store(true)
		return
	}
	c.receivedFiles(in, msg)
}

func toLF(s string) string   { return strings.ReplaceAll(s, "\r\n", "\n") }
func toCRLF(s string) string { return strings.ReplaceAll(toLF(s), "\n", "\r\n") }
