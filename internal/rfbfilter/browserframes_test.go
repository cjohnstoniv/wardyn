// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbfilter

import (
	"bytes"
	"errors"
	"testing"
)

// keyEvent is a browser's RFB KeyEvent, which the filter relays in the normal phase.
var keyEvent = []byte{4, 1, 0, 0, 0, 0, 0, 0x61}

// brokenBrowserFrames holds one frame of each kind that breaks the WebSocket protocol, each carrying what the
// filter would otherwise relay.
func brokenBrowserFrames() map[string][]byte {
	return map[string][]byte{
		"reserved bits":                    clientFrame(0xC2, keyEvent),
		"an unmasked frame":                appendFrame(nil, frame{b0: 0x82}, keyEvent),
		"an unknown data opcode":           clientFrame(0x83, keyEvent),
		"an unknown control opcode":        clientFrame(0x8B, nil),
		"a control frame over 125 bytes":   clientFrame(0x89, make([]byte, 126)),
		"a fragmented control frame":       clientFrame(0x09, nil),
		"an invalid close payload":         closeFrame(1005, ""),
		"a continuation outside a message": clientFrame(0x80, keyEvent),
		"a new message inside one":         cat(clientFrame(0x02, nil), clientFrame(0x82, keyEvent)),
		"a 64-bit length with its top bit": {0x82, 0xFF, 0x80, 0, 0, 0, 0, 0, 0, 5},
	}
}

// writeAll writes wire to w whole, or a byte at a time, and returns the first error.
func writeAll(w interface{ Write([]byte) (int, error) }, wire []byte, split bool) error {
	step := len(wire)
	if split {
		step = 1
	}
	for i := 0; i < len(wire); i += step {
		if _, err := w.Write(wire[i:min(i+step, len(wire))]); err != nil {
			return err
		}
	}
	return nil
}

// TestCheckBrowserFramesRefusesABadFrameBeforeTheFilter: through CheckBrowserFrames, each frame that breaks the
// WebSocket protocol ends the stream through Refuse, once, in fixed words (a WebSocket frame from the browser):
// none of the write that holds it reaches the desktop, the frames before it in that write neither, and a write
// after it is refused too.
func TestCheckBrowserFramesRefusesABadFrameBeforeTheFilter(t *testing.T) {
	for name, bad := range brokenBrowserFrames() {
		t.Run(name, func(t *testing.T) {
			for _, split := range []bool{false, true} {
				h := newHarness(t, Rules{}, false)
				h.handshake38()
				h.fb.written = nil
				w := h.c.CheckBrowserFrames()
				err := writeAll(w, cat(clientFrame(0x82, keyEvent), bad), split)
				var refusal *Refusal
				if !errors.As(err, &refusal) || refusal.Kind != "a WebSocket frame from the browser that it cannot read" || refusal.Err == nil {
					t.Fatalf("split %v: the write ended with %v, want the browser's frame refused and named", split, err)
				}
				if len(h.refused) != 1 || h.refused[0] != err {
					t.Fatalf("split %v: refused %v, want %v once", split, h.refused, err)
				}
				var want []byte
				if split {
					want = keyEvent // the frame before it went on in writes of its own
				}
				if got := payloads(t, h.fb.written); !bytes.Equal(got, want) {
					t.Fatalf("split %v: the desktop got %x, want %x", split, got, want)
				}
				written := len(h.fb.written)
				if _, again := w.Write(clientFrame(0x82, keyEvent)); !errors.As(again, &refusal) || len(h.fb.written) != written || len(h.refused) != 1 {
					t.Fatalf("split %v: a write after the refusal got %v and %d more bytes to the desktop, want it refused", split, again, len(h.fb.written)-written)
				}
			}
		})
	}
}

// TestCheckBrowserFramesPassesWhatTheFilterPasses: what breaks no WebSocket rule reaches the desktop through
// CheckBrowserFrames exactly as through the filter alone, whole or a byte at a time.
func TestCheckBrowserFramesPassesWhatTheFilterPasses(t *testing.T) {
	for name, stream := range map[string][]byte{
		"a key event": clientFrame(0x82, keyEvent),
		"a key event in fragments, with a ping and a pong between them, then another": cat(clientFrame(0x02, keyEvent[:3]),
			clientFrame(0x89, []byte("p")), clientFrame(0x00, keyEvent[3:5]), clientFrame(0x8A, nil), clientFrame(0x80, keyEvent[5:]),
			clientFrame(0x82, keyEvent)),
		"a 125-byte pong":           clientFrame(0x8A, make([]byte, 125)),
		"a bare close":              clientFrame(0x88, nil),
		"a close, then bytes after": cat(closeFrame(1000, "bye"), clientFrame(0x82, keyEvent)),
	} {
		t.Run(name, func(t *testing.T) {
			for _, split := range []bool{false, true} {
				plain, checked := newHarness(t, Rules{}, false), newHarness(t, Rules{}, false)
				plain.handshake38()
				checked.handshake38()
				plain.fb.written, checked.fb.written = nil, nil
				if err := writeAll(plain.c, stream, split); err != nil {
					t.Fatalf("split %v: the filter alone refused %x: %v", split, stream, err)
				}
				if err := writeAll(checked.c.CheckBrowserFrames(), stream, split); err != nil || len(checked.refused) != 0 {
					t.Fatalf("split %v: checked, the write got %v (refused %v), want it relayed", split, err, checked.refused)
				}
				if !bytes.Equal(checked.fb.written, plain.fb.written) {
					t.Fatalf("split %v: checked, the desktop got %x, want the filter's own %x", split, checked.fb.written, plain.fb.written)
				}
			}
		})
	}
}

// TestCheckBrowserFramesAFailedWriteIsNoRefusal: a good frame whose write to the desktop fails ends the write with
// the desktop's error, and nothing is refused: the desktop going away is no refusal of the browser's.
func TestCheckBrowserFramesAFailedWriteIsNoRefusal(t *testing.T) {
	h := &harness{t: t, fb: &fakeBackend{}}
	backend := &writeFailBackend{fakeBackend: h.fb}
	h.c = Wrap(backend, Rules{}, func(ev Event) { h.events = append(h.events, ev) }, func(err error) { h.refused = append(h.refused, err) })
	h.handshake38()
	backend.fail = true
	_, err := h.c.CheckBrowserFrames().Write(clientFrame(0x82, keyEvent))
	if err == nil || errors.As(err, new(*Refusal)) || len(h.refused) != 0 {
		t.Fatalf("the write got %v, refused %v; want the desktop's error and no refusal", err, h.refused)
	}
}

// TestCheckBrowserFramesRefusesAHeldFrameAtOnce: before the desktop's first frame, while the filter holds the
// browser's bytes, a bad frame is refused when it is written, and nothing of it is held.
func TestCheckBrowserFramesRefusesAHeldFrameAtOnce(t *testing.T) {
	h := newHarness(t, Rules{}, false)
	w := h.c.CheckBrowserFrames()
	good := clientFrame(0x82, []byte("RFB 003.008\n"))
	if _, err := w.Write(good); err != nil {
		t.Fatalf("the browser's version, held: %v", err)
	}
	_, err := w.Write(appendFrame(nil, frame{b0: 0x82}, []byte("RFB 003.008\n")))
	if !errors.As(err, new(*Refusal)) || len(h.refused) != 1 || !bytes.Equal(h.c.held, good) {
		t.Fatalf("the unmasked frame got %v (refused %v, held %x), want it refused at once and nothing of it held", err, h.refused, h.c.held)
	}
}
