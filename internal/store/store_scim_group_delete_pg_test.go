// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the SCIM group delete's refusal while a removal it would make is still owed.
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A group delete goes through only when every person it held has been cut: a recorded member of any issuer, or a
// removal whose sessions or tokens step is pending, refuses it and keeps the group; a pending audit step does not.
func TestPG_DeleteScimGroup_RefusesWhileARemovalIsOwed(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now()
	identity := func(principal, issuer string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ($1, $2, $3) RETURNING id`,
			principal, issuer, principal+"@corp.example").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	own, foreign := identity("sub-own", "https://idp.example"), identity("sub-foreign", "https://idp.other.example")
	grp, err := st.CreateScimGroupWithMembers(ctx, "eng", "Engineering", []uuid.UUID{own, foreign}, now)
	if err != nil {
		t.Fatal(err)
	}
	wantOwed := func(what string) {
		t.Helper()
		if err := st.DeleteScimGroup(ctx, grp.ID); !errors.Is(err, store.ErrGroupRemovalOwed) {
			t.Fatalf("%s: delete = %v, want ErrGroupRemovalOwed", what, err)
		}
		if _, err := st.GetScimGroup(ctx, grp.ID); err != nil {
			t.Fatalf("%s: the refused delete removed the group: %v", what, err)
		}
	}
	finish := func(id uuid.UUID, step string) {
		t.Helper()
		if err := st.FinishDeprovisionJob(ctx, id, store.JobKindGroupRemove, store.JobKey{Step: step, Target: grp.ID.String()}, nil); err != nil {
			t.Fatal(err)
		}
	}

	wantOwed("two members recorded")
	if err := st.StartGroupRemoval(ctx, grp.ID, own); err != nil {
		t.Fatal(err)
	}
	wantOwed("another issuer's member still recorded")
	if err := st.StartGroupRemoval(ctx, grp.ID, foreign); err != nil {
		t.Fatal(err)
	}
	wantOwed("both removals opened, nothing cut")
	for _, id := range []uuid.UUID{own, foreign} {
		finish(id, store.GroupStepSessions)
	}
	wantOwed("sessions cut, tokens not revoked")
	finish(own, store.GroupStepTokens)
	wantOwed("one person's tokens still owed")
	finish(foreign, store.GroupStepTokens)

	if err := st.DeleteScimGroup(ctx, grp.ID); err != nil {
		t.Fatalf("delete with only audit rows pending = %v, want nil", err)
	}
	if _, err := st.GetScimGroup(ctx, grp.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetScimGroup after the delete = %v, want ErrNotFound", err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM deprovision_jobs WHERE kind = $1 AND target = $2 AND step = $3 AND state = 'pending'`,
		store.JobKindGroupRemove, grp.ID.String(), store.GroupStepAudit).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Errorf("%d pending audit steps after the delete, want 2: the rows follow the cut", audits)
	}
	if err := st.DeleteScimGroup(ctx, grp.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a second delete = %v, want ErrNotFound", err)
	}
}
