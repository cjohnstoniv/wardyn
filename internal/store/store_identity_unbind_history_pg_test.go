// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// An unbind keeps the principal it released (principal_identity_unbinds), so a WARDYN_AUDIT_SEAL=full row
// written before it reads as that person, and their erasure erases it, whoever holds the identity row now.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// historyKeys is an audit.SealKeys that always has a key: reading a subject opens none.
type historyKeys struct{}

func (historyKeys) Current(context.Context, string, string) (uuid.UUID, []byte, error) {
	return uuid.New(), make([]byte, 32), nil
}
func (historyKeys) Key(context.Context, string, uuid.UUID) ([]byte, error) {
	return make([]byte, 32), nil
}

// destroyAuditKey records the next destroyed audit-seal generation for owner, as an erasure's destroy does
// (at now()), or at the given time.
func destroyAuditKey(t *testing.T, pool *pgxpool.Pool, owner string, at ...time.Time) {
	t.Helper()
	var when *time.Time
	if len(at) > 0 {
		when = &at[0]
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key, destroyed_at)
		VALUES ($1, 'audit-seal', (SELECT COALESCE(max(version), 0) + 1 FROM principal_keys WHERE owner = $1 AND purpose = 'audit-seal'),
			'default', 'k', NULL, COALESCE($2::timestamptz, now()))`, owner, when); err != nil {
		t.Fatal(err)
	}
}

func TestPG_UnbindKeepsTheReleasedPrincipalForAuditReads(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := t.Context()
	st := store.NewPG(pool)
	issuer := "https://login.example/" + uuid.NewString() + "/v2.0"
	tenant, object := uuid.NewString(), uuid.NewString()
	login := func(principal string) store.PrincipalIdentity {
		t.Helper()
		row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: issuer, TenantID: tenant, ObjectID: object}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	s := &audit.Sealer{Keys: historyKeys{}, Subjects: st, SealActor: true}
	seal := func(person string) types.AuditEvent {
		t.Helper()
		ev, pending, err := s.Seal(ctx, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: person, Action: "run.kill", Outcome: "success"})
		if err != nil || pending {
			t.Fatalf("seal for %s: pending %v, %v", person, pending, err)
		}
		return ev
	}
	reads := func(what string, evs []types.AuditEvent, want ...string) {
		t.Helper()
		back, err := s.Unseal(ctx, evs)
		if err != nil {
			t.Fatal(err)
		}
		for i := range want {
			if back[i].Actor != want[i] {
				t.Errorf("%s: row %d reads %q, want %q", what, i, back[i].Actor, want[i])
			}
		}
	}
	sharing := func(principal, other string) bool {
		t.Helper()
		got, err := st.PrincipalsSharingObject(ctx, principal)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(got, other)
	}

	row := login("sub-old")
	old := seal("sub-old")
	if err := st.UnbindIdentity(ctx, row.ID, "sub-old"); err != nil {
		t.Fatal(err)
	}
	login("sub-new")
	cur := seal("sub-new")
	if old.Actor != cur.Actor {
		t.Fatalf("the rebound row's subject changed: %q then %q", old.Actor, cur.Actor)
	}
	eras, err := st.AuditSubjectEras(ctx, row.ID.String())
	if err != nil || len(eras) != 1 || eras[0].Principal != "sub-old" || eras[0].Until.Before(old.Time) || !eras[0].Until.Before(cur.Time) {
		t.Fatalf("binding history = %+v, %v; want sub-old until a time between the two rows", eras, err)
	}
	both := []types.AuditEvent{old, cur}
	reads("after the rebind", both, "sub-old", "sub-new")
	if !sharing("sub-new", "sub-old") {
		t.Error("an erasure of the new holder does not list the released principal as not erased")
	}

	destroyAuditKey(t, pool, "sub-old")
	if n, err := st.EraseBindingHistoryOf(ctx, []string{"sub-old"}); err != nil || n != 1 {
		t.Fatalf("EraseBindingHistoryOf = %d, %v; want 1 row cleared", n, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM principal_identity_unbinds WHERE identity_id = $1 AND principal IS NULL`, row.ID); n != 1 {
		t.Fatalf("%d nameless history rows after the erasure, want the one row kept with no name", n)
	}
	reads("after the released person's erasure", both, audit.ErasedValue, "sub-new")
	if sharing("sub-new", "sub-old") {
		t.Error("an erased principal is still listed from the binding history")
	}

	// A person already erased is unbound with no name recorded.
	destroyAuditKey(t, pool, "sub-new")
	if err := st.UnbindIdentity(ctx, row.ID, "sub-new"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM principal_identity_unbinds WHERE identity_id = $1 AND principal IS NOT NULL`, row.ID); n != 0 {
		t.Fatalf("%d history rows name an erased person, want 0", n)
	}
	reads("after an erased person's unbind", both, audit.ErasedValue, audit.ErasedValue)
}

// An erasure that runs while an unbind of the person's row is in flight (its history row written, not yet
// committed) waits for it and clears the name it wrote.
func TestPG_EraseBindingHistoryWaitsForAnUnbindInFlight(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := t.Context()
	st := store.NewPG(pool)
	row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "sub-held", Issuer: "https://login.example/" + uuid.NewString() + "/v2.0",
		TenantID: uuid.NewString(), ObjectID: uuid.NewString()}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// The unbind's own order: the row locked first, the principal cleared, the history row written last.
	hold, err := testutil.PGConn(t, pool).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.WithoutCancel(ctx)) }()
	for _, q := range []string{
		`SELECT 1 FROM principal_identities WHERE id = $1 FOR UPDATE`,
		`UPDATE principal_identities SET principal = NULL WHERE id = $1`,
		`INSERT INTO principal_identity_unbinds (identity_id, unbound_at, principal) VALUES ($1, clock_timestamp(), 'sub-held')`,
	} {
		if _, err := hold.Exec(ctx, q, row.ID); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var cleared int
	done := make(chan error, 1)
	go func() {
		var err error
		cleared, err = st.EraseBindingHistoryOf(ctx, []string{"sub-held"})
		done <- err
	}()
	waitFor(t, "the erasure to wait on the identity row the unbind holds", func() bool {
		return pendingOn(t, pool, "%principal_identities%FOR KEY SHARE%")
	})
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || cleared != 1 {
		t.Fatalf("EraseBindingHistoryOf = %d, %v; want the in-flight unbind's row cleared", cleared, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM principal_identity_unbinds WHERE principal IS NOT NULL`); n != 0 {
		t.Fatalf("%d history rows still name the erased person", n)
	}
}

// A key destroyed at the epoch erases no row (audit reads compare it with a row's time, and none is before
// it), so an unbind still records the principal it released.
func TestPG_UnbindAfterAKeyDestroyedAtTheEpochKeepsTheName(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := t.Context()
	st := store.NewPG(pool)
	row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "sub-kept", Issuer: "https://login.example/" + uuid.NewString() + "/v2.0",
		TenantID: uuid.NewString(), ObjectID: uuid.NewString()}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	destroyAuditKey(t, pool, "sub-kept", time.Unix(0, 0).UTC())
	if err := st.UnbindIdentity(ctx, row.ID, "sub-kept"); err != nil {
		t.Fatal(err)
	}
	if eras, err := st.AuditSubjectEras(ctx, row.ID.String()); err != nil || len(eras) != 1 || eras[0].Principal != "sub-kept" {
		t.Fatalf("binding history = %+v, %v; want the released principal named", eras, err)
	}
}

// An erasure made under the subject before an unbind that left no record (an older server's) belongs to
// the next recorded binding: that era carries the earlier destruction, never a later holder's, so a row
// sealed before it still reads erased and the era's own rows read as its person.
func TestPG_AuditSubjectErasKeepAnErasureBeforeAnUnrecordedUnbind(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := t.Context()
	st := store.NewPG(pool)
	issuer := "https://login.example/" + uuid.NewString() + "/v2.0"
	tenant, object := uuid.NewString(), uuid.NewString()
	login := func(principal string) store.PrincipalIdentity {
		t.Helper()
		row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: issuer, TenantID: tenant, ObjectID: object}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	s := &audit.Sealer{Keys: historyKeys{}, Subjects: st, SealActor: true}
	seal := func(person string) types.AuditEvent {
		t.Helper()
		ev, pending, err := s.Seal(ctx, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: person, Action: "run.kill", Outcome: "success"})
		if err != nil || pending {
			t.Fatalf("seal for %s: pending %v, %v", person, pending, err)
		}
		return ev
	}

	row := login("sub-first")
	first := seal("sub-first")
	subject := audit.SubjectActorPrefix + row.ID.String()
	gone := time.Now().UTC().Truncate(time.Microsecond)
	destroyAuditKey(t, pool, subject, gone)
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET principal = NULL WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	login("sub-mid")
	mid := seal("sub-mid")
	if err := st.UnbindIdentity(ctx, row.ID, "sub-mid"); err != nil {
		t.Fatal(err)
	}
	login("sub-now")
	destroyAuditKey(t, pool, subject)

	eras, err := st.AuditSubjectEras(ctx, row.ID.String())
	if err != nil || len(eras) != 1 || eras[0].Principal != "sub-mid" || !eras[0].SubjectGone.Equal(gone) {
		t.Fatalf("binding history = %+v, %v; want sub-mid with the destruction made before the unbind", eras, err)
	}
	back, err := s.Unseal(ctx, []types.AuditEvent{first, mid})
	if err != nil {
		t.Fatal(err)
	}
	if back[0].Actor != audit.ErasedValue || back[1].Actor != "sub-mid" {
		t.Errorf("rows read %q, %q; want %q, %q", back[0].Actor, back[1].Actor, audit.ErasedValue, "sub-mid")
	}
}

// The identity row a person was bound to when a row was written, found from the binding history after an
// unbind left them none: the row's first recorded unbind at or after that time must name them, and an
// erasure that cleared their name finds none.
func TestPG_AuditSubjectReleasedIsTheBindingThatHeldThen(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := t.Context()
	st := store.NewPG(pool)
	issuer := "https://login.example/" + uuid.NewString() + "/v2.0"
	tenant, object := uuid.NewString(), uuid.NewString()
	login := func(principal string) store.PrincipalIdentity {
		t.Helper()
		row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: issuer, TenantID: tenant, ObjectID: object}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	released := func(principal string, at time.Time) string {
		t.Helper()
		id, ok, err := st.AuditSubjectReleased(ctx, principal, at)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		return id
	}

	row := login("sub-a")
	first := time.Now().UTC()
	if err := st.UnbindIdentity(ctx, row.ID, "sub-a"); err != nil {
		t.Fatal(err)
	}
	login("sub-b")
	second := time.Now().UTC()
	if err := st.UnbindIdentity(ctx, row.ID, "sub-b"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC()

	if got := released("sub-a", first); got != row.ID.String() {
		t.Errorf("sub-a before its unbind: %q, want the identity row", got)
	}
	if got := released("sub-b", first); got != "" {
		t.Errorf("sub-b before sub-a's unbind: %q, want none (the row was sub-a's then)", got)
	}
	if got := released("sub-b", second); got != row.ID.String() {
		t.Errorf("sub-b between the unbinds: %q, want the identity row", got)
	}
	if got := released("sub-a", later); got != "" {
		t.Errorf("sub-a after every unbind: %q, want none", got)
	}
	destroyAuditKey(t, pool, "sub-a")
	if _, err := st.EraseBindingHistoryOf(ctx, []string{"sub-a"}); err != nil {
		t.Fatal(err)
	}
	if got := released("sub-a", first); got != "" {
		t.Errorf("sub-a after its erasure: %q, want none", got)
	}
}
