// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/rfbfilter"
)

// desk is a desktop's side of the relay: Read hands out what the test queued and Write keeps what reached
// it. A closed desk reads nothing more, as a closed connection does.
type desk struct {
	chunks  [][]byte
	written int
	closed  bool
}

func (d *desk) Read(p []byte) (int, error) {
	if d.closed {
		return 0, io.ErrClosedPipe
	}
	if len(d.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, d.chunks[0])
	if d.chunks[0] = d.chunks[0][n:]; len(d.chunks[0]) == 0 {
		d.chunks = d.chunks[1:]
	}
	return n, nil
}

func (d *desk) Write(p []byte) (int, error) { d.written += len(p); return len(p), nil }
func (d *desk) Close() error                { d.closed = true; return nil }

// relayHandshake runs hello (rfbHello, or its start) through c, the desktop d's side and the browser's
// answers (masked by mask).
func relayHandshake(t *testing.T, c *rfbfilter.Conn, d *desk, mask, hello []byte) {
	t.Helper()
	// the server's version, security types, security result and ServerInit
	for i, msg := range [][]byte{hello[:12], []byte(rfbVersion8), hello[12:14], {1}, hello[14:18], {1}, hello[18:]} {
		if len(msg) == 0 {
			break // hello ends before ServerInit
		}
		if i%2 == 1 { // the browser's version, security type and ClientInit
			if _, err := c.Write(wsFrame(true, 2, msg, mask)); err != nil {
				t.Fatalf("handshake write: %v", err)
			}
			continue
		}
		d.chunks = append(d.chunks, wsFrame(true, 2, msg, nil))
		if _, err := io.ReadAll(c); err != nil {
			t.Fatalf("handshake read: %v", err)
		}
	}
}

// TestARecordedDesktopNeverKeepsTheClipboardAsPixels runs the relay order of a recorded desktop, the
// filter (rfbfilter.Wrap) over the recorder's ReadSide over the desktop. The recorder frames 32-bit pixels
// only, so the filter refuses the browser's ask for 8 before the desktop hears it, and an 8-bit Raw
// rectangle with the desktop's clipboard after it is never kept. Unrecorded, the same ask reaches the
// desktop and a recorder fed what follows keeps the clipboard, so the stream here is one that would leak.
func TestARecordedDesktopNeverKeepsTheClipboardAsPixels(t *testing.T) {
	mask := []byte{1, 2, 3, 4}
	pixelFormat8 := []byte{0, 0, 0, 0, 8, 8, 0, 1, 0, 7, 0, 7, 0, 3, 5, 2, 0, 0, 0, 0}
	for _, recorded := range []bool{true, false} {
		d := &desk{}
		r := NewRFBFrameRecorder(time.Now)
		c := rfbfilter.Wrap(ReadSide(d, r, func() {}), rfbfilter.Rules{CopyOut: true, Recorded: recorded}, func(rfbfilter.Event) {}, func(error) {})
		relayHandshake(t, c, d, mask, rfbHello())
		before := d.written
		_, err := c.Write(wsFrame(true, 2, pixelFormat8, mask))
		if heard := d.written > before; recorded == (err == nil) || heard == recorded {
			t.Fatalf("recorded=%v: SetPixelFormat 8 bits: err %v, reached the desktop %v", recorded, err, heard)
		}
		d.chunks = append(d.chunks, wsFrame(true, 2, cat(fbu(1, append(rect(8, 1, 0), make([]byte, 8)...)), cutText("hunter2", 7)), nil),
			wsFrame(true, 2, bytes.Repeat(bell, 16), nil))
		_, _ = io.ReadAll(c)
		if leaked := bytes.Contains(kept(t, playback(t, r)), []byte("hunter2")); leaked == recorded {
			t.Fatalf("recorded=%v: the recording kept the clipboard: %v", recorded, leaked)
		}
	}
}

// TestARefusedPixelFormatEndsTheRecordingWithAStopMark runs that relay order with the refusal handler the
// desktop doors give the filter: the relay's refusal names the format it refused (PixelFormatError), and Stop
// leaves a recording whose last record is the stop mark, with the reason the door gives it. Nothing the
// desktop sends after the refusal is kept. A SetPixelFormat the recorder cannot frame, and a request for
// pixels in ServerInit's own format when that is not the replay's (rfbHello's has no colour maxima), each
// end so.
func TestARefusedPixelFormatEndsTheRecordingWithAStopMark(t *testing.T) {
	mask := []byte{1, 2, 3, 4}
	for name, tc := range map[string]struct {
		msg     []byte
		refused string
	}{
		"SetPixelFormat": {[]byte{0, 0, 0, 0, 16, 16, 1, 1, 0, 31, 0, 63, 0, 31, 11, 5, 0, 0, 0, 0},
			"16 bits per pixel, depth 16, big-endian true colour, maxima of 31, 63 and 31 and shifts of 11, 5 and 0 for red, green and blue"},
		"ServerInit": {[]byte{3, 0, 0, 0, 0, 0, 4, 0, 3, 0},
			"32 bits per pixel, depth 24, little-endian true colour, maxima of 0, 0 and 0 and shifts of 0, 0 and 0 for red, green and blue"},
	} {
		t.Run(name, func(t *testing.T) {
			at := time.Unix(1, 0)
			d, r := &desk{}, NewRFBFrameRecorder(func() time.Time { return at })
			var refusal *rfbfilter.PixelFormatError
			c := rfbfilter.Wrap(ReadSide(d, r, func() {}), rfbfilter.Rules{Recorded: true}, func(rfbfilter.Event) {}, func(err error) {
				if errors.As(err, &refusal) {
					r.Stop("refused: " + refusal.Refused())
				}
			})
			relayHandshake(t, c, d, mask, rfbHello())
			at = at.Add(time.Second)
			d.chunks = append(d.chunks, wsFrame(true, 2, bell, nil))
			_, _ = io.ReadAll(c)
			at = at.Add(time.Second)
			if _, err := c.Write(wsFrame(true, 2, tc.msg, mask)); err == nil || refusal == nil {
				t.Fatalf("%x on a recorded desktop: err %v, want a PixelFormatError", tc.msg, err)
			}
			if got := refusal.Refused(); got != tc.refused {
				t.Fatalf("the refusal names %q, want %q", got, tc.refused)
			}
			at = at.Add(time.Second)
			_, _ = r.Write(wsFrame(true, 2, bell, nil)) // a Bell the desktop sent as the relay closed
			pb := playback(t, r)
			if pb.StoppedMS != 2000 || pb.StopReason != "refused: "+tc.refused {
				t.Fatalf("stop mark at %d ms for %q, want at 2000 ms for %q", pb.StoppedMS, pb.StopReason, "refused: "+tc.refused)
			}
			for _, f := range pb.Frames {
				ms, _, _ := strings.Cut(f[1:], "{")
				if when, err := strconv.ParseInt(ms, 10, 64); err != nil || when > pb.StoppedMS {
					t.Fatalf("frame %q comes after the stop mark", f)
				}
			}
			if got, want := kept(t, pb), cat(rfbHello(), bell); !bytes.Equal(got, want) {
				t.Fatalf("kept %x, want the handshake and the Bell before the refusal %x", got, want)
			}
		})
	}
}

// TestAServerInitSizeTheRelayRefusesEndsTheRecordingWithAStopMark: a desktop whose ServerInit names a pixel
// size the relay does not take (not 8, 16 or 32 bits) is refused as it arrives. On a recorded desktop that
// refusal names the format too, so the recording ends with the stop mark; unrecorded, it stays the relay's
// plain refusal.
func TestAServerInitSizeTheRelayRefusesEndsTheRecordingWithAStopMark(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		at := time.Unix(1, 0)
		d, r := &desk{}, NewRFBFrameRecorder(func() time.Time { return at })
		var refusal *rfbfilter.PixelFormatError
		c := rfbfilter.Wrap(ReadSide(d, r, func() {}), rfbfilter.Rules{Recorded: recorded}, func(rfbfilter.Event) {}, func(err error) {
			if errors.As(err, &refusal) {
				r.Stop("refused: " + refusal.Refused())
			}
		})
		hello := rfbHello()
		hello[18+4] = 24 // ServerInit's bits per pixel
		relayHandshake(t, c, d, []byte{1, 2, 3, 4}, hello[:18])
		at = at.Add(time.Second)
		d.chunks = append(d.chunks, wsFrame(true, 2, hello[18:], nil))
		if _, err := io.ReadAll(c); err == nil || (refusal != nil) != recorded {
			t.Fatalf("recorded=%v: ServerInit of 24 bits per pixel: err %v, PixelFormatError %v", recorded, err, refusal != nil)
		}
		pb, want := playback(t, r), "refused: 24 bits per pixel, depth 24, little-endian true colour, maxima of 0, 0 and 0 and shifts of 0, 0 and 0 for red, green and blue"
		if !recorded {
			want = ""
		}
		if (pb.StoppedMS == 1000) != recorded || pb.StopReason != want {
			t.Fatalf("recorded=%v: stop mark at %d ms for %q, want %q", recorded, pb.StoppedMS, pb.StopReason, want)
		}
	}
}

// TestStopKeepsTheFirstStopMark: a recording the recorder stopped itself, at a message it could not follow,
// keeps that stop mark when the relay stops it later. No frame after the first mark was kept, so a later
// mark would read as frames kept up to it. A stop in the recording's first millisecond is still a mark.
func TestStopKeepsTheFirstStopMark(t *testing.T) {
	at := time.Unix(1, 0)
	r := NewRFBFrameRecorder(func() time.Time { return at })
	_, _ = r.Write(wsFrame(true, 2, rfbHello(), nil))
	at = at.Add(time.Second)
	_, _ = r.Write(wsFrame(true, 2, []byte{99}, nil)) // a server message the recorder cannot follow
	at = at.Add(time.Second)
	r.Stop("later")
	if pb := playback(t, r); pb.StoppedMS != 1000 || pb.StopReason != "" || r.StoppedMS() != 1000 {
		t.Fatalf("stop mark at %d ms for %q, want the recorder's own at 1000 ms", pb.StoppedMS, pb.StopReason)
	}
	first := NewRFBFrameRecorder(func() time.Time { return at })
	first.Stop("at once")
	at = at.Add(time.Second)
	first.Stop("later")
	if pb := playback(t, first); pb.StoppedMS != 1 || pb.StopReason != "at once" {
		t.Fatalf("stop mark at %d ms for %q, want 1 ms for the first stop", pb.StoppedMS, pb.StopReason)
	}
}

// TestEachRefusalEndsTheRecordingWithItsStopMark runs the relay order with a stand-in for the desktop doors'
// refusal handler (stopRefusedRecording, in internal/api), for every refusal but a pixel format's: each ends the
// recording with a stop mark in the fixed words of what the relay refused (rfbfilter.Refusal), at the refusal,
// after frames the recording keeps. The two connection setup cases have a desktop that sent ahead, which the
// recorder read before the relay did. A desktop message the recorder cannot follow either keeps the recorder's own
// stop, which has no reason.
func TestEachRefusalEndsTheRecordingWithItsStopMark(t *testing.T) {
	type step = func(*rfbfilter.Conn, *desk, *RFBRecorder) error
	mask := []byte{1, 2, 3, 4}
	browser := func(msg []byte, op byte) step {
		return func(c *rfbfilter.Conn, _ *desk, _ *RFBRecorder) error {
			_, err := c.Write(wsFrame(true, op, msg, mask))
			return err
		}
	}
	desktop := func(msg []byte, op byte) step {
		return func(c *rfbfilter.Conn, d *desk, _ *RFBRecorder) error {
			d.chunks = append(d.chunks, wsFrame(true, op, msg, nil))
			_, err := io.ReadAll(c)
			return err
		}
	}
	for name, tc := range map[string]struct {
		viewOnly, setup bool // setup: the case runs the connection setup itself
		refuse          step
		reason          string
	}{
		"a browser frame":    {refuse: browser([]byte("x"), 1), reason: "a WebSocket frame from the browser that it cannot read"},
		"a desktop frame":    {refuse: desktop(bell, 0x42), reason: "a WebSocket frame from the desktop that it cannot read"},
		"a browser message":  {refuse: browser([]byte{7}, 2), reason: "a message from the browser that it cannot follow"},
		"a desktop message":  {refuse: desktop(fbu(1, rect(1, 1, 5), []byte{1}, make([]byte, 4)), 2), reason: "a message from the desktop that it cannot follow"},
		"a view-only action": {viewOnly: true, refuse: browser([]byte{251}, 2), reason: "a control message from the browser on a view-only desktop"},
		"the desktop's setup": {setup: true, refuse: desktop(cat(rfbHello(), bell), 2),
			reason: "the desktop's connection setup, which it cannot follow"},
		"the browser's setup": {setup: true, refuse: func(c *rfbfilter.Conn, d *desk, r *RFBRecorder) error {
			_ = desktop(rfbHello()[:12], 2)(c, d, r)
			_, _ = r.Write(wsFrame(true, 2, cat(rfbHello()[12:], bell), nil)) // read by the recorder, not yet by the relay
			return browser([]byte("RFB 004.000\n"), 2)(c, d, r)
		}, reason: "the browser's connection setup, which it cannot follow"},
		"a desktop message the recorder cannot follow": {refuse: desktop([]byte{99}, 2)},
	} {
		t.Run(name, func(t *testing.T) {
			at := time.Unix(1, 0)
			d, r := &desk{}, NewRFBFrameRecorder(func() time.Time { return at })
			var refusal *rfbfilter.Refusal
			c := rfbfilter.Wrap(ReadSide(d, r, func() {}), rfbfilter.Rules{ViewOnly: tc.viewOnly, Recorded: true}, func(rfbfilter.Event) {}, func(err error) {
				if errors.As(err, &refusal) {
					r.Stop("refused: " + refusal.Kind)
				}
			})
			if at = at.Add(time.Second); !tc.setup {
				relayHandshake(t, c, d, mask, rfbHello())
				d.chunks = append(d.chunks, wsFrame(true, 2, bell, nil))
				_, _ = io.ReadAll(c)
			}
			at = at.Add(time.Second)
			if err := tc.refuse(c, d, r); err == nil || refusal == nil {
				t.Fatalf("err %v, refusal %v: want the relay to refuse and name what it refused", err, refusal)
			}
			want := ""
			if tc.reason != "" {
				want = "refused: " + tc.reason
			}
			pb := playback(t, r)
			if _, had := r.Playback(); !had || pb.StoppedMS != 2000 || pb.StopReason != want {
				t.Fatalf("frames kept %v, stop mark at %d ms for %q: want frames and the mark at 2000 ms for %q", had, pb.StoppedMS, pb.StopReason, want)
			}
		})
	}
}

// TestARefusalBeforeTheDesktopsFirstMessageStoresNothing: a desktop whose first message is not RFB, and a browser
// that sends too much before the desktop's first message, are refused with no Refusal kind, and no recording is
// stored, since none kept a frame.
func TestARefusalBeforeTheDesktopsFirstMessageStoresNothing(t *testing.T) {
	for name, refuse := range map[string]func(*rfbfilter.Conn, *desk) error{
		"not RFB": func(c *rfbfilter.Conn, d *desk) error {
			d.chunks = append(d.chunks, wsFrame(true, 2, []byte(`{"hello":1}`), nil), wsFrame(true, 2, cat(rfbHello(), bell), nil))
			_, err := io.ReadAll(c)
			return err
		},
		"too much held": func(c *rfbfilter.Conn, _ *desk) error { _, err := c.Write(make([]byte, 64<<10+1)); return err },
	} {
		t.Run(name, func(t *testing.T) {
			d, r := &desk{}, NewRFBFrameRecorder(time.Now)
			var refused error
			c := rfbfilter.Wrap(ReadSide(d, r, func() {}), rfbfilter.Rules{Recorded: true}, func(rfbfilter.Event) {}, func(err error) { refused = err })
			var refusal *rfbfilter.Refusal
			if err := refuse(c, d); err == nil || refused == nil || errors.As(refused, &refusal) {
				t.Fatalf("err %v, refused %v: want a refusal with no Refusal kind", err, refused)
			}
			if _, had := r.Playback(); had {
				t.Fatal("a recording refused before the desktop's first message kept a frame, so it would be stored")
			}
		})
	}
}
