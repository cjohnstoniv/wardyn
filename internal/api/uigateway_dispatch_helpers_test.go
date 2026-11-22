// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dispatchStart is one start the dispatch tests' runner saw: "thaw", "sh" (the app probe), "socat" (the relay).
type dispatchStart struct {
	kind string
	at   time.Time
}

// dispatchH is one relay stdin write as the fake driver saw it: the time it returned, an upper bound for its
// bytes' hand-off, and how many bytes it took.
type dispatchH struct {
	at time.Time
	n  int
}

// dispatchRunner is sshFakeRunner with every start recorded, a stall hook per start kind, a thaw, and each
// relay exec's stdin writes recorded at the driver.
type dispatchRunner struct {
	*sshFakeRunner
	mu     sync.Mutex
	starts []dispatchStart
	hs     []dispatchH
	reads  []time.Time // the app's receipt, as the backend read it
	// hook, when set for a kind, runs before that start with its context; an error refuses the start.
	hook   map[string]func(ctx context.Context) error
	closes int
}

func (r *dispatchRunner) record(kind string) {
	r.mu.Lock()
	r.starts = append(r.starts, dispatchStart{kind, time.Now()})
	r.mu.Unlock()
}

func (r *dispatchRunner) hookFor(kind string) func(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hook[kind]
}

func (r *dispatchRunner) setHook(kind string, f func(context.Context) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hook == nil {
		r.hook = map[string]func(context.Context) error{}
	}
	r.hook[kind] = f
}

func (r *dispatchRunner) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	kind := spec.Argv[0]
	if len(spec.Argv) > 1 && spec.Argv[1] == "forward" {
		kind = "socat"
	}
	r.record(kind)
	if h := r.hookFor(kind); h != nil {
		if err := h(ctx); err != nil {
			return nil, err
		}
	}
	sess, err := r.sshFakeRunner.ExecStream(ctx, ref, spec)
	if err != nil || kind != "socat" || sess.Stdin == nil {
		return sess, err
	}
	in, closeFn := sess.Stdin, sess.Close
	sess.Stdin = dispatchStdin{WriteCloser: in, r: r}
	sess.Close = func() error {
		r.mu.Lock()
		r.closes++
		r.mu.Unlock()
		return closeFn()
	}
	return sess, nil
}

type dispatchStdin struct {
	io.WriteCloser
	r *dispatchRunner
}

func (s dispatchStdin) Write(p []byte) (int, error) {
	n, err := s.WriteCloser.Write(p)
	s.r.mu.Lock()
	s.r.hs = append(s.r.hs, dispatchH{time.Now(), n})
	s.r.mu.Unlock()
	return n, err
}

func (r *dispatchRunner) FreezeSandbox(context.Context, string) error { return nil }

func (r *dispatchRunner) ThawSandbox(ctx context.Context, _ string) error {
	r.record("thaw")
	if h := r.hookFor("thaw"); h != nil {
		return h(ctx)
	}
	return nil
}

// startsAtOrAfter lists the starts of kind (any kind for "") at or after by.
func (r *dispatchRunner) startsAtOrAfter(kind string, by time.Time) []dispatchStart {
	r.mu.Lock()
	defer r.mu.Unlock()
	var late []dispatchStart
	for _, s := range r.starts {
		if (kind == "" || s.kind == kind) && !s.at.Before(by) {
			late = append(late, s)
		}
	}
	return late
}

func (r *dispatchRunner) count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := 0
	for _, s := range r.starts {
		if s.kind == kind {
			c++
		}
	}
	return c
}

// bytesAtOrAfter is the bytes the relay's driver took at or after by, and in all.
func (r *dispatchRunner) bytesAtOrAfter(by time.Time) (late, all int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hs {
		all += h.n
		if !h.at.Before(by) {
			late += h.n
		}
	}
	return late, all
}

// pausingStore is the issuer rig's store as a run pauser, so a paused run is thawed through the runner.
type pausingStore struct {
	issuerPGStore
	paused bool
	getRun func(ctx context.Context) // when set, runs before each GetRun
	touch  func(ctx context.Context) // when set, runs before each TouchRun
}

func (p pausingStore) TouchRun(ctx context.Context, id uuid.UUID) error {
	if p.touch != nil {
		p.touch(ctx)
	}
	return p.issuerPGStore.TouchRun(ctx, id)
}

func (p pausingStore) ListPauseCandidates(context.Context) ([]store.PauseCandidate, time.Time, error) {
	return nil, time.Now(), nil
}
func (p pausingStore) StampRunActive(context.Context, uuid.UUID) (bool, error) { return p.paused, nil }
func (p pausingStore) MarkRunPaused(context.Context, uuid.UUID, types.PauseReason, *time.Time) (bool, error) {
	return false, nil
}
func (p pausingStore) ClearRunPaused(context.Context, uuid.UUID) (bool, error)    { return true, nil }
func (p pausingStore) RunHasOpenRequest(context.Context, uuid.UUID) (bool, error) { return false, nil }

func (p pausingStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if p.getRun != nil {
		p.getRun(ctx)
	}
	return p.issuerPGStore.GetRun(ctx, id)
}

// dispatchRig is an issuer portal's relay with uiDispatchFresh shortened and the recording runner installed.
type dispatchRig struct {
	*issuerPGRig
	dr    *dispatchRunner
	fresh time.Duration
}

// dispatchFresh is the shortened dispatch bound these tests run under.
const dispatchFresh = 1500 * time.Millisecond

func newDispatchRig(t *testing.T, backend http.Handler) *dispatchRig {
	t.Helper()
	rig := newIssuerPGRig(t, backend)
	dr := &dispatchRunner{sshFakeRunner: rig.runner}
	rig.srv.cfg.Runner = dr
	setDispatchFresh(t, dispatchFresh)
	return &dispatchRig{issuerPGRig: rig, dr: dr, fresh: dispatchFresh}
}

// setDispatchFresh shortens uiDispatchFresh for one test.
func setDispatchFresh(t *testing.T, d time.Duration) {
	t.Helper()
	old := uiDispatchFresh
	uiDispatchFresh = d
	t.Cleanup(func() { uiDispatchFresh = old })
}

// pausing makes the rig's store a run pauser, its run paused when paused (so the request thaws it), with
// getRun run before each GetRun.
func (rig *dispatchRig) pausing(paused bool, getRun func(context.Context)) {
	ps := pausingStore{issuerPGStore: rig.srv.cfg.Store.(issuerPGStore), paused: paused, getRun: getRun}
	if paused {
		now := time.Now().UTC()
		rig.run.PausedAt, rig.run.PausedReason = &now, types.PauseIdle
		rig.store.putRun(rig.run)
	}
	rig.srv.cfg.Store = ps
}

// inStack reports whether fn is a frame of the calling goroutine's stack.
func inStack(fn string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "."+fn) || strings.Contains(f.Function, "."+fn+"[") {
			return true
		}
		if !more {
			return false
		}
	}
}

// dispatchReason is a refusal body's reason.
func dispatchReason(rec *httptest.ResponseRecorder) string {
	var b errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.Reason
}

// stall blocks until release closes or ctx ends, signalling entered first.
func stall(entered chan<- struct{}, release <-chan struct{}) func(context.Context) error {
	var once sync.Once
	return func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
