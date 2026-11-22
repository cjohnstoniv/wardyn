// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/google/uuid"
)

// uiConnWait bounds how long a relayed request waits for one of its run's connections. The relay
// transport holds a run's app to maxUIConnsPerRun connections, so a request past them waits for one
// to come free: one HTTP/2 page load keeps more than that in flight. Behind that many long-lived
// streams (a websocket, an event stream) none comes free, so a request still waiting after
// uiConnWait gets the connection-cap 503 it got at once before the wait existed. The bound ends when
// the transport hands the request a pooled connection or dials a new one; it never covers a dial or
// a response. Fifteen seconds lets a slow but finite backlog drain before the refusal; the refusal
// itself is unchanged.
var uiConnWait = 15 * time.Second

var uiConnCapMsg = fmt.Sprintf("too many open UI connections for this run (max %d) — close a tab and retry", maxUIConnsPerRun)

type uiConnWaitCtxKey struct{}

// uiConnWaiter is one request's wait. Its timer and the transport's "connection in hand" signal can
// land within microseconds of each other, so the signal sets delivered under mu and expire checks it
// under the same mu: a request whose connection was delivered is never cancelled by a timer that
// fired a moment later.
type uiConnWaiter struct {
	mu        sync.Mutex
	delivered bool
	timer     *time.Timer
	base      context.Context
	cancel    context.CancelFunc
	// end is the wait's own end, its start plus uiConnWait; dispatch is set when an issuer portal's request
	// reaches its dispatch time first, and the timer fires then.
	end      time.Time
	dispatch bool
}

// expire answers the connection-cap 503, or at an issuer portal's dispatch time the dispatch 503, and cancels
// the request, unless a connection was delivered.
func (w *uiConnWaiter) expire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.delivered {
		return
	}
	if w.dispatch {
		_ = uiDispatchRefusal(w.base)
	} else {
		_ = uiFail(w.base, http.StatusServiceUnavailable, reasonUIGatewayConnCapReached, uiConnCapMsg)
	}
	w.cancel()
}

// done ends the wait: the request has a connection slot.
func (w *uiConnWaiter) done() {
	w.mu.Lock()
	w.delivered = true
	w.mu.Unlock()
	w.timer.Stop()
}

// serveUIRelay relays r, answering the connection-cap 503 if no connection is in hand within
// uiConnWait. r's context carries the relay's dial-error box.
//
// An issuer portal's request (uiDispatchFrom) goes out on the single-use proxy (uiPortalProxy). It is refused
// before the hand-off at or after its dispatch time, and its wait also ends then, with the dispatch 503.
func (s *Server) serveUIRelay(w http.ResponseWriter, r *http.Request) {
	base, cancel := context.WithCancel(r.Context())
	defer cancel()
	wait := &uiConnWaiter{base: base, cancel: cancel, end: time.Now().Add(uiConnWait)}
	proxy, every := s.uiReverseProxy(), uiConnWait
	if d, ok := uiDispatchFrom(r.Context()); ok {
		left := d.by.Sub(d.now())
		if left <= 0 {
			_ = uiDispatchRefusal(base)
			uiErrorHandler(w, r, errDispatchPassed)
			return
		}
		if left < every {
			every, wait.dispatch = left, true
		}
		proxy = s.uiPortalProxy()
	}
	wait.timer = time.AfterFunc(every, wait.expire)
	defer wait.done()
	ctx := context.WithValue(base, uiConnWaitCtxKey{}, wait)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { wait.done() }})
	proxy.ServeHTTP(uiInterimWriter{w}, r.WithContext(ctx))
}

// errDispatchPassed is a request refused before its hand-off, at or after its dispatch time.
var errDispatchPassed = errors.New("the request's dispatch time passed before its hand-off")

// uiConnWaitEnd is the end of ctx's connection wait (its start plus uiConnWait), or fallback without one.
func uiConnWaitEnd(ctx context.Context, fallback time.Time) time.Time {
	if wait, ok := ctx.Value(uiConnWaitCtxKey{}).(*uiConnWaiter); ok {
		return wait.end
	}
	return fallback
}

// uiConnWaitDone ends the wait when the transport dials: the request has a connection slot.
func uiConnWaitDone(ctx context.Context) {
	if wait, ok := ctx.Value(uiConnWaitCtxKey{}).(*uiConnWaiter); ok {
		wait.done()
	}
}

// uiConn is an execConn that releases its per-run slot and writes the ui.close
// audit exactly once, whichever of net/http's read or write loop closes first.
// The exec is closed first: closeFn's audit write must never hold a relay open.
type uiConn struct {
	*execConn
	once    sync.Once
	closeFn func()
}

func (c *uiConn) Close() error {
	err := c.execConn.Close()
	c.once.Do(c.closeFn)
	return err
}

// acquireUIConn takes one of the run's connection slots, returning a release
// func and whether a slot was available. Process-local, like every other
// in-memory bound here (the single-replica control plane is a documented
// constraint, docs/OPERATIONS.md).
func (s *Server) acquireUIConn(runID uuid.UUID) (func(), bool) {
	release, ok, _ := s.tryUIConn(runID)
	return release, ok
}

// tryUIConn is acquireUIConn, and with the slots full also the channel the next release closes, taken under
// the same uiConnsMu hold as the refusal, so a dial that waits on it misses no release (acquireUIConnBy).
func (s *Server) tryUIConn(runID uuid.UUID) (func(), bool, <-chan struct{}) {
	s.uiConnsMu.Lock()
	defer s.uiConnsMu.Unlock()
	if s.uiConns == nil {
		s.uiConns = map[uuid.UUID]int{}
	}
	if s.uiConns[runID] >= maxUIConnsPerRun {
		if s.uiDispatch.slotFreed == nil {
			s.uiDispatch.slotFreed = make(chan struct{})
		}
		return nil, false, s.uiDispatch.slotFreed
	}
	s.uiConns[runID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.uiConnsMu.Lock()
			defer s.uiConnsMu.Unlock()
			if freed := s.uiDispatch.slotFreed; freed != nil {
				close(freed) // wakes every dial waiting for a slot (acquireUIConnBy)
				s.uiDispatch.slotFreed = nil
			}
			if s.uiConns[runID] <= 1 {
				delete(s.uiConns, runID)
				return
			}
			s.uiConns[runID]--
		})
	}, true, nil
}
