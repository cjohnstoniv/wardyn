// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package localviewer draws a Linux VM run's display in a window on the person's own computer. It is a
// small RFB 3.8 client (RFC 6143): security type None, since the stream it reads is already the runner's
// uid-checked local socket under a nonce the organisation granted; Raw and CopyRect rectangles in a
// 32-bit pixel format; key and pointer events. It never sends cut text. The window itself is X11
// (window_linux.go); on any other operating system Run returns ErrUnsupportedOS.
package localviewer

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrUnsupportedOS is Run's answer on a computer that is not Linux.
var ErrUnsupportedOS = errors.New("localviewer: a native window needs a Linux computer")

// ErrNoDisplay is Available's answer on Linux when no X display is set (DISPLAY is empty).
var ErrNoDisplay = errors.New("localviewer: no X display is set (DISPLAY is empty)")

// MaxSide bounds the framebuffer the viewer accepts, in pixels on each side.
const MaxSide = 8192

// The limits on what the server may send that the viewer reads and throws away.
const (
	maxReason  = 4 << 10 // a refusal's reason or the desktop's name
	maxCutText = 1 << 20 // ServerCutText, which the runner's filter drops anyway
)

// The RFB message types and encodings the viewer speaks.
const (
	msgSetPixelFormat  = 0
	msgSetEncodings    = 2
	msgUpdateRequest   = 3
	msgKeyEvent        = 4
	msgPointerEvent    = 5
	msgFramebufferUpd  = 0
	msgSetColourMap    = 1
	msgBell            = 2
	msgServerCutText   = 3
	encRaw             = 0
	encCopyRect        = 1
	securityNone       = 1
	bytesPerPixel      = 4
	clientVersion      = "RFB 003.008\n"
	serverVersionBytes = 12
)

// PixelFormat is the 32-bit true-colour layout the viewer asks the server for. The window picks it to
// match its display, so pixels go to the screen as they arrive.
type PixelFormat struct {
	BigEndian                       bool
	RedShift, GreenShift, BlueShift uint8
	RedMax, GreenMax, BlueMax       uint16
}

// bytes is the format's 16-byte wire form: 32 bits per pixel, depth 24, true colour.
func (pf PixelFormat) bytes() []byte {
	b := make([]byte, 16)
	b[0], b[1] = 32, 24
	if pf.BigEndian {
		b[2] = 1
	}
	b[3] = 1
	binary.BigEndian.PutUint16(b[4:], pf.RedMax)
	binary.BigEndian.PutUint16(b[6:], pf.GreenMax)
	binary.BigEndian.PutUint16(b[8:], pf.BlueMax)
	b[10], b[11], b[12] = pf.RedShift, pf.GreenShift, pf.BlueShift
	return b
}

// Rect is a rectangle of the framebuffer, in pixels.
type Rect struct{ X, Y, W, H int }

// Framebuffer is the desktop's pixels, row after row, bytesPerPixel each.
type Framebuffer struct {
	W, H int
	Pix  []byte
}

func (fb *Framebuffer) holds(x, y, w, h int) bool {
	return x >= 0 && y >= 0 && w >= 0 && h >= 0 && x+w <= fb.W && y+h <= fb.H
}

// session is one RFB connection after the handshake. Reads happen on one goroutine (serve); writes come
// from that goroutine and the window's events, so they are serialised.
type session struct {
	br  *bufio.Reader
	w   io.Writer
	wmu sync.Mutex
	fb  *Framebuffer
}

// handshake runs RFB 3.8 to ServerInit with security type None, then asks for pf and for Raw and
// CopyRect only.
func handshake(rw io.ReadWriter, pf PixelFormat) (*session, error) {
	s := &session{br: bufio.NewReaderSize(rw, 64<<10), w: rw}
	var ver [serverVersionBytes]byte
	if _, err := io.ReadFull(s.br, ver[:]); err != nil {
		return nil, fmt.Errorf("localviewer: reading the server's version: %w", err)
	}
	var major, minor int
	if n, err := fmt.Sscanf(string(ver[:]), "RFB %03d.%03d\n", &major, &minor); err != nil || n != 2 {
		return nil, errors.New("localviewer: the display did not speak RFB")
	}
	if major != 3 || minor < 8 {
		return nil, fmt.Errorf("localviewer: the display speaks RFB %d.%d, not 3.8", major, minor)
	}
	if err := s.write([]byte(clientVersion)); err != nil {
		return nil, err
	}
	n, err := s.u8()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, s.failure("the display refused the connection")
	}
	types := make([]byte, n)
	if _, err := io.ReadFull(s.br, types); err != nil {
		return nil, err
	}
	none := false
	for _, t := range types {
		none = none || t == securityNone
	}
	if !none {
		return nil, errors.New("localviewer: the display does not offer security type None")
	}
	if err := s.write([]byte{securityNone}); err != nil {
		return nil, err
	}
	result, err := s.u32()
	if err != nil {
		return nil, err
	}
	if result != 0 {
		return nil, s.failure("the display refused the security handshake")
	}
	if err := s.write([]byte{1}); err != nil { // ClientInit: shared, so a browser view stays connected
		return nil, err
	}
	var init [20]byte
	if _, err := io.ReadFull(s.br, init[:]); err != nil {
		return nil, fmt.Errorf("localviewer: reading ServerInit: %w", err)
	}
	w, h := int(binary.BigEndian.Uint16(init[0:])), int(binary.BigEndian.Uint16(init[2:]))
	if w < 1 || h < 1 || w > MaxSide || h > MaxSide {
		return nil, fmt.Errorf("localviewer: a %dx%d desktop is outside 1x1 to %dx%d", w, h, MaxSide, MaxSide)
	}
	nameLen, err := s.u32()
	if err != nil {
		return nil, err
	}
	if nameLen > maxReason {
		return nil, fmt.Errorf("localviewer: the desktop's name is %d bytes, over %d", nameLen, maxReason)
	}
	if _, err := s.br.Discard(int(nameLen)); err != nil {
		return nil, err
	}
	s.fb = &Framebuffer{W: w, H: h, Pix: make([]byte, w*h*bytesPerPixel)}
	msg := append([]byte{msgSetPixelFormat, 0, 0, 0}, pf.bytes()...)
	msg = append(msg, msgSetEncodings, 0, 0, 2, 0, 0, 0, encRaw, 0, 0, 0, encCopyRect)
	if err := s.write(msg); err != nil {
		return nil, err
	}
	return s, nil
}

// failure reads the reason string the server sends before it closes.
func (s *session) failure(what string) error {
	n, err := s.u32()
	if err != nil || n > maxReason {
		return errors.New("localviewer: " + what)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(s.br, b); err != nil {
		return errors.New("localviewer: " + what)
	}
	return fmt.Errorf("localviewer: %s: %q", what, b)
}

func (s *session) write(b []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.w.Write(b)
	return err
}

func (s *session) u8() (byte, error) { return s.br.ReadByte() }

func (s *session) u16() (int, error) {
	var b [2]byte
	_, err := io.ReadFull(s.br, b[:])
	return int(binary.BigEndian.Uint16(b[:])), err
}

func (s *session) u32() (uint32, error) {
	var b [4]byte
	_, err := io.ReadFull(s.br, b[:])
	return binary.BigEndian.Uint32(b[:]), err
}

// requestUpdate asks for r (clamped to the framebuffer); incremental asks only for what changed.
func (s *session) requestUpdate(incremental bool, r Rect) error {
	r = clamp(r, s.fb)
	if r.W == 0 || r.H == 0 {
		return nil
	}
	b := []byte{msgUpdateRequest, 0}
	if incremental {
		b[1] = 1
	}
	b = binary.BigEndian.AppendUint16(b, uint16(r.X))
	b = binary.BigEndian.AppendUint16(b, uint16(r.Y))
	b = binary.BigEndian.AppendUint16(b, uint16(r.W))
	b = binary.BigEndian.AppendUint16(b, uint16(r.H))
	return s.write(b)
}

// keyEvent sends a key press or release; RFB keys are X11 keysyms.
func (s *session) keyEvent(down bool, keysym uint32) error {
	b := []byte{msgKeyEvent, 0, 0, 0}
	if down {
		b[1] = 1
	}
	return s.write(binary.BigEndian.AppendUint32(b, keysym))
}

// pointerEvent sends the pointer's position (clamped to the framebuffer) and its button mask.
func (s *session) pointerEvent(mask byte, x, y int) error {
	x, y = max(0, min(x, s.fb.W-1)), max(0, min(y, s.fb.H-1))
	b := []byte{msgPointerEvent, mask}
	b = binary.BigEndian.AppendUint16(b, uint16(x))
	return s.write(binary.BigEndian.AppendUint16(b, uint16(y)))
}

// serve asks for the whole desktop, then reads the server's messages until the stream ends or breaks the
// protocol, calling damage for each rectangle drawn and asking for what changes next after each update.
func (s *session) serve(damage func(Rect)) error {
	full := Rect{W: s.fb.W, H: s.fb.H}
	if err := s.requestUpdate(false, full); err != nil {
		return err
	}
	for {
		updated, err := s.readMessage(damage)
		if err != nil {
			return err
		}
		if updated {
			if err := s.requestUpdate(true, full); err != nil {
				return err
			}
		}
	}
}

// readMessage reads one server message. It reports whether it was a FramebufferUpdate. Every rectangle
// must lie inside the framebuffer; an unknown message or encoding ends the session.
func (s *session) readMessage(damage func(Rect)) (bool, error) {
	t, err := s.u8()
	if err != nil {
		return false, err
	}
	switch t {
	case msgFramebufferUpd:
		if _, err := s.u8(); err != nil {
			return false, err
		}
		n, err := s.u16()
		if err != nil {
			return false, err
		}
		for range n {
			r, err := s.readRect()
			if err != nil {
				return false, err
			}
			if r.W > 0 && r.H > 0 {
				damage(r)
			}
		}
		return true, nil
	case msgSetColourMap:
		if _, err := s.br.Discard(3); err != nil {
			return false, err
		}
		n, err := s.u16()
		if err != nil {
			return false, err
		}
		_, err = s.br.Discard(6 * n)
		return false, err
	case msgBell:
		return false, nil
	case msgServerCutText:
		if _, err := s.br.Discard(3); err != nil {
			return false, err
		}
		n, err := s.u32()
		if err != nil {
			return false, err
		}
		if n > maxCutText {
			return false, fmt.Errorf("localviewer: %d bytes of cut text, over %d", n, maxCutText)
		}
		_, err = s.br.Discard(int(n))
		return false, err
	}
	return false, fmt.Errorf("localviewer: unknown server message type %d", t)
}

// readRect reads one rectangle into the framebuffer.
func (s *session) readRect() (Rect, error) {
	var h [12]byte
	if _, err := io.ReadFull(s.br, h[:]); err != nil {
		return Rect{}, err
	}
	r := Rect{
		X: int(binary.BigEndian.Uint16(h[0:])), Y: int(binary.BigEndian.Uint16(h[2:])),
		W: int(binary.BigEndian.Uint16(h[4:])), H: int(binary.BigEndian.Uint16(h[6:])),
	}
	if !s.fb.holds(r.X, r.Y, r.W, r.H) {
		return Rect{}, fmt.Errorf("localviewer: a %dx%d rectangle at %d,%d is outside the %dx%d desktop", r.W, r.H, r.X, r.Y, s.fb.W, s.fb.H)
	}
	switch enc := int32(binary.BigEndian.Uint32(h[8:])); enc {
	case encRaw:
		stride, row := s.fb.W*bytesPerPixel, r.W*bytesPerPixel
		for y := r.Y; y < r.Y+r.H; y++ {
			off := y*stride + r.X*bytesPerPixel
			if _, err := io.ReadFull(s.br, s.fb.Pix[off:off+row]); err != nil {
				return Rect{}, err
			}
		}
	case encCopyRect:
		sx, err := s.u16()
		if err != nil {
			return Rect{}, err
		}
		sy, err := s.u16()
		if err != nil {
			return Rect{}, err
		}
		if !s.fb.holds(sx, sy, r.W, r.H) {
			return Rect{}, fmt.Errorf("localviewer: a copy from %d,%d is outside the %dx%d desktop", sx, sy, s.fb.W, s.fb.H)
		}
		s.fb.copyRect(sx, sy, r)
	default:
		return Rect{}, fmt.Errorf("localviewer: unknown rectangle encoding %d", enc)
	}
	return r, nil
}

// copyRect copies the rectangle at sx,sy to r, row by row in the order that keeps an overlap intact.
func (fb *Framebuffer) copyRect(sx, sy int, r Rect) {
	stride, row := fb.W*bytesPerPixel, r.W*bytesPerPixel
	for i := range r.H {
		y := i
		if r.Y > sy {
			y = r.H - 1 - i
		}
		dst := (r.Y+y)*stride + r.X*bytesPerPixel
		src := (sy+y)*stride + sx*bytesPerPixel
		copy(fb.Pix[dst:dst+row], fb.Pix[src:src+row])
	}
}

// clamp cuts r to the framebuffer.
func clamp(r Rect, fb *Framebuffer) Rect {
	x0, y0 := max(0, r.X), max(0, r.Y)
	x1, y1 := min(fb.W, r.X+r.W), min(fb.H, r.Y+r.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}
