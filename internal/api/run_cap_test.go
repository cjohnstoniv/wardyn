// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// plainRunStore records which insert createRun used; it has no CreateRunUnderCap.
type plainRunStore struct {
	store.Store
	plain int
}

func (s *plainRunStore) CreateRun(_ context.Context, r types.AgentRun) (types.AgentRun, error) {
	s.plain++
	return r, nil
}

// cappedRunStore is plainRunStore plus the capped insert, refusing at the cap.
type cappedRunStore struct {
	plainRunStore
	gotLimit int
	err      error
}

func (s *cappedRunStore) CreateRunUnderCap(_ context.Context, r types.AgentRun, limit int) (types.AgentRun, error) {
	s.gotLimit = limit
	return r, s.err
}

// TestCreateRun_DeploymentCap pins createRun, the one door every launcher passes:
// no cap takes the plain insert, a cap takes the atomic one, a store that cannot
// do it fails closed, and the refusal is a 422 run_quota (the no-audit claim is pinned by TestCreateRun_DeploymentCapRefusesBeforeTheMint).
func TestCreateRun_DeploymentCap(t *testing.T) {
	ctx := context.Background()

	plain := &plainRunStore{}
	if _, err := (&Server{cfg: Config{Store: plain}}).createRun(ctx, types.AgentRun{}); err != nil || plain.plain != 1 {
		t.Fatalf("no cap: err=%v plain inserts=%d, want the plain insert", err, plain.plain)
	}

	if _, err := (&Server{cfg: Config{Store: plain, MaxConcurrentRuns: 2}}).createRun(ctx, types.AgentRun{}); err == nil {
		t.Fatal("a cap on a store without CreateRunUnderCap admitted the run; it must fail closed")
	}

	capped := &cappedRunStore{err: store.ErrRunCapReached}
	srv := &Server{cfg: Config{Store: capped, MaxConcurrentRuns: 2}}
	_, err := srv.createRun(ctx, types.AgentRun{})
	if capped.gotLimit != 2 || capped.plain != 0 {
		t.Fatalf("limit passed = %d, plain inserts = %d; want 2 and 0", capped.gotLimit, capped.plain)
	}
	w := httptest.NewRecorder()
	writeServerError(w, httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), "create run", err)
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != string(authz.ReasonRunQuota) {
		t.Fatalf("refusal = %d %q, want 422 %q (body %s)", w.Code, errorReason(w), authz.ReasonRunQuota, w.Body.String())
	}
}

// TestReviveRun_AtTheDeploymentCapSucceeds: revive re-dispatches a RUNNING row the
// cap already counts and inserts nothing, so it never goes through createRun.
func TestReviveRun_AtTheDeploymentCapSucceeds(t *testing.T) {
	f := newReviveFixture(t)
	f.srv.cfg.MaxConcurrentRuns = 1
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive at the cap: code %d, want 200", code)
	}
}

// TestReviveRun_KeptRunAtTheDeploymentCapIsRefused: a run kept after a reboot or
// its end holds no slot, so reviving it takes one. At the cap the revive gets the
// create's refusal (422 run_quota) before any proxy or agent starts, and the run
// stays kept; with a slot free, the same revive succeeds.
func TestReviveRun_KeptRunAtTheDeploymentCapIsRefused(t *testing.T) {
	for name, keep := range map[string]func(t *testing.T) (*reviveFixture, *startingRunner, bool){
		"reboot": func(t *testing.T) (*reviveFixture, *startingRunner, bool) {
			f, sr := newRebootFixture(t)
			return f, sr, false
		},
		"ended": func(t *testing.T) (*reviveFixture, *startingRunner, bool) {
			f, sr := newEndedFixture(t)
			f.now = f.now.Add(time.Hour)
			if code, body := f.extendAs(t, true, f.now.Add(48*time.Hour)); code != http.StatusOK {
				t.Fatalf("extend = %d %s, want 200", code, body)
			}
			f.run = f.st.run
			return f, sr, true
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, sr, owner := keep(t)
			f.srv.cfg.MaxConcurrentRuns = 1
			f.rs.capFull = true
			code, body := f.reviveAs(t, owner)
			if code != http.StatusUnprocessableEntity || !strings.Contains(body, string(authz.ReasonRunQuota)) {
				t.Fatalf("revive at the cap = %d %s, want 422 run_quota", code, body)
			}
			if len(f.rr.replaced) != 0 || len(sr.starts()) != 0 {
				t.Fatalf("refused revive replaced %d proxies and started agents %v; want neither", len(f.rr.replaced), sr.starts())
			}
			if lostAt, reason := f.st.lost(); lostAt == nil || reason != f.run.LostReason {
				t.Fatalf("refused revive: lost %v %q; want still kept (%s)", lostAt, reason, f.run.LostReason)
			}

			f.rs.capFull = false
			if code, body := f.reviveAs(t, owner); code != http.StatusOK {
				t.Fatalf("revive under the cap = %d %s, want 200", code, body)
			}
			if lostAt, _ := f.st.lost(); lostAt != nil || len(sr.starts()) != 1 {
				t.Fatalf("revive under the cap: lost %v, agent starts %v; want live with its agent started once", lostAt, sr.starts())
			}
		})
	}
}

// TestReviveRun_OutageRunWithStoppedAgentAtTheDeploymentCapIsRefused: an outage-kept run whose
// agent is stopped (its end passed, then was moved later) is revived by starting that agent, so
// the revive takes a slot: at the cap it is refused 422 run_quota with no proxy replaced and no
// agent started. An outage revive whose agent still runs starts nothing and is never refused.
func TestReviveRun_OutageRunWithStoppedAgentAtTheDeploymentCapIsRefused(t *testing.T) {
	f := newReviveFixture(t)
	sr := &startingRunner{reviveRunner: f.rr}
	f.srv.cfg.Runner = sr
	f.rr.status = types.RunStopped
	f.srv.cfg.MaxConcurrentRuns = 1
	f.rs.capFull = true

	code, body := f.reviveAs(t, false)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, string(authz.ReasonRunQuota)) {
		t.Fatalf("revive at the cap = %d %s, want 422 run_quota", code, body)
	}
	if len(f.rr.replaced) != 0 || len(sr.starts()) != 0 {
		t.Fatalf("refused revive replaced %d proxies and started agents %v; want neither", len(f.rr.replaced), sr.starts())
	}
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage {
		t.Fatalf("refused revive: lost %v %q; want still kept (outage)", lostAt, reason)
	}

	f.rr.status = types.RunRunning
	if code, body := f.reviveAs(t, false); code != http.StatusOK {
		t.Fatalf("outage revive of a running agent at the cap = %d %s, want 200", code, body)
	}
	if len(sr.starts()) != 0 {
		t.Fatalf("agent starts = %v, want none for a running agent", sr.starts())
	}
}

// runCapAPIStore adds the deployment-cap store methods to runWarnStore: active is
// what the lock-free count reports, and an insert at the cap answers
// ErrRunCapReached the way store.PG does when a racing create took the last slot.
type runCapAPIStore struct {
	*runWarnStore
	active int
	raced  bool
}

func (s *runCapAPIStore) CountNonTerminalRuns(context.Context) (int, error) { return s.active, nil }

func (s *runCapAPIStore) CreateRunUnderCap(ctx context.Context, r types.AgentRun, _ int) (types.AgentRun, error) {
	if s.raced {
		return types.AgentRun{}, store.ErrRunCapReached
	}
	return s.CreateRun(ctx, r)
}

func (s *runCapAPIStore) CreateRun(_ context.Context, r types.AgentRun) (types.AgentRun, error) {
	s.created = r
	return r, nil
}

// TestCreateRun_DeploymentCapRefusesBeforeTheMint drives the real POST /runs: at
// the cap the answer is 422 run_quota with no run row, no identity.mint and no
// run.create row (the per-member quota's siting); below it the run is created;
// and a create that loses the race at the cap is still a 422 with no run row.
func TestCreateRun_DeploymentCapRefusesBeforeTheMint(t *testing.T) {
	h := newHarness(t)
	st := &runCapAPIStore{runWarnStore: &runWarnStore{capStore: &capStore{}}, active: 2}
	cfg := baseTestConfig(h, st)
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	cfg.MaxConcurrentRuns = 2
	srv := New(cfg)
	const body = `{"agent":"claude-code","task":"t"}`

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != string(authz.ReasonRunQuota) {
		t.Fatalf("at the cap: %d %q, want 422 %q (body %s)", w.Code, errorReason(w), authz.ReasonRunQuota, w.Body.String())
	}
	if st.created.ID != uuid.Nil {
		t.Fatal("at the cap: a run row was written")
	}
	if rows := h.audit.snapshot(); len(rows) != 0 {
		t.Fatalf("at the cap: %d audit rows written, want none: %+v", len(rows), rows)
	}

	if w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body); w.Code != http.StatusUnprocessableEntity || errorReason(w) != string(authz.ReasonRunQuota) {
		t.Fatalf("preflight at the cap: %d %q, want 422 %q", w.Code, errorReason(w), authz.ReasonRunQuota)
	}

	st.active = 1
	if w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body); w.Code != http.StatusCreated || st.created.ID == uuid.Nil {
		t.Fatalf("below the cap: %d, want 201 with a row: %s", w.Code, w.Body)
	}

	st.created, st.raced = types.AgentRun{}, true
	w = do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != string(authz.ReasonRunQuota) || st.created.ID != uuid.Nil {
		t.Fatalf("lost the race: %d %q row=%v, want 422 %q and no row", w.Code, errorReason(w), st.created.ID, authz.ReasonRunQuota)
	}
}
