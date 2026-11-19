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
	removeConnectionNominated(h)
	for _, hh := range hopByHopHeaders {
		h.Del(hh)
	}
}

// removeConnectionNominated deletes every header the Connection header names:
// naming one makes it hop-by-hop, so no forward sends it. ServeHTTP and
// serveMITMRequest run it on each arriving request before any gate reads a
// header, so a gate judges the headers that will be sent. Judged with such a
// header, an Azure DevOps POST carrying X-HTTP-Method-Override: PATCH was
// approved as an Update Ref of the ref its query names, then sent upstream as
// the POST that moves the refs its body names.
func removeConnectionNominated(h http.Header) {
	for _, name := range h.Values("Connection") {
		for _, tok := range strings.Split(name, ",") {
			if t := strings.TrimSpace(tok); t != "" {
				h.Del(t)
			}
		}
	}
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
