// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for leaver deprovisioning's store half (migration 0127). Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// Pending leavers are listed with the fewest failed attempts first: an older leaver whose step keeps
// failing does not take the batch ahead of a newer one no step has tried yet.
func TestPG_PendingLeavers_FewestFailedAttemptsFirst(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	step := store.JobKey{Step: "sweep", Target: "x"}
	suspend := func(principal, email string) store.PrincipalIdentity {
		row := signedIn(t, st, principal, email, "")
		if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: row.ID, Principal: principal, Principals: []string{principal}, PendingJobs: []store.JobKey{step}}); err != nil {
			t.Fatal(err)
		}
		return row
	}
	older := suspend("sub-older", "older@corp.example")
	newer := suspend("sub-newer", "newer@corp.example")
	if _, err := st.Pool.Exec(ctx, `UPDATE principal_identities SET created_at = now() - interval '1 hour' WHERE id = $1`, older.ID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := st.FailDeprovisionJob(ctx, older.ID, store.JobKindSuspend, step, errors.New("held")); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := st.PendingLeavers(ctx, 0, 1)
	if err != nil || len(pending) != 1 || pending[0].ID != newer.ID {
		t.Fatalf("first pending leaver = %+v (%v), want the newer one no step has tried", pending, err)
	}
	if all, err := st.PendingLeavers(ctx, 0, 10); err != nil || len(all) != 2 || all[1].ID != older.ID {
		t.Fatalf("pending leavers = %+v (%v), want the retried one listed after", all, err)
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

const (
	oldSearchUserNameWhere = `(lower(scim_user_name) = $2 OR EXISTS (SELECT 1 FROM principal_identity_aliases a
		WHERE a.identity_id = principal_identities.id AND a.value_lower = $2))`
	oldSearchEmailWhere = `(email_lower = $2 OR EXISTS (SELECT 1 FROM principal_identity_aliases a
		WHERE a.identity_id = principal_identities.id AND a.kind = 'email' AND a.value_lower = $2))`
	oldSearchExternalIDWhere = `(lower(scim_external_id) = $2 OR object_id = $2)`
	oldByAliasSQL            = `SELECT id FROM principal_identities WHERE issuer = $1 AND object_id = '' AND (email_lower = $2 OR EXISTS (
		SELECT 1 FROM principal_identity_aliases a WHERE a.identity_id = principal_identities.id AND a.value_lower = $2))
		ORDER BY created_at, id`
)

func identityIDs(rows []store.PrincipalIdentity) []uuid.UUID {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func queryIDs(t *testing.T, pool *pgxpool.Pool, q string, args ...any) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(t.Context(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// The SCIM lookups were rewritten from an OR over an alias sub-select into a UNION of indexable arms; this
// holds them to the answers of the OR form over a corpus that tells the arms apart.
func TestPG_ScimLookupsMatchTheirOrForm(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	const other = "https://other.example"
	for i, r := range []struct{ issuer, object, email, user, ext string }{
		{idIssuer, "oa", "alice@corp.example", "Alice@Corp.Example", "EXT-A"},
		{idIssuer, "", "bob@corp.example", "bob", ""},
		{other, "oa", "alice@corp.example", "Alice@Corp.Example", "EXT-A"},
		{idIssuer, "od", "", "", ""},
		{idIssuer, "ext-a", "", "", ""},
		{idIssuer, "", "", "", ""},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO principal_identities (issuer, object_id, email_lower, scim_user_name, scim_external_id, created_at)
			VALUES ($1, $2, $3, $4, $5, now() - make_interval(secs => $6))`, r.issuer, r.object, r.email, r.user, r.ext, float64(100-i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []struct{ owner, kind, value string }{
		{"bob@corp.example", "email", "bob.old@corp.example"}, {"bob@corp.example", "user_name", "bobby"},
		{"od", "user_name", "alice@corp.example"}, {"od", "email", "bob.old@corp.example"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO principal_identity_aliases (identity_id, kind, value_lower, source)
			SELECT id, $2, $3, 'scim' FROM principal_identities WHERE issuer = $4 AND (email_lower = $1 OR object_id = $1)`, a.owner, a.kind, a.value, idIssuer); err != nil {
			t.Fatal(err)
		}
	}
	values := []string{"alice@corp.example", " ALICE@Corp.Example ", "ext-a", "EXT-A", "oa", "bobby", "bob.old@corp.example", "bob", "", "nobody"}
	for attr, where := range map[string]string{
		store.SearchUserName: oldSearchUserNameWhere, store.SearchEmail: oldSearchEmailWhere, store.SearchExternalID: oldSearchExternalIDWhere,
	} {
		for _, v := range values {
			for _, limit := range []int{10, 1} {
				got, err := st.SearchIdentities(ctx, idIssuer, attr, v, limit)
				if err != nil {
					t.Fatal(err)
				}
				want := queryIDs(t, pool, `SELECT id FROM principal_identities WHERE issuer = $1 AND `+where+` ORDER BY created_at, id LIMIT $3`,
					idIssuer, strings.ToLower(strings.TrimSpace(v)), limit)
				if !slices.Equal(identityIDs(got), want) {
					t.Errorf("SearchIdentities(%s, %q, %d) = %v, the OR form gives %v", attr, v, limit, identityIDs(got), want)
				}
			}
		}
	}
	nonEmpty := 0
	for _, issuer := range []string{idIssuer, other} {
		for _, v := range values {
			v = strings.ToLower(strings.TrimSpace(v))
			got, err := st.IdentitiesByAlias(ctx, issuer, v)
			if err != nil {
				t.Fatal(err)
			}
			want := queryIDs(t, pool, oldByAliasSQL, issuer, v)
			if !slices.Equal(identityIDs(got), want) {
				t.Errorf("IdentitiesByAlias(%s, %q) = %v, the OR form gives %v", issuer, v, identityIDs(got), want)
			}
			if len(want) > 0 {
				nonEmpty++
			}
		}
	}
	if nonEmpty == 0 {
		t.Fatal("no alias lookup of the corpus found anything, so the comparison proves nothing")
	}
}

// A provisioning sync does one lookup per user, so each lookup reads its own arms by index rather than every
// identity under the issuer, for an Entra directory (object ids) and for one whose rows carry none.
func TestPG_ScimLookupsUseTheirIndexes(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := t.Context()
	const noObjectIDs = "https://idp.example"
	if _, err := pool.Exec(ctx, `
		INSERT INTO principal_identities (issuer, tenant_id, object_id, email_lower, scim_external_id, scim_user_name)
		SELECT i, CASE WHEN i = $1 THEN '' ELSE 't' END, CASE WHEN i = $1 THEN '' ELSE 'o' || g END,
		       'u' || g || '@corp.example', 'X' || g, 'U' || g || '@corp.example'
		FROM unnest(ARRAY[$1, $2]) i, generate_series(1, 20000) g`, noObjectIDs, idIssuer); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO principal_identity_aliases (identity_id, kind, value_lower, source)
		SELECT id, k, k || '-' || id, 'scim' FROM principal_identities, unnest(ARRAY['email', 'user_name']) k`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE principal_identities; ANALYZE principal_identity_aliases`); err != nil {
		t.Fatal(err)
	}
	const aliasIdx = "principal_identity_aliases_value_lower_idx"
	search := func(issuer, attr, value string) func(store.PG) {
		return func(st store.PG) { _, _ = st.SearchIdentities(ctx, issuer, attr, value, 10) }
	}
	byAlias := func(issuer string) func(store.PG) {
		return func(st store.PG) { _, _ = st.IdentitiesByAlias(ctx, issuer, "u77@corp.example") }
	}
	for name, c := range map[string]struct {
		call func(store.PG)
		idx  []string
	}{
		"userName":             {search(noObjectIDs, store.SearchUserName, "U77@corp.example"), []string{"principal_identities_scim_user_name_idx", aliasIdx}},
		"emails.value":         {search(idIssuer, store.SearchEmail, "u77@corp.example"), []string{"principal_identities_email_lower_idx", aliasIdx}},
		"externalId":           {search(idIssuer, store.SearchExternalID, "x77"), []string{"principal_identities_scim_external_id_idx", "principal_identities_object_id_idx"}},
		"by alias, no objects": {byAlias(noObjectIDs), []string{"principal_identities_email_lower_idx", aliasIdx}},
		"by alias, objects":    {byAlias(idIssuer), []string{"principal_identities_email_lower_idx", aliasIdx}},
	} {
		plan := planOfCall(t, ctx, pool, c.call)
		t.Logf("%s plan:\n%s", name, plan)
		if strings.Contains(plan, "Seq Scan on principal_identit") {
			t.Errorf("%s scans a table:\n%s", name, plan)
		}
		for _, idx := range c.idx {
			if !strings.Contains(plan, idx) {
				t.Errorf("%s does not use %s:\n%s", name, idx, plan)
			}
		}
	}
}
