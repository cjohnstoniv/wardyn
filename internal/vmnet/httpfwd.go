// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Port 80 is carried as forward-proxy requests, not a CONNECT tunnel, so the
// proxy sees each request's method and path and its method rules, holds and
// plain-lane rules apply to a VM's plain HTTP as they do to a container's. Each
// request's Host must be the name the flow was resolved as (the plain-HTTP
// twin of the TLS server-name rule); it is rewritten to absolute form with that
// name, so the proxy decides on the name the relay gave out, never on a header
// the guest chose.
const (
	maxRequestHeader = 1 << 20
	httpIdle         = 120 * time.Second
)

// hopHeaders are dropped from a guest request before it is forwarded, with
// any header the guest's Connection header names.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authorization", "Proxy-Authenticate", "Proxy-Connection",
	"Te", "Trailer", "Upgrade",
}

// errNotHTTP: the guest's first bytes on port 80 were not an HTTP request.
var errNotHTTP = errors.New("not an HTTP request")

// hostMatches reports whether a request's Host names the resolved name.
func hostMatches(host, name string) bool {
	host = strings.ToLower(host)
	if h, p, err := net.SplitHostPort(host); err == nil {
		if p != "80" {
			return false
		}
		host = h
	}
	return strings.TrimSuffix(host, ".") == name
}

// limitedReader caps the bytes one request's header may take; n < 0 is no cap
// (a body streams through it uncapped).
type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return l.r.Read(p)
	}
	if l.n == 0 {
		return 0, errNotHTTP
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// serveHTTP carries one guest connection's HTTP requests to the proxy in turn
// (keep-alive and pipelining are sequential). It returns the refusal reason
// the relay reports, or "" when the connection simply ended.
func serveHTTP(gc net.Conn, name string, rt http.RoundTripper) string {
	lim := &limitedReader{r: gc}
	br := bufio.NewReader(lim)
	for first := true; ; first = false {
		lim.n = maxRequestHeader
		_ = gc.SetReadDeadline(time.Now().Add(httpIdle))
		req, err := http.ReadRequest(br)
		if err != nil {
			var ne net.Error
			if first && !errors.Is(err, io.EOF) && !(errors.As(err, &ne) && ne.Timeout()) {
				return "not-http"
			}
			return ""
		}
		lim.n = -1
		_ = gc.SetReadDeadline(time.Time{})
		if !hostMatches(req.Host, name) {
			writeStatus(gc, http.StatusForbidden, "the request's Host is not the name this connection was resolved as\n")
			return "host-mismatch"
		}
		if req.Header.Get("Upgrade") != "" {
			writeStatus(gc, http.StatusNotImplemented, "protocol upgrades over plain HTTP are not carried\n")
			return ""
		}
		resp, err := rt.RoundTrip(forwardRequest(req, name))
		if err != nil {
			writeStatus(gc, http.StatusBadGateway, "the relay could not reach the proxy\n")
			return ""
		}
		werr := resp.Write(gc)
		_ = resp.Body.Close()
		if werr != nil || req.Close || resp.Close {
			return ""
		}
	}
}

// forwardRequest rewrites a guest request to absolute form on the resolved
// name, without the hop-by-hop headers.
func forwardRequest(in *http.Request, name string) *http.Request {
	out := in.Clone(in.Context())
	out.RequestURI = ""
	out.URL = &url.URL{Scheme: "http", Host: name, Path: in.URL.Path, RawPath: in.URL.RawPath, RawQuery: in.URL.RawQuery}
	out.Host = name
	for _, f := range strings.Split(in.Header.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			out.Header.Del(f)
		}
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	// net/http adds its own User-Agent to a request without one; an empty
	// value keeps the guest's request as it was sent.
	if _, ok := out.Header["User-Agent"]; !ok {
		out.Header["User-Agent"] = []string{""}
	}
	return out
}

func writeStatus(w io.Writer, code int, body string) {
	resp := &http.Response{
		StatusCode: code, ProtoMajor: 1, ProtoMinor: 1, Close: true,
		Header:        http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	_ = resp.Write(w)
}
