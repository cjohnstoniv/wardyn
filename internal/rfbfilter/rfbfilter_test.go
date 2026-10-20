// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbfilter

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeBackend is the sandbox side: Read hands out what the test queued, in the
// chunk sizes it queued them; Write records what the relay forwarded.
type fakeBackend struct {
	chunks  [][]byte
	written []byte
}

func (f *fakeBackend) Read(p []byte) (int, error) {
	if len(f.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, f.chunks[0])
	if f.chunks[0] = f.chunks[0][n:]; len(f.chunks[0]) == 0 {
		f.chunks = f.chunks[1:]
	}
	return n, nil
}

func (f *fakeBackend) Write(p []byte) (int, error) {
	f.written = append(f.written, p...)
	return len(p), nil
}
func (f *fakeBackend) Close() error { return nil }

type harness struct {
	t       *testing.T
	fb      *fakeBackend
	c       *Conn
	events  []Event
	refused []error
	split   bool // deliver every frame in 1-byte reads/writes, and every message in two frames
}

func newHarness(t *testing.T, rules Rules, split bool) *harness {
	h := &harness{t: t, fb: &fakeBackend{}, split: split}
	h.c = Wrap(h.fb, rules, func(ev Event) { h.events = append(h.events, ev) }, func(err error) { h.refused = append(h.refused, err) })
	return h
}

func frames(payload []byte, masked, split bool) []byte {
	key := [4]byte{0x11, 0x22, 0x33, 0x44}
	if !split || len(payload) < 2 {
		return appendFrame(nil, frame{b0: 0x82, masked: masked, key: key}, payload)
	}
	half := len(payload) / 2
	out := appendFrame(nil, frame{b0: 0x02, masked: masked, key: key}, payload[:half])
	return appendFrame(out, frame{b0: 0x80, masked: masked, key: key}, payload[half:])
}

func payloads(t *testing.T, b []byte) []byte {
	t.Helper()
	var out []byte
	for len(b) > 0 {
		f, n, err := readFrame(b)
		if err != nil || n == 0 {
			t.Fatalf("relay emitted a broken frame stream (err=%v, %d bytes left)", err, len(b))
		}
		out, b = append(out, f.payload...), b[n:]
	}
	return out
}

// server sends payload from the desktop and returns what the browser receives.
func (h *harness) server(payload []byte) []byte {
	h.t.Helper()
	wire := frames(payload, false, h.split)
	if h.split {
		for i := range wire {
			h.fb.chunks = append(h.fb.chunks, wire[i:i+1])
		}
	} else {
		h.fb.chunks = append(h.fb.chunks, wire)
	}
	got, err := io.ReadAll(h.c)
	if err != nil {
		h.t.Fatalf("relay read: %v", err)
	}
	return payloads(h.t, got)
}

// client sends payload from the browser and returns what reaches the desktop.
func (h *harness) client(payload []byte) []byte {
	h.t.Helper()
	h.fb.written = nil
	wire := frames(payload, true, h.split)
	step := len(wire)
	if h.split {
		step = 1
	}
	for i := 0; i < len(wire); i += step {
		if _, err := h.c.Write(wire[i:min(i+step, len(wire))]); err != nil {
			h.t.Fatalf("relay write: %v", err)
		}
	}
	return payloads(h.t, h.fb.written)
}

func be32(v int32) []byte  { return binary.BigEndian.AppendUint32(nil, uint32(v)) }
func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func cat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func serverInit() []byte {
	pf := []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	return cat(be16(1280), be16(800), pf, be32(4), []byte("desk"))
}

func cutText(typ byte, text string) []byte {
	return cat([]byte{typ, 0, 0, 0}, be32(int32(len(text))), []byte(text))
}

// handshake38 runs an RFB 3.8, security None session up to the normal phase.
func (h *harness) handshake38() {
	h.t.Helper()
	steps := []struct {
		fromServer bool
		msg        []byte
	}{
		{true, []byte("RFB 003.008\n")},
		{false, []byte("RFB 003.008\n")},
		{true, []byte{1, 1}},
		{false, []byte{1}},
		{true, be32(0)},
		{false, []byte{1}},
		{true, serverInit()},
	}
	for _, s := range steps {
		var got []byte
		if s.fromServer {
			got = h.server(s.msg)
		} else {
			got = h.client(s.msg)
		}
		if !bytes.Equal(got, s.msg) {
			h.t.Fatalf("handshake step altered: sent %x, relayed %x", s.msg, got)
		}
	}
}

func sum(s string) string { d := sha256.Sum256([]byte(s)); return hex.EncodeToString(d[:]) }

// The lane's gate: with the default rules (paste in allowed, copy out blocked)
// the desktop's cut text never reaches the browser and is audited by direction,
// size and hash; the browser's paste goes through and is audited too. Split
// reads and fragmented messages must not change a byte of the outcome.
func TestBlockedCopyOutIsDroppedAndAudited(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%v", split), func(t *testing.T) {
			h := newHarness(t, Rules{PasteIn: true}, split)
			h.handshake38()

			// Raw pixels holding a 0x03 byte, a ZRLE rectangle and a Bell must pass
			// untouched around the dropped ServerCutText.
			update := cat([]byte{0, 0}, be16(2),
				be16(0), be16(0), be16(2), be16(1), be32(encRaw), []byte{3, 0, 0, 0, 3, 0, 0, 8},
				be16(0), be16(0), be16(4), be16(4), be32(encZRLE), be32(5), []byte{3, 3, 3, 3, 3})
			got := h.server(cat(update, cutText(3, "TOP-SECRET-COPY"), []byte{2}))
			if want := cat(update, []byte{2}); !bytes.Equal(got, want) {
				t.Fatalf("browser got %x, want %x (the cut text must be gone and nothing else)", got, want)
			}
			paste := cutText(6, "pasted text")
			if got := h.client(paste); !bytes.Equal(got, paste) {
				t.Fatalf("an allowed paste was altered: %x", got)
			}
			want := []Event{
				{Direction: CopyOut, Bytes: 15, SHA256: sum("TOP-SECRET-COPY"), Allowed: false},
				{Direction: PasteIn, Bytes: 11, SHA256: sum("pasted text"), Allowed: true},
			}
			if fmt.Sprint(h.events) != fmt.Sprint(want) {
				t.Fatalf("audit events = %+v, want %+v", h.events, want)
			}
		})
	}
}

func TestBlockedPasteInIsDroppedAndAudited(t *testing.T) {
	h := newHarness(t, Rules{CopyOut: true}, true)
	h.handshake38()
	key := cat([]byte{4, 1, 0, 0}, be32(0x61))
	if got := h.client(cat(cutText(6, "do not paste"), key)); !bytes.Equal(got, key) {
		t.Fatalf("desktop got %x, want only the key event %x", got, key)
	}
	copied := cutText(3, "copy me")
	if got := h.server(copied); !bytes.Equal(got, copied) {
		t.Fatalf("an allowed copy out was altered: %x", got)
	}
	if len(h.events) != 2 || h.events[0].Direction != PasteIn || h.events[0].Allowed || !h.events[1].Allowed {
		t.Fatalf("audit events = %+v", h.events)
	}
}

// SetEncodings comes out holding only what the relay can frame: Tight, the
// Extended Clipboard and Fence pseudo-encodings are gone.
func TestSetEncodingsKeepsOnlyRelayableEncodings(t *testing.T) {
	h := newHarness(t, Rules{}, false)
	h.handshake38()
	list := []int32{7, encZRLE, encRaw, encCursor, -1063131698, -312, -23}
	msg := cat([]byte{2, 0}, be16(uint16(len(list))))
	for _, e := range list {
		msg = append(msg, be32(e)...)
	}
	want := cat([]byte{2, 0}, be16(4), be32(encZRLE), be32(encRaw), be32(encCursor), be32(-23))
	if got := h.client(msg); !bytes.Equal(got, want) {
		t.Fatalf("SetEncodings relayed as %x, want %x", got, want)
	}
}

// RFB 3.3 with VNC authentication: the server picks the type and the relay
// must still find the normal phase.
func TestVersion33VNCAuth(t *testing.T) {
	h := newHarness(t, Rules{}, true)
	h.server([]byte("RFB 003.003\n"))
	h.client([]byte("RFB 003.003\n"))
	h.server(cat(be32(2), bytes.Repeat([]byte{9}, 16)))
	h.client(bytes.Repeat([]byte{7}, 16))
	h.server(be32(0))
	h.client([]byte{1})
	h.server(serverInit())
	if got := h.server(cutText(3, "secret")); len(got) != 0 {
		t.Fatalf("blocked copy out reached the browser: %x", got)
	}
	if len(h.events) != 1 || h.events[0].Bytes != 6 {
		t.Fatalf("audit events = %+v", h.events)
	}
}

// A desktop socket is RFB or it closes: a server whose first data frame is not an RFB
// ProtocolVersion, even one that sends RFB after a short prefix, ends it before a byte reaches
// the browser.
func TestServerNotSpeakingRFBClosesTheSocket(t *testing.T) {
	for name, first := range map[string][]byte{"json": []byte(`{"hello":1}`), "one-byte prefix": []byte("X")} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Rules{}, false)
			h.fb.chunks = append(h.fb.chunks, frames(first, false, false),
				frames([]byte("RFB 003.008\n"), false, false), frames(cutText(3, "SECRET-OUT"), false, false))
			got, err := io.ReadAll(h.c)
			if !errors.Is(err, ErrNotRFB) || len(got) != 0 || len(h.refused) != 1 {
				t.Fatalf("a non-RFB desktop socket was relayed: %q (err %v, %d refusals)", got, err, len(h.refused))
			}
		})
	}
}

// The server speaks first in RFB, so browser frames that arrive before it are held, then parsed
// in order: they cannot switch the filter off, and a blocked copy out after them is still dropped.
func TestBrowserFramesBeforeTheServerAreHeldThenFiltered(t *testing.T) {
	h := newHarness(t, Rules{}, false)
	if _, err := h.c.Write(frames([]byte("RFB 003.008\n"), true, false)); err != nil {
		t.Fatal(err)
	}
	if len(h.fb.written) != 0 {
		t.Fatalf("the desktop got browser bytes before it spoke: %x", h.fb.written)
	}
	if got := h.server([]byte("RFB 003.008\n")); string(got) != "RFB 003.008\n" {
		t.Fatalf("server version: %q", got)
	}
	if got := payloads(t, h.fb.written); string(got) != "RFB 003.008\n" {
		t.Fatalf("the held browser version did not follow the server's: %q", got)
	}
	h.server([]byte{1, 1})
	h.client([]byte{1})
	h.server(be32(0))
	h.client([]byte{1})
	h.server(serverInit())
	if got := h.server(cutText(3, "SECRET-OUT")); bytes.Contains(got, []byte("SECRET-OUT")) {
		t.Fatalf("a blocked copy out reached the browser after a client-first frame: %q", got)
	}
	if len(h.events) != 1 || h.events[0].Allowed {
		t.Fatalf("events = %+v", h.events)
	}
}

// The hold is bounded by size and by time; past either the socket closes.
func TestTheHoldIsBounded(t *testing.T) {
	h := newHarness(t, Rules{}, false)
	if _, err := h.c.Write(make([]byte, maxHeld+1)); err == nil || len(h.refused) != 1 {
		t.Fatalf("the relay held %d bytes (err %v)", maxHeld+1, err)
	}

	defer func(d time.Duration) { holdFor = d }(holdFor)
	holdFor = 10 * time.Millisecond
	refused := make(chan error, 1)
	c := Wrap(&fakeBackend{}, Rules{}, func(Event) {}, func(err error) { refused <- err })
	if _, err := c.Write(frames([]byte("RFB 003.008\n"), true, false)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refused:
	case <-time.After(5 * time.Second):
		t.Fatal("a desktop that never spoke kept the browser's bytes held")
	}
}

// The sink sees the relayed bytes after the filter: an allowed cut text, never a dropped one.
func TestSinkGetsTheFilteredBytes(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		h := newHarness(t, Rules{PasteIn: allowed, CopyOut: allowed}, false)
		var seen []byte
		h.c.SetSink(func(_ bool, b []byte) { seen = append(seen, payloads(t, b)...) })
		h.handshake38()
		h.server(cutText(3, "SENTINEL-OUT"))
		h.client(cutText(6, "SENTINEL-IN"))
		for _, s := range []string{"SENTINEL-OUT", "SENTINEL-IN"} {
			if bytes.Contains(seen, []byte(s)) != allowed {
				t.Fatalf("allowed=%v: the sink saw %s = %v", allowed, s, !allowed)
			}
		}
	}
}

// Anything the relay cannot follow ends the connection.
func TestFailsClosedOnWhatItCannotFollow(t *testing.T) {
	cases := map[string]func(h *harness) error{
		"text frame": func(h *harness) error {
			_, err := h.c.Write(appendFrame(nil, frame{b0: 0x81, masked: true}, []byte("x")))
			return err
		},
		"extension bit": func(h *harness) error {
			_, err := h.c.Write(appendFrame(nil, frame{b0: 0xc2, masked: true}, []byte{4}))
			return err
		},
		"unknown server message": func(h *harness) error { return h.serverErr([]byte{99}) },
		"unframeable encoding": func(h *harness) error {
			return h.serverErr(cat([]byte{0, 0}, be16(1), be16(0), be16(0), be16(1), be16(1), be32(7)))
		},
		"oversized frame": func(h *harness) error {
			return h.serverErr(make([]byte, maxFrame+1))
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Rules{}, false)
			h.handshake38()
			if err := run(h); err == nil || len(h.refused) != 1 {
				t.Fatalf("the relay carried on (err %v, %d refusals reported); it must end the connection and say why once", err, len(h.refused))
			}
		})
	}
	t.Run("encrypting security type", func(t *testing.T) {
		h := newHarness(t, Rules{}, false)
		h.server([]byte("RFB 003.008\n"))
		h.client([]byte("RFB 003.008\n"))
		h.server([]byte{1, 19})
		if _, err := h.c.Write(frames([]byte{19}, true, false)); err == nil {
			t.Fatal("the relay let the browser pick VeNCrypt")
		}
	})
}

func (h *harness) serverErr(payload []byte) error {
	h.fb.chunks = append(h.fb.chunks, frames(payload, false, false))
	_, err := io.ReadAll(h.c)
	return err
}

// Pin: clipboard content never reaches a log line or an audit event, allowed
// or blocked, in either direction.
func TestClipboardContentNeverReachesALogOrAudit(t *testing.T) {
	var logs bytes.Buffer
	prevSlog, prevLog := slog.Default(), log.Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(&logs)
	t.Cleanup(func() { slog.SetDefault(prevSlog); log.SetOutput(prevLog) })

	const sentinel = "SENTINEL-clipboard-7f3a"
	var all []Event
	for _, rules := range []Rules{{}, {PasteIn: true, CopyOut: true}} {
		for _, split := range []bool{false, true} {
			h := newHarness(t, rules, split)
			h.handshake38()
			h.server(cutText(3, sentinel))
			h.client(cutText(6, sentinel))
			all = append(all, h.events...)
		}
	}
	if len(all) != 8 {
		t.Fatalf("want 4 audited transfers, got %+v", all)
	}
	rows, _ := json.Marshal(all)
	for where, text := range map[string]string{"audit": string(rows) + fmt.Sprintf("%+v", all), "log": logs.String()} {
		if strings.Contains(text, sentinel) {
			t.Fatalf("clipboard content reached the %s: %s", where, text)
		}
	}
}
