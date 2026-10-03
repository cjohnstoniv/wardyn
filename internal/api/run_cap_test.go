// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
// do it fails closed, and the refusal is a 422 run_quota that writes no audit row.
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
