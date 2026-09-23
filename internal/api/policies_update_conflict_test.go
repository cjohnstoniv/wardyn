// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// renameConflictPolicyStore fakes the run_policies.name UNIQUE constraint on
// the UPDATE door — the mirror of duplicateNamePolicyStore's CreatePolicy.
type renameConflictPolicyStore struct {
	store.Store
	notFound bool
}

func (s renameConflictPolicyStore) UpdatePolicy(context.Context, uuid.UUID, string, types.RunPolicySpec) (types.RunPolicy, error) {
	if s.notFound {
		return types.RunPolicy{}, store.ErrNotFound
	}
	return types.RunPolicy{}, store.ErrConflict
}

// TestUpdatePolicyDuplicateName. Creating a policy under a name that
// is taken has answered 409 since W20-S1-3; RENAMING one onto a taken name fell
// through to handleUpdatePolicy's blanket 500, which leaks the raw Postgres
// constraint text and tells the admin nothing they can act on.
func TestUpdatePolicyDuplicateName(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, renameConflictPolicyStore{}))
	w := do(t, srv, http.MethodPut, "/api/v1/policies/"+uuid.New().String(), adminToken,
		`{"name":"prod","spec":{"min_confinement_class":"CC2"}}`)
	if w.Code != http.StatusConflict {
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
