// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"
)

// hopByHopHeaders are stripped before forwarding (RFC 7230 §6.1).
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopByHop(h http.Header) {
	// Headers named in Connection are also hop-by-hop.
	for _, name := range h.Values("Connection") {
		for _, tok := range strings.Split(name, ",") {
			if t := strings.TrimSpace(tok); t != "" {
				h.Del(t)
			}
		}
	}
	for _, hh := range hopByHopHeaders {
		h.Del(hh)
	}
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
