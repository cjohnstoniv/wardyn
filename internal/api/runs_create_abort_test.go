// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createGrantFailStore is the real PG store with ONE failure injected: the
// CreateGrant that runs AFTER CreateRun. Everything else — the run insert, the
// state CAS the compensator needs, the reads the response makes — is the real
// driver, because the bug being pinned is about what the handler does with a
// half-built run, not about what a fake would have returned.
type createGrantFailStore struct {
	store.Store
	err error
}

func (s createGrantFailStore) CreateGrant(context.Context, types.CredentialGrant) (types.CredentialGrant, error) {
	return types.CredentialGrant{}, s.err
}

// abortHarness is pgHarnessWithRunner with the broker + audit recorder handed
// back, so a test can assert the revoke cascade and the run.create row. wrap
// lets a case swap the store for a failure-injecting wrapper over the same pool.
func abortHarness(t *testing.T, wrap func(store.Store) store.Store) (*Server, *fakeBroker, *recRecorder, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed run-create abort test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)

	audit := &recRecorder{}
	idp, err := embedded.New(nil, "wardyn.local", identitytest.NewMemRevocationStore(), audit)
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	var st store.Store = store.NewPG(pool)
	if wrap != nil {
		st = wrap(st)
	}
	brk := &fakeBroker{}
	srv := New(Config{
		Store:       st,
		Identity:    idp,
		Approvals:   newFakeApprovals(),
		Broker:      brk,
		Audit:       audit,
		AdminToken:  adminToken,
		TrustDomain: "wardyn.local",
		Secrets:     &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}},
		DefaultPolicy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
			EligibleGrants:      []types.GrantSpec{apiKeyGrant("api.anthropic.com", "anthropic-api-key")},
		},
		ControlPlaneURL: "http://wardynd:8080",
	})
	return srv, brk, audit, pool
}

// handleCreateRun's post-CreateRun early returns answered 500 and walked away:
// the run row stayed PENDING, the identity minted one line earlier was never
// revoked and no run.create row was ever written — a ghost run holding a live
// run token until finalizeUndispatchedRuns swept it (up to ~2h). Its sibling
// door, launchRecordRun, has wrapped the identical window in abort() since
// 0.6 and is pinned by TestLaunchRecordRun_CreateGrantFailureFinalizesRun.
func TestCreateRun_GrantFailureAfterCreateRunFinalizesTheRun(t *testing.T) {
	srv, brk, _, pool := abortHarness(t, func(s store.Store) store.Store {
		return createGrantFailStore{Store: s, err: errors.New("grant store down")}
	})

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi","title":"abort me"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500; body=%s", w.Code, w.Body.String())
	}

	// The run row the handler created must be terminal, not a ghost PENDING.
	runID := onlyRunNamed(t, pool, "abort me")
	got, err := store.NewPG(pool).GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("read the aborted run: %v", err)
	}
	if got.State != types.RunFailed {
		t.Errorf("state = %q, want FAILED — a run whose grants could not be written is not pending, it is dead", got.State)
	}
	if len(brk.revoked) == 0 || brk.revoked[len(brk.revoked)-1] != runID {
		t.Errorf("broker.revoked = %v, want the aborted run %s — the minted identity outlived the run", brk.revoked, runID)
	}
}

// TestCreateRun_HappyPathStillAnswers201WithOneCreateRow is the negative
// control: the compensator must not fire, and the trail must carry exactly one
// run.create row for the run.
func TestCreateRun_HappyPathStillAnswers201WithOneCreateRow(t *testing.T) {
	srv, brk, audit, _ := abortHarness(t, nil)

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi","title":"happy path"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(brk.revoked) != 0 {
		t.Errorf("broker.revoked = %v, want none on the happy path", brk.revoked)
	}
	var creates int
	for _, ev := range audit.events {
		if ev.Action == "run.create" && ev.Target == created.ID.String() {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("run.create rows for %s = %d, want exactly 1", created.ID, creates)
	}
}

// onlyRunNamed finds the single run row carrying title, so the test never has
// to guess an id the 500 response could not carry.
func onlyRunNamed(t *testing.T, pool *pgxpool.Pool, title string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM agent_runs WHERE title = $1 ORDER BY created_at DESC LIMIT 1`, title).Scan(&id); err != nil {
		t.Fatalf("no run row was ever created for %q: %v", title, err)
	}
	return id
}
