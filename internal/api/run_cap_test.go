// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
