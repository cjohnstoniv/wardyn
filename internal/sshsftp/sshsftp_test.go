// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sshsftp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// packet is one sftp packet: its length, its type, then n filler bytes after the type.
func packet(typ byte, n int) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(1+n))
	b = append(b, typ)
	return append(b, bytes.Repeat([]byte{typ}, n)...)
}

// chunked serves b in reads of at most n bytes.
type chunked struct {
	b []byte
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	k := min(len(p), c.n, len(c.b))
	copy(p, c.b[:k])
	c.b = c.b[k:]
	return k, nil
}

func readAll(g *ReplyGuard) ([]byte, error) {
	var out []byte
	buf := make([]byte, 7)
	for {
		n, err := g.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
	}
}

// TestSFTPReplyGuardStopsData: a DATA reply split across reads stops before any of its bytes pass, and the
// replies before it pass whole.
func TestSFTPReplyGuardStopsData(t *testing.T) {
	before := append(append(packet(104, 9), packet(105, 3)...), packet(101, 20)...) // NAME, ATTRS, STATUS
	stream := append(append(append([]byte(nil), before...), packet(TypeData, 40)...), packet(104, 4)...)
	for _, n := range []int{1, 2, 3, 4, 5, 6, 11, 4096} {
		fired := 0
		g := &ReplyGuard{R: &chunked{b: stream, n: n}, OnData: func() { fired++ }}
		got, err := readAll(g)
		if !errors.Is(err, ErrData) {
			t.Fatalf("reads of %d: err = %v, want ErrData", n, err)
		}
		if !bytes.Equal(got, before) {
			t.Errorf("reads of %d: passed %d bytes, want exactly the %d bytes before the DATA reply", n, len(got), len(before))
		}
		if fired != 1 {
			t.Errorf("reads of %d: OnData fired %d times, want once", n, fired)
		}
		if _, err := g.Read(make([]byte, 4)); !errors.Is(err, ErrData) {
			t.Errorf("reads of %d: a later Read = %v, want ErrData again", n, err)
		}
	}
}

// TestSFTPReplyGuardPassesEveryOtherReply: NAME, ATTRS, STATUS and HANDLE replies, and an empty packet, pass
// unchanged, and the end of the stream ends the guard with io.EOF.
func TestSFTPReplyGuardPassesEveryOtherReply(t *testing.T) {
	var stream []byte
	for _, typ := range []byte{101, 102, 104, 105} {
		stream = append(stream, packet(typ, 30)...)
	}
	stream = append(stream, 0, 0, 0, 0)
	stream = append(stream, packet(99, 0)...)
	for _, n := range []int{1, 3, 5, 4096} {
		got, err := readAll(&ReplyGuard{R: &chunked{b: stream, n: n}, OnData: func() { t.Error("OnData fired for a reply that is not DATA") }})
		if !errors.Is(err, io.EOF) || !bytes.Equal(got, stream) {
			t.Errorf("reads of %d: got %d bytes, err %v; want all %d bytes and io.EOF", n, len(got), err, len(stream))
		}
	}
}

// TestFramerPieces: Feed hands each packet's bytes over once, with the head capped at peek and done on the last.
func TestFramerPieces(t *testing.T) {
	stream := append(append(packet(13, 10), 0, 0, 0, 0), packet(18, 2)...)
	var f Framer
	var all []byte
	var done []string
	for i := 0; i < len(stream); i += 3 {
		f.Feed(stream[i:min(i+3, len(stream))], 6, func(b, head []byte, last bool) {
			all = append(all, b...)
			if len(head) > 6 && len(head) > LengthSize {
				t.Errorf("head of %d bytes, want at most 6", len(head))
			}
			if last {
				done = append(done, string(rune('0'+len(head))))
			}
		})
	}
	if !bytes.Equal(all, stream) {
		t.Errorf("pieces held %d bytes, want the stream's %d", len(all), len(stream))
	}
	if len(done) != 3 {
		t.Errorf("%d packets ended, want 3 (two packets and an empty one)", len(done))
	}
}
