// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Per-run bounds of the via_org egress (design 0.9 §7.3): 16 concurrent streams, 8 GiB in total and
// a 10 minute idle timeout, constants and not knobs. Every open stream of a run ends when the run
// ends or is kept (its lease lapsed), or its runner goes offline; the run's engine is released then.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

const (
	viaOrgMaxStreams  = 16
	viaOrgMaxBytes    = int64(8) << 30
	viaOrgIdleTimeout = 10 * time.Minute
	viaOrgWatchPeriod = 5 * time.Second
)

// Test seams only: production reads the constants.
var (
	viaOrgByteCap    = viaOrgMaxBytes
	viaOrgIdle       = viaOrgIdleTimeout
	viaOrgWatchEvery = viaOrgWatchPeriod
)

// viaOrgState is this process's via_org runs, keyed by run id.
type viaOrgState struct {
	mu   sync.Mutex
	runs map[uuid.UUID]*viaOrgRun
	// dial and lookupIP replace the network for tests; nil in production.
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	lookupIP func(host string) ([]net.IP, error)
}

// viaOrgRun is one run's engine and open streams.
type viaOrgRun struct {
	id, runner uuid.UUID
	token      atomic.Pointer[string]
	done       chan struct{}

	mu     sync.Mutex // the engine build
	engine *proxy.ViaOrgEngine
	routes []proxy.ViaOrgRoute

	smu       sync.Mutex
	streams   map[*viaOrgStream]struct{}
	closed    bool
	idleSince time.Time
	bytes     atomic.Int64

	// The bounds, read once when the run's state starts.
	byteCap      int64
	idle, period time.Duration
}

// viaOrgStream is one forwarded request in flight.
type viaOrgStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	run    *viaOrgRun
	last   atomic.Int64 // unix nanoseconds of the last byte either way
}

func (s *Server) viaOrgDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if s.viaOrg.dial != nil {
		return s.viaOrg.dial(ctx, network, addr)
	}
	return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, addr)
}

func (s *Server) viaOrgLookupIP(host string) ([]net.IP, error) {
	if s.viaOrg.lookupIP != nil {
		return s.viaOrg.lookupIP(host)
	}
	return net.DefaultResolver.LookupIP(context.Background(), "ip", host)
}

// viaOrgRunFor is the run's via_org state, started on its first request with its liveness watch.
// false when the run's state belongs to another runner.
func (s *Server) viaOrgRunFor(runID, runner uuid.UUID) (*viaOrgRun, bool) {
	s.viaOrg.mu.Lock()
	defer s.viaOrg.mu.Unlock()
	if run, ok := s.viaOrg.runs[runID]; ok {
		return run, run.runner == runner
	}
	if s.viaOrg.runs == nil {
		s.viaOrg.runs = map[uuid.UUID]*viaOrgRun{}
	}
	run := &viaOrgRun{id: runID, runner: runner, done: make(chan struct{}), streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(),
		byteCap: viaOrgByteCap, idle: viaOrgIdle, period: viaOrgWatchEvery}
	s.viaOrg.runs[runID] = run
	go s.watchViaOrgRun(run)
	return run, true
}

func (run *viaOrgRun) isClosed() bool {
	run.smu.Lock()
	defer run.smu.Unlock()
	return run.closed
}

// open admits one stream under the run's bounds, or names the bound it would cross.
func (run *viaOrgRun) open(parent context.Context) (*viaOrgStream, string) {
	run.smu.Lock()
	defer run.smu.Unlock()
	switch {
	case run.closed:
		return nil, "the run's via_org egress is closed"
	case len(run.streams) >= viaOrgMaxStreams:
		return nil, "the run already has 16 via_org streams open"
	case run.bytes.Load() >= run.byteCap:
		return nil, "the run has moved its 8 GiB of via_org traffic"
	}
	ctx, cancel := context.WithCancel(parent)
	st := &viaOrgStream{ctx: ctx, cancel: cancel, run: run}
	st.last.Store(time.Now().UnixNano())
	run.streams[st] = struct{}{}
	return st, ""
}

func (run *viaOrgRun) release(st *viaOrgStream) {
	st.cancel()
	run.smu.Lock()
	defer run.smu.Unlock()
	delete(run.streams, st)
	if len(run.streams) == 0 {
		run.idleSince = time.Now()
	}
}

// moved counts n bytes of a stream; past the run's total the stream is ended.
func (st *viaOrgStream) moved(n int) bool {
	st.last.Store(time.Now().UnixNano())
	if st.run.bytes.Add(int64(n)) > st.run.byteCap {
		st.cancel()
		return false
	}
	return true
}

// watchViaOrgRun ends the run's streams and engine once the run is no longer live on its runner,
// and ends a stream idle past the timeout. A run with no stream for the idle timeout is released too.
func (s *Server) watchViaOrgRun(run *viaOrgRun) {
	t := time.NewTicker(run.period)
	defer t.Stop()
	for {
		select {
		case <-run.done:
			return
		case <-s.cfg.BaseCtx.Done():
			s.endViaOrgRun(run)
			return
		case <-t.C:
		}
		if !s.viaOrgRunLive(run) {
			s.endViaOrgRun(run)
			return
		}
		if run.closeIdle(time.Now()) {
			s.endViaOrgRun(run)
			return
		}
	}
}

// closeIdle ends every stream idle past the timeout and reports whether the run itself has been idle
// that long with nothing open.
func (run *viaOrgRun) closeIdle(now time.Time) bool {
	run.smu.Lock()
	defer run.smu.Unlock()
	for st := range run.streams {
		if now.Sub(time.Unix(0, st.last.Load())) > run.idle {
			st.cancel()
		}
	}
	return len(run.streams) == 0 && now.Sub(run.idleSince) > run.idle
}

// viaOrgRunLive reports whether the run is still running, not kept, and placed on the runner whose
// relay opened its state, with that runner online.
func (s *Server) viaOrgRunLive(run *viaOrgRun) bool {
	got, err := s.cfg.Store.GetRun(s.cfg.BaseCtx, run.id)
	if err != nil {
		return false
	}
	if isTerminalRunState(got.State) || runIsKept(got) || got.RunnerID == nil || *got.RunnerID != run.runner {
		return false
	}
	return s.cfg.RunnerOnline == nil || s.cfg.RunnerOnline(run.runner)
}

// endViaOrgRun cancels every open stream of the run and releases its engine.
func (s *Server) endViaOrgRun(run *viaOrgRun) {
	s.viaOrg.mu.Lock()
	if s.viaOrg.runs[run.id] == run {
		delete(s.viaOrg.runs, run.id)
	}
	s.viaOrg.mu.Unlock()
	run.smu.Lock()
	if run.closed {
		run.smu.Unlock()
		return
	}
	run.closed = true
	close(run.done)
	for st := range run.streams {
		st.cancel()
	}
	run.smu.Unlock()
	run.mu.Lock()
	engine := run.engine
	run.engine = nil
	run.mu.Unlock()
	if engine != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := engine.Close(ctx); err != nil {
			slog.Warn("api: close a via_org engine", slog.String("run_id", run.id.String()), slog.Any("err", err))
		}
	}
}

// endViaOrgRunner ends every via_org run of a runner that went offline.
func (s *Server) endViaOrgRunner(runner uuid.UUID) {
	s.viaOrg.mu.Lock()
	var runs []*viaOrgRun
	for _, run := range s.viaOrg.runs {
		if run.runner == runner {
			runs = append(runs, run)
		}
	}
	s.viaOrg.mu.Unlock()
	for _, run := range runs {
		s.endViaOrgRun(run)
	}
}

// viaOrgCountingBody counts the sandbox's request body against the run's bounds.
type viaOrgCountingBody struct {
	io.ReadCloser
	st *viaOrgStream
}

func (b *viaOrgCountingBody) Read(p []byte) (int, error) {
	if err := b.st.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.st.moved(n) {
		return n, context.Canceled
	}
	return n, err
}

// viaOrgCountingWriter counts the response against the run's bounds.
type viaOrgCountingWriter struct {
	http.ResponseWriter
	st *viaOrgStream
}

func (w *viaOrgCountingWriter) Write(p []byte) (int, error) {
	if err := w.st.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && !w.st.moved(n) {
		return n, context.Canceled
	}
	return n, err
}

func (w *viaOrgCountingWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *viaOrgCountingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
