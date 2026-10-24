// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"crypto/des"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// A fake of Apple's Screen Sharing: RFB 3.889, VncAuth among other security types, a 1920 × 1080
// 32-bit display.

const shimPassword = "Abcd2345"

func appleServerInit() []byte {
	b := binary.BigEndian.AppendUint16(nil, 1920)
	b = binary.BigEndian.AppendUint16(b, 1080)
	b = append(b, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0) // pixel format
	b = binary.BigEndian.AppendUint32(b, 3)
	return append(b, "mac"...)
}

// reverseBits is VncAuth's key byte order, written out rather than taken from the shim.
func reverseBits(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		r = r<<1 | b>>i&1
	}
	return r
}

type appleFake struct {
	version  string
	offered  []byte
	password string // what the fake accepts
}

func (f appleFake) serve(t *testing.T, c net.Conn) {
	t.Helper()
	read := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(c, b); err != nil {
			return nil
		}
		return b
	}
	if _, err := io.WriteString(c, f.version); err != nil {
		return
	}
	if v := read(12); string(v) != "RFB 003.008\n" {
		return
	}
	_, _ = c.Write(append([]byte{byte(len(f.offered))}, f.offered...))
	if choice := read(1); choice == nil || choice[0] != 2 {
		return
	}
	challenge := []byte("0123456789abcdef")
	_, _ = c.Write(challenge)
	resp := read(16)
	var key [8]byte
	copy(key[:], f.password)
	for i := range key {
		key[i] = reverseBits(key[i])
	}
	ci, _ := des.NewCipher(key[:])
	got := make([]byte, 16)
	if resp != nil {
		ci.Decrypt(got[:8], resp[:8])
		ci.Decrypt(got[8:], resp[8:])
	}
	if !bytes.Equal(got, challenge) {
		_, _ = c.Write([]byte{0, 0, 0, 1, 0, 0, 0, 6, 'f', 'a', 'i', 'l', 'e', 'd'})
		return
	}
	_, _ = c.Write([]byte{0, 0, 0, 0})
	if shared := read(1); shared == nil || shared[0] != 1 {
		return
	}
	_, _ = c.Write(appleServerInit())
}

type shimRig struct {
	viewer, apple net.Conn
	done          chan struct{}
}

func newShimRig(t *testing.T, cred func(context.Context) (string, error)) *shimRig {
	t.Helper()
	viewer, client := net.Pipe()
	guest, apple := net.Pipe()
	r := &shimRig{viewer: viewer, apple: apple, done: make(chan struct{})}
	deadline := time.Now().Add(10 * time.Second)
	_ = viewer.SetDeadline(deadline)
	_ = apple.SetDeadline(deadline)
	go func() {
		(&macvm.RFBShim{Credential: cred}).ServeDesktop(client, guest)
		close(r.done)
	}()
	t.Cleanup(func() {
		_ = viewer.Close()
		_ = apple.Close()
		<-r.done
	})
	return r
}

func password(context.Context) (string, error) { return shimPassword, nil }

func readFull(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return b
}

// viewerHandshake is runnerd's side: RFB 3.8, security None, then ServerInit.
func (r *shimRig) viewerHandshake(t *testing.T) {
	t.Helper()
	if v := readFull(t, r.viewer, 12); string(v) != "RFB 003.008\n" {
		t.Fatalf("the shim's version = %q", v)
	}
	_, _ = io.WriteString(r.viewer, "RFB 003.008\n")
	if sec := readFull(t, r.viewer, 2); !bytes.Equal(sec, []byte{1, 1}) {
		t.Fatalf("security types = %v, want only None", sec)
	}
	_, _ = r.viewer.Write([]byte{1})
	if res := readFull(t, r.viewer, 4); !bytes.Equal(res, []byte{0, 0, 0, 0}) {
		t.Fatalf("SecurityResult = %v", res)
	}
	_, _ = r.viewer.Write([]byte{0}) // ClientInit, not shared: the shim chose shared upstream already
	want := appleServerInit()
	if got := readFull(t, r.viewer, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("ServerInit = %v, want Apple's %v", got, want)
	}
}

func (r *shimRig) handshake(t *testing.T) {
	t.Helper()
	go appleFake{version: "RFB 003.889\n", offered: []byte{30, 2}, password: shimPassword}.serve(t, r.apple)
	r.viewerHandshake(t)
}

// ended says the shim closed c having sent it nothing.
func ended(t *testing.T, c net.Conn, what string) {
	t.Helper()
	if b, err := io.ReadAll(c); err != nil || len(b) != 0 {
		t.Fatalf("%s got %d bytes (%q), err %v; want the session closed with nothing sent", what, len(b), b, err)
	}
}

func be32(v int32) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }

// The shim logs in to Screen Sharing with the run's password and presents runnerd plain RFB 3.8 with
// security None and Apple's ServerInit. Then it carries the standard messages both ways unchanged,
// except SetEncodings, which it cuts to the encodings it frames.
func TestRFBShimLogsInAndRelaysPlainRFB(t *testing.T) {
	r := newShimRig(t, password)
	r.handshake(t)

	// SetEncodings: ZRLE, ExtendedDesktopSize, Raw, Extended Clipboard, QEMU key, Cursor, JPEG quality 6.
	enc := []byte{2, 0, 0, 7}
	for _, e := range []int32{16, -308, 0, -1063131698, -258, -239, -26} {
		enc = append(enc, be32(e)...)
	}
	_, _ = r.viewer.Write(enc)
	want := []byte{2, 0, 0, 4}
	for _, e := range []int32{16, 0, -239, -26} {
		want = append(want, be32(e)...)
	}
	if got := readFull(t, r.apple, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("SetEncodings reached Screen Sharing as %v, want %v", got, want)
	}
	for _, msg := range [][]byte{
		{0, 0, 0, 0, 16, 16, 0, 1, 0, 31, 0, 63, 0, 31, 11, 5, 0, 0, 0, 0}, // SetPixelFormat, 16 bits
		{3, 1, 0, 0, 0, 0, 7, 128, 4, 56},                                  // FramebufferUpdateRequest
		{4, 1, 0, 0, 0, 0, 0, 0x61},                                        // KeyEvent
		{5, 1, 0, 10, 0, 20},                                               // PointerEvent
		{6, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'},                                 // ClientCutText
	} {
		_, _ = r.viewer.Write(msg)
		if got := readFull(t, r.apple, len(msg)); !bytes.Equal(got, msg) {
			t.Fatalf("viewer message %d reached Screen Sharing as %v", msg[0], got)
		}
	}

	// FramebufferUpdate in the viewer's 16-bit format: Raw 2×1, CopyRect, ZRLE, Cursor 1×1,
	// DesktopSize, then LastRect ends it early; then Bell, SetColourMapEntries and ServerCutText.
	rect := func(w, h uint16, e int32) []byte {
		b := []byte{0, 0, 0, 0}
		b = binary.BigEndian.AppendUint16(b, w)
		b = binary.BigEndian.AppendUint16(b, h)
		return append(b, be32(e)...)
	}
	up := []byte{0, 0, 0, 9}
	up = append(append(up, rect(2, 1, 0)...), 1, 2, 3, 4)
	up = append(append(up, rect(4, 4, 1)...), 0, 1, 0, 2)
	up = append(append(up, rect(8, 8, 16)...), 0, 0, 0, 3, 'z', 'z', 'z')
	up = append(append(up, rect(1, 1, -239)...), 9, 9, 0x80)
	up = append(up, rect(1920, 1080, -223)...)
	up = append(up, rect(0, 0, -224)...)
	for _, msg := range [][]byte{up, {2}, {1, 0, 0, 0, 0, 1, 1, 2, 3, 4, 5, 6}, {3, 0, 0, 0, 0, 0, 0, 3, 'c', 'u', 't'}} {
		_, _ = r.apple.Write(msg)
		if got := readFull(t, r.viewer, len(msg)); !bytes.Equal(got, msg) {
			t.Fatalf("server message %d reached the viewer as %v, want %v", msg[0], got, msg)
		}
	}
}

// Nothing reaches runnerd until the shim holds the run's credential and Apple's server has accepted
// it: a run without a credential (its desktop is off), a server older than RFB 3.8, one that offers no
// VncAuth, and a refused password each close the viewer with no handshake at all.
func TestRFBShimRefusesBeforeTheViewersHandshake(t *testing.T) {
	for name, tc := range map[string]struct {
		cred  func(context.Context) (string, error)
		apple *appleFake
	}{
		"no credential":    {cred: func(context.Context) (string, error) { return "", errors.New("404 Not Found") }},
		"empty credential": {cred: func(context.Context) (string, error) { return "", nil }},
		"RFB 3.3 server":   {cred: password, apple: &appleFake{version: "RFB 003.003\n", offered: []byte{2}, password: shimPassword}},
		"no VncAuth":       {cred: password, apple: &appleFake{version: "RFB 003.889\n", offered: []byte{30, 35}, password: shimPassword}},
		"wrong password":   {cred: password, apple: &appleFake{version: "RFB 003.889\n", offered: []byte{2}, password: "Zzzz9999"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShimRig(t, tc.cred)
			if tc.apple == nil {
				ended(t, r.apple, "Screen Sharing") // the shim never spoke to it
			} else {
				go tc.apple.serve(t, r.apple)
			}
			ended(t, r.viewer, "the viewer")
		})
	}
}

// A message type the shim does not frame ends the session, from either side, and none of it crosses:
// a server message Apple's server might add, and a viewer's SetDesktopSize.
func TestRFBShimEndsTheSessionOnAMessageItDoesNotKnow(t *testing.T) {
	r := newShimRig(t, password)
	r.handshake(t)
	_, _ = r.apple.Write([]byte{0x7f, 1, 2, 3})
	ended(t, r.viewer, "the viewer")

	r = newShimRig(t, password)
	r.handshake(t)
	_, _ = r.viewer.Write([]byte{251, 0, 7, 128, 4, 56, 1, 0})
	ended(t, r.apple, "Screen Sharing")

	// A rectangle in an encoding the viewer was never let ask for ends it too, before any of its update.
	r = newShimRig(t, password)
	r.handshake(t)
	_, _ = r.apple.Write(append([]byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1}, be32(-308)...))
	ended(t, r.viewer, "the viewer")
}
