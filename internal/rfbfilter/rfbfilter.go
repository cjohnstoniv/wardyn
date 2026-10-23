// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package rfbfilter enforces a desktop's clipboard rules on a relayed noVNC
// WebSocket. It sits between the browser and the sandbox's websockify, decodes
// the WebSocket frames, follows the RFB stream inside them, drops a cut-text
// message whose direction the run's policy blocks, and reports every cut text
// by direction, size and SHA-256. It never keeps, logs or reports the text.
//
// It fails closed: anything it cannot follow (a text frame, a WebSocket
// extension, a security type that encrypts the stream, an encoding or message
// it cannot frame) ends the connection rather than relaying bytes it has not
// read. The browser's SetEncodings is rewritten to the encodings it can frame.
//
// The caller wraps a desktop's socket only, and that socket is RFB or it closes:
// a server whose first data frame is not an RFB ProtocolVersion ends it with
// ErrNotRFB. The server speaks first in RFB, so browser bytes that arrive before
// that frame are held (up to maxHeld and holdFor) and parsed in order once it
// has. Nothing is ever relayed unread.
package rfbfilter

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"time"
)

// Direction is which way a clipboard transfer crosses the relay.
type Direction string

const (
	// PasteIn is the browser's clipboard sent into the desktop (RFB ClientCutText).
	PasteIn Direction = "paste_in"
	// CopyOut is the desktop's clipboard sent to the browser (RFB ServerCutText).
	CopyOut Direction = "copy_out"
)

// Rules says which clipboard directions the relay lets through.
type Rules struct{ PasteIn, CopyOut bool }

func (r Rules) allows(d Direction) bool {
	if d == PasteIn {
		return r.PasteIn
	}
	return r.CopyOut
}

// Event is one cut text the relay saw. It carries no content by design.
type Event struct {
	Direction Direction
	Bytes     int64
	SHA256    string
	Allowed   bool
}

// ErrNotRFB is the server's first data frame not opening with an RFB ProtocolVersion.
var ErrNotRFB = errors.New("rfb relay: the desktop socket does not speak RFB")

// Sink receives the relayed bytes after the filter, as WebSocket frames: toBrowser is the
// server's side as the browser gets it, otherwise the browser's side as the server gets it.
// A dropped cut text never reaches it. It is called from the relaying goroutine, outside the
// filter's lock, so a slow sink slows that direction of the socket.
type Sink func(toBrowser bool, frames []byte)

// Browser bytes the relay holds until the server's first frame says the socket is RFB.
const maxHeld = 64 << 10

var holdFor = 5 * time.Second // a var so a test can shorten it

// maxFrame bounds one WebSocket frame, which the relay buffers to re-frame.
// websockify sends at most 64 KiB per frame; a larger one closes the socket.
const maxFrame = 8 << 20

type mode int

const (
	sniffing mode = iota // no data frame from the server yet: browser bytes are held
	rfbMode
)

// Conn is the filtered backend connection. Read yields the server's frames for
// the browser; Write takes the browser's frames for the server. net/http's
// upgrade copier calls each from its own goroutine.
type Conn struct {
	backend io.ReadWriteCloser
	audit   func(Event)
	refused func(error)
	sink    Sink
	once    sync.Once
	wmu     sync.Mutex // serialises writes to the backend, so held browser bytes go first

	mu     sync.Mutex // mode and the RFB state both directions share
	mode   mode
	server *parser
	client *parser

	rbuf      [32 << 10]byte
	rin, rout []byte // server bytes not yet a whole frame; bytes ready for the browser
	win       []byte // browser bytes not yet a whole frame
	held      []byte // browser bytes that arrived before the server's first frame
	holdTimer *time.Timer
}

// Wrap filters backend under rules, reporting each cut text to audit, and the
// first thing it could not follow to refused (an error that names a message
// type or an encoding, never content). Both are called outside the lock, from
// the goroutine that relayed the bytes.
func Wrap(backend io.ReadWriteCloser, rules Rules, audit func(Event), refused func(error)) *Conn {
	s := &session{rules: rules}
	c := &Conn{backend: backend, audit: audit, refused: refused, server: &parser{s: s}, client: &parser{s: s}}
	c.server.msg, c.client.msg = c.server.serverMsg, c.client.clientMsg
	c.server.expect(12, c.server.serverVersion)
	c.client.expect(12, c.client.clientVersion)
	return c
}

// SetSink sets the sink for the filtered bytes. Call it before the socket is relayed.
func (c *Conn) SetSink(s Sink) { c.sink = s }

// Read returns the server's bytes for the browser, filtered.
func (c *Conn) Read(p []byte) (int, error) {
	for len(c.rout) == 0 {
		n, err := c.backend.Read(c.rbuf[:])
		if n > 0 {
			out, ferr := c.filter(&c.rin, c.rbuf[:n], true)
			if ferr != nil {
				c.backend.Close()
				return 0, ferr
			}
			c.rout = out
			if c.sink != nil && len(out) > 0 {
				c.sink(true, out)
			}
			if ferr = c.flushHeld(); ferr != nil {
				return 0, ferr
			}
		}
		if err != nil && len(c.rout) == 0 {
			return 0, err
		}
	}
	n := copy(p, c.rout)
	c.rout = c.rout[n:]
	return n, nil
}

// Write takes the browser's bytes for the server and forwards them filtered. Before the
// server's first frame it holds them instead.
func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	if c.mode == sniffing {
		if len(c.held)+len(p) > maxHeld {
			c.mu.Unlock()
			return 0, c.fail(fmt.Errorf("rfb relay: the browser sent over %d bytes before the desktop spoke", maxHeld))
		}
		if c.holdTimer == nil {
			c.holdTimer = time.AfterFunc(holdFor, c.holdExpired)
		}
		c.held = append(c.held, p...)
		c.mu.Unlock()
		return len(p), nil
	}
	held := c.takeHeld()
	c.mu.Unlock()
	if err := c.forward(append(held, p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// flushHeld forwards the held browser bytes once the server's first frame has decided. With
// nothing held it returns without wmu: a browser Write blocked in the backend must never stall
// the server's bytes, or a desktop busy writing a frame and a large paste wait on each other.
func (c *Conn) flushHeld() error {
	c.mu.Lock()
	pending := c.mode != sniffing && len(c.held) > 0
	c.mu.Unlock()
	if !pending {
		return nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	held := c.takeHeld()
	c.mu.Unlock()
	return c.forward(held)
}

// takeHeld hands over the held bytes. The caller holds mu, and the server has decided.
func (c *Conn) takeHeld() []byte {
	if c.mode == sniffing || len(c.held) == 0 {
		return nil
	}
	if c.holdTimer != nil {
		c.holdTimer.Stop()
	}
	held := c.held
	c.held = nil
	return held
}

// forward filters browser bytes and writes them to the backend. The caller holds wmu.
func (c *Conn) forward(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	out, err := c.filter(&c.win, p, false)
	if err != nil {
		c.backend.Close()
		return err
	}
	if len(out) == 0 {
		return nil
	}
	if c.sink != nil {
		c.sink(false, out)
	}
	_, err = c.backend.Write(out)
	return err
}

func (c *Conn) holdExpired() {
	c.mu.Lock()
	waiting := c.mode == sniffing
	c.mu.Unlock()
	if waiting {
		_ = c.fail(fmt.Errorf("rfb relay: the desktop did not speak within %s of the browser", holdFor))
	}
}

// fail reports err (once) and closes the backend, which ends both directions.
func (c *Conn) fail(err error) error {
	c.once.Do(func() { c.refused(err) })
	c.backend.Close()
	return err
}

// Close stops the hold timer and closes the backend.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.holdTimer != nil {
		c.holdTimer.Stop()
	}
	c.mu.Unlock()
	return c.backend.Close()
}

func (c *Conn) filter(pending *[]byte, in []byte, fromServer bool) ([]byte, error) {
	var events []Event
	c.mu.Lock()
	out, err := c.filterLocked(pending, in, fromServer, &events)
	c.mu.Unlock()
	for _, ev := range events {
		c.audit(ev)
	}
	if err != nil {
		c.once.Do(func() { c.refused(err) })
	}
	return out, err
}

func (c *Conn) filterLocked(pending *[]byte, in []byte, fromServer bool, events *[]Event) ([]byte, error) {
	*pending = append(*pending, in...)
	var out []byte
	for {
		f, n, err := readFrame(*pending)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out, nil
		}
		*pending = (*pending)[n:]
		if f.opcode() >= 8 || (c.mode == sniffing && len(f.payload) == 0) {
			out = appendFrame(out, f, f.payload) // control frames carry no RFB
			continue
		}
		if c.mode == sniffing {
			if !looksRFB(f.payload) {
				return nil, ErrNotRFB // only server bytes are filtered while sniffing: the browser's are held
			}
			c.mode = rfbMode
		}
		if f.opcode() == 1 {
			return nil, errors.New("rfb relay: a text frame on an RFB socket")
		}
		p := c.client
		if fromServer {
			p = c.server
		}
		filtered, err := p.feed(f.payload, events)
		if err != nil {
			return nil, err
		}
		if len(filtered) == 0 && f.b0 == 0x82 {
			continue // a whole binary message that was all a dropped cut text
		}
		out = appendFrame(out, f, filtered)
	}
}

func looksRFB(p []byte) bool {
	n := min(len(p), 4)
	return bytes.Equal(p[:n], []byte("RFB ")[:n])
}

// frame is one decoded WebSocket frame.
type frame struct {
	b0      byte // FIN and opcode; the RSV bits are refused
	masked  bool
	key     [4]byte
	payload []byte // unmasked
}

func (f frame) opcode() byte { return f.b0 & 0x0f }

// readFrame decodes the frame at the head of b. n == 0 means b does not yet
// hold a whole one.
func readFrame(b []byte) (f frame, n int, err error) {
	if len(b) < 2 {
		return f, 0, nil
	}
	f.b0 = b[0]
	if b[0]&0x70 != 0 {
		return f, 0, errors.New("rfb relay: a WebSocket frame uses an extension")
	}
	f.masked = b[1]&0x80 != 0
	size, off := uint64(b[1]&0x7f), 2
	switch size {
	case 126:
		if len(b) < 4 {
			return f, 0, nil
		}
		size, off = uint64(binary.BigEndian.Uint16(b[2:])), 4
	case 127:
		if len(b) < 10 {
			return f, 0, nil
		}
		size, off = binary.BigEndian.Uint64(b[2:]), 10
	}
	if size > maxFrame {
		return f, 0, fmt.Errorf("rfb relay: a %d-byte WebSocket frame is over the %d-byte limit", size, maxFrame)
	}
	if f.masked {
		if len(b) < off+4 {
			return f, 0, nil
		}
		copy(f.key[:], b[off:])
		off += 4
	}
	end := off + int(size)
	if len(b) < end {
		return f, 0, nil
	}
	f.payload = append([]byte(nil), b[off:end]...)
	if f.masked {
		mask(f.payload, f.key)
	}
	return f, end, nil
}

func mask(p []byte, key [4]byte) {
	for i := range p {
		p[i] ^= key[i&3]
	}
}

// appendFrame encodes f's header around p. A masked frame keeps its own key:
// the hop it goes on is the sandbox's loopback, where masking guards nothing.
func appendFrame(dst []byte, f frame, p []byte) []byte {
	var b1 byte
	if f.masked {
		b1 = 0x80
	}
	dst = append(dst, f.b0)
	switch n := len(p); {
	case n < 126:
		dst = append(dst, b1|byte(n))
	case n <= 0xffff:
		dst = binary.BigEndian.AppendUint16(append(dst, b1|126), uint16(n))
	default:
		dst = binary.BigEndian.AppendUint64(append(dst, b1|127), uint64(n))
	}
	if !f.masked {
		return append(dst, p...)
	}
	dst = append(dst, f.key[:]...)
	start := len(dst)
	dst = append(dst, p...)
	mask(dst[start:], f.key)
	return dst
}

// session is the RFB state the two directions share.
type session struct {
	rules   Rules
	version int  // 33, 37 or 38 once the browser has chosen
	secType byte // 1 (None) or 2 (VNC auth) once known
	bpp     int  // bytes per pixel: ServerInit's, then SetPixelFormat's
}

type step func(piece []byte) error

// parser follows one direction of the RFB stream incrementally: it gathers
// fixed-size pieces (headers) and streams variable payloads through without
// buffering them.
type parser struct {
	s    *session
	msg  step // the normal-phase message dispatcher for this direction
	want int
	buf  []byte
	next step

	pass   int64 // payload bytes still to stream
	drop   bool  // the payload being streamed is dropped
	sum    hash.Hash
	cut    *Event
	then   func()
	rects  int // rectangles left in a FramebufferUpdate
	out    []byte
	events *[]Event
}

func (p *parser) expect(n int, next step) { p.want, p.next = n, next }

// more widens the current piece to n bytes, keeping what was read of it.
func (p *parser) more(n int, next step, have []byte) error {
	p.buf, p.want, p.next = have, n, next
	return nil
}

func (p *parser) forward(b []byte) { p.out = append(p.out, b...) }

func (p *parser) msgNext() { p.expect(1, p.msg) }

func (p *parser) stream(n int64, drop bool, then func()) {
	p.pass, p.drop, p.then = n, drop, then
	if n == 0 {
		p.endPass()
	}
}

func (p *parser) endPass() {
	if p.cut != nil {
		p.cut.SHA256 = hex.EncodeToString(p.sum.Sum(nil))
		*p.events = append(*p.events, *p.cut)
		p.cut, p.sum = nil, nil
	}
	then := p.then
	p.then = nil
	then()
}

func (p *parser) feed(in []byte, events *[]Event) ([]byte, error) {
	p.out, p.events = nil, events
	for len(in) > 0 {
		if p.pass > 0 {
			n := int(min(p.pass, int64(len(in))))
			if p.sum != nil {
				p.sum.Write(in[:n])
			}
			if !p.drop {
				p.forward(in[:n])
			}
			in, p.pass = in[n:], p.pass-int64(n)
			if p.pass == 0 {
				p.endPass()
			}
			continue
		}
		n := min(p.want-len(p.buf), len(in))
		p.buf = append(p.buf, in[:n]...)
		in = in[n:]
		if len(p.buf) < p.want {
			break
		}
		piece := p.buf
		p.buf = nil
		if err := p.next(piece); err != nil {
			return nil, err
		}
	}
	return p.out, nil
}

// cutText streams one cut text: through when its direction is allowed, dropped
// with its header when not, hashed either way.
func (p *parser) cutText(d Direction, hdr []byte) error {
	size := int64(int32(binary.BigEndian.Uint32(hdr[4:])))
	if size < 0 {
		size = -size // the extended-clipboard form; its payload is framed the same way
	}
	allowed := p.s.rules.allows(d)
	if allowed {
		p.forward(hdr)
	}
	p.cut, p.sum = &Event{Direction: d, Bytes: size, Allowed: allowed}, sha256.New()
	p.stream(size, !allowed, p.msgNext)
	return nil
}

func u16(b []byte) int   { return int(binary.BigEndian.Uint16(b)) }
func u32(b []byte) int64 { return int64(binary.BigEndian.Uint32(b)) }

// fixed forwards a whole fixed-size message and returns to the dispatcher.
func (p *parser) fixed(b []byte) error {
	p.forward(b)
	p.msgNext()
	return nil
}

// trailer forwards a header whose last field counts n units of size bytes.
func (p *parser) trailer(b []byte, n int64) {
	p.forward(b)
	p.stream(n, false, p.msgNext)
}

func setBPP(s *session, bits byte) error {
	if bits != 8 && bits != 16 && bits != 32 {
		return fmt.Errorf("rfb relay: %d bits per pixel", bits)
	}
	s.bpp = int(bits) / 8
	return nil
}
