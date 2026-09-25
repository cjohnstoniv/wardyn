// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the four scoped *Page store methods #657 adds:
// ListGrantsByRunPage, ListSSHKeysByPrincipalPage, ListAPITokensByPrincipalPage
// and ListCapabilityGrantsForPage. Guarded by WARDYN_TEST_PG. Run with:
//
//	WARDYN_TEST_PG=postgres://... go test ./internal/store/...
//
// Each test mints a UNIQUE scope (a fresh run, principal, or subject set) so it
// is isolated within the shared DB, mirroring store_pagination_pg_test.go's
// audit-page pattern.
package store_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ListGrantsByRunPage_LimitOffset proves the per-run grants page walks
// forward with LIMIT/OFFSET without skipping or repeating a row — the contract
// GET /runs/{id}/grants' ?limit=&offset= and X-Wardyn-Truncated depend on.
//
// Every grant carries the SAME created_at, as in production: POST /runs mints
// all of a run's eligible grants with one `now` (runs_create.go). The walk
// fetches Limit+1 and keeps Limit per page, exactly as servePage does, and
// the concatenated pages must equal ListGrantsByRun — the fallback path's
// order — with every grant exactly once.
func TestPG_ListGrantsByRunPage_LimitOffset(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))

	now := time.Now().UTC()
	const n = 40
	for i := 0; i < n; i++ {
		g := types.CredentialGrant{
			ID: uuid.New(), RunID: run.ID, CreatedAt: now,
			Spec: types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["o/r"]}`)},
		}
		if _, err := pg.CreateGrant(ctx, g); err != nil {
			t.Fatalf("create grant %d: %v", i, err)
		}
	}

	all, err := pg.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("unpaged: %v", err)
	}
	if len(all) != n {
		t.Fatalf("unpaged len = %d, want %d", len(all), n)
	}

	const limit = 7
	var walked []types.CredentialGrant
	for off := 0; ; off += limit {
		rows, err := pg.ListGrantsByRunPage(ctx, run.ID, store.Page{Limit: limit + 1, Offset: off})
		if err != nil {
			t.Fatalf("page at offset %d: %v", off, err)
		}
		walked = append(walked, rows[:min(limit, len(rows))]...)
		if len(rows) <= limit {
			break
		}
	}

	seen := map[uuid.UUID]int{}
	for _, g := range walked {
		seen[g.ID]++
	}
	for _, g := range all {
		if seen[g.ID] != 1 {
			t.Errorf("grant %s seen %d times across the pages, want exactly 1", g.ID, seen[g.ID])
		}
	}
	if len(walked) != n {
		t.Fatalf("walked %d rows, want %d", len(walked), n)
	}
	for i := range all {
		if walked[i].ID != all[i].ID {
			t.Fatalf("walked[%d] = %s, want %s (paged order differs from ListGrantsByRun)", i, walked[i].ID, all[i].ID)
		}
	}
}

// TestPG_ListSSHKeysByPrincipalPage_LimitOffset is the same bound/offset proof
// for GET /me/ssh-keys.
func TestPG_ListSSHKeysByPrincipalPage_LimitOffset(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	principal := "ssh-page-" + uuid.NewString()

	const n = 4
	for i := 0; i < n; i++ {
		k := types.SSHPublicKey{
			Fingerprint: "SHA256:" + uuid.NewString(),
			Principal:   principal,
			Name:        "key",
			PublicKey:   "ssh-ed25519 AAAAtest " + uuid.NewString(),
			Role:        "user",
		}
		if _, err := pg.AddSSHKey(ctx, k); err != nil {
			t.Fatalf("add ssh key %d: %v", i, err)
		}
		t.Cleanup(func(fp string) func() {
			return func() { _, _ = pool.Exec(context.Background(), `DELETE FROM ssh_public_keys WHERE fingerprint=$1`, fp) }
		}(k.Fingerprint))
	}

	all, err := pg.ListSSHKeysByPrincipalPage(ctx, principal, store.Page{})
	if err != nil {
		t.Fatalf("unbounded: %v", err)
	}
	if len(all) != n {
		t.Fatalf("unbounded len = %d, want %d", len(all), n)
	}

	page1, err := pg.ListSSHKeysByPrincipalPage(ctx, principal, store.Page{Limit: 2})
	if err != nil {
		t.Fatalf("limit page: %v", err)
	}
	if len(page1) != 2 || page1[0].Fingerprint != all[0].Fingerprint {
		t.Fatalf("limit=2 = %+v, want first 2 of %+v", page1, all[:2])
	}

	page2, err := pg.ListSSHKeysByPrincipalPage(ctx, principal, store.Page{Limit: 10, Offset: 2})
	if err != nil {
		t.Fatalf("offset page: %v", err)
	}
	if len(page2) != n-2 || page2[0].Fingerprint != all[2].Fingerprint {
		t.Fatalf("limit=10 offset=2 = %+v, want tail of %+v", page2, all)
	}
}

// TestPG_ListAPITokensByPrincipalPage_LimitOffset is the same bound/offset
// proof for GET /me/tokens.
func TestPG_ListAPITokensByPrincipalPage_LimitOffset(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	principal := "token-page-" + uuid.NewString()

	const n = 4
	var ids []uuid.UUID
	for i := 0; i < n; i++ {
		tok := types.APIToken{
			ID: uuid.New(), Principal: principal, Role: "user", UserType: types.UserTypeStandard, Name: "ci",
		}
		created, err := pg.CreateAPIToken(ctx, tok, "wdn_"+uuid.NewString())
		if err != nil {
			t.Fatalf("create api token %d: %v", i, err)
		}
		ids = append(ids, created.ID)
		t.Cleanup(func(id uuid.UUID) func() {
			return func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_tokens WHERE id=$1`, id) }
		}(created.ID))
	}

	all, err := pg.ListAPITokensByPrincipalPage(ctx, principal, store.Page{})
	if err != nil {
		t.Fatalf("unbounded: %v", err)
	}
	if len(all) != n {
		t.Fatalf("unbounded len = %d, want %d", len(all), n)
	}

	page1, err := pg.ListAPITokensByPrincipalPage(ctx, principal, store.Page{Limit: 2})
	if err != nil {
		t.Fatalf("limit page: %v", err)
	}
	if len(page1) != 2 || page1[0].ID != all[0].ID {
		t.Fatalf("limit=2 = %+v, want first 2 of %+v", page1, all[:2])
	}
	_ = ids
}

// TestPG_ListCapabilityGrantsForPage_LimitOffset is the same bound/offset proof
// for GET /me/capabilities. Every grant is written against a DISTINCT user
// subject and the page is fetched for exactly those subjects, so the window
// under test is scoped to rows this test owns despite the shared table.
func TestPG_ListCapabilityGrantsForPage_LimitOffset(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	prefix := "cap-page-" + uuid.NewString() + "-"

	const n = 4
	var users []string
	for i := 0; i < n; i++ {
		user := prefix + uuid.NewString()
		users = append(users, user)
		g := types.CapabilityGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: user,
			Capability: "egress_host", Value: "example.com",
			Effect: types.CapabilityAllow, CreatedBy: "test",
		}
		if _, err := pg.UpsertCapabilityGrant(ctx, g); err != nil {
			t.Fatalf("upsert capability grant %d: %v", i, err)
		}
		t.Cleanup(func(subj string) func() {
			return func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM capability_grants WHERE subject_type='user' AND subject=$1`, subj)
			}
		}(user))
	}

	// ListCapabilityGrantsFor also matches every subject_type='all' row (by
	// design — see its own doc comment), so the shared test DB's unbounded total
	// is not necessarily exactly n. Prove LIMIT/OFFSET against the query's OWN
	// unbounded baseline instead of an exact count — still a real end-to-end
	// proof of the SQL window, just not dependent on this test owning the whole
	// table the way the run- and principal-scoped pages above can.
	all, err := pg.ListCapabilityGrantsForPage(ctx, users, nil, "", store.Page{})
	if err != nil {
		t.Fatalf("unbounded: %v", err)
	}
	if len(all) < n {
		t.Fatalf("unbounded len = %d, want >= %d (this test's own %d subjects)", len(all), n, n)
	}

	limit := len(all) - 1
	page1, err := pg.ListCapabilityGrantsForPage(ctx, users, nil, "", store.Page{Limit: limit})
	if err != nil {
		t.Fatalf("limit page: %v", err)
	}
	if len(page1) != limit {
		t.Fatalf("limit=%d len = %d, want %d", limit, len(page1), limit)
	}
	for i := range page1 {
		if page1[i].ID != all[i].ID {
			t.Fatalf("page1[%d].ID = %s, want %s (order mismatch)", i, page1[i].ID, all[i].ID)
		}
	}

	page2, err := pg.ListCapabilityGrantsForPage(ctx, users, nil, "", store.Page{Limit: len(all), Offset: 1})
	if err != nil {
		t.Fatalf("offset page: %v", err)
	}
	if len(page2) != len(all)-1 {
		t.Fatalf("offset=1 len = %d, want %d", len(page2), len(all)-1)
	}
	for i := range page2 {
		if page2[i].ID != all[i+1].ID {
			t.Fatalf("page2[%d].ID = %s, want %s (offset mismatch)", i, page2[i].ID, all[i+1].ID)
		}
	}
}

// TestPG_ListCapabilityGrantsForPage_MatchesUnpaged: GET /me/capabilities
// serves the paged read whenever the store has one, so it must return every
// row ListCapabilityGrantsFor does for the same caller, a `user_type` grant
// (#612) included. Train 17 merged #657's pager, written before user types,
// beside #893's fourth subject arm, and the page silently dropped it.
func TestPG_ListCapabilityGrantsForPage_MatchesUnpaged(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	user, group, userType := "cap-page-u-"+uuid.NewString(), "cap-page-g-"+uuid.NewString(), "cap-page-t-"+uuid.NewString()[:8]
	for _, g := range []types.CapabilityGrant{
		{SubjectType: types.CapabilitySubjectUser, Subject: user},
		{SubjectType: types.CapabilitySubjectGroup, Subject: group},
		{SubjectType: types.CapabilitySubjectUserType, Subject: userType},
	} {
		g.Capability, g.Value, g.Effect, g.CreatedBy = "egress_host", "example.com", types.CapabilityAllow, "test"
		if _, err := pg.UpsertCapabilityGrant(ctx, g); err != nil {
			t.Fatalf("upsert %s grant: %v", g.SubjectType, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM capability_grants WHERE subject_type=$1 AND subject=$2`, string(g.SubjectType), g.Subject)
		})
	}

	users, groups := []string{user}, []string{group}
	want, err := pg.ListCapabilityGrantsFor(ctx, users, groups, userType)
	if err != nil {
		t.Fatalf("unpaged: %v", err)
	}
	got, err := pg.ListCapabilityGrantsForPage(ctx, users, groups, userType, store.Page{})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	var ids []uuid.UUID
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	for _, w := range want {
		if !slices.Contains(ids, w.ID) {
			t.Errorf("the page drops the %s grant %q that the unpaged read returns", w.SubjectType, w.Subject)
		}
	}
	if len(got) != len(want) {
		t.Errorf("paged %d rows, unpaged %d", len(got), len(want))
	}
}
