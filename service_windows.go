package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Windows shows administrator prompts (UAC) and the lock screen on a
// protected desktop, and ignores the input a normal program replays while
// they, or a program run as administrator, are in front. The Ponte service
// gets past that like Input Director and Mouse Without Borders: it runs as
// SYSTEM and starts a helper in the signed-in user's session; the helper
// follows whichever desktop receives input and replays there what Ponte
// sends it through a named pipe.
//
// The tradeoff, accepted by the user: any program running as the user could
// send input to the helper, and so answer "Yes" to administrator prompts.
//
// The service's copy of Ponte lives in Program Files, where only
// administrators can change it.

const (
	serviceName = "PonteInput"
	pipeName    = `\\.\pipe\ponte-input`
)

func serviceExe() string {
	return filepath.Join(os.Getenv("ProgramFiles"), "Ponte", "PonteService.exe")
}

// serviceCommand runs the service modes of Ponte.exe; false means a normal
// start.
func serviceCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "--install-service":
		serviceLog()
		if err := installService(); err != nil {
			logf("installazione del servizio: %v", err)
			os.Exit(1)
		}
	case "--uninstall-service":
		serviceLog()
		if err := uninstallService(); err != nil {
			logf("rimozione del servizio: %v", err)
			os.Exit(1)
		}
	case "--service":
		serviceLog()
		if err := svc.Run(serviceName, ponteService{}); err != nil {
			logf("servizio: %v", err)
		}
	case "--input-helper":
		runInputHelper()
	case "--desktop-helper":
		runDesktopHelper()
	default:
		return false
	}
	return true
}

// serviceLog writes to ProgramData\Ponte\servizio.log, shared by the
// installer, the service and the helper.
func serviceLog() {
	dir := filepath.Join(os.Getenv("ProgramData"), "Ponte")
	os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "servizio.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	if st, err := f.Stat(); err == nil && st.Size() > 1<<20 {
		f.Truncate(0)
	}
	log.SetOutput(f)
}

// ---------- install ----------

func installService() error {
	err := installOrUpdateService()
	if err != nil {
		serviceNote("installazione del servizio non riuscita: %v", err)
	}
	return err
}

// installOrUpdateService updates the service in place when it exists:
// deleting and creating it again fails while anything still holds the old
// one open, and left a Legion Go with no service after an update.
func installOrUpdateService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	dst := serviceExe()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	s, err := m.OpenService(serviceName)
	if err == nil {
		defer s.Close()
		stopService(s)
		if err := copyFileRetry(exe, dst); err != nil {
			return err
		}
		cfg, err := s.Config()
		if err != nil {
			return err
		}
		cfg.BinaryPathName = `"` + dst + `" --service`
		cfg.StartType = mgr.StartAutomatic
		if err := s.UpdateConfig(cfg); err != nil {
			return err
		}
	} else {
		if err := copyFileRetry(exe, dst); err != nil {
			return err
		}
		s, err = m.CreateService(serviceName, dst, mgr.Config{
			DisplayName: "Ponte - mouse e tastiera ovunque",
			Description: "Permette a Ponte di usare mouse e tastiera dell'altro computer anche sulle richieste di amministratore, sulla schermata di blocco e sulle app avviate come amministratore.",
			StartType:   mgr.StartAutomatic,
		}, "--service")
		if err != nil {
			return err
		}
		defer s.Close()
	}
	if err := s.Start(); err != nil {
		return err
	}
	logf("servizio installato (Ponte %s)", version)
	return nil
}

// copyFileRetry copies the service's program, waiting for the stopped
// service and its helpers to let go of the old copy.
func copyFileRetry(src, dst string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = copyFile(src, dst); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// serviceNote adds a line to the service's log from Ponte, which keeps its
// own log.
func serviceNote(format string, args ...any) {
	logf(format, args...)
	f, err := os.OpenFile(filepath.Join(os.Getenv("ProgramData"), "Ponte", "servizio.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006/01/02 15:04:05"), fmt.Sprintf(format, args...))
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil // already gone
	}
	stopService(s)
	err = s.Delete()
	s.Close()
	time.Sleep(time.Second)
	os.Remove(serviceExe())
	os.Remove(filepath.Dir(serviceExe()))
	logf("servizio rimosso")
	return err
}

func stopService(s *mgr.Service) {
	st, err := s.Control(svc.Stop)
	for i := 0; err == nil && st.State != svc.Stopped && i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		st, err = s.Query()
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func queryService() string {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "off"
	}
	defer windows.CloseServiceHandle(m)
	name, _ := windows.UTF16PtrFromString(serviceName)
	h, err := windows.OpenService(m, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return "off"
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if windows.QueryServiceStatus(h, &st) == nil && st.CurrentState == windows.SERVICE_RUNNING {
		return "on"
	}
	return "stopped"
}

// ensureService installs the service, or replaces it when its copy of
// Ponte is not this one (after an update). It needs administrator rights.
func ensureService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if queryService() == "on" && sameFile(exe, serviceExe()) {
		return nil
	}
	return installService()
}

func sameFile(a, b string) bool {
	sum := func(p string) []byte {
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return nil
		}
		return h.Sum(nil)
	}
	x, y := sum(a), sum(b)
	return x != nil && bytes.Equal(x, y)
}

// ---------- service ----------

type ponteService struct{}

func (ponteService) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	logf("servizio avviato (Ponte %s)", version)
	var helper windows.Handle
	session := ^uint32(0)
	stop := func() {
		if helper != 0 {
			windows.TerminateProcess(helper, 0)
			windows.CloseHandle(helper)
			helper = 0
		}
	}
	defer stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		if helper != 0 {
			if ev, _ := windows.WaitForSingleObject(helper, 0); ev == windows.WAIT_OBJECT_0 {
				windows.CloseHandle(helper)
				helper = 0
			}
		}
		// The helper runs in the session on the screen, which changes when
		// someone else signs in or switches user.
		cur := windows.WTSGetActiveConsoleSessionId()
		if helper != 0 && cur != session {
			stop()
		}
		if helper == 0 && cur != ^uint32(0) {
			if h, err := startHelper(cur); err != nil {
				logf("avvio dell'aiutante nella sessione %d: %v", cur, err)
			} else {
				helper, session = h, cur
				logf("aiutante avviato nella sessione %d", cur)
			}
		}
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				logf("servizio fermato")
				st <- svc.Status{State: svc.StopPending}
				return false, 0
			}
		case <-tick.C:
		}
	}
}

// startHelper starts this program with --input-helper in the given session,
// as SYSTEM: only SYSTEM can replay input on the protected desktop.
func startHelper(session uint32) (windows.Handle, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &tok); err != nil {
		return 0, err
	}
	defer tok.Close()
	var dup windows.Token
	if err := windows.DuplicateTokenEx(tok, windows.MAXIMUM_ALLOWED, nil, windows.SecurityIdentification, windows.TokenPrimary, &dup); err != nil {
		return 0, err
	}
	defer dup.Close()
	if err := windows.SetTokenInformation(dup, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), 4); err != nil {
		return 0, err
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd, _ := windows.UTF16PtrFromString(`"` + exe + `" --input-helper`)
	desk, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := windows.StartupInfo{Desktop: desk}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(dup, nil, cmd, nil, nil, false, windows.CREATE_NO_WINDOW, nil, nil, &si, &pi); err != nil {
		return 0, err
	}
	windows.CloseHandle(pi.Thread)
	return pi.Process, nil
}

// ---------- helper ----------

var inHelper bool

var (
	pOpenInputDesktop  = user32.NewProc("OpenInputDesktop")
	pSetThreadDesktop  = user32.NewProc("SetThreadDesktop")
	pCloseDesktop      = user32.NewProc("CloseDesktop")
	pGetUserObjectInfo = user32.NewProc("GetUserObjectInformationW")
)

func desktopName(d uintptr) string {
	var buf [64]uint16
	var n uint32
	pGetUserObjectInfo.Call(d, 2 /* UOI_NAME */, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&n)))
	return windows.UTF16ToString(buf[:])
}

// deskWorker replays input on one desktop. SetThreadDesktop refuses a
// thread that already has windows or hooks, which other programs can put on
// the helper's threads (they did on a Legion Go), so each desktop gets a
// fresh thread that switches before doing anything else.
type deskWorker struct {
	name string
	h    uintptr // the desktop, open while the thread is on it
	in   chan []byte
	done chan struct{}
}

// inputDesktop sends input to the worker of the desktop receiving it: the
// normal one, the administrator prompt or the lock screen.
type inputDesktop struct {
	inj  *winInjector
	w    *deskWorker
	done uintptr // desktop of the last worker, closed at the next switch
}

func (cur *inputDesktop) replay(msg []byte) {
	if d, _, _ := pOpenInputDesktop.Call(0, 0, uintptr(windows.GENERIC_ALL)); d != 0 {
		if name := desktopName(d); cur.w == nil || cur.w.name != name {
			cur.stop()
			cur.w = startDeskWorker(d, name, cur.inj)
		} else {
			pCloseDesktop.Call(d)
		}
	}
	if cur.w == nil {
		cur.w = startDeskWorker(0, "", cur.inj)
	}
	cur.w.in <- msg
}

// stop waits for the current worker to replay what it was given.
func (cur *inputDesktop) stop() {
	if cur.w != nil {
		close(cur.w.in)
		<-cur.w.done
		if cur.done != 0 {
			pCloseDesktop.Call(cur.done) // that thread has surely ended by now
		}
		cur.done, cur.w = cur.w.h, nil
	}
}

// startDeskWorker starts a worker on desktop d; with d 0 it replays wherever
// its thread is.
func startDeskWorker(d uintptr, name string, inj *winInjector) *deskWorker {
	w := &deskWorker{name: name, h: d, in: make(chan []byte, 256), done: make(chan struct{})}
	go func() {
		// The thread is never unlocked, so Go ends it with the goroutine and
		// no later goroutine inherits its desktop.
		runtime.LockOSThread()
		defer close(w.done)
		var child *deskProcess
		if d != 0 {
			// Before any other call that could give the thread a window or
			// a hook.
			if r, _, err := pSetThreadDesktop.Call(d); r == 0 {
				logf("passaggio al desktop %s: %v", name, err)
				if child, err = startDeskProcess(name); err != nil {
					logf("aiutante sul desktop %s: %v", name, err)
				} else {
					logf("desktop attivo: %s (con un aiutante apposito)", name)
				}
			} else {
				logf("desktop attivo: %s", name)
			}
		}
		for msg := range w.in {
			if child != nil && !child.send(msg) {
				child.close()
				child = nil
			}
			if child == nil {
				replayHelperMsg(inj, msg)
			}
		}
		if child != nil {
			child.close()
		}
	}()
	return w
}

// deskProcess is a copy of Ponte started right on a desktop, for when the
// helper cannot move a thread there: every thread of a process begins on
// the desktop it was started on. It replays what it reads on its input.
type deskProcess struct {
	proc windows.Handle
	in   windows.Handle
}

func startDeskProcess(name string) (*deskProcess, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	var r, w windows.Handle
	if err := windows.CreatePipe(&r, &w, sa, 64<<10); err != nil {
		return nil, err
	}
	defer windows.CloseHandle(r)
	windows.SetHandleInformation(w, windows.HANDLE_FLAG_INHERIT, 0)
	cmd, _ := windows.UTF16PtrFromString(`"` + exe + `" --desktop-helper`)
	desk, _ := windows.UTF16PtrFromString(`winsta0\` + name)
	si := windows.StartupInfo{Desktop: desk, Flags: windows.STARTF_USESTDHANDLES, StdInput: r}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, cmd, nil, nil, true, windows.CREATE_NO_WINDOW, nil, nil, &si, &pi); err != nil {
		windows.CloseHandle(w)
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	return &deskProcess{proc: pi.Process, in: w}, nil
}

func (p *deskProcess) send(msg []byte) bool {
	var n uint32
	return windows.WriteFile(p.in, append([]byte{byte(len(msg))}, msg...), &n, nil) == nil
}

// close lets the process replay what it was given and end.
func (p *deskProcess) close() {
	windows.CloseHandle(p.in)
	if ev, _ := windows.WaitForSingleObject(p.proc, 1000); ev != windows.WAIT_OBJECT_0 {
		windows.TerminateProcess(p.proc, 0)
	}
	windows.CloseHandle(p.proc)
}

// runDesktopHelper replays the messages the helper writes on its input,
// until the helper closes it.
func runDesktopHelper() {
	inHelper = true
	serviceLog()
	in, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return
	}
	serveHelper(in, &deskReplay{inj: &winInjector{}})
}

// deskReplay replays on the desktop the thread is on.
type deskReplay struct{ inj *winInjector }

func (d *deskReplay) replay(msg []byte) { replayHelperMsg(d.inj, msg) }

func runInputHelper() {
	inHelper = true
	serviceLog()
	logf("aiutante avviato (Ponte %s)", version)
	sd, err := windows.SecurityDescriptorFromString("O:SYD:(A;;GA;;;SY)(A;;GRGW;;;IU)") // SYSTEM and signed-in users
	if err != nil {
		logf("aiutante: %v", err)
		return
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	name, _ := windows.UTF16PtrFromString(pipeName)
	desk := &inputDesktop{inj: &winInjector{}}
	for {
		h, err := windows.CreateNamedPipe(name,
			windows.PIPE_ACCESS_INBOUND|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
			1, 0, 64<<10, 0, sa)
		if err != nil {
			logf("aiutante: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if err := windows.ConnectNamedPipe(h, nil); err == nil || err == windows.ERROR_PIPE_CONNECTED {
			logf("Ponte collegato all'aiutante")
			serveHelper(h, desk)
			logf("Ponte scollegato dall'aiutante")
		}
		windows.DisconnectNamedPipe(h)
		windows.CloseHandle(h)
	}
}

// serveHelper replays the input Ponte sends: each message is one byte of
// length, then a mouse, button, wheel or key message of the network
// protocol.
func serveHelper(h windows.Handle, desk interface{ replay([]byte) }) {
	var buf [256]byte
	read := func(p []byte) error {
		for len(p) > 0 {
			var n uint32
			if err := windows.ReadFile(h, p, &n, nil); err != nil {
				return err
			}
			if n == 0 {
				return io.EOF
			}
			p = p[n:]
		}
		return nil
	}
	for {
		if err := read(buf[:1]); err != nil {
			return
		}
		n := int(buf[0])
		if n == 0 {
			continue
		}
		if err := read(buf[:n]); err != nil {
			return
		}
		desk.replay(append([]byte(nil), buf[:n]...))
	}
}

// replayHelperMsg replays one message of the network protocol.
func replayHelperMsg(inj *winInjector, msg []byte) {
	r := &rbuf{b: msg[1:]}
	switch msg[0] {
	case msgMouse:
		x, y := r.i32(), r.i32()
		if r.err == nil {
			inj.MouseAbs(int(x), int(y))
		}
	case msgButton:
		b, d := r.u8(), r.u8()
		if r.err == nil {
			inj.Button(b, d != 0)
		}
	case msgWheel:
		axis, delta := r.u8(), int16(r.u16())
		if r.err == nil {
			inj.Wheel(axis, int(delta))
		}
	case msgKey:
		code, state := r.u16(), r.u8()
		if r.err == nil {
			inj.Key(code, state)
		}
	}
}

// ---------- Ponte's side ----------

var helperPipe struct {
	sync.Mutex
	h    windows.Handle
	next time.Time // next connection attempt
}

// helperConnected tells whether input goes through the service.
func helperConnected() bool {
	helperPipe.Lock()
	defer helperPipe.Unlock()
	return helperPipe.h != 0
}

// helperSend hands one input message to the service's helper; false means
// Ponte must replay it itself.
func helperSend(msg []byte) bool {
	if inHelper || len(msg) > 255 {
		return false
	}
	p := &helperPipe
	p.Lock()
	defer p.Unlock()
	if p.h == 0 {
		if time.Now().Before(p.next) {
			return false
		}
		p.next = time.Now().Add(3 * time.Second)
		h, err := openHelper()
		if err != nil {
			return false
		}
		p.h = h
		logf("uso il servizio di Ponte per mouse e tastiera")
	}
	frame := append([]byte{byte(len(msg))}, msg...)
	var n uint32
	if err := windows.WriteFile(p.h, frame, &n, nil); err != nil {
		logf("servizio di Ponte non raggiungibile: %v", err)
		windows.CloseHandle(p.h)
		p.h = 0
		return false
	}
	return true
}

// openHelper connects to the helper and checks the pipe belongs to it:
// another program could create the pipe first to read what is typed. Only
// SYSTEM or an administrator can own the pipe, while a program run by the
// user cannot. (The helper's process itself cannot be inspected unless
// Ponte runs as administrator.)
func openHelper() (windows.Handle, error) {
	name, _ := windows.UTF16PtrFromString(pipeName)
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		windows.CloseHandle(h)
		logf("servizio di Ponte: non riesco a controllare chi è in ascolto: %v", err)
		return 0, err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !(owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid)) {
		windows.CloseHandle(h)
		logf("servizio di Ponte: in ascolto c'è un programma che non è il servizio (%v)", owner)
		return 0, fmt.Errorf("programma in ascolto inatteso: %v", owner)
	}
	return h, nil
}
