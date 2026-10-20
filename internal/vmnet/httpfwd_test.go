// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func serveOnce(t *testing.T, raw string, rt http.RoundTripper) (reason, reply string) {
	t.Helper()
	guest, relay := net.Pipe()
	done := make(chan string, 1)
	go func() { done <- serveHTTP(relay, "origin.test", rt); _ = relay.Close() }()
	go func() { _, _ = io.WriteString(guest, raw) }()
	b, _ := io.ReadAll(guest)
	return <-done, string(b)
}

func TestServeHTTPForwardsAbsoluteForm(t *testing.T) {
	var got []*http.Request
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r)
		return &http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("ok")), ContentLength: 2, Request: r}, nil
	})
	raw := "GET /a?b=1 HTTP/1.1\r\nHost: origin.test\r\nProxy-Authorization: x\r\nConnection: keep-alive, X-Drop\r\nX-Drop: 1\r\nX-Keep: 1\r\n\r\n" +
		"POST /c HTTP/1.1\r\nHost: Origin.Test:80\r\nContent-Length: 3\r\nConnection: close\r\n\r\nabc"
	reason, reply := serveOnce(t, raw, rt)
	if reason != "" || strings.Count(reply, "HTTP/1.1 200") != 2 {
		t.Fatalf("reason %q reply %q", reason, reply)
	}
	if len(got) != 2 {
		t.Fatalf("%d requests forwarded, want 2 (keep-alive)", len(got))
	}
	r := got[0]
	if r.URL.String() != "http://origin.test/a?b=1" || r.Host != "origin.test" || r.RequestURI != "" {
		t.Fatalf("forwarded %q host %q", r.URL, r.Host)
	}
	for _, h := range []string{"Proxy-Authorization", "Connection", "X-Drop"} {
		if r.Header.Get(h) != "" {
			t.Errorf("hop header %s forwarded", h)
		}
	}
	if r.Header.Get("X-Keep") != "1" || r.Header.Get("User-Agent") != "" {
		t.Errorf("headers %v", r.Header)
	}
	if got[1].Method != http.MethodPost || got[1].URL.String() != "http://origin.test/c" {
		t.Fatalf("second request %s %s", got[1].Method, got[1].URL)
	}
}

func TestServeHTTPRefusals(t *testing.T) {
	never := rtFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("forwarded %s", r.URL)
		return nil, io.ErrUnexpectedEOF
	})
	for raw, want := range map[string]string{
		"GET / HTTP/1.1\r\nHost: other.test\r\n\r\n":       "host-mismatch",
		"GET / HTTP/1.1\r\nHost: origin.test:8080\r\n\r\n": "host-mismatch",
		// An absolute-form target names the host it wants, whatever Host says.
		"GET http://other.test/ HTTP/1.1\r\nHost: origin.test\r\n\r\n": "host-mismatch",
		"SSH-2.0-x\r\n\r\n": "not-http",
	} {
		reason, reply := serveOnce(t, raw, never)
		if reason != want {
			t.Errorf("%q: reason %q, want %q", raw, reason, want)
		}
		if want == "host-mismatch" {
			resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(reply)), nil)
			if err != nil || resp.StatusCode != http.StatusForbidden {
				t.Errorf("%q: reply %q", raw, reply)
			}
		}
	}
	reason, reply := serveOnce(t, "GET / HTTP/1.1\r\nHost: origin.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", never)
	if reason != "" || !strings.HasPrefix(reply, "HTTP/1.1 501") {
		t.Errorf("upgrade: reason %q reply %q", reason, reply)
	}
}
