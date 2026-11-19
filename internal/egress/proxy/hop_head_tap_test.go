// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTappedTLSConnKeepsItsState: an HTTP/1.1 TLS conn the egress transport
// taps still shows net/http its TLS state, so the response's TLS field and the
// handshake trace a protocol-mismatch cause names the ALPN from are unchanged.
func TestTappedTLSConnKeepsItsState(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(up), nil, testInsecureTLSConfig)

	tapped := false
	ctx := httptrace.WithClientTrace(context.WithValue(context.Background(), vettedIPKey{}, "93.184.216.34:443"),
		&httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { _, tapped = info.Conn.(*tlsHeadTap) }})
	ctx, alpnState := alpnCapture(ctx)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://tls.test/", nil)
	resp, err := p.roundTripUpstream(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	proto, handshaked := alpnState()
	if !tapped || resp.TLS == nil || resp.TLS.NegotiatedProtocol != "http/1.1" || !handshaked || proto != "http/1.1" {
		t.Fatalf("tapped = %v, resp.TLS = %v, traced ALPN = %q (handshaked %v); want a tapped conn that kept its http/1.1 TLS state",
			tapped, resp.TLS != nil, proto, handshaked)
	}
}

// hopAnswer is an upstream response written by hand: status, then lines, then
// the headers TestRelayStripsHopByHopResponseHeaders checks, and its body.
func hopAnswer(status string, lines ...string) string {
	all := slices.Concat([]string{status}, lines,
		[]string{"X-Hop: 1", "X-Other: 1", "Keep-Alive: timeout=5", "X-Keep: 1", "Content-Length: 7"})
	return strings.Join(all, "\r\n") + "\r\n\r\nrelayed"
}

// writeHopAnswers writes answers on r's own connection, reading each next
// request off it, so an answer after the first reaches only a proxy that
// reused that connection.
func writeHopAnswers(t *testing.T, w http.ResponseWriter, r *http.Request, answers []string) {
	_, _ = io.Copy(io.Discard, r.Body)
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	for i, a := range answers {
		if i > 0 {
			next, err := http.ReadRequest(brw.Reader)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, next.Body)
		}
		if _, err := io.WriteString(c, a); err != nil {
			return
		}
	}
}

// TestHeadCaptureFeed pins how a tap finds the final response head in the
// bytes net/http reads, whatever size the reads come in: a blank line split
// across two reads, bare-LF lines, interim 1xx heads, and the bound.
func TestHeadCaptureFeed(t *testing.T) {
	const final = "HTTP/1.1 200 OK\r\nConnection: close, X-Hop\r\n\r\n"
	cases := []struct {
		name  string
		wire  string
		limit int
		want  string // "" == no head captured
	}{
		{name: "one head", wire: final + "body", want: final},
		{name: "bare LF lines", wire: "HTTP/1.1 200 OK\nX-A: 1\n\nbody", want: "HTTP/1.1 200 OK\nX-A: 1\n\n"},
		{name: "interim heads are read past", wire: "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\n" + final, want: final},
		{name: "101 is final", wire: "HTTP/1.1 101 Switching Protocols\r\nUpgrade: x\r\n\r\nframes", want: "HTTP/1.1 101 Switching Protocols\r\nUpgrade: x\r\n\r\n"},
		{name: "a head at the bound", wire: final + "body", limit: len(final), want: final},
		{name: "a head over the bound", wire: final, limit: len(final) - 1},
	}
	for _, tc := range cases {
		for _, step := range []int{1, 2, 3, len(tc.wire)} {
			c := &headCapture{limit: tc.limit}
			if c.limit == 0 {
				c.limit = defaultHeadCaptureLimit
			}
			over := false
			for i := 0; i < len(tc.wire) && !over; i += step {
				over = c.feed([]byte(tc.wire[i:min(i+step, len(tc.wire))]))
			}
			got := ""
			if h := c.head.Load(); h != nil {
				got = string(*h)
			}
			if !over || got != tc.want {
				t.Errorf("%s, %d-byte reads: over = %v, head = %q, want true, %q", tc.name, step, over, got, tc.want)
			}
		}
	}
}
