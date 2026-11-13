// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"io"
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
		hello := rfbHello() // the server's version, security types, security result and ServerInit
		for i, msg := range [][]byte{hello[:12], []byte(rfbVersion8), hello[12:14], {1}, hello[14:18], {1}, hello[18:]} {
			if i%2 == 1 { // the browser's version, security type and ClientInit
				if _, err := c.Write(wsFrame(true, 2, msg, mask)); err != nil {
					t.Fatalf("recorded=%v: handshake write: %v", recorded, err)
				}
				continue
			}
			d.chunks = append(d.chunks, wsFrame(true, 2, msg, nil))
			if _, err := io.ReadAll(c); err != nil {
				t.Fatalf("recorded=%v: handshake read: %v", recorded, err)
			}
		}
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
