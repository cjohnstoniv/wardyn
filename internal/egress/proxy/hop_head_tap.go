// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync"
	"sync/atomic"

	"golang.org/x/net/http/httpguts"
)

// net/http's client deletes an HTTP/1.1 response's whole Connection header when it carries
// "close", folding it into Response.Close, before any relay code runs; the headers it named
// would then reach the sandbox. So roundTripUpstream reads those names from the raw head: the
// egress transport's HTTP/1.x conns are tapped (headTap), GotConn arms the request's conn, and
// the tap copies that response's head as net/http reads it. The bytes read are never changed.

// headTapKey marks a dial made for a roundTripUpstream request, the one caller that arms a tap
// and reads it. The kube door's copy of the egress transport shares the plain dial but runs TLS
// over it, where a tap would see ciphertext; its requests carry no mark.
type headTapKey struct{}

// defaultHeadCaptureLimit bounds a captured head when the transport sets no
// MaxResponseHeaderBytes. A longer head is relayed as before, with its folded names unread.
const defaultHeadCaptureLimit = 64 << 10

// headTap copies the response head it is armed for out of the bytes net/http reads.
type headTap struct {
	net.Conn
	mu  sync.Mutex // Read runs on the transport's read loop; arm on the request's goroutine
	cur *headCapture
}

func (t *headTap) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if n > 0 {
		t.mu.Lock()
		if t.cur != nil && t.cur.feed(b[:n]) {
			t.cur = nil
		}
		t.mu.Unlock()
	}
	return n, err
}

func (t *headTap) arm(c *headCapture) {
	t.mu.Lock()
	t.cur = c
	t.mu.Unlock()
}

// tlsHeadTap is a tapped TLS conn. net/http sets Response.TLS and fires the handshake trace
// from ConnectionState, so it stays visible past the tap.
type tlsHeadTap struct {
	headTap
	tc *tls.Conn
}

func (t *tlsHeadTap) ConnectionState() tls.ConnectionState { return t.tc.ConnectionState() }

// tapHeads taps a plain conn dialed for a roundTripUpstream request.
func tapHeads(ctx context.Context, c net.Conn) net.Conn {
	if ctx.Value(headTapKey{}) == nil {
		return c
	}
	return &headTap{Conn: c}
}

// tapTLSHeads taps a TLS conn dialed for a roundTripUpstream request, unless it negotiated h2:
// net/http hands only a bare *tls.Conn to its HTTP/2 transport, and HTTP/2 folds no
// Connection header.
func tapTLSHeads(ctx context.Context, tc *tls.Conn) net.Conn {
	if ctx.Value(headTapKey{}) == nil || tc.ConnectionState().NegotiatedProtocol == "h2" {
		return tc
	}
	return &tlsHeadTap{headTap: headTap{Conn: tc}, tc: tc}
}

// headCapture is one response's head as its conn's tap copies it. buf is the tap's, under its
// lock; head is published once the final head is in whole.
type headCapture struct {
	limit int
	buf   []byte
	head  atomic.Pointer[[]byte]
}

// feed takes bytes as they are read and reports whether the capture is over: the final head is
// in, or a head outgrew limit. An interim 1xx head is dropped and the next one read, as
// net/http does.
func (c *headCapture) feed(b []byte) bool {
	for len(b) > 0 {
		old := len(c.buf)
		c.buf = append(c.buf, b[:min(len(b), c.limit-old)]...)
		end := headEnd(c.buf, max(old-2, 0)) // the blank line may straddle two reads
		if end < 0 {
			if len(c.buf) < c.limit {
				return false
			}
			c.buf = nil
			return true
		}
		if !interimHead(c.buf[:end]) {
			h := c.buf[:end]
			c.head.Store(&h)
			return true
		}
		b, c.buf = b[end-old:], c.buf[:0]
	}
	return false
}

// headEnd is the index just past buf's first blank line at or after from, or -1. A line ends
// at "\n" with an optional "\r" before it, as net/textproto reads one.
func headEnd(buf []byte, from int) int {
	for i := from; i+1 < len(buf); i++ {
		switch {
		case buf[i] != '\n':
		case buf[i+1] == '\n':
			return i + 2
		case buf[i+1] == '\r' && i+2 < len(buf) && buf[i+2] == '\n':
			return i + 3
		}
	}
	return -1
}

// interimHead reports a head net/http reads past: any 1xx but 101, its status line split the
// way http.ReadResponse splits it.
func interimHead(head []byte) bool {
	line, _, _ := bytes.Cut(head, []byte("\n"))
	_, status, _ := bytes.Cut(bytes.TrimSuffix(line, []byte("\r")), []byte(" "))
	code, _, _ := bytes.Cut(bytes.TrimLeft(status, " "), []byte(" "))
	return len(code) == 3 && code[0] == '1' && string(code) != "101"
}

// headSlot holds the capture armed on the latest conn one round trip got: net/http may retry a
// request on a fresh conn.
type headSlot struct{ cur atomic.Pointer[headCapture] }

// tapResponseHead marks ctx's dials for tapping and arms each tapped conn the request gets,
// before the request is written to it.
func tapResponseHead(ctx context.Context, limit int64) (context.Context, *headSlot) {
	s := &headSlot{}
	n := int(cmp.Or(limit, defaultHeadCaptureLimit))
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		var c *headCapture
		if t, ok := info.Conn.(interface{ arm(*headCapture) }); ok {
			c = &headCapture{limit: n}
			t.arm(c)
		}
		s.cur.Store(c)
	}}
	return httptrace.WithClientTrace(context.WithValue(ctx, headTapKey{}, true), trace), s
}

// stripFolded deletes from resp.Header each header its head's Connection named, when net/http
// folded that Connection away: resp.Close set, no Connection left, and the captured head's
// Connection said "close" (the test net/http's shouldClose folds on). Any other response kept
// its Connection header for the relay's removeHopByHop, and a head not captured whole (over the
// limit, or h2) is relayed as before. It never fails the request.
func (s *headSlot) stripFolded(resp *http.Response) {
	c := s.cur.Load()
	if c == nil || !resp.Close || resp.Header["Connection"] != nil {
		return
	}
	h := c.head.Load()
	if h == nil {
		return
	}
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(*h)))
	if _, err := tp.ReadLine(); err != nil {
		return
	}
	mh, err := tp.ReadMIMEHeader()
	if err != nil || !httpguts.HeaderValuesContainsToken(mh["Connection"], "close") {
		return
	}
	// The strip every arriving request gets, on the names net/http dropped.
	resp.Header["Connection"] = mh["Connection"]
	removeConnectionNominated(resp.Header)
	delete(resp.Header, "Connection")
}
