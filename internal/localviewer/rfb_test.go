// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package localviewer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

var testFormat = PixelFormat{RedShift: 16, GreenShift: 8, RedMax: 255, GreenMax: 255, BlueMax: 255}

// serverInit is a server's side of the handshake, up to and including ServerInit for a w x h desktop.
func serverInit(w, h int) []byte {
	b := []byte("RFB 003.008\n")
	b = append(b, 1, securityNone)
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(w))
	b = binary.BigEndian.AppendUint16(b, uint16(h))
	b = append(b, make([]byte, 16)...)
	b = binary.BigEndian.AppendUint32(b, 2)
	return append(b, "vm"...)
}

// rect is one rectangle header.
func rect(x, y, w, h int, enc int32) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(x))
	b = binary.BigEndian.AppendUint16(b, uint16(y))
	b = binary.BigEndian.AppendUint16(b, uint16(w))
	b = binary.BigEndian.AppendUint16(b, uint16(h))
	return binary.BigEndian.AppendUint32(b, uint32(enc))
}

// update is a FramebufferUpdate of n rectangles, followed by body.
func update(n int, body ...[]byte) []byte {
	b := []byte{msgFramebufferUpd, 0}
	b = binary.BigEndian.AppendUint16(b, uint16(n))
	for _, p := range body {
		b = append(b, p...)
	}
	return b
}

type fakeServer struct {
	io.Reader
	sent bytes.Buffer
}

func (f *fakeServer) Write(p []byte) (int, error) { return f.sent.Write(p) }

func TestHandshakeAsksForRawAndCopyRect(t *testing.T) {
	srv := &fakeServer{Reader: bytes.NewReader(serverInit(640, 480))}
	s, err := handshake(srv, testFormat)
	if err != nil {
		t.Fatal(err)
	}
	if s.fb.W != 640 || s.fb.H != 480 || len(s.fb.Pix) != 640*480*bytesPerPixel {
		t.Fatalf("framebuffer %dx%d with %d bytes", s.fb.W, s.fb.H, len(s.fb.Pix))
	}
	want := []byte(clientVersion)
	want = append(want, securityNone, 1, msgSetPixelFormat, 0, 0, 0)
	want = append(want, testFormat.bytes()...)
	want = append(want, msgSetEncodings, 0, 0, 2, 0, 0, 0, encRaw, 0, 0, 0, encCopyRect)
	if !bytes.Equal(srv.sent.Bytes(), want) {
		t.Fatalf("client sent % x\nwant        % x", srv.sent.Bytes(), want)
	}
}

func TestHandshakeRefusals(t *testing.T) {
	big := serverInit(MaxSide+1, 10)
	zero := serverInit(0, 10)
	old := append([]byte("RFB 003.003\n"), serverInit(10, 10)[12:]...)
	noNone := append([]byte("RFB 003.008\n"), 1, 2) // VNC authentication only
	refused := append([]byte("RFB 003.008\n"), 0, 0, 0, 0, 4)
	refused = append(refused, "busy"...)
	for name, in := range map[string][]byte{"over 8192": big, "empty": zero, "RFB 3.3": old, "no None": noNone, "refused": refused, "not RFB": []byte("HTTP/1.1 200 OK\r\n")} {
		if _, err := handshake(&fakeServer{Reader: bytes.NewReader(in)}, testFormat); err == nil {
			t.Errorf("%s: handshake succeeded", name)
		}
	}
}

// The viewer never sends ClientCutText (type 6); every message it writes is one of the five it speaks.
func TestClientSendsNoCutText(t *testing.T) {
	srv := &fakeServer{Reader: bytes.NewReader(serverInit(16, 16))}
	s, err := handshake(srv, testFormat)
	if err != nil {
		t.Fatal(err)
	}
	srv.sent.Reset()
	for _, step := range []func() error{
		func() error { return s.requestUpdate(false, Rect{W: 16, H: 16}) },
		func() error { return s.keyEvent(true, 'a') },
		func() error { return s.keyEvent(false, 'a') },
		func() error { return s.pointerEvent(1, 40, -3) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	b := srv.sent.Bytes()
	for _, size := range []int{10, 8, 8, 6} {
		if b[0] != msgUpdateRequest && b[0] != msgKeyEvent && b[0] != msgPointerEvent {
			t.Fatalf("client sent message type %d", b[0])
		}
		b = b[size:]
	}
	if len(b) != 0 {
		t.Fatalf("%d bytes left over", len(b))
	}
	if x, y := binary.BigEndian.Uint16(srv.sent.Bytes()[28:]), binary.BigEndian.Uint16(srv.sent.Bytes()[30:]); x != 15 || y != 0 {
		t.Fatalf("pointer at %d,%d, want it clamped to 15,0", x, y)
	}
}

func TestRawAndCopyRect(t *testing.T) {
	pix := bytes.Repeat([]byte{1, 2, 3, 4}, 4) // a 2x2 rectangle
	in := update(2, rect(1, 1, 2, 2, encRaw), pix, rect(0, 0, 2, 2, encCopyRect), []byte{0, 1, 0, 1})
	s := &session{br: bufio.NewReader(bytes.NewReader(in)), w: io.Discard, fb: &Framebuffer{W: 4, H: 4, Pix: make([]byte, 64)}}
	var got []Rect
	updated, err := s.readMessage(func(r Rect) { got = append(got, r) })
	if err != nil || !updated || len(got) != 2 {
		t.Fatalf("updated=%v err=%v rects=%v", updated, err, got)
	}
	if px := s.fb.Pix[0:4]; !bytes.Equal(px, []byte{1, 2, 3, 4}) {
		t.Fatalf("copied pixel at 0,0 = % x", px)
	}
}

// FuzzRFBServerMessages feeds any bytes to the viewer as server messages after the handshake. It must
// never panic, every rectangle it draws lies inside the framebuffer, and the stream ends in an error: an
// unknown message, an out-of-bounds rectangle or the end of the bytes.
func FuzzRFBServerMessages(f *testing.F) {
	px := func(n int) []byte { return bytes.Repeat([]byte{9, 8, 7, 0}, n) }
	cut := append([]byte{msgServerCutText, 0, 0, 0, 0, 0, 0, 5}, "hello"...)
	for _, seed := range [][]byte{
		nil,
		{msgBell},
		update(1, rect(0, 0, 2, 2, encRaw), px(4)),
		update(1, rect(62, 46, 2, 2, encRaw), px(4)),
		update(1, rect(63, 0, 2, 1, encRaw), px(2)),                   // one pixel past the right edge
		update(1, rect(0, 0, 4, 4, encCopyRect), []byte{0, 61, 0, 0}), // a copy from past the edge
		update(1, rect(10, 10, 8, 8, encCopyRect), []byte{0, 12, 0, 12}),
		update(1, rect(0, 0, 0xffff, 0xffff, encRaw)),
		update(0xffff, rect(0, 0, 1, 1, encRaw), px(1)),
		update(1, rect(0, 0, 1, 1, 16), px(1)), // ZRLE was never asked for
		{msgSetColourMap, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0},
		cut,
		{msgServerCutText, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
		{9},
		{248, 0, 0, 0, 0},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		s := &session{br: bufio.NewReader(bytes.NewReader(in)), w: io.Discard, fb: &Framebuffer{W: 64, H: 48, Pix: make([]byte, 64*48*bytesPerPixel)}}
		for range len(in) + 1 { // every message reads at least one byte
			_, err := s.readMessage(func(r Rect) {
				if r.W <= 0 || r.H <= 0 || !s.fb.holds(r.X, r.Y, r.W, r.H) {
					t.Fatalf("drew %+v outside the 64x48 desktop", r)
				}
			})
			if err != nil {
				return
			}
		}
		t.Fatalf("%d bytes read as more than %d messages", len(in), len(in)+1)
	})
}
