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

// relayHandshake runs rfbHello through c, the desktop d's side and the browser's answers (masked by mask).
func relayHandshake(t *testing.T, c *rfbfilter.Conn, d *desk, mask []byte) {
	t.Helper()
	hello := rfbHello() // the server's version, security types, security result and ServerInit
	for i, msg := range [][]byte{hello[:12], []byte(rfbVersion8), hello[12:14], {1}, hello[14:18], {1}, hello[18:]} {
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
		relayHandshake(t, c, d, mask)
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
			relayHandshake(t, c, d, mask)
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
