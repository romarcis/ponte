package main

import (
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	pOpenClipboard       = user32.NewProc("OpenClipboard")
	pCloseClipboard      = user32.NewProc("CloseClipboard")
	pEmptyClipboard      = user32.NewProc("EmptyClipboard")
	pGetClipboardData    = user32.NewProc("GetClipboardData")
	pSetClipboardData    = user32.NewProc("SetClipboardData")
	pGetClipboardSeq     = user32.NewProc("GetClipboardSequenceNumber")
	pIsClipboardFormatAv = user32.NewProc("IsClipboardFormatAvailable")
	pGlobalAlloc         = kernel32.NewProc("GlobalAlloc")
	pGlobalFree          = kernel32.NewProc("GlobalFree")
	pGlobalLock          = kernel32.NewProc("GlobalLock")
	pGlobalUnlock        = kernel32.NewProc("GlobalUnlock")
	pGlobalSize          = kernel32.NewProc("GlobalSize")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

type winClipboard struct {
	mu   sync.Mutex
	seq  uintptr
	text string
	ok   bool
}

func newClipboard() clipboard { return &winClipboard{} }

func (c *winClipboard) Available() bool { return true }

func openClipboard() bool {
	for range 10 {
		if r, _, _ := pOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond) // another program is using it
	}
	return false
}

func (c *winClipboard) Get() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq, _, _ := pGetClipboardSeq.Call()
	if seq == c.seq && seq != 0 {
		return c.text, c.ok
	}
	if r, _, _ := pIsClipboardFormatAv.Call(cfUnicodeText); r == 0 {
		c.seq, c.text, c.ok = seq, "", false
		return "", false
	}
	if !openClipboard() {
		return c.text, c.ok
	}
	defer pCloseClipboard.Call()
	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", false
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return "", false
	}
	defer pGlobalUnlock.Call(h)
	size, _, _ := pGlobalSize.Call(h)
	u := unsafe.Slice((*uint16)(unsafe.Pointer(p)), size/2)
	for i, v := range u {
		if v == 0 {
			u = u[:i]
			break
		}
	}
	c.seq, c.text, c.ok = seq, toLF(string(utf16.Decode(u))), true
	return c.text, true
}

func (c *winClipboard) Set(text string) {
	u := utf16.Encode([]rune(toCRLF(text)))
	u = append(u, 0)
	h, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(u)*2))
	if h == 0 {
		return
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(p)), len(u)), u)
	pGlobalUnlock.Call(h)
	if !openClipboard() {
		pGlobalFree.Call(h)
		return
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	if r, _, _ := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		pGlobalFree.Call(h) // on success the system owns the memory
	}
}
