// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbfilter

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"
	"time"

	"github.com/coder/websocket"
)

// clientFrame is a browser's frame: b0 and payload, masked.
func clientFrame(b0 byte, payload []byte) []byte {
	return appendFrame(nil, frame{b0: b0, masked: true, key: [4]byte{1, 2, 3, 4}}, payload)
}

func closeFrame(code uint16, reason string) []byte {
	return clientFrame(0x88, append(binary.BigEndian.AppendUint16(nil, code), reason...))
}

// libraryRead hands stream to coder/websocket as the VM door accepts a viewer (no extension, the binary
// subprotocol, no read limit) and reads it as the door's relay does, each message to its end, until the library
// returns an error. With drop the browser's side of the TCP connection then closes.
func libraryRead(t *testing.T, stream []byte, drop bool) error {
	t.Helper()
	got := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			got <- err
			return
		}
		_ = websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		for {
			_, msg, err := ws.Reader(r.Context())
			if err == nil {
				_, err = io.Copy(io.Discard, msg)
			}
			if err != nil {
				got <- err
				return
			}
		}
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: wardyn\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Protocol: binary\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade got %v (err %v)", resp, err)
	}
	if _, err := conn.Write(stream); err != nil {
		t.Fatal(err)
	}
	if drop {
		_ = conn.(*net.TCPConn).CloseWrite()
	}
	select {
	case err := <-got:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the library read on, neither refusing the stream nor ending it")
		return nil
	}
}

// countingReader counts the reads of r.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) { c.reads++; return c.r.Read(p) }

// TestClientFramesRefuseWhatTheWebSocketLibraryRefuses: each frame the VM door's WebSocket library refuses with an
// error it cannot tell from a dropped connection, ClientFrames refuses in fixed words (a WebSocket frame from the
// browser), the read that holds it whole, or a byte at a time, with its own error for the log, and keeps refusing
// without reading on. The library is asked too, so a library that stops refusing one, or a check that drifts from
// it, fails here.
func TestClientFramesRefuseWhatTheWebSocketLibraryRefuses(t *testing.T) {
	ping := clientFrame(0x89, nil)
	for name, stream := range map[string][]byte{
		"reserved bits":                     clientFrame(0xC2, []byte{1}),
		"an unmasked frame":                 appendFrame(nil, frame{b0: 0x82}, []byte{1}),
		"an unknown data opcode":            clientFrame(0x83, []byte{1}),
		"an unknown control opcode":         clientFrame(0x8B, nil),
		"a control frame over 125 bytes":    clientFrame(0x89, make([]byte, 126)),
		"a fragmented control frame":        append(clientFrame(0x09, nil), ping...),
		"a 1-byte close payload":            clientFrame(0x88, []byte{3}),
		"a close status a peer cannot send": closeFrame(1005, ""),
		"a close status out of range":       closeFrame(2000, "x"),
		"the close status 999":              closeFrame(999, ""),
		"the close status 1004":             closeFrame(1004, ""),
		"the close status 1006":             closeFrame(1006, ""),
		"the close status 1015":             closeFrame(1015, ""),
		"the close status 2999":             closeFrame(2999, ""),
		"the close status 5000":             closeFrame(5000, ""),
		"a continuation outside a message":  clientFrame(0x80, []byte{1}),
		"a new message inside one":          append(clientFrame(0x02, []byte{1}), clientFrame(0x82, []byte{2})...),
		"a 64-bit length with its top bit":  {0x82, 0xFF, 0x80, 0, 0, 0, 0, 0, 0, 5},
	} {
		t.Run(name, func(t *testing.T) {
			for _, split := range []bool{false, true} {
				src := &countingReader{r: bytes.NewReader(append(clientFrame(0x82, []byte("before")), stream...))}
				var r io.Reader = src
				if split {
					r = iotest.OneByteReader(src)
				}
				c := CheckClientFrames(r)
				got, err := io.ReadAll(c)
				if !split && len(got) != 0 {
					t.Fatalf("the read that holds the bad frame passed %d bytes, want it refused whole", len(got))
				}
				var refusal *Refusal
				if !errors.As(err, &refusal) || refusal.Kind != "a WebSocket frame from the browser that it cannot read" ||
					refusal.Err == nil || err.Error() != refusal.Err.Error() || c.Refused() != err {
					t.Fatalf("split %v: read ended with %v, want the browser's frame refused, named and kept", split, err)
				}
				reads := src.reads
				if _, again := c.Read(make([]byte, 8)); again != err || src.reads != reads {
					t.Fatalf("split %v: a read after the refusal got %v after %d more reads, want the refusal, unread", split, again, src.reads-reads)
				}
			}
			if err := libraryRead(t, stream, false); err == nil || websocket.CloseStatus(err) != -1 {
				t.Fatalf("the library ended with %v, want it to refuse the stream", err)
			}
		})
	}
}

// TestClientFramesPassWhatTheWebSocketLibraryReads: what the library reads up to a close, ClientFrames passes
// unchanged, whole or a byte at a time, and checks nothing after the close.
func TestClientFramesPassWhatTheWebSocketLibraryReads(t *testing.T) {
	long := make([]byte, 300)
	for name, tc := range map[string]struct {
		stream []byte
		code   websocket.StatusCode
	}{
		"binary, then a bare close": {append(clientFrame(0x82, []byte{1, 2, 3}), clientFrame(0x88, nil)...), websocket.StatusNoStatusRcvd},
		"a message in fragments, with control frames between them, then another": {cat(clientFrame(0x02, []byte{1}), clientFrame(0x89, []byte("p")),
			clientFrame(0x00, []byte{2}), clientFrame(0x8A, nil), clientFrame(0x80, []byte{3}), clientFrame(0x82, []byte{4}),
			closeFrame(1000, "bye")), websocket.StatusNormalClosure},
		"a text message and a 300-byte one": {cat(clientFrame(0x81, []byte("t")), clientFrame(0x82, long), closeFrame(1014, "")), websocket.StatusBadGateway},
		"lengths in longer forms than needed": {cat([]byte{0x82, 0xFE, 0, 1, 1, 2, 3, 4, 8}, []byte{0x82, 0xFF, 0, 0, 0, 0, 0, 0, 0, 1, 1, 2, 3, 4, 8},
			closeFrame(3000, "")), 3000},
		"a 125-byte ping":       {cat(clientFrame(0x89, make([]byte, 125)), closeFrame(1003, "")), websocket.StatusUnsupportedData},
		"the close 1007":        {closeFrame(1007, ""), websocket.StatusInvalidFramePayloadData},
		"bytes after the close": {cat(closeFrame(4999, ""), []byte{0x83, 0x00, 0xFF}), 4999},
	} {
		t.Run(name, func(t *testing.T) {
			for _, split := range []bool{false, true} {
				var r io.Reader = bytes.NewReader(tc.stream)
				if split {
					r = iotest.OneByteReader(r)
				}
				c := CheckClientFrames(r)
				if got, err := io.ReadAll(c); err != nil || !bytes.Equal(got, tc.stream) || c.Refused() != nil {
					t.Fatalf("split %v: read %x (err %v), want the stream unchanged", split, got, err)
				}
			}
			if err := libraryRead(t, tc.stream, false); websocket.CloseStatus(err) != tc.code {
				t.Fatalf("the library ended with %v, want the close %d", err, tc.code)
			}
		})
	}
}

// TestClientFramesADroppedConnectionIsNoRefusal: a connection that ends inside a frame ends the read with the
// connection's own error and no Refusal, while the library, given the same, reads only the end of its connection.
func TestClientFramesADroppedConnectionIsNoRefusal(t *testing.T) {
	for name, stream := range map[string][]byte{
		"inside a header":        {0x82},
		"inside a mask key":      {0x82, 0x81, 1, 2},
		"inside a payload":       clientFrame(0x82, []byte{1, 2, 3})[:7],
		"inside a close payload": closeFrame(1000, "bye")[:7],
	} {
		t.Run(name, func(t *testing.T) {
			c := CheckClientFrames(iotest.OneByteReader(bytes.NewReader(stream)))
			if got, err := io.ReadAll(c); err != nil || !bytes.Equal(got, stream) || c.Refused() != nil {
				t.Fatalf("read %x (err %v, refused %v), want the bytes and no refusal", got, err, c.Refused())
			}
			if err := libraryRead(t, stream, true); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("the library ended with %v, want the end of its connection", err)
			}
		})
	}
}

// TestRefuseEndsTheStreamOnce: a door's refusal reaches refused once and closes the backend, and a refusal the
// filter made first stands.
func TestRefuseEndsTheStreamOnce(t *testing.T) {
	var refused []error
	b := newClosingBackend()
	c := Wrap(b, Rules{}, func(Event) {}, func(err error) { refused = append(refused, err) })
	first := &Refusal{Kind: refusedFrame[false], Err: errors.New("rfb relay: an unmasked WebSocket frame from the browser")}
	c.Refuse(first)
	c.Refuse(errors.New("a second refusal"))
	select {
	case <-b.closed:
	default:
		t.Fatal("the backend stayed open after the refusal")
	}
	if len(refused) != 1 || refused[0] != first {
		t.Fatalf("refused %v, want the first refusal once", refused)
	}

	h := newHarness(t, Rules{}, false)
	h.fb.chunks = [][]byte{frames([]byte("HTTP/1.1 200"), false, false)}
	if _, err := h.c.Read(make([]byte, 64)); !errors.Is(err, ErrNotRFB) {
		t.Fatalf("read %v, want ErrNotRFB", err)
	}
	h.c.Refuse(first)
	if len(h.refused) != 1 || !errors.Is(h.refused[0], ErrNotRFB) {
		t.Fatalf("refused %v, want the filter's own refusal alone", h.refused)
	}
}
