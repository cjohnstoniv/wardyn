// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbfilter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ClientFrames reads a browser's WebSocket stream through the checks a WebSocket server makes on a client's
// frames, for a door whose WebSocket library refuses a bad frame with an error it cannot tell from a dropped
// connection (the VM display's: coder/websocket, frame.go and read.go). The checks are that library's, with no
// extension negotiated: no RSV bit, a mask, a known opcode, a control frame of at most 125 bytes and not
// fragmented, a close payload that is empty or opens with a status code a peer may send, no continuation outside
// a message and no new message inside one, and a 64-bit length with its top bit clear. A read that holds a bad
// frame returns its Refusal instead, the frames before it in that read too, as the filter drops a refused read
// whole, and so does every read after it; Refused returns it. After a close frame it checks nothing, as the
// library reads no further.
type ClientFrames struct {
	r io.Reader

	mu      sync.Mutex // the library's reader reads; the door asks Refused
	hdr     []byte     // the frame header read so far
	op      byte
	key     [4]byte
	left    uint64 // the frame's payload not yet read
	payload []byte // a close frame's payload so far, masked
	inMsg   bool   // a data message is not yet finished
	closed  bool   // a close frame ended the stream
	refused error
}

// CheckClientFrames reads r through ClientFrames's checks.
func CheckClientFrames(r io.Reader) *ClientFrames { return &ClientFrames{r: r} }

// Read reads r, or returns the Refusal of a bad frame.
func (c *ClientFrames) Read(p []byte) (int, error) {
	if err := c.Refused(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if cerr := c.check(p[:n]); cerr != nil {
		return 0, cerr
	}
	return n, err
}

// Refused is the Refusal of the bad frame a read returned, or nil.
func (c *ClientFrames) Refused() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refused
}

func (c *ClientFrames) check(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(p) > 0 && !c.closed {
		var err error
		if c.left > 0 {
			n := min(uint64(len(p)), c.left)
			if c.op == 8 {
				c.payload = append(c.payload, p[:n]...)
			}
			p, c.left = p[n:], c.left-n
			if c.left == 0 && c.op == 8 {
				err = c.closeFrame()
			}
		} else if c.hdr, p = append(c.hdr, p[0]), p[1:]; c.headerRead() {
			err = c.frame()
		}
		if err != nil {
			c.refused = &Refusal{Kind: refusedFrame[false], Err: err}
			return c.refused
		}
	}
	return nil
}

// headerRead is whether hdr holds as much of a frame header as the library reads before it refuses or relays the
// frame: two bytes, the extended length, and the mask key when the frame says it has one. A 64-bit length with its
// top bit set is refused before the key is read, as the library refuses it.
func (c *ClientFrames) headerRead() bool {
	h := c.hdr
	if len(h) < 2 {
		return false
	}
	n := 2
	switch h[1] & 0x7f {
	case 126:
		n = 4
	case 127:
		if len(h) == 10 && h[2]&0x80 != 0 {
			return true
		}
		n = 10
	}
	if h[1]&0x80 != 0 {
		n += 4
	}
	return len(h) == n
}

// frame checks the header in hdr as the library does and starts on the frame's payload.
func (c *ClientFrames) frame() error {
	h := c.hdr
	c.hdr = c.hdr[:0]
	fin, op, size, off := h[0]&0x80 != 0, h[0]&0x0f, uint64(h[1]&0x7f), 2
	switch size {
	case 126:
		size, off = uint64(binary.BigEndian.Uint16(h[2:])), 4
	case 127:
		size, off = binary.BigEndian.Uint64(h[2:]), 10
	}
	control := op >= 8 && op <= 10
	switch {
	case size>>63 != 0:
		return errors.New("rfb relay: a WebSocket frame's 64-bit length has its top bit set")
	case h[0]&0x70 != 0:
		return errors.New("rfb relay: a WebSocket frame uses an extension")
	case h[1]&0x80 == 0:
		return errors.New("rfb relay: an unmasked WebSocket frame from the browser")
	case op > 2 && !control:
		return fmt.Errorf("rfb relay: a WebSocket frame with the unknown opcode %d", op)
	case control && size > 125:
		return fmt.Errorf("rfb relay: a %d-byte WebSocket control frame", size)
	case control && !fin:
		return errors.New("rfb relay: a fragmented WebSocket control frame")
	case op == 0 && !c.inMsg:
		return errors.New("rfb relay: a WebSocket continuation frame outside a message")
	case op != 0 && !control && c.inMsg:
		return errors.New("rfb relay: a new WebSocket message before the last one ended")
	}
	if !control {
		c.inMsg = !fin
	}
	c.op, c.left, c.payload = op, size, c.payload[:0]
	copy(c.key[:], h[off:])
	if op == 8 && size == 0 {
		return c.closeFrame()
	}
	return nil
}

// closeFrame checks a close frame's whole payload as the library does: empty, or a status code a peer may send
// (1000-1003, 1007-1014, 3000-4999) and any reason. The stream ends there.
func (c *ClientFrames) closeFrame() error {
	p := c.payload
	mask(p, c.key)
	if len(p) == 1 {
		return errors.New("rfb relay: a WebSocket close frame with a 1-byte payload")
	}
	if len(p) > 1 {
		code := binary.BigEndian.Uint16(p)
		if !(code >= 1000 && code <= 1014 && (code < 1004 || code > 1006) || code >= 3000 && code <= 4999) {
			return fmt.Errorf("rfb relay: a WebSocket close frame with the status code %d", code)
		}
	}
	c.closed = true
	return nil
}
