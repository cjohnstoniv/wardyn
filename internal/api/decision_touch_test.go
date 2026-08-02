// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// touchStore records TouchRun calls. Every other store.Store method is nil, so
// the handler panics if it reaches one — the decision path must not.
type touchStore struct {
	store.Store
	touched []uuid.UUID
}

func (s *touchStore) TouchRun(_ context.Context, id uuid.UUID) error {
	s.touched = append(s.touched, id)
	return nil
}

// TestInternalDecisionTouchesRun pins the idle clock to real agent activity.
// The reaper measures idleness as the age of agent_runs.updated_at, and only
// the interactive attach keepalive used to bump it — so a NON-interactive run
// was hard-killed at its auto_stop_after_sec cap while actively working, under
// an audit event claiming it was idle (internal/lifecycle). The blind case is
// asserted too because it returns early, before the egress audit write.
func TestInternalDecisionTouchesRun(t *testing.T) {
	h := newHarness(t)
	st := &touchStore{}
	srv := New(baseTestConfig(h, st))
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	const path = "/api/v1/internal/decisions"

	body := `{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`
	if w := do(t, srv, http.MethodPost, path, tok, body); w.Code != http.StatusAccepted {
		t.Fatalf("decision code = %d, want 202", w.Code)
	}
	blind := `{"request":{"host":"api.anthropic.com","method":"CONNECT"},"decision":"allow",` +
		`"scan":{"scanned":false,"coverage":"tunneled-opaque","action":"blind"}}`
	if w := do(t, srv, http.MethodPost, path, tok, blind); w.Code != http.StatusAccepted {
		t.Fatalf("blind decision code = %d, want 202", w.Code)
	}
	if len(st.touched) != 2 || st.touched[0] != runID || st.touched[1] != runID {
		t.Fatalf("TouchRun calls = %v, want two for %s", st.touched, runID)
	}
}
