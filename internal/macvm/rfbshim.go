// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bufio"
	"context"
	"crypto/des"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The guest's desktop is Apple's Screen Sharing, a VNC server that asks for VncAuth (RFC 6143 7.2.2).
// RFBShim sits on desktop.sock between runnerd and it: it logs in to Apple's server with the run's
// password, and only then presents runnerd plain RFB 3.8 with security None, which the gateway's
// rfbfilter relays as it relays QEMU's display. Past the handshake it passes only the standard
// messages it can frame and ends the session on any other, so a message it does not know never
// crosses. Coordinates pass unchanged: RFB's origin is the top left on both sides.

// shimHandshakeWait bounds both handshakes and the credential request together.
const shimHandshakeWait = 30 * time.Second

// rfbMaxName bounds the desktop name in ServerInit, and a refusal's reason the shim reads.
const rfbMaxName = 4 << 10

// The encodings the shim lets the viewer ask for: the pixel encodings and pseudo-encodings whose
// rectangles it frames. ExtendedDesktopSize is left out, so no viewer sends SetDesktopSize (the
// display is fixed), and so is Extended Clipboard, so cut text keeps its plain form.
const (
	rfbEncRaw         = 0
	rfbEncCopyRect    = 1
	rfbEncZRLE        = 16
	rfbEncDesktopSize = -223
	rfbEncLastRect    = -224
	rfbEncCursor      = -239
)

func rfbShimKeeps(e int32) bool {
	switch e {
	case rfbEncRaw, rfbEncCopyRect, rfbEncZRLE, rfbEncDesktopSize, rfbEncLastRect, rfbEncCursor:
		return true
	}
	// The compression-level and JPEG-quality hints, which never appear as a rectangle.
	return (e >= -256 && e <= -247) || (e >= -32 && e <= -23)
}

// RFBShim is the helper's DesktopShim.
type RFBShim struct {
	// Credential returns the run's VNC password (the guest agent's GET /v1/desktop/credential). An
	// error refuses the session before either side's handshake: a run whose desktop is off has none.
	Credential func(ctx context.Context) (string, error)
	// Log gets one line per refused or ended session, never the password; nil is slog.Default().
	Log *slog.Logger
}

var _ DesktopShim = (*RFBShim)(nil)

// ServeDesktop serves one session and closes both connections.
func (s *RFBShim) ServeDesktop(client, guest net.Conn) {
	defer client.Close()
	defer guest.Close()
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	deadline := time.Now().Add(shimHandshakeWait)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if s.Credential == nil {
		log.Warn("macvm: desktop refused: the shim has no credential source")
		return
	}
	password, err := s.Credential(ctx)
	if err == nil && password == "" {
		err = errors.New("the credential is empty")
	}
	if err != nil {
		log.Warn("macvm: desktop refused: no Screen Sharing credential", "err", err.Error())
		return
	}
	_ = guest.SetDeadline(deadline)
	_ = client.SetDeadline(deadline)
	gr := bufio.NewReader(guest)
	serverInit, err := rfbLogin(gr, guest, password)
	if err != nil {
		log.Warn("macvm: desktop refused: Screen Sharing login", "err", err.Error())
		return
	}
	cr := bufio.NewReader(client)
	if err := rfbPresentNone(cr, client, serverInit); err != nil {
		log.Warn("macvm: desktop refused: viewer handshake", "err", err.Error())
		return
	}
	_ = guest.SetDeadline(time.Time{})
	_ = client.SetDeadline(time.Time{})

	r := &rfbRelay{}
	r.bpp.Store(int32(serverInit[4]))
	var once sync.Once
	var first error
	end := func(err error) {
		once.Do(func() {
			first = err
			_ = client.Close()
			_ = guest.Close()
		})
	}
	done := make(chan struct{})
	go func() { end(r.clientMsgs(cr, bufio.NewWriter(guest))); close(done) }()
	end(r.serverMsgs(gr, bufio.NewWriter(client)))
	<-done
	log.Info("macvm: desktop session ended", "err", errText(first),
		"client_cut_texts", r.clientCuts.Load(), "server_cut_texts", r.serverCuts.Load())
}

func errText(err error) string {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return ""
	}
	return err.Error()
}

// rfbLogin logs in to Apple's server as an RFB 3.8 client with VncAuth and returns its ServerInit.
func rfbLogin(r *bufio.Reader, w io.Writer, password string) ([]byte, error) {
	v := make([]byte, 12)
	if _, err := io.ReadFull(r, v); err != nil {
		return nil, fmt.Errorf("read the server's version: %w", err)
	}
	minor, err := strconv.Atoi(string(v[8:11]))
	if string(v[:8]) != "RFB 003." || v[11] != '\n' || err != nil || minor < 8 {
		return nil, fmt.Errorf("the server speaks %q, not RFB 3.8 or later", v)
	}
	if _, err := io.WriteString(w, "RFB 003.008\n"); err != nil {
		return nil, err
	}
	n, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("the server refused the connection: %s", rfbReason(r))
	}
	offered := make([]byte, n)
	if _, err := io.ReadFull(r, offered); err != nil {
		return nil, err
	}
	vncAuth := false
	for _, t := range offered {
		vncAuth = vncAuth || t == 2
	}
	if !vncAuth {
		return nil, fmt.Errorf("the server offers no VncAuth (security types %v)", offered)
	}
	challenge := make([]byte, 16)
	if _, err := w.Write([]byte{2}); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, challenge); err != nil {
		return nil, err
	}
	resp, err := vncAuthResponse(password, challenge)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(resp); err != nil {
		return nil, err
	}
	var result [4]byte
	if _, err := io.ReadFull(r, result[:]); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(result[:]) != 0 {
		return nil, fmt.Errorf("the server refused the password: %s", rfbReason(r))
	}
	if _, err := w.Write([]byte{1}); err != nil { // ClientInit, shared
		return nil, err
	}
	si := make([]byte, 24)
	if _, err := io.ReadFull(r, si); err != nil {
		return nil, fmt.Errorf("read ServerInit: %w", err)
	}
	if _, err := rfbBytesPerPixel(si[4]); err != nil {
		return nil, err
	}
	name := binary.BigEndian.Uint32(si[20:])
	if name > rfbMaxName {
		return nil, fmt.Errorf("the server's desktop name is %d bytes", name)
	}
	si = append(si, make([]byte, name)...)
	if _, err := io.ReadFull(r, si[24:]); err != nil {
		return nil, err
	}
	return si, nil
}

// rfbReason reads a refusal's reason string, best effort, for the log.
func rfbReason(r *bufio.Reader) string {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return "(no reason)"
	}
	b := make([]byte, min(binary.BigEndian.Uint32(n[:]), rfbMaxName))
	k, _ := io.ReadFull(r, b)
	return strconv.Quote(string(b[:k]))
}

// vncAuthResponse is VncAuth's answer to challenge: each half DES-encrypted under the password's
// first eight bytes, zero-padded, each byte's bits reversed (RFC 6143 7.2.2 and its errata).
func vncAuthResponse(password string, challenge []byte) ([]byte, error) {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		key[i] = bits.Reverse8(b)
	}
	c, err := des.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:])
	return out, nil
}

// rfbPresentNone is the server side of RFB 3.8 with security None toward runnerd, ending in si, the
// ServerInit Apple's server sent.
func rfbPresentNone(r *bufio.Reader, w io.Writer, si []byte) error {
	if _, err := io.WriteString(w, "RFB 003.008\n"); err != nil {
		return err
	}
	v := make([]byte, 12)
	if _, err := io.ReadFull(r, v); err != nil {
		return err
	}
	if string(v) != "RFB 003.008\n" {
		return fmt.Errorf("the viewer chose %q, not RFB 3.8", v)
	}
	if _, err := w.Write([]byte{1, 1}); err != nil { // one security type: None
		return err
	}
	t, err := r.ReadByte()
	if err != nil {
		return err
	}
	if t != 1 {
		return fmt.Errorf("the viewer chose security type %d, not None", t)
	}
	if _, err := w.Write([]byte{0, 0, 0, 0}); err != nil { // SecurityResult OK
		return err
	}
	if _, err := r.ReadByte(); err != nil { // ClientInit: the shim already chose shared upstream
		return err
	}
	_, err = w.Write(si)
	return err
}

func rfbBytesPerPixel(bitsPerPixel byte) (int64, error) {
	switch bitsPerPixel {
	case 8, 16, 32:
		return int64(bitsPerPixel / 8), nil
	}
	return 0, fmt.Errorf("pixel format of %d bits per pixel", bitsPerPixel)
}

// rfbRelay frames one session's messages after the handshake.
type rfbRelay struct {
	bpp                    atomic.Int32 // bits per pixel of the server's rectangles, as the viewer last set
	clientCuts, serverCuts atomic.Int64
}

// rfbPass reads n bytes from r into w, and the message header it already holds before them.
func rfbPass(w *bufio.Writer, r *bufio.Reader, head []byte, n int) ([]byte, error) {
	b := append(head, make([]byte, n)...)
	if _, err := io.ReadFull(r, b[len(head):]); err != nil {
		return nil, err
	}
	_, err := w.Write(b)
	return b, err
}

// rfbCutLength is a cut text's length; the high bit is Extended Clipboard, which is never negotiated.
func rfbCutLength(h []byte) (int64, error) {
	n := binary.BigEndian.Uint32(h[4:])
	if n&(1<<31) != 0 {
		return 0, errors.New("an extended clipboard message")
	}
	return int64(n), nil
}

// clientMsgs carries the viewer's SetPixelFormat, SetEncodings (cut to rfbShimKeeps),
// FramebufferUpdateRequest, KeyEvent, PointerEvent and ClientCutText to the server.
func (x *rfbRelay) clientMsgs(r *bufio.Reader, w *bufio.Writer) error {
	for {
		t, err := r.ReadByte()
		if err != nil {
			return err
		}
		head := []byte{t}
		switch t {
		case 0: // SetPixelFormat
			b := make([]byte, 20)
			b[0] = t
			if _, err := io.ReadFull(r, b[1:]); err != nil {
				return err
			}
			if _, err := rfbBytesPerPixel(b[4]); err != nil {
				return fmt.Errorf("SetPixelFormat: %w", err)
			}
			x.bpp.Store(int32(b[4]))
			_, err = w.Write(b)
		case 2: // SetEncodings
			h := make([]byte, 4)
			h[0] = t
			if _, err := io.ReadFull(r, h[1:]); err != nil {
				return err
			}
			list := make([]byte, 4*int(binary.BigEndian.Uint16(h[2:])))
			if _, err := io.ReadFull(r, list); err != nil {
				return err
			}
			kept := make([]byte, 0, len(list))
			for i := 0; i < len(list); i += 4 {
				if rfbShimKeeps(int32(binary.BigEndian.Uint32(list[i:]))) {
					kept = append(kept, list[i:i+4]...)
				}
			}
			binary.BigEndian.PutUint16(h[2:], uint16(len(kept)/4))
			_, err = w.Write(append(h, kept...))
		case 3: // FramebufferUpdateRequest
			_, err = rfbPass(w, r, head, 9)
		case 4: // KeyEvent
			_, err = rfbPass(w, r, head, 7)
		case 5: // PointerEvent
			_, err = rfbPass(w, r, head, 5)
		case 6: // ClientCutText
			var h []byte
			if h, err = rfbPass(w, r, head, 7); err == nil {
				err = x.cut(w, r, h, &x.clientCuts)
			}
		default:
			return fmt.Errorf("viewer message type %d", t)
		}
		if err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
}

func (x *rfbRelay) cut(w *bufio.Writer, r *bufio.Reader, h []byte, count *atomic.Int64) error {
	n, err := rfbCutLength(h)
	if err != nil {
		return err
	}
	count.Add(1)
	_, err = io.CopyN(w, r, n)
	return err
}

// serverMsgs carries the server's FramebufferUpdate (in the kept encodings), SetColourMapEntries, Bell
// and ServerCutText to the viewer.
func (x *rfbRelay) serverMsgs(r *bufio.Reader, w *bufio.Writer) error {
	for {
		t, err := r.ReadByte()
		if err != nil {
			return err
		}
		head := []byte{t}
		switch t {
		case 0: // FramebufferUpdate
			err = x.update(r, w, head)
		case 1: // SetColourMapEntries
			var h []byte
			if h, err = rfbPass(w, r, head, 5); err == nil {
				_, err = io.CopyN(w, r, 6*int64(binary.BigEndian.Uint16(h[4:])))
			}
		case 2: // Bell
			err = w.WriteByte(t)
		case 3: // ServerCutText
			var h []byte
			if h, err = rfbPass(w, r, head, 7); err == nil {
				err = x.cut(w, r, h, &x.serverCuts)
			}
		default:
			return fmt.Errorf("server message type %d", t)
		}
		if err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
}

func (x *rfbRelay) update(r *bufio.Reader, w *bufio.Writer, head []byte) error {
	h, err := rfbPass(w, r, head, 3)
	if err != nil {
		return err
	}
	for rects := binary.BigEndian.Uint16(h[2:]); rects > 0; rects-- {
		rh, err := rfbPass(w, r, nil, 12)
		if err != nil {
			return err
		}
		width, height := int64(binary.BigEndian.Uint16(rh[4:])), int64(binary.BigEndian.Uint16(rh[6:]))
		bpp, err := rfbBytesPerPixel(byte(x.bpp.Load()))
		if err != nil {
			return err
		}
		var n int64
		switch enc := int32(binary.BigEndian.Uint32(rh[8:])); enc {
		case rfbEncRaw:
			n = width * height * bpp
		case rfbEncCopyRect:
			n = 4
		case rfbEncZRLE:
			zh, err := rfbPass(w, r, nil, 4)
			if err != nil {
				return err
			}
			n = int64(binary.BigEndian.Uint32(zh))
		case rfbEncCursor:
			n = width*height*bpp + (width+7)/8*height
		case rfbEncDesktopSize:
		case rfbEncLastRect:
			return nil
		default:
			return fmt.Errorf("rectangle encoding %d", enc)
		}
		if _, err := io.CopyN(w, r, n); err != nil {
			return err
		}
	}
	return nil
}
