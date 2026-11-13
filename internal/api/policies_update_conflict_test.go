// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// renameConflictPolicyStore fakes the run_policies.name UNIQUE constraint on
// the UPDATE door — the mirror of duplicateNamePolicyStore's CreatePolicy.
type renameConflictPolicyStore struct {
	store.Store
	notFound, stale bool
}

// GetPolicy is the read an update's audit row records the old policy from.
func (s renameConflictPolicyStore) GetPolicy(_ context.Context, id uuid.UUID) (types.RunPolicy, error) {
	if s.notFound {
		return types.RunPolicy{}, store.ErrNotFound
	}
	return types.RunPolicy{ID: id, Name: "before"}, nil
}

func (s renameConflictPolicyStore) UpdatePolicy(context.Context, uuid.UUID, time.Time, string, types.RunPolicySpec) (types.RunPolicy, error) {
	switch {
	case s.notFound:
		return types.RunPolicy{}, store.ErrNotFound
	case s.stale:
		return types.RunPolicy{}, store.ErrPolicyStale
	}
	return types.RunPolicy{}, store.ErrConflict
}

const conflictPolicyPut = `{"name":"prod","expected_updated_at":"2026-01-01T00:00:00Z","spec":{"min_confinement_class":"CC2"}}`

// TestUpdatePolicyDuplicateName: renaming a policy onto a taken name must
// answer 409, like creating one under that name does — not fall through to
// handleUpdatePolicy's blanket 500, which leaks the raw Postgres constraint
// text and tells the admin nothing they can act on.
func TestUpdatePolicyDuplicateName(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, renameConflictPolicyStore{}))
	w := do(t, srv, http.MethodPut, "/api/v1/policies/"+uuid.New().String(), adminToken, conflictPolicyPut)
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonPolicyNameConflict {
		t.Fatalf("code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "prod") {
		t.Errorf("body = %s, want the taken name named", w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "constraint") {
		t.Errorf("body = %s, must not leak the driver's constraint text", w.Body.String())
	}
}

// TestUpdatePolicyUnknownIDStillA404 is the negative control: the conflict
// arm must sit BESIDE notFoundIf, never in front of it.
func TestUpdatePolicyUnknownIDStillA404(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, renameConflictPolicyStore{notFound: true}))
	w := do(t, srv, http.MethodPut, "/api/v1/policies/"+uuid.New().String(), adminToken,
		`{"name":"prod","spec":{"min_confinement_class":"CC2"}}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestUpdatePolicyStaleIsNotANameConflict: a store guard miss answers 409 policy_stale with the
// reload sentence, never the name-conflict reason or the 500 below it.
func TestUpdatePolicyStaleIsNotANameConflict(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, renameConflictPolicyStore{stale: true}))
	w := do(t, srv, http.MethodPut, "/api/v1/policies/"+uuid.New().String(), adminToken, conflictPolicyPut)
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonPolicyStale ||
		!strings.Contains(w.Body.String(), "Someone saved this policy since you opened it. Reload it, then apply your edit again.") {
		t.Fatalf("= %d %s, want 409 %s with the reload sentence", w.Code, w.Body.String(), reasonPolicyStale)
	}
}
