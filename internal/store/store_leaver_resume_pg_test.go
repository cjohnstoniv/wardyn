// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A sweeper's resume of a suspension is bound to the deactivation it listed. Guarded by WARDYN_TEST_PG;
// skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A resume (SuspendPlan.ResumeOf) of an identity reactivated since is refused and writes nothing: no
// deactivation, no epoch bump, no new cut. Once the person is suspended again the old instant is still
// refused, and a resume naming the current one repairs that suspension.
func TestPG_SuspendIdentity_ResumeOfAReactivatedIdentityWritesNothing(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	leaver := signedIn(t, st, "sub-leaver", "leaver@corp.example", leaverOID)
	plan := store.SuspendPlan{IdentityID: leaver.ID, Principal: "sub-leaver", Principals: []string{"sub-leaver"}, CutoffSubs: []string{"sub-leaver"}}
	if _, err := st.SuspendIdentity(ctx, plan); err != nil {
		t.Fatal(err)
	}
	listed, err := st.GetIdentity(ctx, leaver.ID)
	if err != nil || listed.DeactivatedAt == nil {
		t.Fatalf("suspended identity = %+v, %v", listed, err)
	}
	if _, err := st.ApplyIdentityUpdate(ctx, leaver.ID, store.IdentityUpdate{Reactivate: true}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cutAt := func() time.Time {
		t.Helper()
		var at time.Time
		if err := st.Pool.QueryRow(ctx, `SELECT revoked_at FROM oidc_session_revocations WHERE sub = 'sub-leaver'`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	cut := cutAt()

	resume := plan
	resume.ResumeOf = listed.DeactivatedAt
	if _, err := st.SuspendIdentity(ctx, resume); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("resume of a reactivated identity = %v, want ErrSuperseded", err)
	}
	if got, _ := st.GetIdentity(ctx, leaver.ID); got.DeactivatedAt != nil || got.AuthorityEpoch != listed.AuthorityEpoch {
		t.Errorf("the refused resume changed the identity: %+v", got)
	}
	if !cutAt().Equal(cut) {
		t.Error("the refused resume cut the person's sessions again")
	}

	if _, err := st.SuspendIdentity(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SuspendIdentity(ctx, resume); !errors.Is(err, store.ErrSuperseded) {
		t.Errorf("resume of the earlier suspension after a new one = %v, want ErrSuperseded", err)
	}
	current, _ := st.GetIdentity(ctx, leaver.ID)
	resume.ResumeOf = current.DeactivatedAt
	if res, err := st.SuspendIdentity(ctx, resume); err != nil || res.WasActive {
		t.Errorf("resume of the current suspension = %+v, %v; want the repair of it", res, err)
	}
}
