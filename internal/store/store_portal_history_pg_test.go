// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func portalsOf(t *testing.T, st store.PG, p issuerPortal, person string) map[uuid.UUID]types.PortalUsed {
	t.Helper()
	got, err := st.LookupPortalPerson(context.Background(), p.Issuer, person)
	if err != nil {
		t.Fatalf("lookup %s: %v", person, err)
	}
	out := map[uuid.UUID]types.PortalUsed{}
	for _, u := range got.PortalsUsed {
		out[u.Delegate] = u
	}
	return out
}

// A person's portal history is the portals that minted them a grant, with each portal's pin, and it survives
// the expiry sweep and a suspension, which delete the grants. A portal that introduced them and then refused
// every mint is not in it. A purge removes it.
func TestLookupPortalPerson_KeepsPortalsAfterSweepAndSuspend_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	a := newIssuerPortal(t, st, "")
	b := newIssuerPortalOn(t, st, a)
	pin := uuid.New()
	if _, err := pool.Exec(ctx, `UPDATE delegates SET run_in_under = $2 WHERE id = $1`, b.Delegate.ID, pin); err != nil {
		t.Fatal(err)
	}

	ga, _, _ := mintFor(t, st, a, "alice")
	mintFor(t, st, b, "alice")
	mintFor(t, st, b, "alice") // a second use of one portal is one entry
	issueFor(t, st, a, "carol")
	if _, _, err := st.MintPortalGrant(ctx, types.PortalGrant{DelegateID: a.Delegate.ID, Principal: types.PortalPrincipal(a.Key, "carol"),
		UserType: "default", SubjectIssuedAt: time.Now().UTC()}, "wdg_"+uuid.NewString(), time.Now().Add(2*time.Second), 10*time.Minute,
		a.Delegate.AdmissionRev); !errors.Is(err, store.ErrSubjectTokenStale) {
		t.Fatalf("a mint with 2 s left = %v, want ErrSubjectTokenStale", err)
	}

	check := func(when string) {
		t.Helper()
		got := portalsOf(t, st, a, "alice")
		if len(got) != 2 || got[a.Delegate.ID].Name != a.Delegate.Name || got[b.Delegate.ID].RunInUnder == nil || *got[b.Delegate.ID].RunInUnder != pin {
			t.Fatalf("%s: portals = %+v, want both with b's pin", when, got)
		}
	}
	check("after the mints")
	if got := portalsOf(t, st, a, "carol"); len(got) != 0 {
		t.Fatalf("a person whose only mint was refused lists %+v, want none", got)
	}

	expireGrants(t, st, ga.ID)
	if n, err := st.SweepExpiredPortalGrants(ctx, time.Now().Add(time.Hour)); err != nil || n == 0 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM portal_grants`); n != 0 {
		t.Fatalf("%d grants after the sweep", n)
	}
	check("after the sweep")

	mintFor(t, st, a, "alice")
	id, _ := issueFor(t, st, a, "alice")
	principal := types.PortalPrincipal(a.Key, "alice")
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: id, Principal: principal, Principals: []string{principal}, PortalLifecycle: true}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM portal_grants WHERE principal = $1`, principal); n != 0 {
		t.Fatalf("%d grants left after the suspension", n)
	}
	check("after the suspension")

	if _, err := st.MarkPortalIdentityPurged(ctx, id, store.PortalPurgeExplicit); err != nil {
		t.Fatal(err)
	}
	if got := portalsOf(t, st, a, "alice"); len(got) != 0 {
		t.Fatalf("after the purge portals = %+v, want none", got)
	}
}

// A credentials-scope erasure deletes the person's portal grants, live and expired, with their delegated
// tokens and their portal history, counts both, and touches nobody else's.
func TestErasure_CredentialsScopeDeletesPortalGrants_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	mintFor(t, st, p, "alice")
	mintFor(t, st, p, "alice")
	old, _, _ := mintFor(t, st, p, "alice")
	expireGrants(t, st, old.ID)
	bob, _, _ := mintFor(t, st, p, "bob")

	console, err := st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "console", IdPClientID: "c", Group: "g"}, "wdp_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	alice := types.PortalPrincipal(p.Key, "alice")
	if _, err := st.MintDelegatedToken(ctx, types.DelegatedToken{ID: uuid.New(), DelegateID: console.ID, Principal: alice, UserType: "default",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}, "wdg_"+uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeleteDelegatedTokensOf(ctx, []string{alice})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 {
		t.Fatalf("deleted = %d, want 3 grants (2 live, 1 expired) + 1 delegated token", deleted)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM portal_grants WHERE principal = $1`, alice); n != 0 {
		t.Fatalf("%d of the person's grants remain", n)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM delegated_tokens WHERE principal = $1`, alice); n != 0 {
		t.Fatalf("%d of the person's delegated tokens remain", n)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM portal_grants WHERE id = $1`, bob.ID); n != 1 {
		t.Fatal("another person's grant was deleted")
	}
	if got := portalsOf(t, st, p, "alice"); len(got) != 0 {
		t.Fatalf("the erased person's portal history = %+v, want none", got)
	}
	if got := portalsOf(t, st, p, "bob"); len(got) != 1 {
		t.Fatalf("another person's portal history = %+v, want one portal", got)
	}
	if again, err := st.DeleteDelegatedTokensOf(ctx, []string{alice}); err != nil || again != 0 {
		t.Fatalf("a second erase = %d, %v; want 0, nil", again, err)
	}
}

// An issuer portal's maximum token age and grant lifetime are checked on their effective values: a missing
// age is the 300 s default, a missing lifetime is the age, so no row slips a longer lifetime past the check
// through a NULL; a console portal is unaffected.
func TestPortalDelegateTTLBounds_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	bound, err := st.BindPortalIssuerQ(ctx, pool, "https://idp.example.com", "https://a.internal", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	create := func(maxAge, ttl int) error {
		_, err := st.CreateDelegateQ(ctx, pool, types.Delegate{ID: uuid.New(), Name: "p-" + uuid.NewString(), IdPClientID: "c",
			Issuer: bound.Issuer, InternalIssuer: bound.InternalIssuerNorm, IssuerKey: bound.IssuerKey, TokenProfile: "access_token",
			UserType: "default", PortalManagedAdmission: true, MaxSubjectTokenAgeSeconds: maxAge, GrantTTLSeconds: ttl}, sha256Hex(uuid.NewString()))
		return err
	}
	for _, c := range []struct {
		maxAge, ttl int
		ok          bool
	}{
		{0, 0, true}, {0, 300, true}, {0, 600, false}, {0, 301, false}, {0, 299, false},
		{300, 0, true}, {600, 0, true}, {600, 300, true}, {600, 600, true}, {450, 450, true},
		{600, 601, false}, {450, 451, false}, {299, 0, false}, {601, 0, false}, {601, 300, false}, {600, 299, false},
	} {
		err := create(c.maxAge, c.ttl)
		var pgErr *pgconn.PgError
		switch {
		case c.ok && err != nil:
			t.Errorf("max age %d, lifetime %d refused: %v", c.maxAge, c.ttl, err)
		case !c.ok && !(errors.As(err, &pgErr) && pgErr.Code == "23514"):
			t.Errorf("max age %d, lifetime %d = %v, want a check violation", c.maxAge, c.ttl, err)
		}
	}
	if _, err := st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "console", IdPClientID: "c", Group: "g"}, "wdp_"+uuid.NewString()); err != nil {
		t.Errorf("a console portal with no ages = %v", err)
	}
}
