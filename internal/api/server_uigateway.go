// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http/httputil"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/rfbfilter"
)

// uiGatewayState is the UI gateway's process state (uigateway.go, uigateway_desktop.go), embedded in
// Server the way deviceRouteState is.
type uiGatewayState struct {
	// uiConns counts concurrent UI-gateway relay connections per run, enforcing
	// maxUIConnsPerRun (uigateway.go) — each one is a live socat exec in the
	// sandbox. uiReady caches the per-(run,app) launcher probe, and uiProxy is
	// the single shared reverse proxy + exec-lane transport built on first use.
	// All process-local, like sshSessions and lastTouch above and correct for
	// the same reason (replicas>1 is refused by construction). Zero values are
	// ready to use.
	uiConnsMu sync.Mutex
	uiConns   map[uuid.UUID]int
	// uiDesktopSink, when set, gets each desktop socket's bytes AFTER the clipboard filter, in
	// both directions (rfbfilter.Sink): the tee point for a recorder, so a dropped cut text is
	// never recorded. Nil (the default) tees nothing. It returns nil to skip a session.
	uiDesktopSink func(uiSession) rfbfilter.Sink
	uiReadyMu     sync.Mutex
	uiReady       map[string]time.Time
	// uiReassert debounces the relay's REQUEST-path authorization re-check, one
	// entry per minted session (uiReassertKey) — see uiReassertRelay.
	uiReassertMu sync.Mutex
	uiReassert   map[string]time.Time
	uiProxyOnce  sync.Once
	uiProxy      *httputil.ReverseProxy
	// uiDesktop caches the desktop sessions their person closed and holds this process's live desktop
	// streams, by uiReassertKey (uigateway_desktop.go). The Closed set itself is in the store.
	uiDesktop uiDesktopSessions
	// uiDesktopClosedEvery, when set, replaces uiDesktopClosedRecheck (tests shorten it).
	uiDesktopClosedEvery time.Duration
}
