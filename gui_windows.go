package main

import (
	"embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
)

// On Windows Ponte has its own window (the page is shown through the
// WebView2 component built into Windows, not in a browser) and an icon in
// the notification area. Closing the window keeps Ponte running there.

//go:embed assets/icon-16.png assets/icon-20.png assets/icon-24.png assets/icon-32.png assets/icon-40.png assets/icon-48.png assets/icon-64.png
var iconFS embed.FS

var (
	pRegisterClassEx    = user32.NewProc("RegisterClassExW")
	pCreateWindowEx     = user32.NewProc("CreateWindowExW")
	pDefWindowProc      = user32.NewProc("DefWindowProcW")
	pDestroyWindow      = user32.NewProc("DestroyWindow")
	pShowWindow         = user32.NewProc("ShowWindow")
	pSetForegroundWin   = user32.NewProc("SetForegroundWindow")
	pTranslateMessage   = user32.NewProc("TranslateMessage")
	pDispatchMessage    = user32.NewProc("DispatchMessageW")
	pPostMessage        = user32.NewProc("PostMessageW")
	pSendMessage        = user32.NewProc("SendMessageW")
	pPostQuitMessage    = user32.NewProc("PostQuitMessage")
	pLoadCursor         = user32.NewProc("LoadCursorW")
	pCreateIconFromRes  = user32.NewProc("CreateIconFromResourceEx")
	pRegisterWindowMsg  = user32.NewProc("RegisterWindowMessageW")
	pCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	pAppendMenu         = user32.NewProc("AppendMenuW")
	pSetMenuDefaultItem = user32.NewProc("SetMenuDefaultItem")
	pTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	pDestroyMenu        = user32.NewProc("DestroyMenu")
	pGetCursorPos       = user32.NewProc("GetCursorPos")
	pSetTimer           = user32.NewProc("SetTimer")
	pGetDpiForSystem    = user32.NewProc("GetDpiForSystem")
	pMessageBox         = user32.NewProc("MessageBoxW")
	pIsIconic           = user32.NewProc("IsIconic")
	shell32             = syscall.NewLazyDLL("shell32.dll")
	pShellNotifyIcon    = shell32.NewProc("Shell_NotifyIconW")
)

const (
	wmDestroy       = 0x0002
	wmMove          = 0x0003
	wmSize          = 0x0005
	wmActivate      = 0x0006
	wmClose         = 0x0010
	wmGetMinMaxInfo = 0x0024
	wmTimer         = 0x0113
	wmNull          = 0x0000
	wmApp           = 0x8000
	wmTrayIcon      = wmApp + 11
	wmShowMain      = wmApp + 12
	wmExitApp       = wmApp + 13
	wmColor         = wmApp + 14
	wmSetIcon       = 0x0080
	wmLButtonUpMsg  = 0x0202
	wmRButtonUpMsg  = 0x0205
	wmContextMenu   = 0x007B

	wsOverlappedWindow = 0x00CF0000
	cwUseDefault       = 0x80000000
	swHide             = 0
	swShow             = 5
	swRestore          = 9

	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2
	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10
	niifUser   = 0x04

	menuOpen = 1
	menuQuit = 2
)

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
	private uint32
}

type notifyIconData struct {
	size        uint32
	hwnd        uintptr
	id          uint32
	flags       uint32
	callbackMsg uint32
	icon        uintptr
	tip         [128]uint16
	state       uint32
	stateMask   uint32
	info        [256]uint16
	version     uint32
	infoTitle   [64]uint16
	infoFlags   uint32
	guid        [16]byte
	balloonIcon uintptr
}

var gui struct {
	mu        sync.Mutex
	app       *App
	url       string
	inst      uintptr
	trayHwnd  uintptr
	mainHwnd  uintptr
	web       *edge.Chromium
	nid       notifyIconData
	iconBig   uintptr
	iconSmall uintptr
	tip       string
	reAdd     uint32 // "TaskbarCreated": Explorer restarted
	dpi       int
}

func wstr(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)-1]
		u = append(u, 0)
	}
	copy(dst, u)
}

// loadIcon builds an icon from the embedded PNG closest to size pixels.
func loadIcon(size int) uintptr {
	best := 64
	for _, s := range []int{16, 20, 24, 32, 40, 48, 64} {
		if s >= size {
			best = s
			break
		}
	}
	b, _ := iconFS.ReadFile(fmt.Sprintf("assets/icon-%d.png", best))
	gui.app.mu.Lock()
	custom := gui.app.cfg.Color != ""
	gui.app.mu.Unlock()
	if custom {
		b = tintPNG(b, gui.app.rippleRGB())
	}
	h, _, _ := pCreateIconFromRes.Call(uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 1, 0x00030000, uintptr(size), uintptr(size), 0)
	return h
}

func scale(v int) int { return v * gui.dpi / 96 }

// runShell shows the window (unless starting in the background) and the
// notification area icon, and serves the page. It returns when Ponte quits.
func runShell(app *App, url string, show bool, serve func()) {
	runtime.LockOSThread()
	go serve()
	gui.app, gui.url, gui.dpi = app, url, 96
	if pGetDpiForSystem.Find() == nil {
		if d, _, _ := pGetDpiForSystem.Call(); d != 0 {
			gui.dpi = int(d)
		}
	}
	gui.inst, _, _ = pGetModuleHandle.Call(0)
	gui.iconBig = loadIcon(metric(11))   // SM_CXICON
	gui.iconSmall = loadIcon(metric(49)) // SM_CXSMICON
	cursor, _, _ := pLoadCursor.Call(0, 32512)

	for _, c := range []struct {
		name string
		proc uintptr
	}{{"PonteTray", syscall.NewCallback(trayProc)}, {"PonteMain", syscall.NewCallback(mainProc)}} {
		wc := wndClassEx{
			wndProc: c.proc, instance: gui.inst, icon: gui.iconBig, iconSm: gui.iconSmall,
			cursor: cursor, background: 5 + 1, className: wstr(c.name),
		}
		wc.size = uint32(unsafe.Sizeof(wc))
		pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	}
	gui.trayHwnd, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(wstr("PonteTray"))), uintptr(unsafe.Pointer(wstr("Ponte"))),
		0, 0, 0, 0, 0, 0, 0, gui.inst, 0)
	gui.reAdd = registerMsg("TaskbarCreated")
	addTrayIcon()
	pSetTimer.Call(gui.trayHwnd, 1, 1000, 0)
	if show {
		showMain()
	}

	var m winMsg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func registerMsg(name string) uint32 {
	r, _, _ := pRegisterWindowMsg.Call(uintptr(unsafe.Pointer(wstr(name))))
	return uint32(r)
}

func addTrayIcon() {
	gui.nid = notifyIconData{hwnd: gui.trayHwnd, id: 1, flags: nifMessage | nifIcon | nifTip, callbackMsg: wmTrayIcon, icon: gui.iconSmall}
	gui.nid.size = uint32(unsafe.Sizeof(gui.nid))
	copyUTF16(gui.nid.tip[:], "Ponte")
	pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&gui.nid)))
}

func removeTrayIcon() {
	gui.mu.Lock()
	defer gui.mu.Unlock()
	if gui.nid.hwnd != 0 {
		pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&gui.nid)))
		gui.nid.hwnd = 0
	}
}

func updateTrayTip() {
	s := gui.app.status()
	tip := "Ponte"
	switch s.State {
	case "waiting":
		tip = "Ponte · In attesa dell'altro computer"
	case "searching", "connecting":
		tip = "Ponte · Cerco l'altro computer"
	case "connected":
		tip = "Ponte · Collegato a " + s.Peer
	case "active":
		if s.Role == "share" {
			tip = "Ponte · Stai usando " + s.Peer
		} else {
			tip = "Ponte · " + s.Peer + " sta usando questo computer"
		}
	case "error":
		tip = "Ponte · " + s.Error
	}
	if tip == gui.tip || gui.nid.hwnd == 0 {
		return
	}
	gui.tip = tip
	gui.nid.flags = nifTip
	copyUTF16(gui.nid.tip[:], tip)
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&gui.nid)))
}

func trayBalloon(title, text string) {
	gui.nid.flags = nifInfo
	gui.nid.infoFlags = niifUser
	copyUTF16(gui.nid.infoTitle[:], title)
	copyUTF16(gui.nid.info[:], text)
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&gui.nid)))
}

func trayMenu() {
	h, _, _ := pCreatePopupMenu.Call()
	pAppendMenu.Call(h, 0, menuOpen, uintptr(unsafe.Pointer(wstr("Apri Ponte"))))
	pAppendMenu.Call(h, 0x800, 0, 0) // separator
	pAppendMenu.Call(h, 0, menuQuit, uintptr(unsafe.Pointer(wstr("Esci da Ponte"))))
	pSetMenuDefaultItem.Call(h, menuOpen, 0)
	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWin.Call(gui.trayHwnd)
	cmd, _, _ := pTrackPopupMenu.Call(h, 0x0100|0x0080|0x0002, uintptr(pt.x), uintptr(pt.y), 0, gui.trayHwnd, 0)
	pPostMessage.Call(gui.trayHwnd, wmNull, 0, 0)
	pDestroyMenu.Call(h)
	switch cmd {
	case menuOpen:
		showMain()
	case menuQuit:
		exitApp()
	}
}

func trayProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch {
	case msg == wmTrayIcon:
		switch lp & 0xFFFF {
		case wmLButtonUpMsg:
			showMain()
		case wmRButtonUpMsg, wmContextMenu:
			trayMenu()
		}
		return 0
	case msg == wmShowMain:
		showMain()
		return 0
	case msg == wmExitApp:
		exitApp()
		return 0
	case msg == wmColor:
		reloadIcons()
		return 0
	case msg == wmTimer:
		updateTrayTip()
		return 0
	case gui.reAdd != 0 && uint32(msg) == gui.reAdd:
		addTrayIcon()
		gui.tip = ""
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wp, lp)
	return r
}

// showMain opens the window, creating it (and the page inside it) if it was
// closed. Closing destroys it, so a hidden Ponte uses little memory.
func showMain() {
	if gui.mainHwnd != 0 {
		if r, _, _ := pIsIconic.Call(gui.mainHwnd); r != 0 {
			pShowWindow.Call(gui.mainHwnd, swRestore)
		}
		pShowWindow.Call(gui.mainHwnd, swShow)
		pSetForegroundWin.Call(gui.mainHwnd)
		return
	}
	w, h := scale(1000), scale(760)
	x, y, sw, sh := metric(76), metric(77), metric(78), metric(79)
	if pw, ph := metric(0), metric(1); pw > 0 { // center on the primary screen
		x, y, sw, sh = 0, 0, pw, ph
	}
	hwnd, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(wstr("PonteMain"))), uintptr(unsafe.Pointer(wstr("Ponte"))),
		wsOverlappedWindow, uintptr(x+(sw-w)/2), uintptr(y+(sh-h)/2), uintptr(w), uintptr(h), 0, 0, gui.inst, 0)
	gui.mainHwnd = hwnd
	setWindowIcons()
	pShowWindow.Call(hwnd, swShow)
	pSetForegroundWin.Call(hwnd)

	web := edge.NewChromium()
	web.DataPath = filepath.Join(configDir(), "WebView2")
	if !web.Embed(hwnd) {
		pDestroyWindow.Call(hwnd)
		gui.mainHwnd = 0
		pMessageBox.Call(0, uintptr(unsafe.Pointer(wstr("Per mostrare la sua finestra Ponte usa il componente Microsoft WebView2, che su questo computer manca.\n\nScaricalo da https://go.microsoft.com/fwlink/p/?LinkId=2124703 e riapri Ponte."))),
			uintptr(unsafe.Pointer(wstr("Ponte"))), 0x40)
		return
	}
	if st, err := web.GetSettings(); err == nil {
		st.PutAreDefaultContextMenusEnabled(false)
		st.PutAreDevToolsEnabled(false)
		st.PutIsStatusBarEnabled(false)
		st.PutIsZoomControlEnabled(false)
	}
	gui.web = web
	web.Resize()
	web.Navigate(gui.url)
}

func closeMain() {
	if gui.web != nil {
		gui.web = nil // destroying the window releases the page too
	}
	if gui.mainHwnd != 0 {
		pDestroyWindow.Call(gui.mainHwnd)
		gui.mainHwnd = 0
	}
	gui.app.mu.Lock()
	first := !gui.app.cfg.TrayHintShown
	gui.app.cfg.TrayHintShown = true
	gui.app.cfg.save()
	gui.app.mu.Unlock()
	if first {
		trayBalloon("Ponte è ancora attivo", "Lo trovi qui, nell'area di notifica. Clicca sull'icona del mouse per riaprirlo.")
	}
}

func mainProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmSize:
		if gui.web != nil {
			gui.web.Resize()
		}
	case wmMove:
		if gui.web != nil {
			gui.web.NotifyParentWindowPositionChanged()
		}
	case wmActivate:
		if wp&0xFFFF != 0 && gui.web != nil {
			gui.web.Focus()
		}
	case wmGetMinMaxInfo:
		mmi := (*[5]point)(unsafe.Pointer(lp))
		mmi[3] = point{int32(scale(760)), int32(scale(600))}
		return 0
	case wmClose:
		closeMain()
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wp, lp)
	return r
}

func quitApp(*App) { quitShell() }

func exitApp() {
	removeTrayIcon()
	gui.app.shutdown()
	os.Exit(0)
}

// quitShell is called when Ponte is closed from its page.
func quitShell() {
	pPostMessage.Call(gui.trayHwnd, wmExitApp, 0, 0)
}

// showExisting asks the Ponte that is already running to show its window.
func showExisting(url string, token string) {
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/show", uiPort), nil)
	req.Header.Set("X-Ponte-Token", token)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

func showWindow() { pPostMessage.Call(gui.trayHwnd, wmShowMain, 0, 0) }

// colorChanged repaints the icons in the color chosen in the window.
func colorChanged() { pPostMessage.Call(gui.trayHwnd, wmColor, 0, 0) }

// ponytail: old icons are not destroyed (a few KB per color change).
func reloadIcons() {
	gui.iconBig = loadIcon(metric(11))
	gui.iconSmall = loadIcon(metric(49))
	if gui.nid.hwnd != 0 {
		gui.nid.flags = nifIcon
		gui.nid.icon = gui.iconSmall
		pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&gui.nid)))
	}
	setWindowIcons()
}

func setWindowIcons() {
	if gui.mainHwnd != 0 {
		pSendMessage.Call(gui.mainHwnd, wmSetIcon, 1, gui.iconBig)
		pSendMessage.Call(gui.mainHwnd, wmSetIcon, 0, gui.iconSmall)
	}
}
