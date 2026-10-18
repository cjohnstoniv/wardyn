// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"time"
)

// uiConnWait bounds how long a relayed request waits for one of its run's connections. The relay
// transport holds a run's app to maxUIConnsPerRun connections, so a request past them waits for one
// to come free: one HTTP/2 page load keeps more than that in flight. Behind that many long-lived
// streams (a websocket, an event stream) none comes free, so a request still waiting after
// uiConnWait gets the connection-cap 503 it got at once before the wait existed. The bound ends when
// the transport hands the request a pooled connection or dials a new one; it never covers a dial or
// a response.
var uiConnWait = 5 * time.Second

var uiConnCapMsg = fmt.Sprintf("too many open UI connections for this run (max %d) — close a tab and retry", maxUIConnsPerRun)

type uiConnWaitCtxKey struct{}

// serveUIRelay relays r, answering the connection-cap 503 if no connection is in hand within
// uiConnWait. r's context carries the relay's dial-error box.
func (s *Server) serveUIRelay(w http.ResponseWriter, r *http.Request) {
	base, cancel := context.WithCancel(r.Context())
	defer cancel()
	wait := time.AfterFunc(uiConnWait, func() {
		_ = uiFail(base, http.StatusServiceUnavailable, reasonUIGatewayConnCapReached, uiConnCapMsg)
		cancel()
	})
	defer wait.Stop()
	ctx := context.WithValue(base, uiConnWaitCtxKey{}, wait)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { wait.Stop() }})
	s.uiReverseProxy().ServeHTTP(uiInterimWriter{w}, r.WithContext(ctx))
}

// uiConnWaitDone ends the wait when the transport dials: the request has a connection slot.
func uiConnWaitDone(ctx context.Context) {
	if wait, ok := ctx.Value(uiConnWaitCtxKey{}).(*time.Timer); ok {
		wait.Stop()
	}
}
