// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package dispatchgate

import (
	"context"
	"net"
	"net/http"
)

// NewTransport is the single-use transport a gated relay goes out on: every request is dialed a connection of
// its own, which serves that one request and is never pooled (DisableKeepAlives), so a gate on the connection
// is a gate on its request. A failed write on a fresh connection is not retried, so each request is handed
// exactly one connection. maxConnsPerHost bounds one host's connections, as the shared transport does.
func NewTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error), maxConnsPerHost int) *http.Transport {
	return &http.Transport{
		DialContext:       dial,
		DisableKeepAlives: true,
		MaxConnsPerHost:   maxConnsPerHost,
		// No proxy, no TLS: the destination is a sandbox's own loopback reached over an exec.
		Proxy: nil,
	}
}
