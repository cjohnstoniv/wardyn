// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ApprovalRuleVersionRisesOnEveryWrite: a create is version 1, every replace adds one and keeps the id, and a
// delete returns the row it removed.
func TestPG_ApprovalRuleVersionRisesOnEveryWrite(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	w := types.ApprovalRuleWrite{Name: "ci-review", Mode: types.ApprovalRuleDryRun, ApprovalRuleSpec: types.ApprovalRuleSpec{
		Kinds: []types.ApprovalKind{types.ApprovalToolCall},
		Match: types.ApprovalRuleMatch{Repos: []string{"org/repo"}}, Approve: types.ApprovalRuleApprove{Tools: []string{"t"}},
	}}
	first, err := st.PutApprovalRule(ctx, w, "alice")
	if err != nil || first.Version != 1 || first.CreatedBy != "alice" {
		t.Fatalf("create = %+v, %v; want version 1 by alice", first, err)
	}
	w.Mode = types.ApprovalRuleEnforce
	second, err := st.PutApprovalRule(ctx, w, "bob")
	if err != nil || second.Version != 2 || second.ID != first.ID || second.Mode != types.ApprovalRuleEnforce || second.UpdatedBy != "bob" {
		t.Fatalf("replace = %+v, %v; want version 2, same id, enforce, by bob", second, err)
	}
	got, err := st.GetApprovalRule(ctx, "ci-review")
	if err != nil || got.Version != 2 || len(got.Match.Repos) != 1 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if list, err := st.ListApprovalRules(ctx); err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	removed, err := st.DeleteApprovalRule(ctx, "ci-review")
	if err != nil || removed.Version != 2 {
		t.Fatalf("delete = %+v, %v; want the version-2 row", removed, err)
	}
	if _, err := st.GetApprovalRule(ctx, "ci-review"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
	if _, err := st.DeleteApprovalRule(ctx, "ci-review"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
}
