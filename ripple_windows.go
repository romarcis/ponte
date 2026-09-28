package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// The circles are drawn in a transparent window that stays on top and lets
// clicks through, following the pointer until they fade.

var (
	gdi32                = syscall.NewLazyDLL("gdi32.dll")
	pCreateCompatibleDC  = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection    = gdi32.NewProc("CreateDIBSection")
	pSelectObject        = gdi32.NewProc("SelectObject")
	pDeleteObject        = gdi32.NewProc("DeleteObject")
	pDeleteDC            = gdi32.NewProc("DeleteDC")
	pGetDC               = user32.NewProc("GetDC")
	pReleaseDC           = user32.NewProc("ReleaseDC")
	pUpdateLayeredWindow = user32.NewProc("UpdateLayeredWindow")
	pPeekMessage         = user32.NewProc("PeekMessageW")

	rippleClass sync.Once
	rippleBusy  atomic.Bool
)

type bitmapInfoHeader struct {
	size          uint32
	width, height int32
	planes, bits  uint16
	compression   uint32
	sizeImage     uint32
	xPels, yPels  int32
	clrUsed       uint32
	clrImportant  uint32
}

// showRipple shows the circles around x, y (relative to the whole desktop,
// as for MouseAbs), in color (RGB).
func showRipple(x, y int, color uint32) {
	if !rippleBusy.CompareAndSwap(false, true) {
		return
	}
	vx, vy, _, _ := virtualScreen()
	go func() {
		defer rippleBusy.Store(false)
		ripple(vx+x, vy+y, color)
	}()
}

func ripple(x, y int, color uint32) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	inst, _, _ := pGetModuleHandle.Call(0)
	rippleClass.Do(func() {
		wc := wndClassEx{wndProc: pDefWindowProc.Addr(), instance: inst, className: wstr("PonteRipple")}
		wc.size = uint32(unsafe.Sizeof(wc))
		pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	})

	dpi := 96
	if pGetDpiForSystem.Find() == nil {
		if d, _, _ := pGetDpiForSystem.Call(); d != 0 {
			dpi = int(d)
		}
	}
	size := 180 * dpi / 96
	width := 4 * float64(dpi) / 96

	screen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, screen)
	mem, _, _ := pCreateCompatibleDC.Call(screen)
	defer pDeleteDC.Call(mem)
	bi := bitmapInfoHeader{width: int32(size), height: -int32(size), planes: 1, bits: 32}
	bi.size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	bmp, _, _ := pCreateDIBSection.Call(mem, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 {
		return
	}
	defer pDeleteObject.Call(bmp)
	old, _, _ := pSelectObject.Call(mem, bmp)
	defer pSelectObject.Call(mem, old)
	buf := unsafe.Slice((*uint32)(bits), size*size)

	// WS_EX_LAYERED | TRANSPARENT | TOPMOST | TOOLWINDOW | NOACTIVATE, WS_POPUP
	hwnd, _, _ := pCreateWindowEx.Call(0x80000|0x20|0x8|0x80|0x08000000, uintptr(unsafe.Pointer(wstr("PonteRipple"))), 0,
		0x80000000, 0, 0, uintptr(size), uintptr(size), 0, 0, inst, 0)
	if hwnd == 0 {
		return
	}
	defer pDestroyWindow.Call(hwnd)

	const frames = 45              // about 0.75 s
	blend := [4]byte{0, 0, 255, 1} // AC_SRC_OVER, opacity 255, AC_SRC_ALPHA
	dim, src := point{int32(size), int32(size)}, point{}
	var m winMsg
	for f := 0; f <= frames; f++ {
		if f > 0 { // follow the pointer as it moves on
			var pt point
			pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
			x, y = int(pt.x), int(pt.y)
		}
		rippleFrame(buf, size, float64(f)/frames, width, color)
		pos := point{int32(x - size/2), int32(y - size/2)}
		pUpdateLayeredWindow.Call(hwnd, screen, uintptr(unsafe.Pointer(&pos)), uintptr(unsafe.Pointer(&dim)),
			mem, uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&blend)), 2) // ULW_ALPHA
		if f == 0 {
			pShowWindow.Call(hwnd, 4) // SW_SHOWNOACTIVATE
		}
		for {
			r, _, _ := pPeekMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1) // PM_REMOVE
			if r == 0 {
				break
			}
			pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
		}
		time.Sleep(16 * time.Millisecond)
	}
}
