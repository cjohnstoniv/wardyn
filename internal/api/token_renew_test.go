// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// renewStore is a minimal Store for the renew gate: one run whose state the test
// controls. Everything else is left to the embedded nil Store (never called).
type renewStore struct {
	store.Store
	mu    sync.Mutex
	run   types.AgentRun
	state types.RunState
	err   error
}

func (s *renewStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return types.AgentRun{}, s.err
	}
	if id != s.run.ID {
		return types.AgentRun{}, store.ErrNotFound
	}
	r := s.run
	r.State = s.state
	return r, nil
}

func (s *renewStore) setState(st types.RunState) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// hasAudit reports whether an event with action+outcome was recorded.
func hasAudit(h *harness, action, outcome string) bool {
	for _, ev := range h.audit.events {
		if ev.Action == action && ev.Outcome == outcome {
			return true
		}
	}
	return false
}

// newRenewHarness builds a Server whose Store carries a single RUNNING run.
func newRenewHarness(t *testing.T) (*harness, *Server, *renewStore, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	st := &renewStore{run: types.AgentRun{ID: runID}, state: types.RunRunning}
	srv := New(baseTestConfig(h, st))
	return h, srv, st, runID
}

// TestRenewU070_ValidRunRenewsAndFreshTokenWorks is the CORE counterfactual: a
// live run trades its still-valid token for a fresh one, and the fresh token is
// accepted on a DIFFERENT internal endpoint. Without the renew route this 404s
// (chi has no such path) and the run has no way to ever get a new token.
func TestRenewU070_ValidRunRenewsAndFreshTokenWorks(t *testing.T) {
	h, srv, _, runID := newRenewHarness(t)
	old := h.mintRunToken(t, runID)

	w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", old, "")
	if w.Code != http.StatusOK {
		t.Fatalf("renew: code = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var got tokenRenewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode renew response: %v", err)
	}
	if got.Token == "" {
		t.Fatal("renew returned an empty token")
	}
	if got.Token == old {
		t.Fatal("renew returned the SAME token — a renew must be a fresh jti, not an echo")
	}
	if _, err := time.Parse(time.RFC3339, got.ExpiresAt); err != nil {
		t.Fatalf("renew expires_at %q is not RFC3339: %v", got.ExpiresAt, err)
	}

	// The fresh token must actually authenticate the internal surface. Use the
	// mint endpoint: a 401 would mean the token is not accepted at all; anything
	// else means internalAuth verified it (the grant itself is absent here).
	mw := do(t, srv, http.MethodPost, "/api/v1/internal/credentials/mint",
		got.Token, `{"grant_id":"`+uuid.New().String()+`"}`)
	if mw.Code == http.StatusUnauthorized {
		t.Fatalf("renewed token rejected by internalAuth: code = %d, want non-401", mw.Code)
	}

	// HONEST TRAIL: an identity.renew success naming both jtis must exist.
	if !hasAudit(h, "identity.renew", "success") {
		t.Error("no identity.renew/success audit event for a successful renew")
	}
}

// TestRenewU070_RevokedRunRefusedFailClosed proves gate #1: a KILLED run whose
// identity was revoked can never renew. Revocation is run-scoped, so the token
// it still physically holds is dead on arrival.
func TestRenewU070_RevokedRunRefusedFailClosed(t *testing.T) {
	h, srv, _, runID := newRenewHarness(t)
	tok := h.mintRunToken(t, runID)

	// Sanity: it renews while the run is live and un-revoked.
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("pre-revoke renew: code = %d, want 200", w.Code)
	}

	// The kill cascade's identity half.
	if err := h.idp.RevokeRun(context.Background(), runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}

	w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", tok, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked run renew: code = %d, want 401 (fail closed)", w.Code)
	}
}

// TestRenewU070_TerminalRunRefusedFailClosed proves gate #2, the one that is NOT
// covered by revocation: revokeRunCascade is best-effort, so a run can go
// terminal with its revocation write having failed. The token still verifies —
// and must still be refused, because the run's authority is over.
func TestRenewU070_TerminalRunRefusedFailClosed(t *testing.T) {
	h, srv, st, runID := newRenewHarness(t)
	tok := h.mintRunToken(t, runID)

	for _, term := range []types.RunState{
		types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived,
	} {
		st.setState(term)
		w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", tok, "")
		if w.Code != http.StatusForbidden {
			t.Errorf("state %s: renew code = %d, want 403 (terminal runs never renew)", term, w.Code)
		}
	}
	if !hasAudit(h, "identity.renew", "denied") {
		t.Error("no identity.renew/denied audit event for a refused renew")
	}

	// Control: back to RUNNING and it renews again — the gate keys on state, not
	// on some sticky refusal.
	st.setState(types.RunRunning)
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("running run renew: code = %d, want 200", w.Code)
	}
}

// TestRenewU070_UnknownRunAndNoStoreRefused covers the remaining fail-closed
// paths: a token naming a run the store does not have, and a Server with no
// store at all (we cannot prove the run is alive => we do not renew).
func TestRenewU070_UnknownRunAndNoStoreRefused(t *testing.T) {
	h, srv, _, _ := newRenewHarness(t)

	// Valid signature/audience, but the run does not exist.
	orphan := h.mintRunToken(t, uuid.New())
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", orphan, ""); w.Code != http.StatusForbidden {
		t.Errorf("unknown run renew: code = %d, want 403", w.Code)
	}

	// No Store wired: refuse rather than renew on unverifiable authority.
	noStore := New(baseTestConfig(h, nil))
	tok := h.mintRunToken(t, uuid.New())
	if w := do(t, noStore, http.MethodPost, "/api/v1/internal/token/renew", tok, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no-store renew: code = %d, want 503", w.Code)
	}
}

// TestRenewU070_RejectsBadTokens pins the auth boundary: the renew route is not
// a way around it. An admin token, a ground-truth-audience token, and no token
// at all must all fail — otherwise renew would be a token-laundering endpoint.
func TestRenewU070_RejectsBadTokens(t *testing.T) {
	h, srv, _, _ := newRenewHarness(t)

	if w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d, want 401", w.Code)
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", adminToken, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("admin token: code = %d, want 401", w.Code)
	}
	gt := h.mintGroundtruthToken(t)
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", gt, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("groundtruth token: code = %d, want 401 (audience separation)", w.Code)
	}
}
