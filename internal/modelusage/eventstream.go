// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

import (
	"encoding/binary"
	"hash/crc32"
)

// frame is one AWS event-stream message: string headers and the raw payload. The wire format (prelude,
// headers, payload, two CRC32s) is small enough to read by hand, which avoids a new module dependency.
type frame struct {
	headers map[string]string
	payload []byte
}

const preludeLen, trailerLen = 12, 4

// feedFrames consumes the stream in bounded slices, so the buffer never holds more than one capped frame
// plus one slice, however large a single Write is.
func (p *parser) feedFrames(b []byte) {
	for len(b) > 0 && p.bad == "" {
		n := min(len(b), 64<<10)
		p.buf = append(p.buf, b[:n]...)
		b = b[n:]
		p.drainFrames()
	}
}

func (p *parser) drainFrames() {
	for p.bad == "" && len(p.buf) >= preludeLen {
		total, hdrLen, ok := p.prelude(p.buf)
		if !ok || len(p.buf) < total {
			return
		}
		f, ok := p.readFrame(p.buf[:total], hdrLen)
		if !ok {
			return
		}
		p.onFrame(f)
		if p.bad != "" {
			// The handler failed the parse, which drops the buffer: there is nothing left to reslice.
			return
		}
		p.buf = append(p.buf[:0], p.buf[total:]...)
	}
}

// prelude validates the 12-byte prelude: lengths in range and its CRC.
func (p *parser) prelude(b []byte) (total, hdrLen int, ok bool) {
	t := binary.BigEndian.Uint32(b[0:4])
	h := binary.BigEndian.Uint32(b[4:8])
	if crc32.ChecksumIEEE(b[0:8]) != binary.BigEndian.Uint32(b[8:12]) {
		p.fail(ReasonParseError)
		return 0, 0, false
	}
	if t < preludeLen+trailerLen || uint64(h) > uint64(t)-preludeLen-trailerLen {
		p.fail(ReasonParseError)
		return 0, 0, false
	}
	if t > maxFrame {
		p.fail(ReasonBodyOversize)
		return 0, 0, false
	}
	return int(t), int(h), true
}

func (p *parser) readFrame(msg []byte, hdrLen int) (frame, bool) {
	n := len(msg)
	if crc32.ChecksumIEEE(msg[:n-trailerLen]) != binary.BigEndian.Uint32(msg[n-trailerLen:]) {
		p.fail(ReasonParseError)
		return frame{}, false
	}
	h, ok := headerMap(msg[preludeLen : preludeLen+hdrLen])
	if !ok {
		p.fail(ReasonParseError)
		return frame{}, false
	}
	return frame{headers: h, payload: msg[preludeLen+hdrLen : n-trailerLen]}, true
}

// headerMap reads the header block, keeping string values (type 7) and skipping the rest by their width.
func headerMap(b []byte) (map[string]string, bool) {
	out := map[string]string{}
	for len(b) > 0 {
		nl := int(b[0])
		if len(b) < 1+nl+1 {
			return nil, false
		}
		name, typ := string(b[1:1+nl]), b[1+nl]
		b = b[2+nl:]
		var width int
		switch typ {
		case 0, 1:
			width = 0
		case 2:
			width = 1
		case 3:
			width = 2
		case 4:
			width = 4
		case 5, 8:
			width = 8
		case 9:
			width = 16
		case 6, 7:
			if len(b) < 2 {
				return nil, false
			}
			width = 2 + int(binary.BigEndian.Uint16(b))
		default:
			return nil, false
		}
		if len(b) < width {
			return nil, false
		}
		if typ == 7 {
			out[name] = string(b[2:width])
		}
		b = b[width:]
	}
	return out, true
}
