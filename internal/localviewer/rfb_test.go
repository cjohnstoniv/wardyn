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
	for name, in := range map[string][]byte{"over 8192": big, "empty": zero, "10x8193": serverInit(10, 8193), "10x0": serverInit(10, 0), "RFB 3.3": old, "no None": noNone, "refused": refused, "not RFB": []byte("HTTP/1.1 200 OK\r\n")} {
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
		update(1, rect(63, 0, 2, 1, encRaw), px(2)),
		update(1, rect(0, 47, 1, 2, encRaw), px(2)),                   // one row past the bottom edge                   // one pixel past the right edge
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
				if r.W <= 0 || r.H <= 0 || r.X < 0 || r.Y < 0 || r.X+r.W > 64 || r.Y+r.H > 48 {
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

// The largest desktop side is accepted, on either side.
func TestHandshakeAcceptsTheLargestSide(t *testing.T) {
	for _, wh := range [][2]int{{8192, 1}, {1, 8192}} {
		s, err := handshake(&fakeServer{Reader: bytes.NewReader(serverInit(wh[0], wh[1]))}, testFormat)
		if err != nil || s.fb.W != wh[0] || s.fb.H != wh[1] {
			t.Fatalf("%dx%d: err=%v", wh[0], wh[1], err)
		}
	}
}

// The bounds of a 64x48 desktop, with the answers written out rather than computed.
func TestFramebufferHolds(t *testing.T) {
	fb := &Framebuffer{W: 64, H: 48}
	for _, tc := range []struct {
		x, y, w, h int
		want       bool
	}{
		{0, 0, 64, 48, true},
		{63, 47, 1, 1, true},
		{62, 46, 2, 2, true},
		{63, 0, 2, 1, false},
		{0, 47, 1, 2, false},
		{0, 0, 65, 1, false},
		{0, 0, 1, 49, false},
		{64, 0, 1, 1, false},
		{0, 48, 1, 1, false},
		{-1, 0, 1, 1, false},
		{0, 0, 0, 0, true},
		{64, 48, 0, 0, true},
	} {
		if got := fb.holds(tc.x, tc.y, tc.w, tc.h); got != tc.want {
			t.Errorf("holds(%d,%d,%d,%d) = %v, want %v", tc.x, tc.y, tc.w, tc.h, got, tc.want)
		}
	}
}

// Rectangles and copy sources at the far edges of a 64x48 desktop are read; one pixel further is refused.
func TestRectanglesAtTheFarEdges(t *testing.T) {
	px := bytes.Repeat([]byte{9, 8, 7, 6}, 4)
	for _, tc := range []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"raw in the bottom-right corner", update(1, rect(62, 46, 2, 2, encRaw), px), true},
		{"raw one column past the right edge", update(1, rect(63, 46, 2, 2, encRaw), px), false},
		{"raw one row past the bottom edge", update(1, rect(62, 47, 2, 2, encRaw), px), false},
		{"copy from the bottom-right corner", update(1, rect(0, 0, 2, 2, encCopyRect), []byte{0, 62, 0, 46}), true},
		{"copy from one column past the right edge", update(1, rect(0, 0, 2, 2, encCopyRect), []byte{0, 63, 0, 46}), false},
		{"copy from one row past the bottom edge", update(1, rect(0, 0, 2, 2, encCopyRect), []byte{0, 62, 0, 47}), false},
		{"copy to the bottom-right corner", update(1, rect(62, 46, 2, 2, encCopyRect), []byte{0, 0, 0, 0}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{br: bufio.NewReader(bytes.NewReader(tc.in)), w: io.Discard, fb: &Framebuffer{W: 64, H: 48, Pix: make([]byte, 64*48*bytesPerPixel)}}
			_, err := s.readMessage(func(Rect) {})
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	// The corner's last pixel is the last 4 bytes of the framebuffer.
	s := &session{br: bufio.NewReader(bytes.NewReader(update(1, rect(62, 46, 2, 2, encRaw), px))), w: io.Discard, fb: &Framebuffer{W: 64, H: 48, Pix: make([]byte, 64*48*bytesPerPixel)}}
	if _, err := s.readMessage(func(Rect) {}); err != nil {
		t.Fatal(err)
	}
	if last := s.fb.Pix[64*48*4-4:]; !bytes.Equal(last, []byte{9, 8, 7, 6}) {
		t.Fatalf("last pixel = % x", last)
	}
}

func TestClamp(t *testing.T) {
	fb := &Framebuffer{W: 100, H: 50}
	for _, tc := range []struct{ in, want Rect }{
		{Rect{10, 10, 5, 5}, Rect{10, 10, 5, 5}},
		{Rect{95, 45, 10, 10}, Rect{95, 45, 5, 5}},
		{Rect{-5, -5, 10, 10}, Rect{0, 0, 5, 5}},
		{Rect{100, 0, 5, 5}, Rect{}},
		{Rect{0, 60, 5, 5}, Rect{}},
		{Rect{-10, 0, 5, 5}, Rect{}},
	} {
		if got := clamp(tc.in, fb); got != tc.want {
			t.Errorf("clamp(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}
