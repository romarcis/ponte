package main

import (
	"encoding/binary"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

type clipboardProc interface {
	Call(...uintptr) (uintptr, uintptr, error)
}

var (
	pOpenClipboard       clipboardProc = user32.NewProc("OpenClipboard")
	pCloseClipboard      clipboardProc = user32.NewProc("CloseClipboard")
	pEmptyClipboard      clipboardProc = user32.NewProc("EmptyClipboard")
	pGetClipboardData    clipboardProc = user32.NewProc("GetClipboardData")
	pSetClipboardData    clipboardProc = user32.NewProc("SetClipboardData")
	pGetClipboardSeq     clipboardProc = user32.NewProc("GetClipboardSequenceNumber")
	pIsClipboardFormatAv clipboardProc = user32.NewProc("IsClipboardFormatAvailable")
	pGlobalAlloc                       = kernel32.NewProc("GlobalAlloc")
	pGlobalFree                        = kernel32.NewProc("GlobalFree")
	pGlobalLock                        = kernel32.NewProc("GlobalLock")
	pGlobalUnlock                      = kernel32.NewProc("GlobalUnlock")
	pGlobalSize                        = kernel32.NewProc("GlobalSize")
	pDragQueryFile                     = shell32.NewProc("DragQueryFileW")
)

const (
	cfUnicodeText = 13
	cfHDrop       = 15
	gmemMoveable  = 0x0002
)

type winClipboard struct {
	mu   sync.Mutex
	seq  uintptr
	text string
	ok   bool
	fseq uintptr // clipboard sequence when files were last looked at
}

func newClipboard() clipboard { return &winClipboard{} }

func (c *winClipboard) Available() bool { return true }

var pGetOpenClipboardWindow = user32.NewProc("GetOpenClipboardWindow")

var lastBusyLog time.Time

func openClipboard() bool {
	// Windows associates the open clipboard with the calling OS thread.
	// A goroutine can migrate while reading large text or waiting for its
	// owner to render it, leaving CloseClipboard on the wrong thread.
	runtime.LockOSThread()
	for range 10 {
		if r, _, _ := pOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond) // another program is using it
	}
	if time.Since(lastBusyLog) > 5*time.Second {
		lastBusyLog = time.Now()
		who := "un programma"
		if h, _, _ := pGetOpenClipboardWindow.Call(); h != 0 {
			var class [128]uint16
			pGetClassName.Call(h, uintptr(unsafe.Pointer(&class[0])), 128)
			who = syscall.UTF16ToString(class[:])
		}
		logf("appunti occupati da %s: li leggo alla prossima occasione", who)
	}
	runtime.UnlockOSThread()
	return false
}

func closeClipboard() {
	if r, _, err := pCloseClipboard.Call(); r == 0 {
		logf("chiusura appunti: %v", err)
	}
	runtime.UnlockOSThread()
}

// Copy the native buffer while the clipboard is open, then release it
// before decoding the text. Very large copies stay local.
func readClipboardText() ([]uint16, bool) {
	if !openClipboard() {
		return nil, false
	}
	defer closeClipboard()
	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return nil, false
	}
	size, _, _ := pGlobalSize.Call(h)
	// CRLF in UTF-16 can take four bytes per byte of normalized UTF-8.
	if size > maxClipboard*4+2 {
		return nil, true
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return nil, false
	}
	defer pGlobalUnlock.Call(h)
	u := unsafe.Slice((*uint16)(unsafe.Pointer(p)), size/2)
	for i, v := range u {
		if v == 0 {
			u = u[:i]
			break
		}
	}
	return append([]uint16{}, u...), true
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
	u, read := readClipboardText()
	if !read {
		return c.text, c.ok
	}
	if u == nil {
		c.seq, c.text, c.ok = seq, "", false
		return "", false
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
	defer closeClipboard()
	pEmptyClipboard.Call()
	if r, _, _ := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		pGlobalFree.Call(h) // on success the system owns the memory
	}
}

// Files returns the files copied in Explorer (CF_HDROP).
func (c *winClipboard) Files() ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq, _, _ := pGetClipboardSeq.Call()
	if seq == c.fseq {
		return nil, false
	}
	if r, _, _ := pIsClipboardFormatAv.Call(cfHDrop); r == 0 {
		c.fseq = seq
		return nil, true
	}
	if !openClipboard() {
		return nil, false // busy: try again at the next look
	}
	defer closeClipboard()
	c.fseq = seq
	h, _, _ := pGetClipboardData.Call(cfHDrop)
	if h == 0 {
		return nil, false
	}
	n, _, _ := pDragQueryFile.Call(h, 0xFFFFFFFF, 0, 0)
	var paths []string
	for i := uintptr(0); i < n; i++ {
		l, _, _ := pDragQueryFile.Call(h, i, 0, 0)
		buf := make([]uint16, l+1)
		pDragQueryFile.Call(h, i, uintptr(unsafe.Pointer(&buf[0])), l+1)
		paths = append(paths, syscall.UTF16ToString(buf))
	}
	return paths, true
}

// SetFiles puts files on the clipboard, as Explorer does when copying them.
func (c *winClipboard) SetFiles(paths []string) {
	var u []uint16
	for _, p := range paths {
		u = append(u, utf16.Encode([]rune(p))...)
		u = append(u, 0)
	}
	u = append(u, 0)
	const hdr = 20                                                         // DROPFILES
	h, _, _ := pGlobalAlloc.Call(gmemMoveable|0x40, uintptr(hdr+len(u)*2)) // GMEM_ZEROINIT
	if h == 0 {
		return
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(p)), hdr+len(u)*2)
	binary.LittleEndian.PutUint32(b[0:], hdr) // pFiles: where the names start
	binary.LittleEndian.PutUint32(b[16:], 1)  // fWide: UTF-16 names
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[hdr+2*i:], v)
	}
	pGlobalUnlock.Call(h)
	if !openClipboard() {
		pGlobalFree.Call(h)
		return
	}
	defer closeClipboard()
	pEmptyClipboard.Call()
	if r, _, _ := pSetClipboardData.Call(cfHDrop, h); r == 0 {
		pGlobalFree.Call(h)
	}
	seq, _, _ := pGetClipboardSeq.Call()
	c.mu.Lock()
	c.fseq = seq // not sent back to where the files came from
	c.mu.Unlock()
}
