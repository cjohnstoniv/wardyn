// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// What a suspension's tail reads to reach only what it owed. Guarded by WARDYN_TEST_PG; skipped cleanly when
// unset.
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

// A new run, token and SSH key get inserted_at from the column default, inside the database's own readings
// around the insert, whatever the application put in created_at. SuspendIdentity reports the deactivated_at it
// left on the row, and a repeat keeps it. While the person is deactivated nothing is spared: the probes name no
// row and DeleteSSHKeysUpTo deletes a key stamped after the bound. Once the person is active again, a row stamped
// strictly after the bound is the only one not owed: one stamped at the bound itself, one before it and one from
// before the stamp existed (NULL) are owed. RunsInsertedAfter and APITokensInsertedAfter name only the later
// row, and DeleteSSHKeysUpTo deletes every key but the later one. SubCutSince counts a cutoff written at the
// bound and not one written before it, and none while the person is deactivated. FinishSuspensionJobAt closes a
// step for the identity's current deactivation and refuses one bound to another instant.
func TestPG_SuspensionBounds_TheBoundItselfIsOwed(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	leaver := signedIn(t, st, "sub-stamp", "stamp@corp.example", leaverOID)
	plan := store.SuspendPlan{IdentityID: leaver.ID, Principal: "sub-stamp", Principals: []string{"sub-stamp"}}
	clock := func() time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	stamp := func(table, key string, id any) *time.Time {
		t.Helper()
		var at *time.Time
		if err := pool.QueryRow(ctx, `SELECT inserted_at FROM `+table+` WHERE `+key+` = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}

	lo := clock()
	var runs, toks []uuid.UUID
	var keys []string
	for i := 0; i < 4; i++ {
		r := newRun(types.RunRunning)
		r.CreatedAt = r.CreatedAt.Add(24 * time.Hour) // an app clock far ahead moves created_at, never inserted_at
		runs = append(runs, persistRun(t, ctx, pool, r).ID)
		toks = append(toks, seedToken(t, st, "sub-stamp", uuid.NewString(), nil).ID)
		fp := "SHA256:" + uuid.NewString()
		if _, err := st.AddSSHKey(ctx, types.SSHPublicKey{Fingerprint: fp, Principal: "sub-stamp", Name: "k",
			PublicKey: "ssh-ed25519 AAAA " + fp, Role: "user", CreatedAt: time.Now().UTC().Add(24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, fp)
	}
	hi := clock()
	for i := range runs {
		for _, got := range []*time.Time{stamp("agent_runs", "id", runs[i]), stamp("api_tokens", "id", toks[i]), stamp("ssh_public_keys", "fingerprint", keys[i])} {
			if got == nil || got.Before(lo) || got.After(hi) {
				t.Fatalf("inserted_at = %v, want the database's clock inside [%v, %v]", got, lo, hi)
			}
		}
	}

	bound := hi.Add(time.Hour).Truncate(time.Microsecond)
	set := []any{nil, bound.Add(-time.Microsecond), bound, bound.Add(time.Microsecond)}
	for i, v := range set {
		for _, q := range []struct {
			sql string
			id  any
		}{
			{`UPDATE agent_runs SET inserted_at = $2 WHERE id = $1`, runs[i]},
			{`UPDATE api_tokens SET inserted_at = $2 WHERE id = $1`, toks[i]},
			{`UPDATE ssh_public_keys SET inserted_at = $2 WHERE fingerprint = $1`, keys[i]},
		} {
			if _, err := pool.Exec(ctx, q.sql, q.id, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := st.SuspendIdentity(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetIdentity(ctx, leaver.ID); err != nil || got.DeactivatedAt == nil || !got.DeactivatedAt.Equal(res.DeactivatedAt) {
		t.Fatalf("SuspendIdentity reported deactivated_at %v; the row holds %v (%v)", res.DeactivatedAt, got.DeactivatedAt, err)
	}
	if again, err := st.SuspendIdentity(ctx, plan); err != nil || !again.DeactivatedAt.Equal(res.DeactivatedAt) {
		t.Errorf("a repeat suspension reported %v, %v; want the kept %v", again.DeactivatedAt, err, res.DeactivatedAt)
	}
	gotRuns, err := st.RunsInsertedAfter(ctx, leaver.ID, runs, bound)
	if err != nil {
		t.Fatal(err)
	}
	gotToks, err := st.APITokensInsertedAfter(ctx, leaver.ID, toks, bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotRuns) != 0 || len(gotToks) != 0 {
		t.Errorf("while the person is deactivated the probes spared runs %v and tokens %v, want none", gotRuns, gotToks)
	}
	other := "SHA256:" + uuid.NewString()
	if _, err := st.AddSSHKey(ctx, types.SSHPublicKey{Fingerprint: other, Principal: "sub-other", Name: "k",
		PublicKey: "ssh-ed25519 AAAA " + other, Role: "user", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ssh_public_keys SET inserted_at = $2 WHERE fingerprint = $1`, other, bound.Add(time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if n, err := st.DeleteSSHKeysUpTo(ctx, leaver.ID, "sub-other", bound); err != nil || n != 1 {
		t.Errorf("while the person is deactivated, keys deleted = %d, %v; want the one stamped after the bound", n, err)
	}

	if _, err := st.ApplyIdentityUpdate(ctx, leaver.ID, store.IdentityUpdate{Reactivate: true}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if gotRuns, err = st.RunsInsertedAfter(ctx, leaver.ID, runs, bound); err != nil {
		t.Fatal(err)
	}
	if gotToks, err = st.APITokensInsertedAfter(ctx, leaver.ID, toks, bound); err != nil {
		t.Fatal(err)
	}
	if len(gotRuns) != 1 || !gotRuns[runs[3]] {
		t.Errorf("runs after the bound = %v, want only %s, stamped a microsecond after it", gotRuns, runs[3])
	}
	if len(gotToks) != 1 || !gotToks[toks[3]] {
		t.Errorf("tokens after the bound = %v, want only %s, stamped a microsecond after it", gotToks, toks[3])
	}
	if n, err := st.DeleteSSHKeysUpTo(ctx, leaver.ID, "sub-stamp", bound); err != nil || n != 3 {
		t.Errorf("keys deleted up to the bound = %d, %v; want 3", n, err)
	}
	if left, err := st.ListSSHKeysByPrincipal(ctx, "sub-stamp"); err != nil || len(left) != 1 || left[0].Fingerprint != keys[3] {
		t.Errorf("keys left = %+v, %v; want only %s, stamped a microsecond after the bound", left, err, keys[3])
	}

	for _, c := range []struct {
		at   time.Time
		want bool
	}{{bound.Add(-time.Microsecond), false}, {bound, true}} {
		if _, err := pool.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ('sub-cut', $1)
			ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`, c.at); err != nil {
			t.Fatal(err)
		}
		if got, err := st.SubCutSince(ctx, leaver.ID, "sub-cut", bound); err != nil || got != c.want {
			t.Errorf("cutoff written %v before the bound: cut since = %v, %v; want %v", bound.Sub(c.at), got, err, c.want)
		}
	}
	if got, err := st.SubCutSince(ctx, leaver.ID, "sub-never-cut", bound); err != nil || got {
		t.Errorf("a sub with no cutoff: cut since = %v, %v; want false", got, err)
	}

	res, err = st.SuspendIdentity(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.SubCutSince(ctx, leaver.ID, "sub-cut", bound); err != nil || got {
		t.Errorf("while the person is deactivated, a cutoff at the bound: cut since = %v, %v; want false", got, err)
	}
	key := store.JobKey{Step: "sweep", Target: "x"}
	if err := st.EnsureDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend, []store.JobKey{key}); err != nil {
		t.Fatal(err)
	}
	done := func() bool {
		t.Helper()
		jobs, err := st.ListDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if j.Step == key.Step && j.Target == key.Target {
				return j.Done
			}
		}
		t.Fatalf("no ledger row %+v", key)
		return false
	}
	if err := st.FinishSuspensionJobAt(ctx, leaver.ID, key, nil, res.DeactivatedAt.Add(-time.Microsecond)); !errors.Is(err, store.ErrSuperseded) || done() {
		t.Errorf("a finish bound to another instant = %v, row done %v; want ErrSuperseded and the row pending", err, done())
	}
	if err := st.FinishSuspensionJobAt(ctx, leaver.ID, key, nil, res.DeactivatedAt); err != nil || !done() {
		t.Errorf("a finish bound to the current deactivation = %v, row done %v; want it done", err, done())
	}
}

// A finish closes a step only for the suspension whose ledger it is. A tail bound to a first deactivation closes
// nothing once a second suspension reset the ledger, also after the person is reactivated again, and the second
// suspension's failed row keeps its failure; a tail bound to the second deactivation still closes its own step
// with the person active. A ledger whose cutoff step holds no record of its deactivation (an earlier release
// wrote it) is closed while the identity is still deactivated at the tail's bound, and not once it is active.
// A suspension of another identity that names this one's principal deactivates it again without resetting its
// ledger: a tail bound to the deactivation the ledger records then closes nothing and a failed row keeps its
// failure, while a tail bound to the later deactivation closes it.
func TestPG_SuspensionBounds_AFinishClosesOnlyItsOwnSuspensionsLedger(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	leaver := signedIn(t, st, "sub-twice", "twice@corp.example", leaverOID)
	plan := store.SuspendPlan{IdentityID: leaver.ID, Principal: "sub-twice", Principals: []string{"sub-twice"}}
	key := store.JobKey{Step: "kill_run", Target: uuid.NewString()}
	suspend := func() time.Time {
		t.Helper()
		res, err := st.SuspendIdentity(ctx, plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.EnsureDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend, []store.JobKey{key}); err != nil {
			t.Fatal(err)
		}
		return res.DeactivatedAt
	}
	reactivate := func() {
		t.Helper()
		if _, err := st.ApplyIdentityUpdate(ctx, leaver.ID, store.IdentityUpdate{Reactivate: true}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	row := func() store.DeprovisionJob {
		t.Helper()
		jobs, err := st.ListDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if j.Step == key.Step && j.Target == key.Target {
				return j
			}
		}
		t.Fatalf("no ledger row %+v", key)
		return store.DeprovisionJob{}
	}

	first := suspend()
	reactivate()
	second := suspend()
	if err := st.FailDeprovisionJob(ctx, leaver.ID, store.JobKindSuspend, key, errors.New("teardown failed")); err != nil {
		t.Fatal(err)
	}
	reactivate()
	if err := st.FinishSuspensionJobAt(ctx, leaver.ID, key, map[string]int{"runs_killed": 0}, first); !errors.Is(err, store.ErrSuperseded) {
		t.Errorf("a finish bound to the first deactivation, person active again = %v, want ErrSuperseded", err)
	}
	if j := row(); j.Done || j.LastError != "teardown failed" {
		t.Errorf("the second suspension's row = %+v, want pending with its failure", j)
	}
	if err := st.FinishSuspensionJobAt(ctx, leaver.ID, key, nil, second); err != nil || !row().Done {
		t.Errorf("a finish bound to the second deactivation, person active again = %v, row done %v; want it done", err, row().Done)
	}

	third := suspend()
	if _, err := pool.Exec(ctx, `UPDATE deprovision_jobs SET detail = detail - 'deactivated_at_us' WHERE identity_id = $1 AND step = 'cutoff'`, leaver.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSuspensionJobAt(ctx, leaver.ID, key, nil, third); err != nil || !row().Done {
		t.Errorf("a finish over an unrecorded cutoff, identity deactivated at the bound = %v, row done %v; want it done", err, row().Done)
	}
	if err := st.ReopenDeprovisionJob(ctx, leaver.ID, store.JobKindSuspend, key); err != nil {
		t.Fatal(err)
	}
	reactivate()
	if err := st.FinishSuspensionJobAt(ctx, leaver.ID, key, nil, third); !errors.Is(err, store.ErrSuperseded) || row().Done {
		t.Errorf("a finish over an unrecorded cutoff, person active again = %v, row done %v; want ErrSuperseded and pending", err, row().Done)
	}

	fourth := suspend()
	reactivate()
	other := signedIn(t, st, "sub-alongside", "alongside@corp.example", "")
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: other.ID, Principal: "sub-alongside", Principals: []string{"sub-alongside", "sub-twice"}}); err != nil {
		t.Fatal(err)
	}
	alongside, err := st.GetIdentity(ctx, leaver.ID)
	if err != nil || alongside.DeactivatedAt == nil || alongside.DeactivatedAt.Equal(fourth) {
		t.Fatalf("control: the identity after another identity's suspension = %+v (%v), want it deactivated at a later instant", alongside, err)
	}
	if err := st.FailDeprovisionJob(ctx, leaver.ID, store.JobKindSuspend, key, errors.New("teardown failed again")); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSuspensionJobAt(ctx, leaver.ID, key, map[string]int{"runs_killed": 0}, fourth); !errors.Is(err, store.ErrSuperseded) {
		t.Errorf("a finish bound to the recorded deactivation, identity deactivated at a later instant = %v, want ErrSuperseded", err)
	}
	if j := row(); j.Done || j.LastError != "teardown failed again" {
		t.Errorf("the row under the later deactivation = %+v, want pending with its failure", j)
	}
	if err := st.FinishSuspensionJobAt(ctx, leaver.ID, key, nil, *alongside.DeactivatedAt); err != nil || !row().Done {
		t.Errorf("a finish bound to the later deactivation, ledger recording the earlier one = %v, row done %v; want it done", err, row().Done)
	}
}
