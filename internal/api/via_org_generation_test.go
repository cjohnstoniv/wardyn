// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// viaOrgSlotStore is the local dispatch store holding the run's policy generations as agent_runs does. The via_org
// path only reads them; advance and abandon move them as a revive's writes do and leave the stored config alone, as
// a restart with no egress edit stores the config it started from.
type viaOrgSlotStore struct {
	localDispatchStore
	gmu     sync.Mutex
	state   types.RunPolicyState
	readErr error
	reads   atomic.Int32
}

var _ store.RunPolicyGenerations = (*viaOrgSlotStore)(nil)

func (s *viaOrgSlotStore) RunPolicyState(context.Context, uuid.UUID) (types.RunPolicyState, bool, error) {
	s.reads.Add(1)
	s.gmu.Lock()
	defer s.gmu.Unlock()
	return s.state, true, s.readErr
}

func (s *viaOrgSlotStore) AdvanceRunPolicyGeneration(context.Context, uuid.UUID, store.PolicySlot, string, []string, string, []byte) (int, error) {
	return 0, errors.New("the via_org path never advances a generation")
}

func (s *viaOrgSlotStore) AbandonRunPolicyGeneration(context.Context, uuid.UUID, int, []byte) error {
	return errors.New("the via_org path never abandons a generation")
}

// advance is a restart's generation write.
func (s *viaOrgSlotStore) advance() {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	s.state, _ = s.state.Advance("sha256:restart", nil, "")
}

// abandon marks the newest generation abandoned, as a revive that never replaced the proxy leaves it.
func (s *viaOrgSlotStore) abandon() {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	s.state.Generations[len(s.state.Generations)-1].Abandoned = true
}

// viaOrgSlotFixture is a local run whose organisation-side config is stored, on a store holding its policy generations.
func viaOrgSlotFixture(t *testing.T) (*Server, *viaOrgSlotStore, types.AgentRun) {
	t.Helper()
	srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
	spec := localOrgNetworkSpec(run.ID)
	spec.ProxyConfig.ControlPlaneURL = "http://127.0.0.1:9"
	if !srv.classifyLocalDispatch(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()),
		types.SiteConfig{}, &spec, nil, llmTransport{}, false) {
		t.Fatal("an all-own plan was refused")
	}
	gs := &viaOrgSlotStore{localDispatchStore: ls}
	srv.cfg.Store = gs
	return srv, gs, run
}

// newViaOrgSlotRun is one replica's via_org state for the run, with no watcher; its engine closes at cleanup.
func newViaOrgSlotRun(t *testing.T, runID uuid.UUID) *viaOrgRun {
	t.Helper()
	run := &viaOrgRun{id: runID, runner: uuid.New(), bytes: new(atomic.Int64), done: make(chan struct{}),
		streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(), byteCap: viaOrgMaxBytes, idle: time.Hour, period: time.Hour}
	t.Cleanup(func() {
		run.mu.Lock()
		gen := run.engine
		run.engine = nil
		run.mu.Unlock()
		if gen != nil {
			closeViaOrgGen(runID, gen)
		}
	})
	return run
}

// viaOrgEngineOf is the replica's engine for the run, as admitViaOrg takes it.
func viaOrgEngineOf(t *testing.T, s *Server, run *viaOrgRun) *viaOrgGen {
	t.Helper()
	gen, _, err := s.viaOrgEngine(context.Background(), run, run.id)
	if err != nil {
		t.Fatal(err)
	}
	return gen
}

// A restart with no egress edit moves the run's policy generation and re-applies the owner's walls, so an engine
// built at the earlier generation is never served again: the next request retires it, ends its streams, and builds
// from the config as it stands. At the current generation the engine is reused, request after request.
func TestViaOrg_RestartRetiresTheEngineBuiltAtTheEarlierGeneration(t *testing.T) {
	srv, gs, run := viaOrgSlotFixture(t)
	vr := newViaOrgSlotRun(t, run.ID)
	first := viaOrgEngineOf(t, srv, vr)
	for range 3 {
		if viaOrgEngineOf(t, srv, vr) != first {
			t.Fatal("the engine at the run's current generation was rebuilt for a later request")
		}
	}
	st, refused, retired := vr.open(context.Background(), first)
	if st == nil || refused != "" || retired {
		t.Fatalf("open = %v %q retired=%v, want a stream", st, refused, retired)
	}

	gs.advance()
	second := viaOrgEngineOf(t, srv, vr)
	if second == first {
		t.Fatal("the engine built before the restart's generation is still served after it")
	}
	if st.ctx.Err() == nil {
		t.Fatal("a stream on the engine built before the restart was not ended")
	}
	vr.release(st)
	waitFor(t, "the earlier engine to close", first.engine.Closed)
	if viaOrgEngineOf(t, srv, vr) != second {
		t.Fatal("the engine at the new generation was rebuilt for a later request")
	}

	// An abandon puts the revive's source config back without moving the number: that slot is newer too.
	gs.abandon()
	if viaOrgEngineOf(t, srv, vr) == second {
		t.Fatal("the engine built at a generation since abandoned is still served")
	}
}

// With several replicas, the one that handled the restart releases its own engine, and the one holding the runner's
// relay never hears of it: that replica's engine, built at the earlier generation, is retired at its next request,
// which reads the run's newer generation from the shared store.
func TestViaOrg_AnotherReplicaRebuildsAtTheNewGeneration(t *testing.T) {
	srv, gs, run := viaOrgSlotFixture(t)
	other := New(srv.cfg) // another replica over the same database
	vr := newViaOrgSlotRun(t, run.ID)
	before := viaOrgEngineOf(t, other, vr)

	gs.advance()
	after := viaOrgEngineOf(t, other, vr)
	if after == before {
		t.Fatal("another replica still serves the engine it built before the restart's generation")
	}
	waitFor(t, "the other replica's earlier engine to close", before.engine.Closed)
	if viaOrgEngineOf(t, other, vr) != after {
		t.Fatal("the other replica rebuilt its engine again at the same generation")
	}
}

// On a replica that gets no new request, the watch retires an engine whose generation moved: its next tick ends the
// stream still open on the engine built before the restart. A generation that then stays unreadable ends the next
// engine's stream too, once two reads in a row have failed, as two failed reads of the run do.
func TestViaOrg_WatchEndsTheStreamOfAnEngineWhoseGenerationMoved(t *testing.T) {
	srv, gs, run := viaOrgSlotFixture(t)
	vr := newViaOrgSlotRun(t, run.ID)
	ds := gs.dispatchTestStore
	ds.mu.Lock()
	ds.run.RunnerID, ds.state = &vr.runner, types.RunRunning
	ds.mu.Unlock()
	first := viaOrgEngineOf(t, srv, vr)
	st, refused, retired := vr.open(context.Background(), first)
	if st == nil || refused != "" || retired {
		t.Fatalf("open = %v %q retired=%v, want a stream", st, refused, retired)
	}
	vr.period = 2 * time.Millisecond
	srv.cfg.RunnerOnline = func(uuid.UUID) bool { return true } // the run's runner is connected to this replica
	go srv.watchViaOrgRun(vr)
	defer srv.releaseViaOrgRun(vr, true, false)

	from := gs.reads.Load()
	waitFor(t, "the watch to read the generation", func() bool { return gs.reads.Load() >= from+3 })
	if st.ctx.Err() != nil {
		t.Fatal("the watch ended a stream on an engine at the run's current generation")
	}
	gs.advance()
	waitFor(t, "the watch to end the stream of the engine built before the restart", func() bool { return st.ctx.Err() != nil })
	vr.release(st)
	waitFor(t, "the earlier engine to close", first.engine.Closed)
	if vr.isClosed() {
		t.Fatal("retiring a stale engine dropped the run's via_org state")
	}

	second := viaOrgEngineOf(t, srv, vr)
	st2, refused, retired := vr.open(context.Background(), second)
	if st2 == nil || refused != "" || retired {
		t.Fatalf("open = %v %q retired=%v, want a stream", st2, refused, retired)
	}
	defer vr.release(st2)
	gs.gmu.Lock()
	gs.readErr = errors.New("connection reset")
	gs.gmu.Unlock()
	waitFor(t, "the watch to end the stream once the generation stays unreadable", func() bool { return st2.ctx.Err() != nil })
}

// Requests racing the rebuild after a restart build one engine between them, and none is served the earlier one.
func TestViaOrg_RacingRequestsBuildOneEngineAtTheNewGeneration(t *testing.T) {
	srv, gs, run := viaOrgSlotFixture(t)
	vr := newViaOrgSlotRun(t, run.ID)
	first := viaOrgEngineOf(t, srv, vr)
	gs.advance()
	got := make([]*viaOrgGen, 8)
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() {
			g, _, err := srv.viaOrgEngine(context.Background(), vr, run.ID)
			if err != nil {
				t.Error(err)
			}
			got[i] = g
		})
	}
	wg.Wait()
	for i, g := range got {
		if g == nil || g == first || g != got[0] {
			t.Fatalf("request %d took engine %p; want the one rebuilt engine %p, never the earlier %p", i, g, got[0], first)
		}
	}
}

// A generation that cannot be read serves no engine, not even the cached one: whether it is current is unknown.
func TestViaOrg_UnreadableGenerationServesNoEngine(t *testing.T) {
	srv, gs, run := viaOrgSlotFixture(t)
	vr := newViaOrgSlotRun(t, run.ID)
	viaOrgEngineOf(t, srv, vr)
	gs.gmu.Lock()
	gs.readErr = errors.New("connection reset")
	gs.gmu.Unlock()
	if g, _, err := srv.viaOrgEngine(context.Background(), vr, run.ID); err == nil || g != nil {
		t.Fatalf("viaOrgEngine = %p, %v with the generation unreadable; want no engine and an error", g, err)
	}
}

// A slot read before a newer engine was built is not newer than that engine's, so it keeps it; any later slot,
// an abandon's included, is newer.
func TestViaOrg_SlotOrderKeepsTheNewerEngine(t *testing.T) {
	for _, tc := range []struct {
		read, built store.PolicySlot
		want        bool
	}{
		{store.PolicySlot{Generation: 2}, store.PolicySlot{Generation: 1}, true},
		{store.PolicySlot{Generation: 1, Abandoned: true}, store.PolicySlot{Generation: 1}, true},
		{store.PolicySlot{Generation: 3}, store.PolicySlot{Generation: 2, Abandoned: true}, true},
		{store.PolicySlot{Generation: 1}, store.PolicySlot{Generation: 1}, false},
		{store.PolicySlot{Generation: 1}, store.PolicySlot{Generation: 2}, false},
		{store.PolicySlot{Generation: 2}, store.PolicySlot{Generation: 2, Abandoned: true}, false},
		{store.PolicySlot{}, store.PolicySlot{}, false},
	} {
		if got := viaOrgSlotNewer(tc.read, tc.built); got != tc.want {
			t.Errorf("viaOrgSlotNewer(%+v, %+v) = %v, want %v", tc.read, tc.built, got, tc.want)
		}
	}
}

// A restart that is not an edit releases this replica's cached via_org engine before it answers, as an edit does
// (TestEgressEdit_RetiresTheViaOrgEngineBeforeTheAnswer): the restart stored the run's config in its generation's
// write, with the owner's walls re-applied, so no engine built before it serves again.
func TestViaOrg_RestartRetiresThisReplicasEngineBeforeTheAnswer(t *testing.T) {
	f := newEgressEditFixture(t)
	eng, err := proxy.NewViaOrgEngine(context.Background(),
		&proxy.Config{RunID: f.run.ID, ControlPlaneURL: "http://127.0.0.1:9", RunToken: "run-token", DecisionBufferSize: 16},
		proxy.ViaOrgEngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vr := &viaOrgRun{id: f.run.ID, runner: uuid.New(), done: make(chan struct{}), streams: map[*viaOrgStream]struct{}{},
		bytes: new(atomic.Int64), engine: &viaOrgGen{engine: eng}}
	f.srv.viaOrg.mu.Lock()
	f.srv.viaOrg.runs = map[uuid.UUID]*viaOrgRun{f.run.ID: vr}
	f.srv.viaOrg.mu.Unlock()
	f.confirmEachReplace(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: %d, want 200", code)
	}
	vr.mu.Lock()
	retired := vr.engine == nil
	vr.mu.Unlock()
	if !retired {
		t.Fatal("the cached via_org engine built before the restart is still served after the restart's 200")
	}
	waitFor(t, "the released engine to close", eng.Closed)
}
