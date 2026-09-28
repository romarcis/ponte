package main

import (
	"encoding/binary"
	"errors"
)

// Messages exchanged inside the encrypted channel. The first byte is the type.
const (
	msgHello     = 1 // C->S: w, h int32, name, os string
	msgWelcome   = 2 // S->C: name string, pairing key (empty unless pairing)
	msgMouse     = 3 // x, y int32 (absolute, in client pixels)
	msgButton    = 4 // button u8, down u8
	msgWheel     = 5 // axis u8 (0 vertical, 1 horizontal), delta int16 (120 = one notch)
	msgKey       = 6 // code u16 (Linux evdev code), state u8 (0 up, 1 down, 2 repeat)
	msgEnter     = 7 // x, y int32
	msgLeave     = 8
	msgPing      = 9
	msgClipboard = 10 // text u32 length + UTF-8 bytes, both directions
	msgBye       = 11 // the sender is closing Ponte
	msgBlocked   = 12 // C->S: Windows refuses the replayed input; reason string

	// Copied files, both directions, in this order: start, then for each
	// file or folder an entry followed by its data, then end.
	msgFileStart = 13
	msgFileEntry = 14 // dir u8, path string (relative, with /)
	msgFileData  = 15 // raw bytes of the current file
	msgFileEnd   = 16 // count u16, names string: what goes on the clipboard
)

// Mouse buttons.
const (
	btnLeft    = 1
	btnRight   = 2
	btnMiddle  = 3
	btnBack    = 4
	btnForward = 5
)

type wbuf []byte

func (b wbuf) u8(v uint8) wbuf   { return append(b, v) }
func (b wbuf) u16(v uint16) wbuf { return binary.BigEndian.AppendUint16(b, v) }
func (b wbuf) i32(v int32) wbuf  { return binary.BigEndian.AppendUint32(b, uint32(v)) }
func (b wbuf) u32(v uint32) wbuf { return binary.BigEndian.AppendUint32(b, v) }
func (b wbuf) raw(s string) wbuf { return append(b, s...) }
func (b wbuf) str(s string) wbuf {
	if len(s) > 1000 {
		s = s[:1000]
	}
	return append(b.u16(uint16(len(s))), s...)
}
func (b wbuf) bytes(p []byte) wbuf { return append(b.u16(uint16(len(p))), p...) }

var errShort = errors.New("messaggio troncato")

type rbuf struct {
	b   []byte
	err error
}

func (r *rbuf) take(n int) []byte {
	if r.err != nil || len(r.b) < n {
		r.err = errShort
		return make([]byte, n)
	}
	p := r.b[:n]
	r.b = r.b[n:]
	return p
}
func (r *rbuf) u8() uint8     { return r.take(1)[0] }
func (r *rbuf) u16() uint16   { return binary.BigEndian.Uint16(r.take(2)) }
func (r *rbuf) i32() int32    { return int32(binary.BigEndian.Uint32(r.take(4))) }
func (r *rbuf) u32() uint32   { return binary.BigEndian.Uint32(r.take(4)) }
func (r *rbuf) str() string   { return string(r.take(int(r.u16()))) }
func (r *rbuf) bytes() []byte { return append([]byte{}, r.take(int(r.u16()))...) }

func encHello(w, h int, name, os string) []byte {
	return wbuf{msgHello}.i32(int32(w)).i32(int32(h)).str(name).str(os)
}
func encWelcome(name string, key []byte) []byte {
	return wbuf{msgWelcome}.str(name).bytes(key)
}
func encMouse(x, y int) []byte { return wbuf{msgMouse}.i32(int32(x)).i32(int32(y)) }
func encEnter(x, y int) []byte { return wbuf{msgEnter}.i32(int32(x)).i32(int32(y)) }
func encButton(b uint8, down bool) []byte {
	d := uint8(0)
	if down {
		d = 1
	}
	return wbuf{msgButton}.u8(b).u8(d)
}
func encWheel(axis uint8, delta int) []byte {
	return wbuf{msgWheel}.u8(axis).u16(uint16(int16(delta)))
}
func encKey(code uint16, state uint8) []byte { return wbuf{msgKey}.u16(code).u8(state) }
