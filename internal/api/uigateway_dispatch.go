// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/dispatchgate"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// uiDispatchFresh bounds when an issuer portal's relayed request may start its first write into the sandbox:
// dispatchBy is the start of the authority check that admitted the request plus this. Nothing the request's
// dial starts in the sandbox, and no first write of the request, starts at or after it (dispatchgate). Held
// atomically so tests can shorten it while an earlier test's attach goroutines still read it.
func uiDispatchFresh() time.Duration { return time.Duration(uiDispatchFreshNanos.Load()) }

var uiDispatchFreshNanos = func() *atomic.Int64 {
	var v atomic.Int64
	v.Store(int64(25 * time.Second))
	return &v
}()

// uiDispatchState is the dispatch gate's process state: the single-use reverse proxy issuer portals' requests
// go out on, the wake for dials waiting for a run's slot, and the one idle-pool close a portal dial may start.
type uiDispatchState struct {
	proxyOnce sync.Once
	proxy     *httputil.ReverseProxy
	// slotFreed is closed, and replaced, when a run's slot is released; guarded by Server.uiConnsMu.
	slotFreed   chan struct{}
	idleClosing atomic.Bool
	// idleCloseStarts counts the idle-pool closes portal dials started (tests read it).
	idleCloseStarts atomic.Int64
	// beforeAdmit and refuseHook are test seams, nil in production: beforeAdmit holds a gate's writer before
	// its admission check (dispatchgate.Config.BeforeAdmit), and refuseHook runs inside a gate's refusal record,
	// before the typed 503 is set.
	beforeAdmit func()
	refuseHook  func()
}

type uiDispatchCtxKey struct{}

// uiDispatch is an issuer portal's request's dispatch time and the clock it is read against.
type uiDispatch struct {
	by  time.Time
	now func() time.Time
}

// withUIDispatchBy sets a relayed request's dispatch time (relayUISession). by is on the wall clock (the check's
// start, read with time.Now), and so is the clock it is read against: the gate's timers are wall-clock timers,
// and Config.Now, which a test may move, is not the clock by was read on.
func withUIDispatchBy(ctx context.Context, by time.Time) context.Context {
	return context.WithValue(ctx, uiDispatchCtxKey{}, uiDispatch{by: by, now: time.Now})
}

// uiDispatchFrom is ctx's dispatch time; false for a request not on an issuer portal.
func uiDispatchFrom(ctx context.Context) (uiDispatch, bool) {
	d, ok := ctx.Value(uiDispatchCtxKey{}).(uiDispatch)
	return d, ok
}

// uiDispatchPast reports whether ctx is an issuer portal's request at or after its dispatch time.
func uiDispatchPast(ctx context.Context) bool {
	d, ok := uiDispatchFrom(ctx)
	return ok && !d.now().Before(d.by)
}

// uiDispatchRefusal is the 503 a request past its dispatch time gets, the one a timed-out issuer-portal check
// answers.
func uiDispatchRefusal(ctx context.Context) error {
	return uiFail(ctx, http.StatusServiceUnavailable, uiDeniedReasonDelegationUnavailable, uiSessionUnverifiableMsg)
}

// uiStartCtx is the context of a start an issuer portal's request makes: the start cutoff set to its dispatch
// time (runner.WithStartCutoff), so the start lock's wait and the start end there and a registered runner's
// start CALL is refused at the link's writer after it. The context gets no deadline: the exec lives on it.
func uiStartCtx(ctx context.Context) context.Context {
	if d, ok := uiDispatchFrom(ctx); ok {
		return runner.WithStartCutoff(ctx, d.by)
	}
	return ctx
}

// uiBoundedCtx is uiStartCtx with a deadline at the dispatch time too, for the thaw and the app probe, whose
// whole lives are the start.
func uiBoundedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if d, ok := uiDispatchFrom(ctx); ok {
		return context.WithDeadline(runner.WithStartCutoff(ctx, d.by), d.by)
	}
	return ctx, func() {}
}

// uiPortalProxy is the reverse proxy issuer portals' requests go out on: the shared one's handlers over a
// single-use transport (dispatchgate.NewTransport), so each request is dialed, and gated on, a connection of
// its own.
func (s *Server) uiPortalProxy() *httputil.ReverseProxy {
	s.uiDispatch.proxyOnce.Do(func() {
		shared := s.uiReverseProxy()
		s.uiDispatch.proxy = &httputil.ReverseProxy{
			Rewrite:        shared.Rewrite,
			ModifyResponse: shared.ModifyResponse,
			ErrorHandler:   shared.ErrorHandler,
			FlushInterval:  shared.FlushInterval,
			Transport:      dispatchgate.NewTransport(s.uiDial, maxUIConnsPerRun),
		}
	})
	return s.uiDispatch.proxy
}

// uiGate wraps conn, the exec sess's relay connection, in the dispatch gate when ctx is an issuer portal's
// request. The hard stop is sess.Abort when set, else sess.Close; the refusal is the dispatch 503 on the
// request's dial-error box; the outcome is a ui.dispatch row, written off the closing path.
func (s *Server) uiGate(ctx context.Context, conn net.Conn, sess *runner.ExecSession, us uiSession, target string) net.Conn {
	d, ok := uiDispatchFrom(ctx)
	if !ok {
		return conn
	}
	stop := func() { closeExecSession(sess) }
	if sess.Abort != nil {
		stop = sess.Abort
	}
	return dispatchgate.New(conn, dispatchgate.Config{
		By:        d.by,
		Now:       d.now,
		AfterFunc: func(dt time.Duration, f func()) func() bool { return time.AfterFunc(dt, f).Stop },
		Stop:      stop,
		Refuse: func() {
			if h := s.uiDispatch.refuseHook; h != nil {
				h()
			}
			_ = uiDispatchRefusal(ctx)
		},
		Record:      func(o dispatchgate.Outcome) { s.auditUIDispatch(us, target, o) },
		BeforeAdmit: s.uiDispatch.beforeAdmit,
	})
}

// auditUIDispatch writes a gated connection's one outcome row: refused (zero bytes passed), interrupted (a
// write had started; bytes_done is what the driver reported, and partial is "unknown" when that write failed),
// dispatched, or unused.
func (s *Server) auditUIDispatch(us uiSession, target string, o dispatchgate.Outcome) {
	outcome := "success"
	switch o.Kind {
	case dispatchgate.KindRefused:
		outcome = "denied"
	case dispatchgate.KindInterrupted:
		outcome = "failure"
	}
	data := map[string]any{"app": us.App, "dispatch": o.Kind, "dispatch_by": o.By, "bytes_done": o.BytesDone}
	if o.Partial {
		data["partial"] = "unknown"
	}
	for k, t := range map[string]time.Time{"started_at": o.StartedAt, "write_returned_at": o.WriteReturned, "stop_returned_at": o.StopReturned} {
		if !t.IsZero() {
			data[k] = t
		}
	}
	s.auditUISession(us, "ui.dispatch", target, outcome, data)
}

// acquireUIConnBy is acquireUIConn for an issuer portal's dial: with the run's slots full it waits, holding no
// lock and no slot, for one to be released until until, and refuses then.
func (s *Server) acquireUIConnBy(runID uuid.UUID, until time.Time) (func(), bool) {
	for {
		release, ok, freed := s.tryUIConn(runID)
		if ok {
			return release, true
		}
		left := time.Until(until)
		if left <= 0 {
			return nil, false
		}
		t := time.NewTimer(left)
		select {
		case <-freed:
			t.Stop()
		case <-t.C:
			return nil, false
		}
	}
}

// uiIdleCloseAsync starts the shared pool's idle close on a goroutine of its own, at most one at a time in the
// process, and does not wait for it: each idle close can wait for another run's exec close.
func (s *Server) uiIdleCloseAsync() {
	tr, ok := s.uiReverseProxy().Transport.(*http.Transport)
	if !ok || !s.uiDispatch.idleClosing.CompareAndSwap(false, true) {
		return
	}
	s.uiDispatch.idleCloseStarts.Add(1)
	go func() {
		defer s.uiDispatch.idleClosing.Store(false)
		tr.CloseIdleConnections()
	}()
}

// uiDialSlot takes the run's slot for uiDial. An issuer portal's dial does not close the idle pool inline: it
// starts that close off its path (uiIdleCloseAsync) and waits for a slot until the earlier of its connection
// wait's end and its dispatch time (acquireUIConnBy). Any other dial takes acquireUIConn's answer.
func (s *Server) uiDialSlot(ctx context.Context, runID uuid.UUID) (func(), bool) {
	release, ok := s.acquireUIConn(runID)
	d, gated := uiDispatchFrom(ctx)
	if ok || !gated {
		return release, ok
	}
	s.uiIdleCloseAsync()
	until := uiConnWaitEnd(ctx, d.by)
	if d.by.Before(until) {
		until = d.by
	}
	return s.acquireUIConnBy(runID, until)
}

// uiThaw is uiDial's thaw. For an issuer portal's request it runs under a context that ends at the dispatch
// time, and a thaw cut there is the dispatch 503.
func (s *Server) uiThaw(ctx context.Context, run types.AgentRun, sess uiSession) error {
	tctx, cancel := uiBoundedCtx(ctx)
	defer cancel()
	if err := s.thawForExec(tctx, run, uiActorType(sess), sess.Principal, "presence"); err != nil {
		if uiDispatchPast(ctx) {
			return uiDispatchRefusal(ctx)
		}
		return uiFail(ctx, http.StatusBadGateway, reasonUIGatewayResumeFailed, "run is paused and could not be resumed; try again")
	}
	return nil
}

// uiEnsureAppBy is uiEnsureApp, checked first and, for an issuer portal's request, run under a context that
// ends at the dispatch time: the probe waits for its own exit, so its whole exec is the start. A probe cut
// there is the dispatch 503.
func (s *Server) uiEnsureAppBy(ctx context.Context, run types.AgentRun, sess uiSession) error {
	if uiDispatchPast(ctx) {
		return uiDispatchRefusal(ctx)
	}
	pctx, cancel := uiBoundedCtx(ctx)
	defer cancel()
	if err := s.uiEnsureApp(pctx, run, sess); err != nil {
		if uiDispatchPast(ctx) {
			return uiDispatchRefusal(ctx)
		}
		return err
	}
	return nil
}
