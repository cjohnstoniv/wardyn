// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import "encoding/binary"

// rfbStream follows the sandbox-to-browser RFB stream (RFC 6143 and the extensions noVNC asks for)
// message by message, so a recording can leave out ServerCutText: the desktop's clipboard, which can
// hold a password copied inside the desktop. It needs message boundaries only, never pixels. What the
// browser receives is not touched; only the recording's copy loses those messages.
//
// It fails closed: a stream it cannot follow (another protocol version, a security type or encoding it
// does not know, a server message type it has not seen) stops it, and nothing after that is kept.
//
// The pixel format is the one noVNC sets on every connection, 32 bits per pixel at depth 24 (true
// colour), so a Tight pixel is 3 bytes. The browser's own messages are never read, so the format is
// taken from noVNC rather than from the SetPixelFormat it sends.
type rfbStream struct {
	want     int
	buf      []byte
	then     func(b []byte)
	typeStep bool // the bytes being read are a server message's type

	skip    uint64
	after   func()
	passing bool

	drop     bool // the current message is ServerCutText: none of its bytes are kept
	failed   bool
	rects    int
	cutTexts int
}

const (
	rfbBPP      = 4 // bytes per pixel noVNC asks for
	rfbTPixel   = 3 // a Tight pixel at depth 24
	rfbMaxName  = 1 << 16
	rfbCutText  = 3
	rfbAMTKVM   = "Intel(r) AMT KVM" // the one server noVNC gives 8 bits per pixel
	rfbVersion8 = "RFB 003.008\n"
)

func newRFBStream() *rfbStream {
	s := &rfbStream{}
	s.read(12, s.version)
	return s
}

// feed follows p and hands keep the bytes that belong in the recording, in order.
func (s *rfbStream) feed(p []byte, keep func([]byte)) {
	for len(p) > 0 && !s.failed {
		if s.passing {
			if s.skip == 0 {
				s.passing = false
				s.after()
				continue
			}
			k := min(uint64(len(p)), s.skip)
			if !s.drop {
				keep(p[:k])
			}
			p, s.skip = p[k:], s.skip-k
			continue
		}
		k := min(s.want-len(s.buf), len(p))
		s.buf = append(s.buf, p[:k]...)
		p = p[k:]
		if len(s.buf) < s.want {
			continue
		}
		b := s.buf
		s.buf = nil
		if !s.drop && !(s.typeStep && rfbDropType(b[0])) {
			keep(b)
		}
		s.then(b)
	}
}

// rfbDropType is the server message types a recording leaves out: ServerCutText, and any type this
// reader does not know (which also stops it).
func rfbDropType(t byte) bool {
	switch t {
	case 0, 1, 2, 150, 248, 250:
		return false
	}
	return true
}

func (s *rfbStream) read(n int, then func([]byte)) {
	s.want, s.then, s.typeStep = n, then, false
}

func (s *rfbStream) pass(n uint64, after func()) {
	s.skip, s.after, s.passing = n, after, true
}

func (s *rfbStream) fail() { s.failed = true }

func be16(b []byte) uint64 { return uint64(binary.BigEndian.Uint16(b)) }
func be32(b []byte) uint64 { return uint64(binary.BigEndian.Uint32(b)) }

func (s *rfbStream) version(b []byte) {
	if string(b) != rfbVersion8 {
		s.fail()
		return
	}
	s.read(1, s.securityCount)
}

// securityCount accepts one offered type only: with more, which one the browser chose is on the side
// never read.
func (s *rfbStream) securityCount(b []byte) {
	if b[0] != 1 {
		s.fail()
		return
	}
	s.read(1, s.securityType)
}

func (s *rfbStream) securityType(b []byte) {
	switch b[0] {
	case 1: // None
		s.read(4, s.securityResult)
	case 2: // VNC authentication: a 16-byte challenge first
		s.read(16, func([]byte) { s.read(4, s.securityResult) })
	default:
		s.fail()
	}
}

func (s *rfbStream) securityResult(b []byte) {
	if be32(b) != 0 {
		s.fail()
		return
	}
	s.read(24, s.serverInit)
}

func (s *rfbStream) serverInit(b []byte) {
	n := be32(b[20:24])
	switch {
	case n > rfbMaxName:
		s.fail()
	case n == 0:
		s.next()
	default:
		s.read(int(n), func(name []byte) {
			if string(name) == rfbAMTKVM {
				s.fail()
				return
			}
			s.next()
		})
	}
}

// next starts the next server message.
func (s *rfbStream) next() {
	s.drop = false
	s.read(1, s.message)
	s.typeStep = true
}

func (s *rfbStream) message(b []byte) {
	switch b[0] {
	case 0: // FramebufferUpdate
		s.read(3, func(h []byte) { s.rects = int(be16(h[1:3])); s.rect() })
	case 1: // SetColourMapEntries
		s.read(5, func(h []byte) { s.pass(be16(h[3:5])*6, s.next) })
	case 2, 150: // Bell, EndOfContinuousUpdates
		s.next()
	case rfbCutText: // ServerCutText; a negative length is an extended clipboard message
		s.cutTexts++
		s.drop = true
		s.read(7, func(h []byte) {
			n := int64(int32(binary.BigEndian.Uint32(h[3:7])))
			s.pass(uint64(max(n, -n)), s.next)
		})
	case 248: // ServerFence
		s.read(8, func(h []byte) { s.pass(uint64(h[7]), s.next) })
	case 250: // xvp
		s.read(3, func([]byte) { s.next() })
	default:
		s.fail()
	}
}

func (s *rfbStream) rect() {
	if s.rects == 0 {
		s.next()
		return
	}
	s.rects--
	s.read(12, s.rectHeader)
}

func (s *rfbStream) rectHeader(h []byte) {
	w, hh := be16(h[4:6]), be16(h[6:8])
	switch int32(binary.BigEndian.Uint32(h[8:12])) {
	case 0: // Raw
		s.pass(w*hh*rfbBPP, s.rect)
	case 1: // CopyRect
		s.pass(4, s.rect)
	case 2: // RRE
		s.read(4, func(b []byte) { s.pass(rfbBPP+be32(b)*(rfbBPP+8), s.rect) })
	case 5:
		s.hextile(w, hh)
	case 6, 16: // Zlib, ZRLE
		s.read(4, func(b []byte) { s.pass(be32(b), s.rect) })
	case 7:
		s.tight(w, hh, false)
	case -260:
		s.tight(w, hh, true)
	case -223, -258, -309, -312, -313, -316: // DesktopSize, QEMU key, xvp, Fence, ContinuousUpdates, mouse buttons
		s.rect()
	case -224: // LastRect
		s.next()
	case -239: // Cursor: pixels and a bitmask
		s.pass(w*hh*rfbBPP+(w+7)/8*hh, s.rect)
	case -261: // QEMU LED state
		s.pass(1, s.rect)
	case -307: // DesktopName
		s.read(4, func(b []byte) { s.pass(be32(b), s.rect) })
	case -308: // ExtendedDesktopSize
		s.read(4, func(b []byte) { s.pass(uint64(b[0])*16, s.rect) })
	default:
		s.fail()
	}
}

func (s *rfbStream) hextile(w, h uint64) {
	cols, tiles := (w+15)/16, (w+15)/16*((h+15)/16)
	var i uint64
	var tile func()
	tile = func() {
		if i == tiles {
			s.rect()
			return
		}
		tw, th := min(16, w-i%cols*16), min(16, h-i/cols*16)
		i++
		s.read(1, func(b []byte) {
			sub := b[0]
			if sub&1 != 0 { // raw tile
				s.pass(tw*th*rfbBPP, tile)
				return
			}
			var fixed uint64
			if sub&2 != 0 {
				fixed += rfbBPP
			}
			if sub&4 != 0 {
				fixed += rfbBPP
			}
			if sub&8 == 0 {
				s.pass(fixed, tile)
				return
			}
			s.pass(fixed, func() {
				s.read(1, func(c []byte) {
					per := uint64(2)
					if sub&16 != 0 {
						per += rfbBPP
					}
					s.pass(uint64(c[0])*per, tile)
				})
			})
		})
	}
	tile()
}

func (s *rfbStream) tight(w, h uint64, png bool) {
	s.read(1, func(b []byte) {
		switch ctl := b[0] >> 4; {
		case ctl == 8: // fill
			s.pass(rfbTPixel, s.rect)
		case ctl == 9 || png && ctl == 10: // JPEG, PNG
			s.compact(s.rect)
		case ctl > 9 || png:
			s.fail()
		case ctl&4 != 0: // an explicit filter
			s.read(1, func(f []byte) { s.tightFilter(w, h, f[0]) })
		default:
			s.tightData(w * rfbTPixel * h)
		}
	})
}

func (s *rfbStream) tightFilter(w, h uint64, filter byte) {
	switch filter {
	case 0, 2: // copy, gradient
		s.tightData(w * rfbTPixel * h)
	case 1: // palette
		s.read(1, func(b []byte) {
			n := uint64(b[0]) + 1
			row := w
			if n <= 2 {
				row = (w + 7) / 8
			}
			s.pass(n*rfbTPixel, func() { s.tightData(row * h) })
		})
	default:
		s.fail()
	}
}

// tightData is a Tight rectangle's pixel data: raw under 12 bytes, otherwise zlib with a compact length.
func (s *rfbStream) tightData(size uint64) {
	if size < 12 {
		s.pass(size, s.rect)
		return
	}
	s.compact(s.rect)
}

// compact reads a Tight compact length (1 to 3 bytes) and passes that many bytes.
func (s *rfbStream) compact(after func()) {
	s.read(1, func(b0 []byte) {
		n := uint64(b0[0] & 0x7f)
		if b0[0]&0x80 == 0 {
			s.pass(n, after)
			return
		}
		s.read(1, func(b1 []byte) {
			n |= uint64(b1[0]&0x7f) << 7
			if b1[0]&0x80 == 0 {
				s.pass(n, after)
				return
			}
			s.read(1, func(b2 []byte) { s.pass(n|uint64(b2[0])<<14, after) })
		})
	})
}
