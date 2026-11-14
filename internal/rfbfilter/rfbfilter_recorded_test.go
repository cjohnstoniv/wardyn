// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbfilter

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
)

func setPixelFormat(bits byte) []byte {
	return cat([]byte{0, 0, 0, 0, bits, 8, 0, 1}, be16(255), be16(255), be16(255), []byte{16, 8, 0, 0, 0, 0})
}

// setPixelFormatOf is a SetPixelFormat of the 13 format bytes f and 3 bytes of padding.
func setPixelFormatOf(f []byte) []byte { return cat([]byte{0, 0, 0, 0}, f, []byte{0, 0, 0}) }

func serverInitAt(bits byte) []byte {
	b := serverInit()
	b[4] = bits
	return b
}

// serverInitOf is serverInit with the 13 format bytes f.
func serverInitOf(f []byte) []byte {
	b := serverInit()
	copy(b[4:], f)
	return b
}

// A recorded desktop's recorder frames 32-bit pixels only, and its replay reads them in one layout
// (Rules.Recorded): the browser may not set another format, nor ask for pixels while the desktop's own
// format is another, and either ends the connection before anything reaches the desktop. Every field of
// the format counts. Unrecorded, other formats are followed as before.
func TestRecordedRelayKeepsPixelsInThePlaybackFormat(t *testing.T) {
	request := []byte{3, 1, 0, 0, 0, 0, 7, 128, 4, 56}
	continuous := []byte{150, 1, 0, 0, 0, 0, 7, 128, 4, 56}
	redAt16 := serverInit()[4:17] // 32 bits, depth 24, little-endian true colour, red, green and blue at 16, 8 and 0
	type relayCase struct {
		rules Rules
		init  []byte
		msg   []byte
		pass  bool
	}
	cases := map[string]relayCase{
		"recorded, the browser sets 8 bits":            {Rules{Recorded: true}, serverInitOf(playbackPixels), cat(setPixelFormat(8), request), false},
		"recorded, 8 bits set and nothing asked":       {Rules{Recorded: true}, serverInitOf(playbackPixels), setPixelFormat(8), false},
		"recorded, the browser sets red at shift 16":   {Rules{Recorded: true}, serverInitOf(playbackPixels), setPixelFormatOf(redAt16), false},
		"recorded, the desktop's own 16 bits":          {Rules{Recorded: true}, serverInitAt(16), request, false},
		"recorded, continuous at its 16 bits":          {Rules{Recorded: true}, serverInitAt(16), continuous, false},
		"recorded, the desktop's own red at shift 16":  {Rules{Recorded: true}, serverInit(), request, false},
		"recorded, continuous at its red at shift 16":  {Rules{Recorded: true}, serverInit(), continuous, false},
		"recorded, the browser sets the playback form": {Rules{Recorded: true}, serverInitAt(16), cat(setPixelFormatOf(playbackPixels), request), true},
		"recorded, the desktop's own playback format":  {Rules{Recorded: true}, serverInitOf(playbackPixels), request, true},
		// The 20 bytes noVNC and the console's viewer send, written out here and not built from playbackPixels.
		"recorded, noVNC's SetPixelFormat as sent":     {Rules{Recorded: true}, serverInit(), cat([]byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 0, 8, 16, 0, 0, 0}, request), true},
		"recorded, continuous in the playback format":  {Rules{Recorded: true}, serverInitOf(playbackPixels), continuous, true},
		"unrecorded, the browser sets 8 bits":          {Rules{}, serverInit(), cat(setPixelFormat(8), request), true},
		"unrecorded, the browser sets red at shift 16": {Rules{}, serverInitOf(playbackPixels), cat(setPixelFormatOf(redAt16), request), true},
		"unrecorded, the desktop's own 16 bits":        {Rules{}, serverInitAt(16), request, true},
	}
	// One field off the playback format at a time, each refused when recorded. A maximum is two bytes, so
	// each of its bytes is changed alone: 511 and 254.
	for _, f := range []struct {
		name  string
		at    int
		value []byte
	}{
		{"bits per pixel", 0, []byte{16}}, {"depth", 1, []byte{32}}, {"big-endian", 2, []byte{1}},
		{"true colour", 3, []byte{0}}, {"red maximum 511", 4, be16(511)}, {"red maximum 254", 4, be16(254)},
		{"green maximum 511", 6, be16(511)}, {"green maximum 254", 6, be16(254)}, {"blue maximum 511", 8, be16(511)},
		{"blue maximum 254", 8, be16(254)}, {"red shift", 10, []byte{16}}, {"green shift", 11, []byte{0}},
		{"blue shift", 12, []byte{0}},
	} {
		format := slices.Clone(playbackPixels)
		copy(format[f.at:], f.value)
		cases["recorded, the browser sets another "+f.name] = relayCase{Rules{Recorded: true}, serverInitOf(playbackPixels), setPixelFormatOf(format), false}
		cases["recorded, the desktop's own other "+f.name] = relayCase{Rules{Recorded: true}, serverInitOf(format), request, false}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.rules, false)
			h.handshake38With(tc.init)
			h.fb.written = nil
			_, err := h.c.Write(frames(tc.msg, true, false))
			if got := payloads(t, h.fb.written); tc.pass != (err == nil) || tc.pass != bytes.Equal(got, tc.msg) || !tc.pass && len(got) != 0 {
				t.Fatalf("write err %v, desktop got %x; want relayed %v", err, got, tc.pass)
			}
		})
	}
}

// writeFailBackend is a fakeBackend whose writes fail once fail is set.
type writeFailBackend struct {
	*fakeBackend
	fail bool
}

func (b *writeFailBackend) Write(p []byte) (int, error) {
	if b.fail {
		return 0, errors.New("the desktop socket closed")
	}
	return b.fakeBackend.Write(p)
}

// A paste the rules allow is reported as relayed only once its bytes reach the desktop: a frame the relay
// refuses as a whole (an unknown message after the paste) forwards nothing and reports no relayed paste,
// and neither does a paste whose write to the desktop fails.
func TestAnAllowedPasteIsReportedOnlyOnceWritten(t *testing.T) {
	paste := cutText(6, "pasted text")
	for name, tc := range map[string]struct {
		msg       []byte
		failWrite bool
	}{
		"refused frame": {msg: cat(paste, []byte{99})},
		"failed write":  {msg: paste, failWrite: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := &harness{t: t, fb: &fakeBackend{}}
			backend := &writeFailBackend{fakeBackend: h.fb}
			h.c = Wrap(backend, Rules{PasteIn: true}, func(ev Event) { h.events = append(h.events, ev) }, func(err error) { h.refused = append(h.refused, err) })
			h.handshake38()
			h.fb.written, backend.fail = nil, tc.failWrite
			if _, err := h.c.Write(frames(tc.msg, true, false)); err == nil {
				t.Fatal("the write succeeded")
			}
			if len(h.fb.written) != 0 || slices.ContainsFunc(h.events, func(ev Event) bool { return ev.Allowed }) {
				t.Fatalf("desktop got %x, events %+v; want nothing relayed and no relayed paste", h.fb.written, h.events)
			}
		})
	}
	// The same paste, written, is reported as relayed.
	h := newHarness(t, Rules{PasteIn: true}, false)
	h.handshake38()
	if got := h.client(paste); !bytes.Equal(got, paste) || len(h.events) != 1 || !h.events[0].Allowed {
		t.Fatalf("desktop got %x, events %+v; want the paste relayed and reported", got, h.events)
	}
}

// A message the relay refuses as a whole reports no cut text as relayed, in either direction, and still
// reports the cut texts the rules blocked in it: those were denied whatever became of the message.
func TestARefusedMessageReportsOnlyItsRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		rules      Rules
		fromServer bool
		want       []Event
	}{
		"allowed copy out": {Rules{CopyOut: true}, true, nil},
		"blocked copy out": {Rules{}, true, []Event{{Direction: CopyOut, Bytes: 6}}},
		"blocked paste in": {Rules{}, false, []Event{{Direction: PasteIn, Bytes: 6}}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.rules, false)
			h.handshake38()
			h.fb.written = nil
			var got []byte
			var err error
			if tc.fromServer {
				h.fb.chunks = append(h.fb.chunks, frames(cat(cutText(3, "copied"), []byte{99}), false, false))
				got, err = io.ReadAll(h.c)
			} else {
				_, err = h.c.Write(frames(cat(cutText(6, "pasted"), []byte{99}), true, false))
			}
			if err == nil || len(got) != 0 || len(h.fb.written) != 0 || fmt.Sprint(h.events) != fmt.Sprint(tc.want) {
				t.Fatalf("err %v, browser got %x, desktop got %x, events %+v; want a refusal, nothing relayed and events %+v", err, got, h.fb.written, h.events, tc.want)
			}
		})
	}
}
