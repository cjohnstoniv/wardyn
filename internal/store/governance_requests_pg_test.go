// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Elevations against Postgres (0273_governance_elevation): a membership with an end, the reads that ignore it
// once it has passed, the sweep and the request reads. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
// Every case mints its own unit and subjects, so these are safe on the shared substrate.
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func elevationUnit(t *testing.T, st store.PG) types.GovernanceUnit {
	t.Helper()
	u, err := st.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{Name: "elev-" + uuid.NewString(), CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// elevated inserts a user membership in u that ends at end.
func elevated(t *testing.T, st store.PG, u types.GovernanceUnit, subject string, end time.Time) types.GovernanceMembership {
	t.Helper()
	m := types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: subject, UnitID: u.ID,
		CreatedAt: time.Now().UTC(), CreatedBy: "seed", ExpiresAt: &end}
	if err := store.InsertGovernanceMembershipQ(context.Background(), st.Pool, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestUpsertMembershipClearsExpiry: an admin's membership write over a live elevation makes it permanent.
func TestUpsertMembershipClearsExpiry(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	u := elevationUnit(t, st)
	sub := "sub-elev-" + uuid.NewString()
	elevated(t, st, u, sub, time.Now().UTC().Add(time.Hour))
	out, err := store.UpsertGovernanceMembershipQ(ctx, pool, types.GovernanceMembership{SubjectType: types.CapabilitySubjectUser, Subject: sub, UnitID: u.ID, Priority: 2, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if out.ExpiresAt != nil {
		t.Errorf("upsert over an elevation = %+v, want expires_at cleared", out)
	}
	got, err := store.GetGovernanceMembershipByKeyQ(ctx, pool, u.ID, "user", sub, false)
	if err != nil || got.ExpiresAt != nil || got.Priority != 2 {
		t.Errorf("stored row = %+v, %v; want priority 2 and no expiry", got, err)
	}
}

// TestProposeGovernanceChangeStoresNote: a proposal's note is stored with it; one with none stores empties.
func TestProposeGovernanceChangeStoresNote(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	ch := newChange(uuid.NewString())
	ch.Note = &types.ChangeNote{Reason: "on call", Ticket: "INC-42"}
	saved, _, err := st.ProposeGovernanceChange(ctx, ch, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Note == nil || saved.Note.Reason != "on call" || saved.Note.Ticket != "INC-42" {
		t.Errorf("saved note = %+v, want reason and ticket", saved.Note)
	}
	plain, _, err := st.ProposeGovernanceChange(ctx, newChange(uuid.NewString()), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Note != nil {
		t.Errorf("a change proposed with no note reads %+v, want none", plain.Note)
	}
}

// TestElevationReadsIgnoreExpired: the caller and list reads skip a row past its end; the key read sees it.
func TestElevationReadsIgnoreExpired(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	u := elevationUnit(t, st)
	live, ended := "sub-live-"+uuid.NewString(), "sub-ended-"+uuid.NewString()
	elevated(t, st, u, live, time.Now().UTC().Add(time.Hour))
	elevated(t, st, u, ended, time.Now().UTC().Add(-time.Minute))
	for sub, want := range map[string]int{live: 1, ended: 0} {
		ms, err := store.ListMembershipsForQ(ctx, pool, []string{sub}, nil, "")
		if err != nil || len(ms) != want {
			t.Errorf("ListMembershipsForQ(%s) = %d rows, %v; want %d", sub, len(ms), err, want)
		}
	}
	all, err := store.ListGovernanceMembershipsQ(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Subject == ended {
			t.Errorf("ListGovernanceMembershipsQ lists the ended elevation %+v", m)
		}
	}
	if _, err := store.GetGovernanceMembershipByKeyQ(ctx, pool, u.ID, "user", ended, false); err != nil {
		t.Errorf("the key read of an ended, unswept row: %v, want the row", err)
	}
}

// TestSweepExpiredMemberships: the sweep deletes the rows past their end and returns them, and leaves the
// live ones. DeleteExpiredMembershipAtKeyQ deletes an ended row at one key only.
func TestSweepExpiredMemberships(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	u := elevationUnit(t, st)
	live, ended, atKey := "sub-live-"+uuid.NewString(), "sub-ended-"+uuid.NewString(), "sub-key-"+uuid.NewString()
	elevated(t, st, u, live, time.Now().UTC().Add(time.Hour))
	gone := elevated(t, st, u, ended, time.Now().UTC().Add(-time.Minute))
	keyRow := elevated(t, st, u, atKey, time.Now().UTC().Add(-time.Minute))

	if got, err := store.DeleteExpiredMembershipAtKeyQ(ctx, pool, u.ID, "user", live); err != nil || got != nil {
		t.Errorf("delete at a live key = %+v, %v; want nothing deleted", got, err)
	}
	if got, err := store.DeleteExpiredMembershipAtKeyQ(ctx, pool, u.ID, "user", atKey); err != nil || got == nil || got.ID != keyRow.ID {
		t.Errorf("delete at an ended key = %+v, %v; want %s", got, err, keyRow.ID)
	}
	swept := map[uuid.UUID]bool{}
	for {
		rows, err := st.SweepExpiredMemberships(ctx, time.Now().UTC(), 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rows {
			swept[m.ID] = true
		}
		if len(rows) < 200 {
			break
		}
	}
	if !swept[gone.ID] {
		t.Errorf("the sweep did not return the ended row %s", gone.ID)
	}
	if _, err := store.GetGovernanceMembershipByKeyQ(ctx, pool, u.ID, "user", live, false); err != nil {
		t.Errorf("the live elevation after the sweep: %v, want it kept", err)
	}
}

// TestGovernanceRequestReads: the list holds pending rows and ones decided since the cutoff; the last
// applied read is the newest applied change at the key.
func TestGovernanceRequestReads(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	key := uuid.NewString()
	ch := newChange(key)
	ch.TargetKind = types.GovernanceTargetElevation
	ch.Payload = json.RawMessage(`{"duration":"1h"}`)
	saved, _, err := st.ProposeGovernanceChange(ctx, ch, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.LastAppliedGovernanceChangeQ(ctx, pool, types.GovernanceTargetElevation, key); err != nil || got != nil {
		t.Errorf("last applied before any = %+v, %v; want nil", got, err)
	}
	list, err := st.ListGovernanceRequests(ctx, []string{types.GovernanceTargetElevation}, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range list {
		found = found || c.ID == saved.ID
	}
	if !found {
		t.Errorf("the pending request %s is not listed", saved.ID)
	}
	if _, err := pool.Exec(ctx, `UPDATE governance_changes SET state = 'applied', decided_at = now() - interval '2 days' WHERE id = $1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.LastAppliedGovernanceChangeQ(ctx, pool, types.GovernanceTargetElevation, key)
	if err != nil || got == nil || got.ID != saved.ID {
		t.Errorf("last applied = %+v, %v; want %s", got, err, saved.ID)
	}
	if list, err = st.ListGovernanceRequests(ctx, []string{types.GovernanceTargetElevation}, time.Now().UTC().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.ID == saved.ID {
			t.Errorf("a request decided two days ago is still listed")
		}
	}
}
