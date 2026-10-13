// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Per-run bounds of the via_org egress (design 0.9 §7.3): 16 concurrent streams, 8 GiB in total and
// a 10 minute idle timeout, constants and not knobs. Every open stream of a run ends when the run
// ends or is kept (its lease lapsed), or its runner goes offline; the run's engine is released then.
// The byte total is the run's: it outlives those releases and is dropped only when the run ends.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
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
	// spent is each run's via_org byte total: a ledger apart from the run's state, engine and
	// streams, kept through every release and dropped only when the run ends. It is this
	// process's: a wardynd restart, or a request served by another replica, starts from zero.
	spent map[uuid.UUID]*atomic.Int64
	// dial and lookupIP replace the network for tests; nil in production.
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	lookupIP func(host string) ([]net.IP, error)
}

// viaOrgRun is one run's engine and open streams.
type viaOrgRun struct {
	id, runner uuid.UUID
	token      atomic.Pointer[string]
	done       chan struct{}

	mu     sync.Mutex // the engine build and its generation; taken before smu, never after
	engine *viaOrgGen
	routes []proxy.ViaOrgRoute

	smu       sync.Mutex
	streams   map[*viaOrgStream]struct{}
	closed    bool
	idleSince time.Time
	bytes     *atomic.Int64 // the run's entry in viaOrgState.spent

	// The bounds, read once when the run's state starts.
	byteCap      int64
	idle, period time.Duration
}

// viaOrgGen is one engine generation of a run. A stream is admitted only to the run's current
// generation (open), and a retired generation's engine closes only once every stream admitted to
// it has returned, so its decision sink and masking corpus outlive their last producer.
type viaOrgGen struct {
	engine *proxy.ViaOrgEngine
	active sync.WaitGroup
}

// viaOrgStream is one forwarded request in flight.
type viaOrgStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	run    *viaOrgRun
	gen    *viaOrgGen
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
	if s.viaOrg.spent == nil {
		s.viaOrg.spent = map[uuid.UUID]*atomic.Int64{}
	}
	spent, ok := s.viaOrg.spent[runID]
	if !ok {
		spent = new(atomic.Int64)
		s.viaOrg.spent[runID] = spent
	}
	run := &viaOrgRun{id: runID, runner: runner, bytes: spent, done: make(chan struct{}), streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(),
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

// open admits one stream to gen under the run's bounds, or names the bound it would cross. retired
// is true when gen is no longer the run's generation: the caller takes the current one and asks again.
func (run *viaOrgRun) open(parent context.Context, gen *viaOrgGen) (st *viaOrgStream, refused string, retired bool) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.engine != gen {
		return nil, "", true
	}
	run.smu.Lock()
	defer run.smu.Unlock()
	switch {
	case run.closed:
		return nil, "the run's via_org egress is closed", false
	case len(run.streams) >= viaOrgMaxStreams:
		return nil, "the run already has 16 via_org streams open", false
	case run.bytes.Load() >= run.byteCap:
		return nil, "the run has moved its 8 GiB of via_org traffic", false
	}
	ctx, cancel := context.WithCancel(parent)
	st = &viaOrgStream{ctx: ctx, cancel: cancel, run: run, gen: gen}
	st.last.Store(time.Now().UnixNano())
	run.streams[st] = struct{}{}
	gen.active.Add(1)
	return st, "", false
}

func (run *viaOrgRun) release(st *viaOrgStream) {
	st.cancel()
	run.smu.Lock()
	delete(run.streams, st)
	if len(run.streams) == 0 {
		run.idleSince = time.Now()
	}
	run.smu.Unlock()
	st.gen.active.Done()
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

// watchViaOrgRun keeps the run's state until the run ends. A stream idle past the timeout is ended;
// while the run is away (kept, or its runner offline) every stream is ended and the engine released,
// and so is an engine idle with no stream; the run's byte total is kept through all of these. The
// state, and with it the total, is dropped only when the run ends (design §7.3: the bounds are per run).
func (s *Server) watchViaOrgRun(run *viaOrgRun) {
	t := time.NewTicker(run.period)
	defer t.Stop()
	for {
		select {
		case <-run.done:
			return
		case <-s.cfg.BaseCtx.Done():
			s.releaseViaOrgRun(run, true, false)
			return
		case <-t.C:
		}
		switch s.viaOrgRunState(run) {
		case viaOrgRunEnded:
			s.releaseViaOrgRun(run, true, true)
			return
		case viaOrgRunMoved:
			s.releaseViaOrgRun(run, true, false)
			return
		case viaOrgRunAway:
			s.releaseViaOrgRun(run, false, false)
		default:
			if run.closeIdle(time.Now()) {
				s.releaseViaOrgRun(run, false, false)
			}
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

type viaOrgRunStatus int

const (
	viaOrgRunLive  viaOrgRunStatus = iota
	viaOrgRunAway                  // kept, its runner offline, or the run unreadable: nothing may stream
	viaOrgRunMoved                 // placed on another runner: this runner's state is dropped
	viaOrgRunEnded                 // terminal or gone: the state and the run's byte total are dropped
)

// viaOrgRunState reads where the run stands for its via_org egress.
func (s *Server) viaOrgRunState(run *viaOrgRun) viaOrgRunStatus {
	got, err := s.cfg.Store.GetRun(s.cfg.BaseCtx, run.id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return viaOrgRunEnded
	case err != nil:
		return viaOrgRunAway
	case isTerminalRunState(got.State):
		return viaOrgRunEnded
	case got.RunnerID == nil || *got.RunnerID != run.runner:
		return viaOrgRunMoved
	case runIsKept(got) || (s.cfg.RunnerOnline != nil && !s.cfg.RunnerOnline(run.runner)):
		return viaOrgRunAway
	}
	return viaOrgRunLive
}

// releaseViaOrgRun cancels every open stream of the run and releases its engine. final also drops
// the run's state; retire, only for a run that has ended, drops its byte total too.
func (s *Server) releaseViaOrgRun(run *viaOrgRun, final, retire bool) {
	// Retire the generation first: from here no stream can be admitted to it.
	run.mu.Lock()
	gen := run.engine
	run.engine = nil
	run.mu.Unlock()
	if gen != nil {
		go closeViaOrgGen(run.id, gen)
	}
	run.smu.Lock()
	if run.closed {
		run.smu.Unlock()
		return
	}
	if final {
		run.closed = true
		close(run.done)
		s.viaOrg.mu.Lock()
		if s.viaOrg.runs[run.id] == run {
			delete(s.viaOrg.runs, run.id)
		}
		if retire {
			delete(s.viaOrg.spent, run.id)
		}
		s.viaOrg.mu.Unlock()
	}
	for st := range run.streams {
		st.cancel()
	}
	run.smu.Unlock()
}

// closeViaOrgGen closes a retired generation's engine once every stream admitted to it has returned
// (they were cancelled): its sink and masking corpus stay alive for their last producer, and the
// engine keeps them until its sink has posted every queued row (proxy.Server.Shutdown).
func closeViaOrgGen(runID uuid.UUID, gen *viaOrgGen) {
	gen.active.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := gen.engine.Close(ctx); err != nil {
		slog.Warn("api: close a via_org engine", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// endViaOrgRunner ends every open via_org stream of a runner that went offline; each run keeps its
// state and byte total for when the runner is back.
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
		s.releaseViaOrgRun(run, false, false)
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
