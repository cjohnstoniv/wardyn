// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package rfbauthtest is a fake of the Mac's Screen Sharing for tests: RFB 003.889 offering Apple
// authentication (security type 30) and VncAuth. It is written from the server's side, so it
// checks a client rather than mirroring it.
package rfbauthtest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math/big"
	"math/bits"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

// Group2 is the 1024-bit MODP prime of RFC 2409's second Oakley group, used with generator 2.
var Group2 = func() *big.Int {
	p, _ := new(big.Int).SetString(strings.Join([]string{
		"FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD1",
		"29024E088A67CC74020BBEA63B139B22514A08798E3404DD",
		"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245",
		"E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED",
		"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381",
		"FFFFFFFFFFFFFFFF",
	}, ""), 16)
	return p
}()

// Server answers each connection Dial makes. Set the fields before the first Dial.
type Server struct {
	// Offered is the security types the server offers, in order. Nil offers 30 and 2.
	Offered []byte
	// User and Password are the account Apple authentication accepts. VNCPassword is VncAuth's.
	User, Password, VNCPassword string
	// Prime is the Diffie-Hellman group's prime, with generator 2. Nil is Group2.
	Prime *big.Int
	// Private is the server's Diffie-Hellman private key. Nil picks a fresh one per sign-in.
	Private *big.Int
	// Session runs on a signed-in connection after ServerInit. Nil holds it, reading and discarding,
	// until the client closes it.
	Session func(net.Conn)

	conns, signIns atomic.Int64
	mu             sync.Mutex
	block          []byte
}

// Dial serves the far end of a new pipe and returns the near end.
func (s *Server) Dial(context.Context) (net.Conn, error) {
	near, far := net.Pipe()
	s.conns.Add(1)
	go s.serve(far)
	return near, nil
}

// Conns is how many connections the server took.
func (s *Server) Conns() int64 { return s.conns.Load() }

// ARDSignIns is how many Apple authentication credentials blocks the server read.
func (s *Server) ARDSignIns() int64 { return s.signIns.Load() }

// Block is a copy of the last credentials block the server decrypted.
func (s *Server) Block() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.block)
}

// ServerInit is what the server sends after ClientInit: a 1920 × 1080 32-bit display named "agent".
func ServerInit() []byte {
	si := []byte{
		0x07, 0x80, 0x04, 0x38, // 1920 × 1080
		32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0, // 32 bpp, depth 24, true colour
		0, 0, 0, 5,
	}
	return append(si, "agent"...)
}

func (s *Server) serve(c net.Conn) {
	switch {
	case !s.handshake(c):
	case s.Session != nil:
		s.Session(c)
		return
	default:
		_, _ = io.Copy(io.Discard, c)
	}
	_ = c.Close()
}

func (s *Server) handshake(c net.Conn) bool {
	if _, err := io.WriteString(c, "RFB 003.889\n"); err != nil {
		return false
	}
	v := make([]byte, 12)
	if _, err := io.ReadFull(c, v); err != nil || string(v) != "RFB 003.008\n" {
		return false
	}
	offered := s.Offered
	if offered == nil {
		offered = []byte{30, 2}
	}
	if _, err := c.Write(append([]byte{byte(len(offered))}, offered...)); err != nil {
		return false
	}
	choice := make([]byte, 1)
	if _, err := io.ReadFull(c, choice); err != nil || bytes.IndexByte(offered, choice[0]) < 0 {
		return false
	}
	var ok bool
	switch choice[0] {
	case 30:
		ok = s.ard(c)
	case 2:
		ok = s.vncAuth(c)
	default:
		return false
	}
	if !ok {
		_, _ = c.Write([]byte{0, 0, 0, 1, 0, 0, 0, 21})
		_, _ = io.WriteString(c, "authentication failed")
		return false
	}
	if _, err := c.Write([]byte{0, 0, 0, 0}); err != nil {
		return false
	}
	shared := make([]byte, 1)
	if _, err := io.ReadFull(c, shared); err != nil || shared[0] != 1 {
		return false
	}
	_, err := c.Write(ServerInit())
	return err == nil
}

// ard is the server half of Apple authentication: g, the key length L, p and the server's public
// key, then the client's 128-byte AES-128-ECB credentials block under MD5 of the shared key, then
// the client's public key. Both keys and the shared key are left-padded to L bytes.
func (s *Server) ard(c net.Conn) bool {
	p := s.Prime
	if p == nil {
		p = Group2
	}
	n := (p.BitLen() + 7) / 8
	y := s.Private
	if y == nil {
		var err error
		if y, err = rand.Int(rand.Reader, new(big.Int).Sub(p, big.NewInt(3))); err != nil {
			return false
		}
		y.Add(y, big.NewInt(2))
	}
	g := big.NewInt(2)
	head := make([]byte, 4, 4+2*n)
	binary.BigEndian.PutUint16(head, 2)
	binary.BigEndian.PutUint16(head[2:], uint16(n))
	head = append(head, p.FillBytes(make([]byte, n))...)
	head = append(head, new(big.Int).Exp(g, y, p).FillBytes(make([]byte, n))...)
	if _, err := c.Write(head); err != nil {
		return false
	}
	msg := make([]byte, 128+n)
	if _, err := io.ReadFull(c, msg); err != nil {
		return false
	}
	s.signIns.Add(1)
	yc := new(big.Int).SetBytes(msg[128:])
	if yc.Cmp(big.NewInt(1)) <= 0 || yc.Cmp(new(big.Int).Sub(p, big.NewInt(1))) >= 0 {
		return false
	}
	key := md5.Sum(new(big.Int).Exp(yc, y, p).FillBytes(make([]byte, n)))
	ci, err := aes.NewCipher(key[:])
	if err != nil {
		return false
	}
	block := make([]byte, 128)
	for i := 0; i < 128; i += 16 {
		ci.Decrypt(block[i:i+16], msg[i:i+16])
	}
	s.mu.Lock()
	s.block = block
	s.mu.Unlock()
	user, uok := cString(block[:64])
	pw, pok := cString(block[64:])
	return uok && pok && user == s.User && pw == s.Password
}

// cString is b up to its first NUL, and whether b has one.
func cString(b []byte) (string, bool) {
	before, _, found := bytes.Cut(b, []byte{0})
	return string(before), found
}

func (s *Server) vncAuth(c net.Conn) bool {
	challenge := make([]byte, 16)
	if _, err := rand.Read(challenge); err != nil {
		return false
	}
	if _, err := c.Write(challenge); err != nil {
		return false
	}
	resp := make([]byte, 16)
	if _, err := io.ReadFull(c, resp); err != nil {
		return false
	}
	var key [8]byte
	copy(key[:], s.VNCPassword)
	for i, b := range key {
		key[i] = bits.Reverse8(b)
	}
	ci, err := des.NewCipher(key[:])
	if err != nil {
		return false
	}
	got := make([]byte, 16)
	ci.Decrypt(got[:8], resp[:8])
	ci.Decrypt(got[8:], resp[8:])
	return bytes.Equal(got, challenge)
}
