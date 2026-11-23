// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the approvals of a held governance change. Guarded by WARDYN_TEST_PG; skipped
// cleanly when unset.
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

// TestPG_GovernanceApprovals_RecordedKeepsTheChangePending: the keep-pending sentinel commits what the decision
// wrote and leaves the change row alone; a decision that returns another error rolls the rows back.
func TestPG_GovernanceApprovals_RecordedKeepsTheChangePending(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	ch := newChange(uuid.NewString())
	ch.RequiredApprovals = 2
	saved, _, err := st.ProposeGovernanceChange(ctx, ch, time.Hour)
	if err != nil || saved.RequiredApprovals != 2 || saved.RequireSecurity {
		t.Fatalf("proposal = %+v, %v; want a snapshot of two approvals and no security rule", saved, err)
	}
	row := store.GovernanceChangeApproval{ChangeID: saved.ID, Approver: "bob", ApproverEmail: "bob@corp.example", Tier: "unit_admin", ApprovedAt: time.Now().UTC()}

	boom := errors.New("boom")
	if _, err := st.DecideGovernanceChange(ctx, saved.ID, store.GovernanceDecision{}, func(q store.Querier, _ types.GovernanceChange) error {
		if err := store.PutGovernanceChangeApprovalQ(ctx, q, row); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("decision error = %v, want it returned", err)
	}
	if rows, _ := st.ListGovernanceChangeApprovals(ctx, []uuid.UUID{saved.ID}, false); len(rows) != 0 {
		t.Fatalf("a failed decision left %d approval rows, want none", len(rows))
	}

	out, err := st.DecideGovernanceChange(ctx, saved.ID, store.GovernanceDecision{}, func(q store.Querier, _ types.GovernanceChange) error {
		if err := store.PutGovernanceChangeApprovalQ(ctx, q, row); err != nil {
			return err
		}
		return store.ErrGovernanceApprovalRecorded
	})
	if !errors.Is(err, store.ErrGovernanceApprovalRecorded) || out.State != types.GovernanceChangePending {
		t.Fatalf("recorded decision = %+v, %v; want the change still pending", out, err)
	}
	rows, err := st.ListGovernanceChangeApprovals(ctx, []uuid.UUID{saved.ID}, true)
	if err != nil || len(rows) != 1 || rows[0].Approver != "bob" || !rows[0].Counts() {
		t.Fatalf("approval rows = %+v, %v; want bob's, counting", rows, err)
	}
}

// TestPG_GovernanceApprovals_DropReplaceAndErase: a dropped row stops counting but stays; a new approval from the
// same person replaces it; erasing a person deletes their rows on every change.
func TestPG_GovernanceApprovals_DropReplaceAndErase(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	who := "approver-" + uuid.NewString()
	saved, _, err := st.ProposeGovernanceChange(ctx, newChange(uuid.NewString()), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	row := store.GovernanceChangeApproval{ChangeID: saved.ID, Approver: who, ApproverEmail: who + "@Corp.Example", Tier: "unit_admin", ApprovedAt: time.Now().UTC()}
	if err := store.PutGovernanceChangeApprovalQ(ctx, pool, row); err != nil {
		t.Fatal(err)
	}
	if err := store.DropGovernanceChangeApprovalQ(ctx, pool, saved.ID, who, "deactivated", nil); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.ListGovernanceChangeApprovals(ctx, []uuid.UUID{saved.ID}, true); len(rows) != 0 {
		t.Fatalf("a dropped approval is listed as counting: %+v", rows)
	}
	if err := store.PutGovernanceChangeApprovalQ(ctx, pool, row); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ListGovernanceChangeApprovals(ctx, []uuid.UUID{saved.ID}, false)
	if len(rows) != 1 || !rows[0].Counts() || rows[0].DroppedAt != nil {
		t.Fatalf("after a new approval = %+v, want one counting row with no drop", rows)
	}
	if n, err := st.EraseGovernanceChangePersonalFields(ctx, nil, []string{who + "@corp.example"}); err != nil || n != 0 {
		t.Fatalf("erase = %d, %v; want no change rows touched", n, err)
	}
	if rows, _ := st.ListGovernanceChangeApprovals(ctx, []uuid.UUID{saved.ID}, false); len(rows) != 0 {
		t.Fatalf("erasing the approver left %d approval rows", len(rows))
	}
}

// TestPG_PrincipalSignIn_KeepsTheRolesClaim: PutPrincipalSignIn records the roles claim beside the groups; the
// a later write that carries no roles leaves it alone.
func TestPG_PrincipalSignIn_KeepsTheRolesClaim(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	who := "roles-" + uuid.NewString()
	if err := st.PutPrincipalSignIn(ctx, who, who+"@corp.example", []string{"eng"}, []string{"wardyn-security"}, false); err != nil {
		t.Fatal(err)
	}
	var roles []string
	read := func() []string {
		var r []string
		if err := pool.QueryRow(ctx, `SELECT coalesce(roles, '[]'::jsonb) FROM principal_groups WHERE principal = $1 AND portal = ''`, who).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if roles = read(); len(roles) != 1 || roles[0] != "wardyn-security" {
		t.Fatalf("roles after sign-in = %v, want the claim", roles)
	}
	if err := st.PutPrincipalGroups(ctx, who, "", who+"@corp.example", []string{"eng"}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if roles = read(); len(roles) != 1 {
		t.Fatalf("roles after a groups-only write = %v, want them kept", roles)
	}
}
