package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// Handshake:
//
//	C -> S: magic | mode | clientID(16) | X25519 pub(32) | nonce(16)
//	S -> C: magic | serverID(16) | X25519 pub(32) | nonce(16)
//	C -> S: HMAC(auth, "C" | transcript)
//	S -> C: HMAC(auth, "S" | transcript)
//
// auth is derived from the 6 digit pairing code (first time) or is the
// random 32 byte key exchanged during pairing (every time after).
// Session keys come from the X25519 secret mixed with auth, so a passive
// listener on the network sees nothing, and without the code/key nobody
// can connect.

const protoMagic = "PONTE/4\n"

const maxFrame = 4 << 20

const (
	authModeCode byte = 0
	authModeKey  byte = 1
)

var errAuth = errors.New("autenticazione fallita")

type secureConn struct {
	c       net.Conn
	r       *bufio.Reader
	send    cipher.AEAD
	recv    cipher.AEAD
	sendCtr uint64
	recvCtr uint64
	wmu     sync.Mutex
}

func newAEAD(key []byte) cipher.AEAD {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

func (s *secureConn) WriteMsg(p []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], s.sendCtr)
	s.sendCtr++
	buf := make([]byte, 4, 4+len(p)+s.send.Overhead())
	buf = s.send.Seal(buf, nonce[:], p, nil)
	binary.BigEndian.PutUint32(buf, uint32(len(buf)-4))
	s.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := s.c.Write(buf)
	return err
}

func (s *secureConn) ReadMsg(timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		s.c.SetReadDeadline(time.Now().Add(timeout))
	}
	var hdr [4]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, errors.New("messaggio troppo grande")
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(s.r, ct); err != nil {
		return nil, err
	}
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], s.recvCtr)
	s.recvCtr++
	return s.recv.Open(ct[:0], nonce[:], ct, nil)
}

func (s *secureConn) Close() error { return s.c.Close() }

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func authKeyFor(mode byte, secret, transcript []byte) []byte {
	if mode == authModeCode {
		k, err := pbkdf2.Key(sha256.New, string(secret), transcript, 50000, 32)
		if err != nil {
			panic(err)
		}
		return k
	}
	return secret
}

func mac(key []byte, label string, transcript []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	m.Write(transcript)
	return m.Sum(nil)
}

func sessionKeys(shared, auth, transcript []byte) (c2s, s2c []byte) {
	ikm := append(append([]byte{}, shared...), auth...)
	var err error
	if c2s, err = hkdf.Key(sha256.New, ikm, transcript, "ponte c2s", 32); err != nil {
		panic(err)
	}
	if s2c, err = hkdf.Key(sha256.New, ikm, transcript, "ponte s2c", 32); err != nil {
		panic(err)
	}
	return
}

var errOldPeer = errors.New("l'altro computer usa una versione diversa di Ponte")

var errWrongPeer = errors.New("a questo indirizzo risponde un altro computer")

// clientHandshake authenticates to a server. It returns the server's ID.
// With expect, it stops before proving anything when another computer
// answers: its address changed, and a stored key must not look wrong.
func clientHandshake(c net.Conn, clientID []byte, mode byte, secret, expect []byte) (*secureConn, []byte, error) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.SetDeadline(time.Time{})
	r := bufio.NewReader(c)

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	hello := []byte(protoMagic)
	hello = append(hello, mode)
	hello = append(hello, clientID...)
	hello = append(hello, priv.PublicKey().Bytes()...)
	hello = append(hello, randomBytes(16)...)
	if _, err := c.Write(hello); err != nil {
		return nil, nil, err
	}
	reply := make([]byte, len(protoMagic)+16+32+16)
	if _, err := io.ReadFull(r, reply); err != nil {
		return nil, nil, err
	}
	if string(reply[:len(protoMagic)]) != protoMagic {
		return nil, nil, errOldPeer
	}
	serverID := reply[8:24]
	if len(expect) > 0 && !hmac.Equal(serverID, expect) {
		return nil, nil, errWrongPeer
	}
	spub, err := ecdh.X25519().NewPublicKey(reply[24:56])
	if err != nil {
		return nil, nil, err
	}
	shared, err := priv.ECDH(spub)
	if err != nil {
		return nil, nil, err
	}
	th := sha256.Sum256(append(append([]byte{}, hello...), reply...))
	auth := authKeyFor(mode, secret, th[:])
	if _, err := c.Write(mac(auth, "C", th[:])); err != nil {
		return nil, nil, err
	}
	proof := make([]byte, 32)
	if _, err := io.ReadFull(r, proof); err != nil {
		// Not a refusal: the connection dropped, which must never look
		// like a forgotten pairing.
		return nil, nil, err
	}
	if !hmac.Equal(proof, mac(auth, "S", th[:])) {
		return nil, nil, errAuth
	}
	c2s, s2c := sessionKeys(shared, auth, th[:])
	return &secureConn{c: c, r: r, send: newAEAD(c2s), recv: newAEAD(s2c)}, append([]byte{}, serverID...), nil
}

// serverHandshake authenticates a client. lookup returns the secret for the
// client's auth mode and ID. It returns the client's ID and mode.
func serverHandshake(c net.Conn, serverID []byte, lookup func(mode byte, clientID []byte) ([]byte, bool)) (*secureConn, []byte, byte, error) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.SetDeadline(time.Time{})
	r := bufio.NewReader(c)

	hello := make([]byte, len(protoMagic)+1+16+32+16)
	if _, err := io.ReadFull(r, hello); err != nil {
		return nil, nil, 0, err
	}
	if string(hello[:len(protoMagic)]) != protoMagic {
		return nil, nil, 0, errors.New("client sconosciuto")
	}
	mode := hello[8]
	clientID := append([]byte{}, hello[9:25]...)
	cpub, err := ecdh.X25519().NewPublicKey(hello[25:57])
	if err != nil {
		return nil, nil, 0, err
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, 0, err
	}
	reply := []byte(protoMagic)
	reply = append(reply, serverID...)
	reply = append(reply, priv.PublicKey().Bytes()...)
	reply = append(reply, randomBytes(16)...)
	if _, err := c.Write(reply); err != nil {
		return nil, nil, 0, err
	}
	shared, err := priv.ECDH(cpub)
	if err != nil {
		return nil, nil, 0, err
	}
	th := sha256.Sum256(append(append([]byte{}, hello...), reply...))
	proof := make([]byte, 32)
	if _, err := io.ReadFull(r, proof); err != nil {
		return nil, nil, 0, err
	}
	// A refusal is said out loud (a proof of zeros), so the client can
	// tell it from a dropped connection.
	secret, ok := lookup(mode, clientID)
	if !ok {
		c.Write(make([]byte, 32))
		return nil, clientID, mode, errAuth
	}
	auth := authKeyFor(mode, secret, th[:])
	if !hmac.Equal(proof, mac(auth, "C", th[:])) {
		c.Write(make([]byte, 32))
		return nil, clientID, mode, errAuth
	}
	if _, err := c.Write(mac(auth, "S", th[:])); err != nil {
		return nil, nil, 0, err
	}
	c2s, s2c := sessionKeys(shared, auth, th[:])
	return &secureConn{c: c, r: r, send: newAEAD(s2c), recv: newAEAD(c2s)}, clientID, mode, nil
}
