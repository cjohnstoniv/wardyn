// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// memComposeStore models the compose_results rows in memory with the same
// delete-on-read contract as the SQL (migration 0026). The real atomicity is
// proven against Postgres in internal/store/store_ephemeral_pg_test.go; here the
// handler's own rules (which run id a body lands under, and that a rejected
// cross-run PUT stores nothing) are what is under test.
type memComposeStore struct {
	store.Store
	mu sync.Mutex
	m  map[uuid.UUID][]byte
}

func newMemComposeStore() *memComposeStore {
	return &memComposeStore{m: map[uuid.UUID][]byte{}}
}

func (s *memComposeStore) PutComposeResult(_ context.Context, runID uuid.UUID, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[runID] = payload
	return nil
}

func (s *memComposeStore) TakeComposeResult(_ context.Context, runID uuid.UUID) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.m[runID]
	delete(s.m, runID)
	return raw, ok, nil
}

// TestComposeRunActor pins item 6: RunClaudeCompose must attribute a
// compose-launched run's created_by to the REQUESTING HUMAN (threaded via
// withComposeRequestActor, which handleComposeRun stashes on the ctx that
// flows unchanged through Registry.Propose -> the sandbox backend's Propose
// -> the bound RunClaudeFunc), not composeActor()'s fixed system marker — a
// member's own compose-launched run must be visible/killable to them under
// ownership scoping (item 2), which composeActor()'s "wardyn-composer" broke.
// The fallback (no request actor tracked) still resolves to composeActor(),
// so a hypothetical future non-request-scoped caller degrades safely instead
// of attributing to "".
func TestComposeRunActor(t *testing.T) {
	s := &Server{}
	ctx := withComposeRequestActor(context.Background(), "alice@corp.example")
	if got := s.composeRunActor(ctx); got != "alice@corp.example" {
		t.Fatalf("composeRunActor = %q, want the request actor %q", got, "alice@corp.example")
	}
	if got := s.composeRunActor(context.Background()); got != "wardyn-composer" {
		t.Fatalf("composeRunActor with no tracked request actor = %q, want the composeActor() fallback %q", got, "wardyn-composer")
	}
	s.cfg.LocalOperator = "local:bob"
	if got := s.composeRunActor(context.Background()); got != "local:bob" {
		t.Fatalf("composeRunActor fallback with LocalOperator set = %q, want %q", got, "local:bob")
	}
}

// TestUploadComposeResult_RoundTrip: a PUT under the run's OWN id is accepted
// (204) and the raw body is stashed in the compose-results store for the waiting
// RunClaudeCompose to take once (delete-on-read).
func TestUploadComposeResult_RoundTrip(t *testing.T) {
	h := newHarness(t)
	st := newMemComposeStore()
	srv := New(baseTestConfig(h, st))
	ctx := context.Background()
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	body := `{"structured_output":{"run":{"agent":"claude-code"}}}`

	w := do(t, srv, http.MethodPut,
		"/api/v1/internal/compose-results/"+runID.String(), tok, body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("compose upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	got, ok, err := st.TakeComposeResult(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("compose result was not stored for the run: ok=%v err=%v", ok, err)
	}
	if string(got) != body {
		t.Errorf("stored body = %q, want %q", got, body)
	}
	// Take is delete-on-read: a second take must miss.
	if _, ok, _ := st.TakeComposeResult(ctx, runID); ok {
		t.Error("compose result should be taken exactly once (delete-on-read)")
	}
}

// TestUploadComposeResult_CrossRunRejected mirrors the scan/recording cross-run
// guard: a token minted for run A cannot PUT a proposal under run B's id (403),
// before any body read.
func TestUploadComposeResult_CrossRunRejected(t *testing.T) {
	h := newHarness(t)
	st := newMemComposeStore()
	srv := New(baseTestConfig(h, st))
	tok := h.mintRunToken(t, uuid.New())
	other := uuid.New()
	w := do(t, srv, http.MethodPut,
		"/api/v1/internal/compose-results/"+other.String(), tok, `{"x":1}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-run compose upload: code = %d, want 403", w.Code)
	}
	if _, ok, _ := st.TakeComposeResult(context.Background(), other); ok {
		t.Error("a rejected cross-run upload must store nothing")
	}
}
