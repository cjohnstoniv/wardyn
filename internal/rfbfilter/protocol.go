// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbfilter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
)

// The RFB steps, per RFC 6143. Each step gets one whole piece, forwards what
// may pass, and names the next piece it needs.

var versionRE = regexp.MustCompile(`^RFB \d{3}\.\d{3}\n$`)

func (p *parser) serverVersion(b []byte) error {
	if !versionRE.Match(b) {
		return errors.New("rfb relay: the server's ProtocolVersion is malformed")
	}
	p.forward(b)
	p.expect(1, p.serverSecurity)
	return nil
}

// clientVersion is the browser's choice, which the server must follow.
func (p *parser) clientVersion(b []byte) error {
	switch string(b) {
	case "RFB 003.003\n":
		p.s.version = 33
		p.expect(1, p.clientAfterSecurity33)
	case "RFB 003.007\n":
		p.s.version = 37
		p.expect(1, p.clientSecurityChoice)
	case "RFB 003.008\n":
		p.s.version = 38
		p.expect(1, p.clientSecurityChoice)
	default:
		return fmt.Errorf("rfb relay: unsupported client ProtocolVersion %q", b)
	}
	p.forward(b)
	return nil
}

func (p *parser) serverSecurity(b []byte) error {
	switch p.s.version {
	case 33:
		return p.more(4, p.serverSecurity33, b)
	case 37, 38:
		p.forward(b)
		if b[0] == 0 {
			p.expect(4, p.reason)
			return nil
		}
		p.expect(int(b[0]), p.serverSecurityTypes)
		return nil
	}
	return errors.New("rfb relay: the server sent security data before the browser chose a version")
}

func (p *parser) serverSecurity33(b []byte) error {
	p.forward(b)
	switch t := binary.BigEndian.Uint32(b); t {
	case 0:
		p.expect(4, p.reason)
	case 1, 2:
		p.s.secType = byte(t)
		p.afterSecurity()
	default:
		return fmt.Errorf("rfb relay: security type %d hides the stream from the relay", t)
	}
	return nil
}

func (p *parser) serverSecurityTypes(b []byte) error {
	p.forward(b)
	p.expect(1, p.serverAfterChoice)
	return nil
}

// serverAfterChoice is the first byte the server sends once the browser has
// picked a security type (3.7 and 3.8).
func (p *parser) serverAfterChoice(b []byte) error {
	if p.s.secType == 0 {
		return errors.New("rfb relay: the server went on before the browser chose a security type")
	}
	if p.s.secType == 1 && p.s.version == 37 {
		return p.more(24, p.serverInit, b) // 3.7 sends no SecurityResult for None
	}
	p.afterSecurity()
	return p.more(p.want, p.next, b)
}

// afterSecurity names the server's next piece once the security type is known.
func (p *parser) afterSecurity() {
	switch {
	case p.s.secType == 2:
		p.expect(16, p.serverChallenge)
	case p.s.version == 33 || p.s.version == 37:
		p.expect(24, p.serverInit)
	default:
		p.expect(4, p.securityResult)
	}
}

func (p *parser) serverChallenge(b []byte) error {
	p.forward(b)
	p.expect(4, p.securityResult)
	return nil
}

func (p *parser) securityResult(b []byte) error {
	p.forward(b)
	switch {
	case binary.BigEndian.Uint32(b) == 0:
		p.expect(24, p.serverInit)
	case p.s.version == 38:
		p.expect(4, p.reason)
	default:
		p.expect(1, p.refused)
	}
	return nil
}

// reason relays the server's failure text; the server closes after it.
func (p *parser) reason(b []byte) error {
	p.forward(b)
	p.stream(u32(b), false, func() { p.expect(1, p.refused) })
	return nil
}

func (p *parser) refused([]byte) error {
	return errors.New("rfb relay: the server sent data after refusing the connection")
}

func (p *parser) serverInit(b []byte) error {
	if err := setBPP(p.s, b[4]); err != nil {
		return err
	}
	p.forward(b)
	p.stream(u32(b[20:]), false, p.msgNext)
	return nil
}

func (p *parser) clientAfterSecurity33(b []byte) error {
	switch p.s.secType {
	case 1:
		return p.clientInit(b)
	case 2:
		return p.more(16, p.clientAuth, b)
	}
	return errors.New("rfb relay: the browser went on before the server chose a security type")
}

func (p *parser) clientSecurityChoice(b []byte) error {
	if b[0] != 1 && b[0] != 2 {
		return fmt.Errorf("rfb relay: security type %d hides the stream from the relay", b[0])
	}
	p.s.secType = b[0]
	p.forward(b)
	if b[0] == 2 {
		p.expect(16, p.clientAuth)
	} else {
		p.expect(1, p.clientInit)
	}
	return nil
}

func (p *parser) clientAuth(b []byte) error {
	p.forward(b)
	p.expect(1, p.clientInit)
	return nil
}

func (p *parser) clientInit(b []byte) error {
	p.forward(b)
	p.msgNext()
	return nil
}

// Server-to-client message types the relay frames.
func (p *parser) serverMsg(b []byte) error {
	switch b[0] {
	case 0: // FramebufferUpdate
		return p.more(4, p.framebufferUpdate, b)
	case 1: // SetColourMapEntries
		return p.more(6, func(h []byte) error { p.trailer(h, 6*int64(u16(h[4:]))); return nil }, b)
	case 2, 150: // Bell, EndOfContinuousUpdates
		return p.fixed(b)
	case 3: // ServerCutText
		return p.more(8, func(h []byte) error { return p.cutText(CopyOut, h) }, b)
	case 248: // ServerFence
		return p.more(9, p.fence, b)
	case 250: // xvp
		return p.more(4, p.fixed, b)
	}
	return fmt.Errorf("rfb relay: server message type %d", b[0])
}

// Client-to-server message types the relay frames.
func (p *parser) clientMsg(b []byte) error {
	switch b[0] {
	case 0: // SetPixelFormat
		return p.more(20, func(h []byte) error {
			if err := setBPP(p.s, h[4]); err != nil {
				return err
			}
			return p.fixed(h)
		}, b)
	case 2: // SetEncodings
		return p.more(4, p.setEncodings, b)
	case 3: // FramebufferUpdateRequest
		return p.more(10, p.fixed, b)
	case 4: // KeyEvent
		return p.more(8, p.fixed, b)
	case 5: // PointerEvent
		return p.more(6, p.fixed, b)
	case 6: // ClientCutText
		return p.more(8, func(h []byte) error { return p.cutText(PasteIn, h) }, b)
	case 150: // EnableContinuousUpdates
		return p.more(10, p.fixed, b)
	case 248: // ClientFence
		return p.more(9, p.fence, b)
	case 250: // xvp
		return p.more(4, p.fixed, b)
	case 251: // SetDesktopSize
		return p.more(8, func(h []byte) error { p.trailer(h, 16*int64(h[6])); return nil }, b)
	case 255: // QEMU: only the extended key event
		return p.more(2, func(h []byte) error {
			if h[1] != 0 {
				return fmt.Errorf("rfb relay: QEMU client message subtype %d", h[1])
			}
			return p.more(12, p.fixed, h)
		}, b)
	}
	return fmt.Errorf("rfb relay: client message type %d", b[0])
}

func (p *parser) fence(h []byte) error {
	p.trailer(h, int64(h[8]))
	return nil
}

// setEncodings keeps only the encodings whose rectangles the relay can frame,
// so the server answers in nothing it cannot follow.
func (p *parser) setEncodings(h []byte) error {
	n := u16(h[2:])
	if n == 0 {
		return p.fixed(h)
	}
	p.expect(4*n, func(list []byte) error {
		kept := make([]byte, 0, len(list))
		for i := 0; i < len(list); i += 4 {
			if relayable(int32(binary.BigEndian.Uint32(list[i:]))) {
				kept = append(kept, list[i:i+4]...)
			}
		}
		p.forward(binary.BigEndian.AppendUint16(h[:2:2], uint16(len(kept)/4)))
		p.forward(kept)
		p.msgNext()
		return nil
	})
	return nil
}

const (
	encRaw          = 0
	encCopyRect     = 1
	encZRLE         = 16
	encDesktopSize  = -223
	encLastRect     = -224
	encCursor       = -239
	encQEMUKey      = -258
	encExtDesktopSz = -308
)

// relayable is the SetEncodings keep-list: the pixel encodings and the
// pseudo-encodings whose rectangles the relay frames, plus the JPEG quality
// and compression hints, which never appear as a rectangle. Extended
// Clipboard is deliberately absent: cut text stays in its plain form.
func relayable(e int32) bool {
	switch e {
	case encRaw, encCopyRect, encZRLE, encDesktopSize, encLastRect, encCursor, encQEMUKey, encExtDesktopSz:
		return true
	}
	return (e >= -256 && e <= -247) || (e >= -32 && e <= -23)
}

func (p *parser) framebufferUpdate(h []byte) error {
	p.forward(h)
	p.rects = u16(h[2:])
	p.nextRect()
	return nil
}

func (p *parser) nextRect() {
	if p.rects == 0 {
		p.msgNext()
		return
	}
	p.expect(12, p.rect)
}

func (p *parser) rect(h []byte) error {
	p.forward(h)
	p.rects--
	w, hgt := int64(u16(h[4:])), int64(u16(h[6:]))
	if p.s.bpp == 0 {
		return errors.New("rfb relay: a rectangle before ServerInit")
	}
	bpp := int64(p.s.bpp)
	switch enc := int32(binary.BigEndian.Uint32(h[8:])); enc {
	case encRaw:
		p.stream(w*hgt*bpp, false, p.nextRect)
	case encCursor:
		p.stream(w*hgt*bpp+(w+7)/8*hgt, false, p.nextRect)
	case encCopyRect:
		p.expect(4, func(b []byte) error { p.forward(b); p.nextRect(); return nil })
	case encZRLE:
		p.expect(4, func(b []byte) error { p.forward(b); p.stream(u32(b), false, p.nextRect); return nil })
	case encExtDesktopSz:
		p.expect(4, func(b []byte) error { p.forward(b); p.stream(16*int64(b[0]), false, p.nextRect); return nil })
	case encLastRect:
		p.rects = 0
		p.nextRect()
	case encDesktopSize, encQEMUKey:
		p.nextRect()
	default:
		return fmt.Errorf("rfb relay: rectangle encoding %d", enc)
	}
	return nil
}
