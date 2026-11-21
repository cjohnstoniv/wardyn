// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mintFor mints one grant for person on p, with a signed end an hour out and the portal's own TTL.
func mintFor(t *testing.T, st store.PG, p issuerPortal, person string) (types.PortalGrant, string, time.Time) {
	t.Helper()
	issueFor(t, st, p, person)
	raw := "wdg_" + uuid.NewString()
	g, dbnow, err := st.MintPortalGrant(context.Background(), types.PortalGrant{
		DelegateID: p.Delegate.ID, Principal: types.PortalPrincipal(p.Key, person), UserType: "default",
		SubjectIssuedAt: time.Now().UTC(),
	}, raw, time.Now().Add(time.Hour), 10*time.Minute, p.Delegate.AdmissionRev)
	if err != nil {
		t.Fatalf("mint for %s: %v", person, err)
	}
	return g, raw, dbnow
}

func expireGrants(t *testing.T, st store.PG, ids ...uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		if _, err := st.Pool.Exec(context.Background(), `UPDATE portal_grants SET expires_at = clock_timestamp() - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
}

// A grant joins the chain of the person's latest-expiring grant that is still live; once every grant of
// the chain has ended, the next one starts a chain of its own. Another person, and another portal on the
// same issuer, never share a chain.
func TestPortalGrant_MintInheritsLiveChainOnly_PG(t *testing.T) {
	st, _ := isolatedStore(t)
	p := newIssuerPortal(t, st, "")
	g1, _, _ := mintFor(t, st, p, "alice")
	if g1.ChainID != g1.ID {
		t.Fatalf("a first grant's chain = %s, want its own id %s", g1.ChainID, g1.ID)
	}
	g2, _, _ := mintFor(t, st, p, "alice")
	if g2.ChainID != g1.ChainID {
		t.Fatalf("a second live grant's chain = %s, want %s", g2.ChainID, g1.ChainID)
	}
	if other, _, _ := mintFor(t, st, p, "bob"); other.ChainID == g1.ChainID {
		t.Fatal("another person joined the chain")
	}
	second := newIssuerPortalOn(t, st, p)
	if other, _, _ := mintFor(t, st, second, "alice"); other.ChainID == g1.ChainID {
		t.Fatal("another portal on the same issuer joined the chain")
	}

	expireGrants(t, st, g1.ID, g2.ID)
	g3, _, _ := mintFor(t, st, p, "alice")
	if g3.ChainID != g3.ID || g3.ChainID == g1.ChainID {
		t.Fatalf("a grant after the whole chain ended has chain %s, want a new one equal to its id %s", g3.ChainID, g3.ID)
	}
}

func newIssuerPortalOn(t *testing.T, st store.PG, p issuerPortal) issuerPortal {
	t.Helper()
	bound, err := st.BindPortalIssuerQ(context.Background(), st.Pool, p.Issuer, p.Delegate.InternalIssuer, "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	return registerPortalOn(t, st, bound)
}

// The lookup returns the portal's issuer and key for the reverse check, and a revoked portal's grants and
// an expired grant are not found.
func TestPortalGrant_LookupReturnsIssuerPairAndJoinsRevoked_PG(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	g, raw, _ := mintFor(t, st, p, "alice")
	now := time.Now().UTC()

	byRaw, err := st.GetPortalGrantByRaw(ctx, raw, now)
	if err != nil {
		t.Fatal(err)
	}
	byID, err := st.GetPortalGrantByID(ctx, g.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []types.PortalGrant{byRaw, byID} {
		if got.ID != g.ID || got.Issuer != p.Issuer || got.IssuerKey != p.Key || got.Principal != g.Principal {
			t.Fatalf("lookup = %+v, want grant %s of issuer %s key %s", got, g.ID, p.Issuer, p.Key)
		}
	}
	if _, err := st.GetPortalGrantByRaw(ctx, "wdg_"+uuid.NewString(), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown secret = %v, want ErrNotFound", err)
	}
	if _, err := st.GetPortalGrantByRaw(ctx, raw, g.ExpiresAt.Add(time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired grant = %v, want ErrNotFound", err)
	}
	if _, _, err := st.RevokeDelegate(ctx, p.Delegate.ID, now, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPortalGrantByRaw(ctx, raw, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("grant of a revoked portal by raw = %v, want ErrNotFound", err)
	}
	if _, err := st.GetPortalGrantByID(ctx, g.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("grant of a revoked portal by id = %v, want ErrNotFound", err)
	}
}

// PortalChainLive returns the latest end among the chain's live grants, the portal's renewal flag, and the
// zero time for a chain that has ended or a portal that is revoked or unknown.
func TestPortalChainLive_LatestLiveGrantPerPortal_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	g1, _, _ := mintFor(t, st, p, "alice")
	g2, _, _ := mintFor(t, st, p, "alice")
	if !g2.ExpiresAt.After(g1.ExpiresAt) {
		t.Fatalf("second grant ends %v, not after the first %v", g2.ExpiresAt, g1.ExpiresAt)
	}

	end, renewal, err := st.PortalChainLive(ctx, p.Delegate.ID, g1.Principal, g1.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	if !end.Equal(g2.ExpiresAt) || renewal {
		t.Fatalf("chain live = %v, %v; want the later end %v and no renewal", end, renewal, g2.ExpiresAt)
	}
	if _, err := pool.Exec(ctx, `UPDATE delegates SET ui_session_renewal = true WHERE id = $1`, p.Delegate.ID); err != nil {
		t.Fatal(err)
	}
	if _, renewal, _ = st.PortalChainLive(ctx, p.Delegate.ID, g1.Principal, g1.ChainID); !renewal {
		t.Fatal("renewal flag not returned")
	}

	// The same chain id read through another portal on the issuer is not live there.
	other := newIssuerPortalOn(t, st, p)
	if end, _, _ := st.PortalChainLive(ctx, other.Delegate.ID, g1.Principal, g1.ChainID); !end.IsZero() {
		t.Fatalf("another portal reads end %v for this chain, want zero", end)
	}

	expireGrants(t, st, g2.ID)
	if end, _, _ := st.PortalChainLive(ctx, p.Delegate.ID, g1.Principal, g1.ChainID); !end.Equal(g1.ExpiresAt) {
		t.Fatalf("end after the later grant ended = %v, want the first grant's %v", end, g1.ExpiresAt)
	}
	expireGrants(t, st, g1.ID)
	if end, _, _ := st.PortalChainLive(ctx, p.Delegate.ID, g1.Principal, g1.ChainID); !end.IsZero() {
		t.Fatalf("end of an ended chain = %v, want zero", end)
	}

	live, _, _ := mintFor(t, st, p, "bob")
	if _, _, err := st.RevokeDelegate(ctx, p.Delegate.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	if end, renewal, _ := st.PortalChainLive(ctx, p.Delegate.ID, live.Principal, live.ChainID); !end.IsZero() || renewal {
		t.Fatalf("a revoked portal answers %v, %v; want zero, false", end, renewal)
	}
	if end, renewal, err := st.PortalChainLive(ctx, uuid.New(), live.Principal, live.ChainID); err != nil || !end.IsZero() || renewal {
		t.Fatalf("an unknown portal answers %v, %v, %v; want zero, false, nil", end, renewal, err)
	}
}

// A mint that waits for the person's lock while the token's signed end passes is refused with
// ErrSubjectTokenStale, and writes nothing.
func TestPortalGrant_MintAfterCutoffWhileWaitingRefused_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	principal := types.PortalPrincipal(p.Key, "alice")

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || $2, 0))`, p.Delegate.ID, principal); err != nil {
		t.Fatal(err)
	}

	signedEnd := time.Now().Add(7 * time.Second)
	done := make(chan error, 1)
	go func() {
		_, _, err := st.MintPortalGrant(ctx, types.PortalGrant{DelegateID: p.Delegate.ID, Principal: principal, UserType: "default",
			SubjectIssuedAt: time.Now().UTC()}, "wdg_"+uuid.NewString(), signedEnd, 10*time.Minute, p.Delegate.AdmissionRev)
		done <- err
	}()
	untilBlockedBy(t, pool, pidOf(t, holder), 1) // the mint waits on the person's lock
	// The token's cutoff passes while it waits: a mint needs five seconds of life left.
	for time.Now().Before(signedEnd.Add(-4 * time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrSubjectTokenStale) {
		t.Fatalf("mint after the cutoff passed = %v, want ErrSubjectTokenStale", err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM portal_grants`); n != 0 {
		t.Fatalf("a refused mint left %d grants", n)
	}
}

// Concurrent first mints for one person through one portal leave exactly one chain, started by one grant.
func TestPortalGrant_ConcurrentFirstMintsOneChain_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	p := newIssuerPortal(t, st, "")
	const n = 12
	var wg sync.WaitGroup
	chains := make([]uuid.UUID, n)
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, _, err := st.MintPortalGrant(context.Background(), types.PortalGrant{DelegateID: p.Delegate.ID,
				Principal: types.PortalPrincipal(p.Key, "alice"), UserType: "default", SubjectIssuedAt: time.Now().UTC()},
				"wdg_"+uuid.NewString(), time.Now().Add(time.Hour), 10*time.Minute, p.Delegate.AdmissionRev)
			chains[i], ids[i], errs[i] = g.ChainID, g.ID, err
		}()
	}
	wg.Wait()
	starters := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("mint %d: %v", i, errs[i])
		}
		if chains[i] != chains[0] {
			t.Fatalf("mint %d chain %s differs from %s", i, chains[i], chains[0])
		}
		if ids[i] == chains[i] {
			starters++
		}
	}
	if starters != 1 {
		t.Fatalf("%d grants started the chain, want 1", starters)
	}
	if got := rowCount(t, pool, `SELECT count(DISTINCT chain_id) FROM portal_grants`); got != 1 {
		t.Fatalf("%d chains stored, want 1", got)
	}
}

// A grant ends at the mint time plus the portal's TTL, whatever the signed end allows, and at the signed
// end when that comes first.
func TestPortalGrant_EndCappedByMintPlusGrantTTL_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	principal := types.PortalPrincipal(p.Key, "alice")
	mint := func(signedEnd time.Time, ttl time.Duration) (types.PortalGrant, time.Time) {
		g, dbnow, err := st.MintPortalGrant(ctx, types.PortalGrant{DelegateID: p.Delegate.ID, Principal: principal, UserType: "default",
			SubjectIssuedAt: time.Now().UTC()}, "wdg_"+uuid.NewString(), signedEnd, ttl, p.Delegate.AdmissionRev)
		if err != nil {
			t.Fatal(err)
		}
		return g, dbnow
	}

	g, dbnow := mint(time.Now().Add(24*time.Hour), 5*time.Minute)
	if want := dbnow.Add(5 * time.Minute); !g.ExpiresAt.Equal(want) {
		t.Fatalf("end = %v, want mint time + TTL = %v", g.ExpiresAt, want)
	}
	signed := time.Now().Add(2 * time.Minute).Truncate(time.Microsecond)
	g, _ = mint(signed, 10*time.Minute)
	if !g.ExpiresAt.Equal(signed) {
		t.Fatalf("end = %v, want the signed end %v", g.ExpiresAt, signed)
	}
	if _, _, err := st.MintPortalGrant(ctx, types.PortalGrant{DelegateID: p.Delegate.ID, Principal: principal, UserType: "default",
		SubjectIssuedAt: time.Now().UTC()}, "wdg_"+uuid.NewString(), time.Now().Add(3*time.Second), 10*time.Minute, p.Delegate.AdmissionRev); !errors.Is(err, store.ErrSubjectTokenStale) {
		t.Fatalf("a mint with 3 s left = %v, want ErrSubjectTokenStale", err)
	}

	// A second transaction holds the person's lock for two seconds first: the end comes from the database clock
	// read after the wait, whichever of the two terms is first.
	for _, c := range []struct {
		name     string
		signedIn time.Duration // the signed end, from when the holder starts
		ttl      time.Duration
		ttlFirst bool
	}{{"the TTL term first", 24 * time.Hour, 5 * time.Minute, true}, {"the signed end first", 2*time.Minute + 2*time.Second, 10 * time.Minute, false}} {
		t.Run("held lock, "+c.name, func(t *testing.T) {
			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || $2, 0))`, p.Delegate.ID, principal); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			signed := started.Add(c.signedIn).Truncate(time.Microsecond)
			type minted struct {
				g     types.PortalGrant
				dbnow time.Time
				err   error
			}
			done := make(chan minted, 1)
			go func() {
				g, dbnow, err := st.MintPortalGrant(ctx, types.PortalGrant{DelegateID: p.Delegate.ID, Principal: principal, UserType: "default",
					SubjectIssuedAt: time.Now().UTC()}, "wdg_"+uuid.NewString(), signed, c.ttl, p.Delegate.AdmissionRev)
				done <- minted{g, dbnow, err}
			}()
			untilBlockedBy(t, pool, pidOf(t, holder), 1) // the mint is waiting on the lock
			for time.Since(started) < 2*time.Second {
				time.Sleep(10 * time.Millisecond)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-done
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.dbnow.Before(started.Add(2 * time.Second).Add(-100 * time.Millisecond)) {
				t.Errorf("dbnow %v is before the lock was released: the clock was read before the wait", r.dbnow)
			}
			want := signed
			if c.ttlFirst {
				want = r.dbnow.Add(c.ttl)
			}
			if !r.g.ExpiresAt.Equal(want) {
				t.Errorf("end = %v, want %v (dbnow %v)", r.g.ExpiresAt, want, r.dbnow)
			}
		})
	}
}

// Two people's mints through one portal, repeated, finish without a deadlock or an error: the mint takes the
// portal row only FOR SHARE and writes nothing to it.
func TestPortalGrant_ConcurrentPeopleOnePortalNoDeadlock_PG(t *testing.T) {
	st, _ := isolatedStore(t)
	p := newIssuerPortal(t, st, "")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const pairs = 50
	var wg sync.WaitGroup
	errs := make(chan error, 2*pairs)
	for range pairs {
		for _, person := range []string{"alice", "bob"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := st.MintPortalGrant(ctx, types.PortalGrant{DelegateID: p.Delegate.ID,
					Principal: types.PortalPrincipal(p.Key, person), UserType: "default", SubjectIssuedAt: time.Now().UTC()},
					"wdg_"+uuid.NewString(), time.Now().Add(time.Hour), 10*time.Minute, p.Delegate.AdmissionRev)
				if err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a mint failed: %v", err)
	}
}
