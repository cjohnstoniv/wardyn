// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// genStopRunner is the lost fixture's runner that also stops by policy generation: proxyGen is the
// generation of the proxy that is running, and a stop that names an older one leaves it (as the Docker
// driver does). Plain stops are counted by the embedded runner.
type genStopRunner struct {
	*lostRunner
	gmu      sync.Mutex
	proxyGen int
	removed  bool
	calls    []string
}

func (r *genStopRunner) stopAt(kind string, gen int) error {
	r.gmu.Lock()
	defer r.gmu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("%s@%d", kind, gen))
	if gen < r.proxyGen {
		return runner.ErrProxyGenerationNewer
	}
	r.removed = true
	return nil
}

func (r *genStopRunner) StopProxyAt(_ context.Context, _ string, gen int) error {
	return r.stopAt("stop", gen)
}

func (r *genStopRunner) EndSandboxAt(_ context.Context, _ string, gen int) error {
	return r.stopAt("end", gen)
}

func (r *genStopRunner) atCalls() []string {
	r.gmu.Lock()
	defer r.gmu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *genStopRunner) proxyRemoved() bool {
	r.gmu.Lock()
	defer r.gmu.Unlock()
	return r.removed
}

// genLostStore is lostStore that marks a run lost at a generation as the database does, and can fail the mark.
type genLostStore struct {
	*lostStore
	markErr error
}

func (s *genLostStore) MarkRunLostAtGeneration(ctx context.Context, id uuid.UUID, _ int, reason types.LostReason, now time.Time, tokenLife time.Duration) (bool, error) {
	if s.markErr != nil {
		return false, s.markErr
	}
	return s.lostStore.MarkRunLost(ctx, id, reason, now, tokenLife)
}

func (s *genLostStore) RunLostFate(context.Context, uuid.UUID) (store.RunLostFate, error) {
	return store.RunLostFate{}, nil
}

func (s *genLostStore) RestoreRunLostFateAtGeneration(context.Context, uuid.UUID, int, store.RunLostFate) (bool, error) {
	return false, nil
}

// newGenStopFixture is a lost fixture whose runner stops by generation and whose running proxy belongs to
// generation proxyGen.
func newGenStopFixture(t *testing.T, proxyGen int) (*lostFixture, *genStopRunner, *genLostStore) {
	t.Helper()
	f := newLostFixture(t)
	gr := &genStopRunner{lostRunner: f.lr, proxyGen: proxyGen}
	gs := &genLostStore{lostStore: f.ls}
	f.srv.cfg.Runner = gr
	f.srv.cfg.Store = gs
	return f, gr, gs
}

// eachProxyGeneration runs body for both outcomes of a fenced stop: a newer revive's proxy (gen 4) survives a stop that names gen 3, and
// a proxy of the same generation is still stopped.
func eachProxyGeneration(t *testing.T, body func(t *testing.T, proxyGen int, survives bool)) {
	t.Helper()
	for _, c := range []struct {
		name     string
		proxyGen int
		survives bool
	}{{"newer proxy survives", 4, true}, {"same generation is stopped", 3, false}} {
		t.Run(c.name, func(t *testing.T) { body(t, c.proxyGen, c.survives) })
	}
}

func assertFencedStop(t *testing.T, f *lostFixture, gr *genStopRunner, survives bool) {
	t.Helper()
	if got := gr.atCalls(); len(got) != 1 || got[0] != "stop@3" {
		t.Errorf("generation stops = %v, want [stop@3]", got)
	}
	if n := f.lr.proxyStopCount(); n != 0 {
		t.Errorf("plain StopProxy = %d, want 0: the stop must carry its generation", n)
	}
	if gr.proxyRemoved() == survives {
		t.Errorf("proxy removed = %v, want %v", gr.proxyRemoved(), !survives)
	}
}

// A failed proxy start of generation 3 whose lost mark cannot be written still stops the proxy, and that
// stop names generation 3: the newer revive's proxy (gen 4) survives. Catches a plain StopProxy.
func TestLoseWriteFailed_StopIsGenerationFenced(t *testing.T) {
	eachProxyGeneration(t, func(t *testing.T, proxyGen int, survives bool) {
		f, gr, gs := newGenStopFixture(t, proxyGen)
		gs.markErr = errors.New("database blip")
		handled, err := f.srv.loseUnconfirmedRun(context.Background(), f.run, 3)
		if !handled || !errors.Is(err, errLostMarkUnwritten) {
			t.Fatalf("loseUnconfirmedRun = %v, %v; want handled, errLostMarkUnwritten", handled, err)
		}
		assertFencedStop(t, f, gr, survives)
	})
}

// A revive that failed after its claim and cannot write the lost mark stops the proxy under its own
// generation. Catches a plain StopProxy at run_revive_after_claim.go.
func TestRelose_WriteFailedStopIsGenerationFenced(t *testing.T) {
	eachProxyGeneration(t, func(t *testing.T, proxyGen int, survives bool) {
		f, gr, gs := newGenStopFixture(t, proxyGen)
		gs.markErr = errors.New("database blip")
		fate, err := f.srv.reloseRun(context.Background(), f.run, 3)
		if fate != relostUnwritten || err != nil {
			t.Fatalf("reloseRun = %v, %v; want relostUnwritten, nil", fate, err)
		}
		assertFencedStop(t, f, gr, survives)
	})
}

// The stop after a lost mark that landed names the generation the caller acts for, so a newer revive that
// took the run between the mark and the stop keeps its proxy. Catches a plain stopLostSandbox at run_lost.go.
func TestPostMarkStop_NewerRevivesProxySurvives(t *testing.T) {
	eachProxyGeneration(t, func(t *testing.T, proxyGen int, survives bool) {
		f, gr, gs := newGenStopFixture(t, proxyGen)
		o, stopErr := f.srv.loseRunStopped(context.Background(), gs, gs, f.run, types.LostOutage, types.RunFailed, 0, f.srv.cfg.Now(), 3)
		if o != loseApplied || stopErr != nil {
			t.Fatalf("loseRunStopped = %v, %v; want loseApplied, nil", o, stopErr)
		}
		assertFencedStop(t, f, gr, survives)
	})
}

// The retry of a kept run's unconfirmed stop names its generation. Catches a plain stopLostSandbox at
// run_proxy_readback.go.
func TestKeptRunContainmentRetry_IsGenerationFenced(t *testing.T) {
	eachProxyGeneration(t, func(t *testing.T, proxyGen int, survives bool) {
		f, gr, _ := newGenStopFixture(t, proxyGen)
		run := f.run
		at := f.srv.cfg.Now()
		run.LostAt, run.LostReason = &at, types.LostOutage
		if err := f.srv.stopUnconfirmedProxyAt(context.Background(), run, 3); err != nil {
			t.Fatalf("stopUnconfirmedProxyAt: %v", err)
		}
		assertFencedStop(t, f, gr, survives)
	})
}

// A stop that finds a newer revive's proxy is not a failure (it would block RecordProxyContainment): nil, one
// info line, for the proxy-only stop and the end alike.
func TestStopLostSandboxAt_NewerIsNil(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason types.LostReason
		want   string
	}{{"proxy only", types.LostOutage, "stop@3"}, {"end", types.LostEnded, "end@3"}} {
		t.Run(c.name, func(t *testing.T) {
			f, gr, _ := newGenStopFixture(t, 4)
			var buf bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			run := f.run
			now := f.srv.cfg.Now()
			run.LostAt, run.LostReason = &now, c.reason
			if c.reason == types.LostEnded {
				past := now.Add(-time.Minute)
				run.EndsAt = &past
			}
			if err := f.srv.stopLostSandboxAt(context.Background(), run, now, 3); err != nil {
				t.Fatalf("stopLostSandboxAt = %v, want nil: a newer revive owns the proxy", err)
			}
			if got := gr.atCalls(); len(got) != 1 || got[0] != c.want {
				t.Errorf("generation stops = %v, want [%s]", got, c.want)
			}
			out := buf.String()
			if n := strings.Count(out, "a newer revive owns the run's proxy"); n != 1 || !strings.Contains(out, "level=INFO") {
				t.Errorf("log = %q; want one info line", out)
			}
		})
	}
}

// Generation 0 is the plain stop, whatever the runner can do.
func TestStopLostSandboxAt_ZeroIsUnchanged(t *testing.T) {
	f, gr, _ := newGenStopFixture(t, 4)
	run := f.run
	now := f.srv.cfg.Now()
	run.LostAt, run.LostReason = &now, types.LostOutage
	if err := f.srv.stopLostSandboxAt(context.Background(), run, now, 0); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Minute)
	run.EndsAt, run.LostReason = &past, types.LostEnded
	if err := f.srv.stopLostSandboxAt(context.Background(), run, now, 0); err != nil {
		t.Fatal(err)
	}
	if got := gr.atCalls(); len(got) != 0 {
		t.Errorf("generation stops = %v, want none at gen 0", got)
	}
	if f.lr.proxyStopCount() != 1 || f.lr.endCount() != 1 {
		t.Errorf("plain StopProxy = %d, EndSandbox = %d; want 1 and 1", f.lr.proxyStopCount(), f.lr.endCount())
	}
}

// A run on a runner that is not connected is stopped the plain way (which queues), however its last caps
// read; connected with ProxyFence it is stopped by generation; connected without it, plain again.
func TestStopLostSandboxAt_UnconnectedRunnerUsesPlainStop(t *testing.T) {
	f, gr, _ := newGenStopFixture(t, 4)
	rid := uuid.New()
	var online atomic.Bool
	f.srv.cfg.RunnerOnline = func(uuid.UUID) bool { return online.Load() }
	seamConnect(t, f.srv, rid, runnerwire.Caps{ProxyFence: true})
	run := f.run
	run.RunnerID = &rid
	now := f.srv.cfg.Now()
	run.LostAt, run.LostReason = &now, types.LostOutage
	stop := func() {
		t.Helper()
		if err := f.srv.stopLostSandboxAt(context.Background(), run, now, 3); err != nil {
			t.Fatal(err)
		}
	}

	stop() // stale caps with ProxyFence, session not live
	if got := gr.atCalls(); len(got) != 0 || f.lr.proxyStopCount() != 1 {
		t.Fatalf("not connected: generation stops %v, plain %d; want none and 1", got, f.lr.proxyStopCount())
	}
	online.Store(true)
	stop()
	if got := gr.atCalls(); len(got) != 1 || got[0] != "stop@3" || f.lr.proxyStopCount() != 1 {
		t.Fatalf("connected with ProxyFence: generation stops %v, plain %d; want [stop@3] and 1", got, f.lr.proxyStopCount())
	}
	seamConnect(t, f.srv, rid, runnerwire.Caps{})
	stop()
	if got := gr.atCalls(); len(got) != 1 || f.lr.proxyStopCount() != 2 {
		t.Fatalf("connected without ProxyFence: generation stops %v, plain %d; want 1 and 2", got, f.lr.proxyStopCount())
	}
	vm := run
	vm.MachineKind = types.MachineLinuxVM
	seamConnect(t, f.srv, rid, runnerwire.Caps{ProxyFence: true})
	if err := f.srv.stopLostSandboxAt(context.Background(), vm, now, 3); err != nil {
		t.Fatal(err)
	}
	if got := gr.atCalls(); len(got) != 1 || f.lr.proxyStopCount() != 3 {
		t.Fatalf("a VM: generation stops %v, plain %d; want 1 and 3", got, f.lr.proxyStopCount())
	}
}
