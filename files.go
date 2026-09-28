package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Copied files travel like copied text: files and folders copied on one
// computer are sent right away, saved in a temporary folder on the other and
// put on its clipboard, ready to paste.

const (
	maxFiles  = 200 << 20 // bytes per copy; bigger copies stay local
	fileChunk = 256 << 10
)

// fileClipboard is a clipboard that can also hold files (Windows).
type fileClipboard interface {
	// Files returns the files on the clipboard; changed is false when the
	// clipboard did not change since the last call.
	Files() (paths []string, changed bool)
	SetFiles(paths []string)
}

var errTooBig = errors.New("troppo grandi")

// sendFiles sends the files and folders in paths, with what they contain.
func sendFiles(paths []string, send func([]byte)) {
	type entry struct {
		abs, rel string
		dir      bool
	}
	var list []entry
	var total int64
	for _, p := range paths {
		base := filepath.Dir(p)
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable: skipped
			}
			rel, _ := filepath.Rel(base, path)
			if d.IsDir() {
				list = append(list, entry{path, rel, true})
				return nil
			}
			if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
				if total += info.Size(); total > maxFiles {
					return errTooBig
				}
				list = append(list, entry{path, rel, false})
			}
			return nil
		})
		if err == errTooBig {
			logf("file copiati non inviati: più di %d MB", maxFiles>>20)
			notify("File non inviati all'altro computer", fmt.Sprintf("Ponte invia al massimo %d MB per volta: questi file restano solo qui.", maxFiles>>20))
			return
		}
	}
	logf("invio %d file e cartelle (%.1f MB)", len(list), float64(total)/(1<<20))
	send([]byte{msgFileStart})
	buf := make([]byte, fileChunk)
	for _, e := range list {
		send(wbuf{msgFileEntry}.u8(b2u8(e.dir)).str(filepath.ToSlash(e.rel)))
		if e.dir {
			continue
		}
		f, err := os.Open(e.abs)
		if err != nil {
			continue
		}
		for {
			n, err := f.Read(buf)
			if n > 0 {
				send(append(wbuf{msgFileData}, buf[:n]...))
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
	end := wbuf{msgFileEnd}.u16(uint16(len(paths)))
	for _, p := range paths {
		end = end.str(filepath.Base(p))
	}
	send(end)
}

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// inbox is the copy being received.
type inbox struct {
	dir   string
	f     *os.File
	ok    bool
	total int64
}

func inboxDir() string { return filepath.Join(os.TempDir(), "Ponte", "incoming") }

func (c *clipSync) receivedFiles(msg []byte) {
	in := &c.in
	r := &rbuf{b: msg[1:]}
	switch msg[0] {
	case msgFileStart:
		in.close()
		_, fc := c.cb.(fileClipboard)
		*in = inbox{dir: inboxDir(), ok: fc && c.app.clipboardOn()}
		if in.ok {
			os.RemoveAll(in.dir) // only the last copy is kept
			in.ok = os.MkdirAll(in.dir, 0o700) == nil
		}
	case msgFileEntry:
		in.close()
		dir, rel := r.u8() != 0, filepath.FromSlash(r.str())
		if !in.ok || r.err != nil {
			return
		}
		if !filepath.IsLocal(rel) { // never outside the temporary folder
			logf("file ricevuto rifiutato: %q", rel)
			in.ok = false
			return
		}
		path := filepath.Join(in.dir, rel)
		if dir {
			in.ok = os.MkdirAll(path, 0o700) == nil
			return
		}
		os.MkdirAll(filepath.Dir(path), 0o700)
		f, err := os.Create(path)
		in.f, in.ok = f, err == nil
	case msgFileData:
		if !in.ok || in.f == nil {
			return
		}
		if in.total += int64(len(r.b)); in.total > maxFiles {
			in.ok = false
			return
		}
		if _, err := in.f.Write(r.b); err != nil {
			in.ok = false
		}
	case msgFileEnd:
		in.close()
		n := int(r.u16())
		var paths []string
		for range n {
			name := r.str()
			if r.err != nil || !filepath.IsLocal(name) || strings.ContainsAny(name, `/\`) {
				in.ok = false
				break
			}
			paths = append(paths, filepath.Join(in.dir, name))
		}
		if !in.ok || len(paths) == 0 {
			return
		}
		c.cb.(fileClipboard).SetFiles(paths)
		logf("ricevuti %d file e cartelle (%.1f MB), pronti da incollare", len(paths), float64(in.total)/(1<<20))
		notify("File copiati dall'altro computer", "Sono pronti: incollali qui con Ctrl+V.")
	}
}

func (in *inbox) close() {
	if in.f != nil {
		if err := in.f.Close(); err != nil {
			in.ok = false
		}
		in.f = nil
	}
}
