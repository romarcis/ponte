# Ponte

One mouse and one keyboard for two computers. Move the pointer past the edge of
the screen and keep working on the other computer, like Synergy or Barrier, but
in a single file of about 6 MB with no installer.

The app's interface is in Italian; the labels below are quoted as they appear,
with a translation.

## Download

The latest version is in [Releases](https://github.com/romarcis/ponte/releases/latest):
`Ponte.exe` for Windows (`Ponte-arm64.exe` for Windows PCs with an ARM processor),
`ponte-linux-x64` for Linux.

Ponte updates itself: when a new version is out, the window offers it, and
Settings has a **Cerca aggiornamenti** (check for updates) button.

## How to use it

1. Start `Ponte` on both computers (on the same network). A window opens.
2. On the computer with the mouse and keyboard, choose
   **Questo computer ha mouse e tastiera** (this computer has the mouse and
   keyboard). A 6-digit code appears.
3. On the other one, choose **Questo computer verrà controllato** (this
   computer will be controlled), click the computer found on the network and
   type the code.
4. Done. Move the pointer past the edge of the screen (the right one by
   default, you can change it in the window) to switch to the other computer.
   To come back, move the pointer out the opposite side, or press
   **Scroll Lock** or **Ctrl + Alt + Esc** (handy if the keyboard has no
   Scroll Lock).

Text copied on one computer can be pasted on the other; on Windows, files
copied in Explorer too (up to 200 MB at a time). It can be turned off in
Settings.

Pairing happens once: after that the two computers reconnect on their own.
The settings (gear button at the top right) include **Avvia con il computer**
(start with the computer).

Closing the window keeps Ponte running in the notification area (the mouse
icon next to the clock): a click opens it again, a right-click offers
**Esci da Ponte** (quit). If you quit Ponte on the controlled computer while
using it, the mouse and keyboard go straight back to the other computer; if
the other computer stops answering, they come back on their own within
3 seconds.

## First start

**Windows**: as a new, unsigned program, Ponte may trigger "Windows protected
your PC": click *More info* → *Run anyway*. Windows also asks to allow network
access: choose **Private networks**.

The first time you open Ponte it asks once for administrator rights. With
them it installs the Ponte service, so that the mouse and keyboard of the
other computer also work on Windows confirmation prompts (UAC), on the lock
screen and in programs run as administrator. After that it starts as
administrator without asking again. If you say no, Ponte still works without
those, and the window shows a warning with a **Concedi i permessi** (grant
permissions) button. **Rimuovi Ponte da questo PC** (remove Ponte from this
PC) in Settings removes the service and the start with the computer.

The window uses the WebView2 component, already part of Windows 10 and 11.

**Linux**: Ponte must be able to read and simulate the mouse and keyboard. If
the permission is missing, the window says so and the **Risolvi** (fix) button
sets it up (it asks for the administrator password). Switching at the screen
edge needs an X11 session; on Wayland, switch computers with **Scroll Lock**.

## Security

The connection is encrypted (X25519 + AES-GCM): someone on the same network
cannot read what you type. Only a computer that knows the code can pair; after
5 wrong codes the code changes. Pairing then uses a random key stored on both
computers, which you can revoke with the trash icon.

The Ponte service on Windows runs as SYSTEM, like Input Director and Mouse
Without Borders, to reach the protected desktop. The tradeoff: a program
already running as the signed-in user could send it input, and so answer
"Yes" to administrator prompts.

## Building

Needs Go 1.24 or later.

```sh
./build.sh          # builds dist/Ponte.exe (Windows) and dist/ponte-linux-*
go test ./...       # end-to-end test of pairing, edge switching, keyboard
```

## Publishing a new version

From the Actions tab, run the **Build** workflow on `main` with the version
(for example `v1.3.1`), or push a tag starting with `v`:

```sh
git tag v1.3.1
git push origin v1.3.1
```

GitHub Actions runs the tests, builds the executables and creates the Release
with the files attached. Every change on `main` is tested and built anyway:
the executables are among the run's artifacts.

## How it is built

| File | What it holds |
|---|---|
| `share.go` | computer that shares: screen edge, switching, Scroll Lock |
| `recv.go` | controlled computer: reconnecting, replaying the input |
| `crypto.go`, `proto.go` | encrypted connection and messages |
| `clip.go`, `clip_windows.go`, `clip_unix.go`, `files.go` | copied text and files shared between the computers |
| `discovery.go` | network announcements (UDP 24802) to find the other computer |
| `input_windows.go` | low-level hooks and `SendInput` |
| `input_linux.go` | `/dev/input` (evdev) and `/dev/uinput`, pointer position from X11 |
| `admin_windows.go`, `service_windows.go` | start as administrator and the Ponte service for the protected desktop |
| `update.go` | self-update from GitHub Releases |
| `ui.go`, `web/index.html` | interface (local page on 127.0.0.1:24801) |
| `gui_windows.go` | Ponte's window (Windows WebView2 component, no browser) and notification area icon |
| `assets/`, `rsrc_windows_*.syso` | mouse icon, embedded in the executable |

Ports: TCP 24800 (connection), UDP 24802 (discovery), TCP 24801 local only
(window). Keys travel as physical keys: use the same keyboard layout (for
example Italian) on both computers.

## Current limits

- macOS is not supported yet.
- Copied files are shared only between Windows computers; images are not
  shared. On Linux, text needs `xclip` (X11) or `wl-clipboard` (Wayland).
- On Windows, while you control the other computer, the local pointer stays
  still in the middle of the screen.
