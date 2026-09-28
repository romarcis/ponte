# Ponte

One mouse and one keyboard for all your computers. Move the pointer past the
edge of the screen and keep working on the computer next to it, like Synergy,
Barrier or Mouse Without Borders, but in a single file of about 6 MB with no
installer. Every computer is equal: the one whose mouse or keyboard you touch
is the one in control.

The app's interface is in Italian; the labels below are quoted as they appear,
with a translation.

## Download

The latest version is in [Releases](https://github.com/romarcis/ponte/releases/latest):
`Ponte.exe` for Windows (`Ponte-arm64.exe` for Windows PCs with an ARM processor),
`ponte-linux-x64` for Linux.

Ponte updates itself: when a new version is out, the window offers it, and
Settings has a **Cerca aggiornamenti** (check for updates) button.

## How to use it

1. Start `Ponte` on each computer (on the same network). A window opens with
   the screen map and a 6-digit code.
2. On one computer, pick another from **Computer nella rete** (computers on
   the network) and type the code that computer shows. Pair a third computer
   with any computer of the group: the others learn about it on their own.
3. Drag the screens on the map to match your desk (or tap a screen, then tap
   its new place). The map is the same on every computer.
4. Move the pointer past a screen edge to go to the computer on that side of
   the map, and on from there to the next one. **Scroll Lock** jumps to the
   next computer; **Ctrl + Alt + Esc** brings mouse and keyboard back to the
   computer they belong to.

Whoever touches a computer's mouse or keyboard controls from there: if
someone uses the mouse of a computer being controlled, that computer takes it
back at once, and from there it can control the others.

Text copied on one computer can be pasted on the others; on Windows, files
copied in Explorer too (up to 200 MB at a time). Files travel only when you
paste them with Ctrl+V on another computer, and only to that one. It can be
turned off in Settings.

Pairing happens once: after that the computers reconnect on their own. The
trash icon next to a computer removes it from the whole group. The settings
(gear button at the top right) include **Avvia con il computer** (start with
the computer).

Closing the window keeps Ponte running in the notification area (the mouse
icon next to the clock): a click opens it again, a right-click offers
**Esci da Ponte** (quit). If you quit Ponte on a computer while it is being
controlled, the mouse and keyboard go straight back; if a computer stops
answering, they come back on their own within 3 seconds.

All the computers need Ponte 1.4 or later: older versions (with the two roles
"share" and "receive") cannot talk to it. Pairings made with an older version
are kept.

## First start

**Windows**: as a new, unsigned program, Ponte may trigger "Windows protected
your PC": click *More info* → *Run anyway*. Windows also asks to allow network
access: choose **Private networks**.

The first time you open Ponte it asks once for administrator rights. With
them it installs the Ponte service, so that the mouse and keyboard of the
other computers also work on Windows confirmation prompts (UAC), on the lock
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
| `node.go` | connections to the group, switching at screen edges, Scroll Lock, replaying the input of another computer |
| `layout.go` | the shared screen map |
| `crypto.go`, `proto.go` | encrypted connection and messages |
| `clip.go`, `clip_windows.go`, `clip_unix.go`, `files.go` | copied text and files shared between the computers |
| `discovery.go` | network announcements (UDP 24802) to find the other computers |
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
  shared. Files from another computer come with Ctrl+V only: **Incolla**
  (paste) in the right-click menu pastes what was there before. On Linux, text needs `xclip` (X11) or `wl-clipboard` (Wayland).
- On Windows, while you control another computer, the local pointer stays
  still in the middle of the screen.
