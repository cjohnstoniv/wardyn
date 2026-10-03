// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the people directory (GET /people's store half). Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func listingByPrincipal(t *testing.T, people []store.PersonListing, principal string) store.PersonListing {
	t.Helper()
	for _, p := range people {
		if p.Principal == principal {
			return p
		}
	}
	t.Fatalf("directory has no %q in %v", principal, principals(people))
	return store.PersonListing{}
}

func principals(people []store.PersonListing) []string {
	out := make([]string, len(people))
	for i, p := range people {
		out[i] = p.Principal
	}
	return out
}

// A pre-created person who never signed in, a sub-keyed Entra user with no people row and a
// deactivated person are each listed with the right fields.
func TestPG_PeopleDirectory_RowsAndFields(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	if _, _, err := st.CreatePerson(ctx, types.Person{Principal: "pre-1", Email: "pre@corp.example", CreatedBy: "admin"}); err != nil {
		t.Fatalf("create person: %v", err)
	}
	oid := uuid.NewString()
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: "pairwise-" + oid, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "Pat@Corp.Example",
	}, now.Add(-time.Hour)); err != nil {
		t.Fatalf("entra sign-in: %v", err)
	}
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "dex-1", Issuer: "https://dex.example", Email: "gone@corp.example"}, now); err != nil {
		t.Fatalf("oidc sign-in: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = $1 WHERE principal = 'dex-1'`, now); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	// An identity row written before its first sign-in binds has no principal and is not a person yet.
	if _, err := pool.Exec(ctx, `INSERT INTO principal_identities (issuer, email_lower) VALUES ('https://dex.example', 'unbound@corp.example')`); err != nil {
		t.Fatalf("unbound identity: %v", err)
	}

	page, err := st.ListPeopleDirectory(ctx, store.PeopleDirectoryFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.People) != 3 || page.Next != "" {
		t.Fatalf("directory = %v next %q, want the three people on one page", principals(page.People), page.Next)
	}

	pre := listingByPrincipal(t, page.People, "pre-1")
	if !pre.PreCreated || pre.Email != "pre@corp.example" || pre.Entra || pre.FirstSignInAt != nil || pre.LastSignInAt != nil || pre.DeactivatedAt != nil || pre.ActiveSessions != 0 {
		t.Errorf("pre-created person = %+v", pre)
	}
	entra := listingByPrincipal(t, page.People, "pairwise-"+oid)
	if entra.PreCreated || !entra.Entra || entra.Email != "pat@corp.example" || entra.DeactivatedAt != nil ||
		entra.FirstSignInAt == nil || entra.LastSignInAt == nil || !entra.LastSignInAt.Equal(now.Add(-time.Hour)) || entra.ActiveSessions != 1 {
		t.Errorf("sub-keyed entra person = %+v", entra)
	}
	gone := listingByPrincipal(t, page.People, "dex-1")
	if gone.PreCreated || gone.Entra || gone.DeactivatedAt == nil || !gone.DeactivatedAt.Equal(now) || gone.ActiveSessions != 0 {
		t.Errorf("deactivated person = %+v", gone)
	}
}

// An Entra person set up by object id, then signed in, is one row: the people row's email wins and
// the sign-in fills the rest.
func TestPG_PeopleDirectory_PreCreatedThenSignedInIsOneRow(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	oid := uuid.NewString()
	principal := "entra:" + idTenant + ":" + oid

	if _, _, err := st.CreatePerson(ctx, types.Person{Principal: principal, Email: "kit@corp.example", Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, CreatedBy: "admin"}); err != nil {
		t.Fatalf("create person: %v", err)
	}
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "kit@corp.example"}, now); err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	if err := st.MarkPersonSignedIn(ctx, principal, now); err != nil {
		t.Fatalf("mark: %v", err)
	}
	page, err := st.ListPeopleDirectory(ctx, store.PeopleDirectoryFilter{})
	if err != nil || len(page.People) != 1 {
		t.Fatalf("directory = %v, %v; want the one person", principals(page.People), err)
	}
	if p := page.People[0]; !p.PreCreated || !p.Entra || p.FirstSignInAt == nil || p.LastSignInAt == nil {
		t.Errorf("person = %+v", p)
	}
}

// Each count reads its own fixtures, and ignores what a person does not hold or no longer holds.
func TestPG_PeopleDirectory_Counts(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, sub := range []string{"alice", "bob"} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: sub, Issuer: "https://dex.example", Email: sub + "@corp.example"}, now); err != nil {
			t.Fatalf("sign-in %s: %v", sub, err)
		}
	}
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	for i, tok := range []types.APIToken{
		{Principal: "alice", Role: "user", UserType: "standard", Name: "live"},
		{Principal: "alice", Role: "user", UserType: "standard", Name: "future", ExpiresAt: &future},
		{Principal: "alice", Role: "user", UserType: "standard", Name: "revoked", RevokedAt: &past},
		{Principal: "alice", Role: "user", UserType: "standard", Name: "expired", ExpiresAt: &past},
		{Principal: "bob", Role: "user", UserType: "standard", Name: "bobs"},
	} {
		tok.ID, tok.CreatedAt = uuid.New(), now
		created, err := st.CreateAPIToken(ctx, tok, fmt.Sprintf("wdn_directory_%d_%s", i, uuid.NewString()))
		if err != nil {
			t.Fatalf("create token %s: %v", tok.Name, err)
		}
		if tok.RevokedAt != nil {
			if _, err := st.RevokeAPIToken(ctx, created.ID, "alice", past); err != nil {
				t.Fatalf("revoke token: %v", err)
			}
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := st.AddSSHKey(ctx, types.SSHPublicKey{
			Fingerprint: "SHA256:dir-" + uuid.NewString(), Principal: "alice", Name: "k", PublicKey: "ssh-ed25519 AAAA", Role: "member", CreatedAt: now,
		}); err != nil {
			t.Fatalf("add ssh key: %v", err)
		}
	}
	for _, s := range []struct{ owner, name string }{{"alice", "a"}, {"alice", "b"}, {"bob", "a"}, {"", "operator"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO secrets (owned_by, name, ciphertext) VALUES ($1, $2, $3)`, s.owner, s.name, []byte("x")); err != nil {
			t.Fatalf("insert secret: %v", err)
		}
	}
	for _, c := range []struct {
		owner string
		state types.RunState
	}{
		{"alice", types.RunRunning}, {"alice", types.RunPending}, {"alice", types.RunCompleted}, {"alice", types.RunKilled}, {"bob", types.RunWaiting},
	} {
		r := newRun(c.state)
		r.CreatedBy = c.owner
		persistRun(t, ctx, pool, r)
	}

	page, err := st.ListPeopleDirectory(ctx, store.PeopleDirectoryFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []struct {
		principal                       string
		tokens, keys, credentials, runs int
	}{{"alice", 2, 3, 2, 2}, {"bob", 1, 0, 1, 1}} {
		got := listingByPrincipal(t, page.People, want.principal)
		if got.APITokens != want.tokens || got.SSHKeys != want.keys || got.Credentials != want.credentials || got.ActiveRuns != want.runs {
			t.Errorf("%s counts = tokens %d keys %d credentials %d runs %d, want %d %d %d %d", want.principal,
				got.APITokens, got.SSHKeys, got.Credentials, got.ActiveRuns, want.tokens, want.keys, want.credentials, want.runs)
		}
	}
}

// A live session is one whose last sign-in is recent and past no revoke cutoff: a revoke of the
// sub, of the email in another case, or of everyone each end it, and a later sign-in revives it.
func TestPG_PeopleDirectory_ActiveSessions(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	signIn := func(sub string, at time.Time) {
		t.Helper()
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: sub, Issuer: "https://dex.example", Email: sub + "@Corp.Example"}, at); err != nil {
			t.Fatalf("sign-in %s: %v", sub, err)
		}
	}
	signIn("fresh", now.Add(-time.Minute))
	signIn("stale", now.Add(-48*time.Hour))
	signIn("by-sub", now.Add(-time.Hour))
	signIn("by-email", now.Add(-time.Hour))
	signIn("again", now.Add(-time.Hour))
	for _, sub := range []string{"by-sub", "BY-EMAIL@corp.example", "again"} {
		if _, err := pool.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, $2)`, sub, now.Add(-30*time.Minute)); err != nil {
			t.Fatalf("revoke %s: %v", sub, err)
		}
	}
	signIn("again", now.Add(-10*time.Minute))

	page, err := st.ListPeopleDirectory(ctx, store.PeopleDirectoryFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for sub, want := range map[string]int{"fresh": 1, "stale": 0, "by-sub": 0, "by-email": 0, "again": 1} {
		if got := listingByPrincipal(t, page.People, sub).ActiveSessions; got != want {
			t.Errorf("%s active sessions = %d, want %d", sub, got, want)
		}
	}

	if _, err := pool.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ('', $1)`, now); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	page, err = st.ListPeopleDirectory(ctx, store.PeopleDirectoryFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range page.People {
		if p.ActiveSessions != 0 {
			t.Errorf("%s still has a session after a revoke of everyone", p.Principal)
		}
	}
}

// Paging returns every row exactly once across pages, in principal order, and q and state filter.
func TestPG_PeopleDirectory_PagingAndFilters(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	var want []string
	for i := 0; i < 7; i++ {
		sub := fmt.Sprintf("sub-%02d", i)
		want = append(want, sub)
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: sub, Issuer: "https://dex.example", Email: fmt.Sprintf("user%d@corp.example", i)}, now); err != nil {
			t.Fatalf("sign-in: %v", err)
		}
	}
	// Both sources, and a principal whose case sorts differently under a locale collation.
	for _, p := range []string{"Zed-pre", "alpha-pre"} {
		want = append(want, p)
		if _, _, err := st.CreatePerson(ctx, types.Person{Principal: p, Email: p + "@corp.example", CreatedBy: "admin"}); err != nil {
			t.Fatalf("create person: %v", err)
		}
	}
	slices.Sort(want)

	var got []string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging never ended")
		}
		page, err := st.ListPeopleDirectory(ctx, store.PeopleDirectoryFilter{Limit: 4, After: after})
		if err != nil {
			t.Fatalf("page after %q: %v", after, err)
		}
		got = append(got, principals(page.People)...)
		if page.Next == "" {
			break
		}
		if len(page.People) != 4 {
			t.Fatalf("a non-final page holds %d rows, want 4", len(page.People))
		}
		after = page.Next
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows across pages = %v, want each exactly once, in order: %v", got, want)
	}

	filtered := func(f store.PeopleDirectoryFilter) []string {
		t.Helper()
		page, err := st.ListPeopleDirectory(ctx, f)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		return principals(page.People)
	}
	if got := filtered(store.PeopleDirectoryFilter{Query: "sub-0"}); len(got) != 7 {
		t.Errorf("q=sub-0 = %v, want the 7 subs", got)
	}
	if got := filtered(store.PeopleDirectoryFilter{Query: "SUB-0"}); len(got) != 0 {
		t.Errorf("q=SUB-0 = %v, want none: a principal prefix is case-sensitive", got)
	}
	if got := filtered(store.PeopleDirectoryFilter{Query: "USER3@"}); !slices.Equal(got, []string{"sub-03"}) {
		t.Errorf("q=USER3@ = %v, want sub-03: an email prefix is not", got)
	}
	if got := filtered(store.PeopleDirectoryFilter{Query: "50%"}); len(got) != 0 {
		t.Errorf("q=50%% = %v, want none: the prefix is literal, not a pattern", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = now() WHERE principal IN ('sub-01', 'sub-05')`); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if got := filtered(store.PeopleDirectoryFilter{State: "deactivated"}); !slices.Equal(got, []string{"sub-01", "sub-05"}) {
		t.Errorf("state=deactivated = %v", got)
	}
	if got := filtered(store.PeopleDirectoryFilter{State: "active"}); len(got) != len(want)-2 || slices.Contains(got, "sub-01") {
		t.Errorf("state=active = %v, want everyone but the two", got)
	}
	if got := filtered(store.PeopleDirectoryFilter{State: "deactivated", Query: "sub-05"}); !slices.Equal(got, []string{"sub-05"}) {
		t.Errorf("state=deactivated q=sub-05 = %v", got)
	}
}

type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A page of 50 people issues the same fixed number of queries as a page of 3.
func TestPG_PeopleDirectory_QueryCountIsFixedPerPage(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seed := store.NewPG(pool)
	for i := 0; i < 60; i++ {
		sub := fmt.Sprintf("p-%03d", i)
		if _, err := seed.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: sub, Issuer: "https://dex.example", Email: sub + "@corp.example"}, now); err != nil {
			t.Fatalf("sign-in: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO secrets (owned_by, name, ciphertext) VALUES ($1, 'k', $2)`, sub, []byte("x")); err != nil {
			t.Fatalf("insert secret: %v", err)
		}
	}

	counter := &queryCounter{}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Tracer = counter
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	st := store.NewPG(traced)

	queries := func(limit int) (int64, int) {
		t.Helper()
		before := counter.n.Load()
		page, err := st.ListPeopleDirectory(ctx, store.PeopleDirectoryFilter{Limit: limit})
		if err != nil {
			t.Fatalf("list %d: %v", limit, err)
		}
		return counter.n.Load() - before, len(page.People)
	}
	small, smallRows := queries(3)
	large, largeRows := queries(50)
	if smallRows != 3 || largeRows != 50 {
		t.Fatalf("pages held %d and %d rows, want 3 and 50", smallRows, largeRows)
	}
	if small != large {
		t.Errorf("a page of 3 issued %d queries and a page of 50 issued %d, want one fixed number", small, large)
	}
	if large > 8 {
		t.Errorf("a page issued %d queries, want the directory read plus one per kind of count", large)
	}
}
