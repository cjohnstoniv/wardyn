// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fixedRunStore answers GetRun with one run and one error.
type fixedRunStore struct {
	store.Store
	run types.AgentRun
	err error
}

func (f fixedRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return f.run, f.err
}

// A run's state for its via_org egress: a paused run is away, like a kept run or one whose runner is offline.
func TestViaOrgRunState_PausedIsAway(t *testing.T) {
	mine := uuid.New()
	other := uuid.New()
	now := time.Now()
	running := types.AgentRun{State: types.RunRunning, RunnerID: &mine}
	with := func(f func(*types.AgentRun)) types.AgentRun { r := running; f(&r); return r }
	offline := func(uuid.UUID) bool { return false }
	for _, tc := range []struct {
		name   string
		run    types.AgentRun
		err    error
		online func(uuid.UUID) bool
		want   viaOrgRunStatus
	}{
		{"live", running, nil, nil, viaOrgRunLive},
		{"paused", with(func(r *types.AgentRun) { r.PausedAt = &now }), nil, nil, viaOrgRunAway},
		{"kept", with(func(r *types.AgentRun) { r.LostAt = &now }), nil, nil, viaOrgRunAway},
		{"runner offline", running, nil, offline, viaOrgRunAway},
		{"moved", with(func(r *types.AgentRun) { r.RunnerID = &other }), nil, nil, viaOrgRunMoved},
		{"moved while paused", with(func(r *types.AgentRun) { r.RunnerID, r.PausedAt = &other, &now }), nil, nil, viaOrgRunMoved},
		{"terminal", with(func(r *types.AgentRun) { r.State = types.RunCompleted }), nil, nil, viaOrgRunEnded},
		{"terminal while paused", with(func(r *types.AgentRun) { r.State, r.PausedAt = types.RunCompleted, &now }), nil, nil, viaOrgRunEnded},
		{"gone", types.AgentRun{}, store.ErrNotFound, nil, viaOrgRunEnded},
		{"unreadable", types.AgentRun{}, errors.New("connection reset"), nil, viaOrgRunUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{Store: fixedRunStore{run: tc.run, err: tc.err}, BaseCtx: context.Background(), RunnerOnline: tc.online}}
			if got := s.viaOrgRunState(&viaOrgRun{id: uuid.New(), runner: mine}); got != tc.want {
				t.Errorf("viaOrgRunState = %d, want %d", got, tc.want)
			}
		})
	}
}

// A stream open when its run is paused ends on the watcher's next tick, and the engine is released; the run keeps its
// state and byte total. Once the run is resumed its next request is served by a fresh engine.
func TestViaOrg_PausedRunEndsItsOpenStreamsAndServesAfterResume(t *testing.T) {
	srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
	gh := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryViaOrg, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}}
	ls.dispatchTestStore.grants = []types.CredentialGrant{gh}
	ctx := context.Background()
	if err := srv.keepRunProxyConfig(ctx, run.ID, runner.ProxyConfig{RunToken: "run-token", ControlPlaneURL: "http://127.0.0.1:9",
		ViaOrgRoutes: []proxy.ViaOrgRoute{{GrantID: gh.ID, Host: githubHost}}}); err != nil {
		t.Fatal(err)
	}
	runnerID := uuid.New()
	ds := ls.dispatchTestStore
	ds.mu.Lock()
	ds.run.RunnerID = &runnerID
	ds.mu.Unlock()
	setPaused := func(paused bool) {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		ds.run.PausedAt = nil
		if paused {
			now := time.Now()
			ds.run.PausedAt = &now
		}
	}
	// The watcher asks RunnerOnline only of a run it read as neither paused nor kept, so the count is of live reads.
	var liveReads atomic.Int32
	srv.cfg.RunnerOnline = func(uuid.UUID) bool { liveReads.Add(1); return true }
	srv.cfg.BaseCtx = ctx
	srv.bgWaitBudget = 10 * time.Second

	state := &viaOrgRun{id: run.ID, runner: runnerID, bytes: new(atomic.Int64), done: make(chan struct{}),
		streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(), engine: newIdleTestGen(t, run.ID),
		routes:  []proxy.ViaOrgRoute{{GrantID: gh.ID, Host: githubHost}},
		byteCap: viaOrgMaxBytes, idle: time.Hour, period: 2 * time.Millisecond}
	first := state.engine
	srv.viaOrg.runs = map[uuid.UUID]*viaOrgRun{run.ID: state}
	state.bytes.Store(7)
	st, refused, retired := state.open(ctx, first)
	if st == nil || refused != "" || retired {
		t.Fatalf("open = %v %q retired=%v, want a stream", st, refused, retired)
	}
	go srv.watchViaOrgRun(state)
	defer srv.releaseViaOrgRun(state, true, false)

	waitFor(t, "the watcher to read the live run", func() bool { return liveReads.Load() >= 3 })
	if st.ctx.Err() != nil {
		t.Fatal("the watcher ended a stream of a run that is not paused")
	}
	setPaused(true)
	waitFor(t, "the paused run's stream to end", func() bool { return st.ctx.Err() != nil })
	state.release(st)
	waitFor(t, "the paused run's engine to be released", func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.engine == nil && first.engine.Closed()
	})
	if state.isClosed() || srv.viaOrg.runs[run.ID] != state {
		t.Fatal("a pause dropped the run's via_org state instead of keeping it")
	}
	if got := state.bytes.Load(); got != 7 {
		t.Fatalf("the run's byte total = %d after a pause, want 7", got)
	}

	// Resume. Once the watcher reads the run live again, no tick still holds the paused answer.
	setPaused(false)
	resumed := liveReads.Load()
	waitFor(t, "the watcher to read the resumed run", func() bool { return liveReads.Load() > resumed })
	r := httptest.NewRequest(http.MethodGet, proxy.ViaOrgPathPrefix+gh.ID.String()+"/wardyn/gh/octocat/hello-world/info/refs", nil)
	r.Header.Set(proxy.ViaOrgAuthorityHeader, githubHost)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("grantID", gh.ID.String())
	rc := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	rc = context.WithValue(rc, claimsCtxKey{}, &identity.Claims{RunID: run.ID})
	rc = context.WithValue(rc, relayRunnerKey{}, runnerID)
	w := httptest.NewRecorder()
	srv.handleViaOrg(w, r.WithContext(rc))
	switch errorReason(w) {
	case string(authz.ReasonRunPaused), reasonInternalLivenessReadFailed, string(placement.ReasonDeliveryNotViaOrg):
		t.Fatalf("the first request of a resumed run = %d %q, want it served", w.Code, w.Body)
	}
	state.mu.Lock()
	rebuilt := state.engine != nil && state.engine != first
	state.mu.Unlock()
	if !rebuilt {
		t.Fatal("the resumed run's request did not build a fresh engine")
	}
}
