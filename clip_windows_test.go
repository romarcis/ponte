package main

import (
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"
)

func TestMain(m *testing.M) {
	// Headless tests don't create the Windows tray or display notifications.
	gui.app = &App{cfg: &config{NoNotify: true}}
	os.Exit(m.Run())
}

type clipboardCall func(...uintptr) (uintptr, uintptr, error)

func (f clipboardCall) Call(args ...uintptr) (uintptr, uintptr, error) { return f(args...) }

// Replace only clipboard access; native memory operations remain real.
// These tests never read, empty, or overwrite the user's clipboard.
type clipboardFixture struct {
	t             *testing.T
	owner         uintptr
	open          bool
	opens, closes int
	reads         int
	data          uintptr
}

func mockClipboard(t *testing.T) *clipboardFixture {
	f := &clipboardFixture{t: t}
	op, cl, get, seq, format, empty, set := pOpenClipboard, pCloseClipboard, pGetClipboardData, pGetClipboardSeq, pIsClipboardFormatAv, pEmptyClipboard, pSetClipboardData
	t.Cleanup(func() {
		pOpenClipboard, pCloseClipboard, pGetClipboardData, pGetClipboardSeq, pIsClipboardFormatAv, pEmptyClipboard, pSetClipboardData = op, cl, get, seq, format, empty, set
		if f.open {
			t.Error("Ponte left the clipboard open")
		}
	})
	pOpenClipboard = clipboardCall(func(...uintptr) (uintptr, uintptr, error) {
		if f.open {
			t.Error("clipboard already open")
		}
		f.owner, _, _ = pGetCurrentThreadId.Call()
		f.open = true
		f.opens++
		return 1, 0, nil
	})
	pCloseClipboard = clipboardCall(func(...uintptr) (uintptr, uintptr, error) {
		f.checkThread()
		f.open = false
		f.closes++
		return 1, 0, nil
	})
	pGetClipboardSeq = clipboardCall(func(...uintptr) (uintptr, uintptr, error) { return 1, 0, nil })
	pIsClipboardFormatAv = clipboardCall(func(...uintptr) (uintptr, uintptr, error) { return 1, 0, nil })
	pGetClipboardData = clipboardCall(func(...uintptr) (uintptr, uintptr, error) {
		// Delayed rendering and scheduler activity must not migrate the
		// transaction to another Windows thread.
		for range 10 {
			runtime.Gosched()
			time.Sleep(time.Millisecond)
		}
		f.checkThread()
		f.reads++
		return f.data, 0, nil
	})
	pEmptyClipboard = clipboardCall(func(...uintptr) (uintptr, uintptr, error) {
		f.checkThread()
		return 1, 0, nil
	})
	pSetClipboardData = clipboardCall(func(...uintptr) (uintptr, uintptr, error) {
		f.checkThread()
		return 0, 0, syscall.ERROR_ACCESS_DENIED // caller must free the allocation and close
	})
	return f
}

func (f *clipboardFixture) checkThread() {
	f.t.Helper()
	tid, _, _ := pGetCurrentThreadId.Call()
	if !f.open || tid != f.owner {
		f.t.Errorf("clipboard transaction changed OS thread: got %d, want %d (open=%v)", tid, f.owner, f.open)
	}
}

func clipboardMemory(t *testing.T, units []uint16, size uintptr) uintptr {
	t.Helper()
	h, _, err := pGlobalAlloc.Call(gmemMoveable|0x40, size)
	if h == 0 {
		t.Fatal(err)
	}
	t.Cleanup(func() { pGlobalFree.Call(h) })
	p, _, err := pGlobalLock.Call(h)
	if p == 0 {
		t.Fatal(err)
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(p)), int(size/2)), units)
	pGlobalUnlock.Call(h)
	return h
}

func TestWindowsClipboardLargeTextReleasesClipboard(t *testing.T) {
	f := mockClipboard(t)
	text := strings.Repeat("riga è 🙂\r\n", 60000)
	u := append(utf16.Encode([]rune(text)), 0)
	f.data = clipboardMemory(t, u, uintptr(len(u)*2))
	c := &winClipboard{}
	got, ok := c.Get()
	if !ok || got != toLF(text) {
		t.Fatal("large clipboard text corrupted")
	}
	if f.open || f.opens != 1 || f.closes != 1 {
		t.Fatal("clipboard not released after large copy")
	}
	if cached, ok := c.Get(); !ok || cached != got || f.opens != 1 {
		t.Fatal("unchanged clipboard was read again")
	}
}

func TestWindowsClipboardOversizedTextStaysLocal(t *testing.T) {
	f := mockClipboard(t)
	f.data = clipboardMemory(t, nil, maxClipboard*4+4)
	c := &winClipboard{}
	if got, ok := c.Get(); ok || got != "" {
		t.Fatal("oversized clipboard must stay local")
	}
	c.Get()
	if f.open || f.opens != 1 || f.closes != 1 {
		t.Fatal("oversized clipboard held open or repeatedly read")
	}
}

func TestWindowsClipboardErrorPathsReleaseClipboard(t *testing.T) {
	for _, method := range []string{"Get", "Files", "Set", "SetFiles"} {
		t.Run(method, func(t *testing.T) {
			f := mockClipboard(t)
			c := &winClipboard{}
			switch method {
			case "Get":
				c.Get() // GetClipboardData fails
			case "Files":
				c.Files()
			case "Set":
				c.Set("test\nè 🙂") // SetClipboardData fails
			case "SetFiles":
				c.SetFiles([]string{`C:\test.txt`})
			}
			if f.open || f.opens != 1 || f.closes != 1 {
				t.Fatal("clipboard not released on error")
			}
		})
	}
}

func TestWindowsClipboardFailedReadRetries(t *testing.T) {
	f := mockClipboard(t)
	c := &winClipboard{}
	c.Get()
	u := []uint16{'o', 'k', 0}
	f.data = clipboardMemory(t, u, uintptr(len(u)*2))
	if got, ok := c.Get(); !ok || got != "ok" {
		t.Fatal("failed clipboard read was cached")
	}
}

func TestWindowsClipboardBusyDoesNotCloseOthersClipboard(t *testing.T) {
	f := mockClipboard(t)
	pOpenClipboard = clipboardCall(func(...uintptr) (uintptr, uintptr, error) {
		return 0, 0, syscall.ERROR_ACCESS_DENIED
	})
	if openClipboard() {
		closeClipboard()
		t.Fatal("busy clipboard opened")
	}
	if f.closes != 0 {
		t.Fatal("attempted to close another application's clipboard")
	}
}
