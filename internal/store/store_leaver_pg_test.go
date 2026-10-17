// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for leaver deprovisioning's store half (migration 0127). Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const leaverOID = "aaaaaaaa-0000-4000-8000-000000000001"

func signedIn(t *testing.T, st store.PG, principal, email, oid string) store.PrincipalIdentity {
	t.Helper()
	in := store.LoginIdentity{Principal: principal, Issuer: idIssuer, Email: email}
	if oid != "" {
		in.TenantID, in.ObjectID = idTenant, oid
	}
	row, err := st.UpsertLoginIdentity(context.Background(), in, time.Now().UTC())
	if err != nil {
		t.Fatalf("sign in %s: %v", principal, err)
	}
	return row
}

func cutoffs(t *testing.T, st store.PG) map[string]bool {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `SELECT sub FROM oidc_session_revocations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sub string
		if err := rows.Scan(&sub); err != nil {
			t.Fatal(err)
		}
		out[sub] = true
	}
	return out
}

// Step 1 is one transaction: the cutoff for every form, the deactivation and the epoch bump of the
// identity and of every row bound to its principal, the people row, and the ledger row. Another
// person's identity is untouched, a repeat bumps the epoch again and keeps the ledger, and a
// suspension of a reactivated identity starts a fresh ledger.
func TestPG_SuspendIdentity_OneTransaction(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	leaver := signedIn(t, st, "sub-leaver", "leaver@corp.example", leaverOID)
	other := signedIn(t, st, "sub-other", "other@corp.example", "")
	if _, err := st.Pool.Exec(ctx, `INSERT INTO people (principal, email, created_by) VALUES ('sub-leaver', 'leaver@corp.example', 'admin')`); err != nil {
		t.Fatal(err)
	}

	res, err := st.SuspendIdentity(ctx, store.SuspendPlan{
		IdentityID: leaver.ID, Principal: "sub-leaver", Principals: []string{"sub-leaver"}, CutoffSubs: []string{"sub-leaver", "leaver@corp.example", ""},
	})
	if err != nil || !res.WasActive || res.Epoch != 1 {
		t.Fatalf("suspend = %+v, %v; want a new suspension at epoch 1", res, err)
	}
	got, _ := st.GetIdentity(ctx, leaver.ID)
	if got.DeactivatedAt == nil || got.AuthorityEpoch != 1 {
		t.Errorf("leaver row = %+v, want deactivated at epoch 1", got)
	}
	if o, _ := st.GetIdentity(ctx, other.ID); o.DeactivatedAt != nil || o.AuthorityEpoch != 0 {
		t.Errorf("another person's identity was touched: %+v", o)
	}
	if c := cutoffs(t, st); !c["sub-leaver"] || !c["leaver@corp.example"] || c[""] || len(c) != 2 {
		t.Errorf("cutoff rows = %v, want exactly the sub and the email (never the global one)", c)
	}
	var personDeactivated bool
	if err := st.Pool.QueryRow(ctx, `SELECT deactivated_at IS NOT NULL FROM people WHERE principal = 'sub-leaver'`).Scan(&personDeactivated); err != nil || !personDeactivated {
		t.Errorf("people.deactivated_at not set (%v, %v)", personDeactivated, err)
	}
	jobs, _ := st.ListDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend)
	if len(jobs) != 1 || jobs[0].Step != store.JobStepCutoff || !jobs[0].Done || jobs[0].Detail["sessions_cut"] != 2 {
		t.Errorf("ledger = %+v, want one done cutoff row counting two sessions", jobs)
	}

	if err := st.EnsureDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend, []store.JobKey{{Step: "sweep", Target: "sub-leaver"}}); err != nil {
		t.Fatal(err)
	}
	again, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: leaver.ID, Principal: "sub-leaver", Principals: []string{"sub-leaver"}})
	if err != nil || again.WasActive || again.Epoch != 2 {
		t.Fatalf("repeat suspend = %+v, %v; want the repair of an old one, epoch 2", again, err)
	}
	if jobs, _ = st.ListDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend); len(jobs) != 2 {
		t.Errorf("a repeat dropped ledger rows: %+v", jobs)
	}

	react, err := st.ApplyIdentityUpdate(ctx, leaver.ID, store.IdentityUpdate{Reactivate: true}, time.Now().UTC())
	if err != nil || react.DeactivatedAt != nil || react.AuthorityEpoch != 2 {
		t.Fatalf("reactivate = %+v, %v; want active, and the epoch never lowered", react, err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT deactivated_at IS NOT NULL FROM people WHERE principal = 'sub-leaver'`).Scan(&personDeactivated); err != nil || personDeactivated {
		t.Errorf("reactivation left people.deactivated_at set (%v, %v)", personDeactivated, err)
	}
	next, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: leaver.ID, Principal: "sub-leaver", Principals: []string{"sub-leaver"}})
	if err != nil || !next.WasActive || next.Epoch != 3 {
		t.Fatalf("second suspension = %+v, %v; want a new one at epoch 3", next, err)
	}
	if jobs, _ = st.ListDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend); len(jobs) != 1 {
		t.Errorf("a new suspension kept the old ledger: %+v", jobs)
	}
}

// A purged identity is a permanent tombstone: reactivation is refused and changes nothing.
func TestPG_ReactivatePurgedIsRefused(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	row := signedIn(t, st, "sub-gone", "gone@corp.example", leaverOID)
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: row.ID, Principal: "sub-gone", Principals: []string{"sub-gone"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE principal_identities SET purged_at = now() WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyIdentityUpdate(ctx, row.ID, store.IdentityUpdate{Reactivate: true, UserName: "new-name"}, time.Now().UTC()); !errors.Is(err, store.ErrIdentityPurged) {
		t.Fatalf("reactivate a purged identity = %v, want ErrIdentityPurged", err)
	}
	got, _ := st.GetIdentity(ctx, row.ID)
	if got.DeactivatedAt == nil || got.ScimUserName != "" {
		t.Errorf("the refused reactivation changed the row: %+v", got)
	}
	if _, err := st.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: "sub-gone", Issuer: idIssuer, TenantID: idTenant, ObjectID: leaverOID}, time.Now().UTC()); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("a purged identity's sign-in = %v, want ErrIdentityDeactivated", err)
	}
}

// Issuance upserts and binds the row, returns the epoch the cookie carries, and a deactivated identity
// is refused without a write.
func TestPG_IssueLoginIdentity(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	// A SCIM user who has never signed in: no principal yet.
	scimRow, created, err := st.CreateScimIdentity(ctx, idIssuer, idTenant, leaverOID, store.IdentityUpdate{ExternalID: leaverOID, UserName: "New.User@corp.example"}, time.Now().UTC())
	if err != nil || !created || scimRow.Principal != "" {
		t.Fatalf("create scim identity = %+v, %v, %v", scimRow, created, err)
	}
	in := store.LoginIdentity{Principal: "sub-new", Issuer: idIssuer, TenantID: idTenant, ObjectID: leaverOID, Email: "new.user@corp.example"}
	epoch, err := st.IssueLoginIdentity(ctx, in, time.Now().UTC())
	if err != nil || epoch != 0 {
		t.Fatalf("issue = %d, %v; want epoch 0", epoch, err)
	}
	if bound, _ := st.GetIdentity(ctx, scimRow.ID); bound.Principal != "sub-new" || bound.LastLoginAt == nil {
		t.Errorf("the first sign-in did not bind the SCIM row: %+v", bound)
	}
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: scimRow.ID, Principal: "sub-new", Principals: []string{"sub-new"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetIdentity(ctx, scimRow.ID)
	if _, err := st.IssueLoginIdentity(ctx, in, time.Now().UTC().Add(time.Minute)); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Fatalf("issue for a deactivated identity = %v, want ErrIdentityDeactivated", err)
	}
	if after, _ := st.GetIdentity(ctx, scimRow.ID); !after.LastLoginAt.Equal(*before.LastLoginAt) {
		t.Errorf("a refused sign-in stamped last_login_at: %v -> %v", before.LastLoginAt, after.LastLoginAt)
	}
	refused, err := st.IdentityRefused(ctx, idIssuer, idTenant, leaverOID, "")
	if err != nil || !refused {
		t.Errorf("IdentityRefused by object id = %v, %v; want true", refused, err)
	}
	if refused, _ = st.IdentityRefused(ctx, idIssuer, idTenant, "bbbbbbbb-0000-4000-8000-000000000002", "sub-other"); refused {
		t.Error("an unrelated identity is refused")
	}
}

// A sign-in naming an identity already bound to another principal is refused, at upsert and at
// issuance, and writes nothing: no binding move, no login stamp, no email alias.
func TestPG_IssueLoginIdentity_RefusesAnotherPrincipal(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	orig := signedIn(t, st, "pairwise-a", "a@corp.example", leaverOID)
	moved := store.LoginIdentity{Principal: "entra:" + idTenant + ":" + leaverOID, Issuer: idIssuer, TenantID: idTenant, ObjectID: leaverOID, Email: "moved@corp.example"}
	later := time.Now().UTC().Add(time.Minute)
	if _, err := st.UpsertLoginIdentity(ctx, moved, later); !errors.Is(err, store.ErrIdentityBindingMismatch) {
		t.Errorf("upsert under another principal = %v, want ErrIdentityBindingMismatch", err)
	}
	if _, err := st.IssueLoginIdentity(ctx, moved, later); !errors.Is(err, store.ErrIdentityBindingMismatch) {
		t.Errorf("issue under another principal = %v, want ErrIdentityBindingMismatch", err)
	}
	got, err := st.GetIdentity(ctx, orig.ID)
	if err != nil || got.Principal != "pairwise-a" || got.EmailLower != "a@corp.example" || !got.LastLoginAt.Equal(*orig.LastLoginAt) {
		t.Errorf("identity = %+v (%v), want the original binding, email and login stamp", got, err)
	}
	if a := identityAliases(t, pool, orig.ID); len(a) != 1 || a[0] != "a@corp.example" {
		t.Errorf("aliases = %v, want only the original email", a)
	}
}

// The pending work a suspension names is written in its own transaction, beside the done cutoff, so a
// crash after the commit still leaves the suspension for the sweeper to resume.
func TestPG_SuspendIdentity_WritesPendingJobsWithTheCutoff(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	leaver := signedIn(t, st, "sub-pending", "pending@corp.example", leaverOID)
	keys := []store.JobKey{{Step: "audit_deactivate"}, {Step: "sweep", Target: "sub-pending"}}
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: leaver.ID, Principal: "sub-pending", Principals: []string{"sub-pending"}, PendingJobs: keys}); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListDeprovisionJobs(ctx, leaver.ID, store.JobKindSuspend)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("ledger = %+v (%v), want the cutoff and two pending rows", jobs, err)
	}
	for _, j := range jobs {
		if (j.Step == store.JobStepCutoff) != j.Done {
			t.Errorf("job %+v: only the cutoff is done", j)
		}
	}
	pending, err := st.PendingLeavers(ctx, 0, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != leaver.ID {
		t.Errorf("pending leavers = %+v (%v), want the suspension", pending, err)
	}
}

// The owner guard: the insert writers refuse a deactivated or past-epoch owner inside their own
// transaction, admit the current epoch, a caller with no epoch and a principal with no row, and a
// guard never reaches another person.
func TestPG_IdentityGuardRefusesInsertWriters(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	row := signedIn(t, st, "sub-guard", "guard@corp.example", "")
	token := func(ctx context.Context, principal string) error {
		_, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: principal, Role: "user", UserType: "standard", Name: "t", CreatedAt: time.Now().UTC()}, "wdn_"+uuid.NewString())
		return err
	}
	key := func(ctx context.Context, principal string) error {
		_, err := st.AddSSHKey(ctx, types.SSHPublicKey{Fingerprint: "SHA256:" + uuid.NewString(), Principal: principal, Name: "k", PublicKey: "ssh-ed25519 AAAA", Role: "user", CreatedAt: time.Now().UTC()})
		return err
	}
	run := func(ctx context.Context, principal string) error {
		r := newRun(types.RunPending)
		r.CreatedBy = principal
		_, err := st.CreateRun(ctx, r)
		return err
	}
	writers := map[string]func(context.Context, string) error{"api token": token, "ssh key": key, "run": run}
	guard := func(principal string, epoch int64) context.Context {
		return store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: principal, Epoch: epoch})
	}
	for name, write := range writers {
		if err := write(guard("sub-guard", 0), "sub-guard"); err != nil {
			t.Errorf("%s: an active owner at the current epoch was refused: %v", name, err)
		}
		if err := write(guard("sub-nobody", 0), "sub-nobody"); err != nil {
			t.Errorf("%s: a principal with no identity row was refused: %v", name, err)
		}
	}
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: row.ID, Principal: "sub-guard", Principals: []string{"sub-guard"}}); err != nil {
		t.Fatal(err)
	}
	for name, write := range writers {
		if err := write(guard("sub-guard", 0), "sub-guard"); !errors.Is(err, store.ErrIdentityDeactivated) {
			t.Errorf("%s: an owner admitted before the suspension = %v, want ErrIdentityDeactivated", name, err)
		}
		if err := write(guard("sub-guard", -1), "sub-guard"); !errors.Is(err, store.ErrIdentityDeactivated) {
			t.Errorf("%s: a deactivated owner with no epoch = %v, want ErrIdentityDeactivated", name, err)
		}
		if err := write(guard("sub-nobody", 0), "sub-nobody"); err != nil {
			t.Errorf("%s: the guard reached another person: %v", name, err)
		}
		if err := write(ctx, "sub-guard"); err != nil {
			t.Errorf("%s: an unguarded write was refused: %v", name, err)
		}
	}
	// After a reactivation the identity is active, but a credential admitted under the old epoch stays refused.
	if _, err := st.ApplyIdentityUpdate(ctx, row.ID, store.IdentityUpdate{Reactivate: true}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for name, write := range writers {
		if err := write(guard("sub-guard", 0), "sub-guard"); !errors.Is(err, store.ErrIdentityDeactivated) {
			t.Errorf("%s: an old-epoch owner after a reactivation = %v, want ErrIdentityDeactivated", name, err)
		}
		if err := write(guard("sub-guard", 1), "sub-guard"); err != nil {
			t.Errorf("%s: an owner admitted at the new epoch was refused: %v", name, err)
		}
	}
}

// A suspension of an identity no principal is bound to (someone who never signed in) deactivates that
// row only: another person's identity row, token and key stay as they were.
func TestPG_SuspendNeverSignedInTouchesNobodyElse(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	other := signedIn(t, st, "sub-bystander", "bystander@corp.example", "")
	ghost, _, err := st.CreateScimIdentity(ctx, idIssuer, idTenant, leaverOID, store.IdentityUpdate{ExternalID: leaverOID, UserName: "ghost@corp.example"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: ghost.ID, CutoffSubs: []string{"ghost@corp.example"}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.GetIdentity(ctx, ghost.ID); g.DeactivatedAt == nil || g.Principal != "" {
		t.Errorf("ghost row = %+v, want deactivated and still unbound", g)
	}
	if o, _ := st.GetIdentity(ctx, other.ID); o.DeactivatedAt != nil || o.AuthorityEpoch != 0 {
		t.Errorf("a bystander's identity was touched: %+v", o)
	}
	if blocked, err := st.IdentityBlocked(ctx, "sub-bystander", -1); err != nil || blocked {
		t.Errorf("IdentityBlocked(bystander) = %v, %v; want false", blocked, err)
	}
}

// A captured credential's write borrows from the pool while its owner's identity rows are held, so
// the hold takes no pool connection: on a one-connection pool the write still lands.
func TestPG_WithIdentitySharedLeavesThePoolToFn(t *testing.T) {
	cfg := runsPGPoolIsolated(t).Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st := store.NewPG(pool)
	signedIn(t, st, "sub-shared", "shared@corp.example", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = st.WithIdentityShared(ctx, store.IdentityGuard{Principal: "sub-shared", Epoch: -1}, func() error {
		_, err := pool.Exec(ctx, `SELECT 1`)
		return err
	})
	if err != nil {
		t.Fatalf("WithIdentityShared on a one-connection pool: %v", err)
	}
}
