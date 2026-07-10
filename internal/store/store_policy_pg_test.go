// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the RunPolicy store CRUD. Guarded by WARDYN_TEST_PG.
// Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_PolicyCRUD_RoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	now := time.Now().UTC()
	id := uuid.New()
	// Unique name keeps the run_policies UNIQUE(name) constraint happy across reruns.
	name := "policy-crud-" + id.String()
	p := types.RunPolicy{
		ID:        id,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
		Spec: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
			FirstUseApproval:    types.FirstUseDenyWithReview,
			EligibleGrants: []types.GrantSpec{
				{Kind: types.GrantGitHubToken, TTLSeconds: 3600, RequiresApproval: true},
			},
		},
	}

	// Create.
	created, err := store.NewPG(pool).CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if created.ID != id || created.Name != name {
		t.Fatalf("created policy mismatch: %+v", created)
	}

	// List must contain it.
	list, err := store.NewPG(pool).ListPolicies(ctx)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	var found bool
	for _, lp := range list {
		if lp.ID == id {
			found = true
			if lp.Spec.MinConfinementClass != types.CC2 {
				t.Errorf("listed spec min cc = %q, want CC2", lp.Spec.MinConfinementClass)
			}
		}
	}
	if !found {
		t.Fatal("created policy not present in ListPolicies")
	}

	// Get.
	got, err := store.NewPG(pool).GetPolicy(ctx, id)
	if err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if len(got.Spec.EligibleGrants) != 1 || got.Spec.EligibleGrants[0].Kind != types.GrantGitHubToken {
		t.Errorf("get spec grants = %+v", got.Spec.EligibleGrants)
	}

	// Update: change name + spec.
	newName := name + "-v2"
	updatedSpec := types.RunPolicySpec{
		AllowedDomains:      []string{"github.com"},
		MinConfinementClass: types.CC3,
	}
	updated, err := store.NewPG(pool).UpdatePolicy(ctx, id, newName, updatedSpec)
	if err != nil {
		t.Fatalf("update policy: %v", err)
	}
	if updated.Name != newName {
		t.Errorf("updated name = %q, want %q", updated.Name, newName)
	}
	if updated.Spec.MinConfinementClass != types.CC3 {
		t.Errorf("updated spec min cc = %q, want CC3", updated.Spec.MinConfinementClass)
	}
	if !updated.UpdatedAt.After(created.CreatedAt) && !updated.UpdatedAt.Equal(created.CreatedAt) {
		// updated_at is bumped to now(); it must be >= created_at.
		t.Errorf("updated_at %v not >= created_at %v", updated.UpdatedAt, created.CreatedAt)
	}

	// Update of an unknown id => ErrNotFound.
	if _, err := store.NewPG(pool).UpdatePolicy(ctx, uuid.New(), "ghost", updatedSpec); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update unknown id err = %v, want ErrNotFound", err)
	}

	// Delete.
	if err := store.NewPG(pool).DeletePolicy(ctx, id); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	// Get after delete => ErrNotFound.
	if _, err := store.NewPG(pool).GetPolicy(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after delete err = %v, want ErrNotFound", err)
	}
	// Delete again => ErrNotFound.
	if err := store.NewPG(pool).DeletePolicy(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id err = %v, want ErrNotFound", err)
	}
}
