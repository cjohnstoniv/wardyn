// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for what a SCIM group change does to the record of each person's last sign-ins.
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A SCIM removal takes the group out of every record of the principal the identity row binds, its
// sign-in's and each portal's, matched as the token step matches it (case and spaces aside; a blank id
// takes every group), and leaves the rest of the record, a person with no record, an identity no sign-in
// bound (whatever its email) and anyone else's record as they were. A SCIM add never puts a group in a
// record.
func TestPG_PrincipalGroups_ScimRemovalNarrowsTheRecord(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now()
	identity := func(principal any, email string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ($1, 'https://idp.example', $2) RETURNING id`,
			principal, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	const portal = "0b7f6f5e-2a1c-4d3e-9f80-1a2b3c4d5e6f"
	record := func(principal, door string) []string {
		t.Helper()
		got, err := st.GetPrincipalGroups(ctx, principal, door)
		if err != nil {
			t.Fatalf("%s: %v", principal, err)
		}
		return got.Groups
	}
	ann, ben, dee, cy, eve := identity("sub-ann", "ann@corp.example"), identity("sub-ben", "ben@corp.example"),
		identity("sub-dee", "dee@corp.example"), identity(nil, "cy@corp.example"), identity("sub-eve", "eve@corp.example")
	for p, groups := range map[string][]string{"sub-ann": {"eng", "ops"}, "sub-ben": {"eng"}, "sub-cy": {"eng"}, "sub-eve": {"eng", "ops"}} {
		if err := st.PutPrincipalGroups(ctx, p, "", strings.TrimPrefix(p, "sub-")+"@corp.example", groups, p == "sub-ann", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutPrincipalGroups(ctx, "sub-ann", portal, "ann@corp.example", []string{"eng", "sales"}, false, now); err != nil {
		t.Fatal(err)
	}
	eng, err := st.CreateScimGroup(ctx, " ENG ", "Engineering", now)
	if err != nil {
		t.Fatal(err)
	}
	admins, err := st.CreateScimGroup(ctx, "admins", "Admins", now)
	if err != nil {
		t.Fatal(err)
	}
	blank, err := st.CreateScimGroup(ctx, "  ", "No id", now)
	if err != nil {
		t.Fatal(err)
	}
	// CreateScimGroup trims the id; a row written another way keeps its spaces, which the match trims.
	if _, err := pool.Exec(ctx, `UPDATE scim_groups SET external_id = ' ENG ' WHERE id = $1`, eng.ID); err != nil {
		t.Fatal(err)
	}
	for _, g := range []uuid.UUID{eng.ID, admins.ID, blank.ID} {
		if err := st.AddScimGroupMembers(ctx, g, []uuid.UUID{ann, ben, dee, cy, eve}, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := record("sub-ann", ""); !slices.Equal(got, []string{"eng", "ops"}) {
		t.Fatalf("after SCIM adds: %v, want the sign-in's groups alone", got)
	}
	if _, err := st.GetPrincipalGroups(ctx, "sub-dee", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a SCIM add made a record: %v", err)
	}
	for range 2 {
		for _, id := range []uuid.UUID{ann, dee, cy} {
			if err := st.StartGroupRemoval(ctx, eng.ID, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := st.GetPrincipalGroups(ctx, "sub-ann", "")
	if err != nil || !slices.Equal(got.Groups, []string{"ops"}) || !got.Truncated {
		t.Fatalf("removed from eng: %+v, %v; want ops alone, still partial", got, err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-ann", portal); err != nil || !slices.Equal(got.Groups, []string{"sales"}) || got.Truncated {
		t.Errorf("removed from eng, the portal's record: %+v, %v; want sales alone, still complete", got, err)
	}
	if _, err := st.GetPrincipalGroups(ctx, "sub-dee", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a person with no record: %v, want still none", err)
	}
	if got := record("sub-cy", ""); !slices.Equal(got, []string{"eng"}) {
		t.Errorf("an identity no sign-in bound narrowed sub-cy: %v", got)
	}
	if got := record("sub-ben", ""); !slices.Equal(got, []string{"eng"}) {
		t.Errorf("a member not removed: %v, want untouched", got)
	}
	if err := st.StartGroupRemoval(ctx, blank.ID, eve); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-eve", ""); err != nil || got.Groups == nil || len(got.Groups) != 0 || !got.Truncated {
		t.Errorf("removed from a group with a blank id: %+v, %v; want no groups (not nil), marked partial", got, err)
	}
}

// A record whose groups are not a JSON list (here JSON null) is left as it is by a SCIM removal, which
// still removes the membership: the list step is never asked of it, so it cannot fail the removal.
func TestPG_PrincipalGroups_ScimRemovalSkipsARecordThatIsNoList(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ('sub-ann', 'https://idp.example', 'ann@corp.example') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-ann", "", "ann@corp.example", []string{"eng"}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_groups SET groups = 'null'::jsonb WHERE principal = 'sub-ann'`); err != nil {
		t.Fatal(err)
	}
	eng, err := st.CreateScimGroup(ctx, "eng", "Engineering", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddScimGroupMembers(ctx, eng.ID, []uuid.UUID{id}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.StartGroupRemoval(ctx, eng.ID, id); err != nil {
		t.Fatalf("removal with a record that is no list: %v, want it to succeed", err)
	}
	var members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scim_group_members WHERE group_id = $1 AND identity_id = $2`, eng.ID, id).Scan(&members); err != nil || members != 0 {
		t.Fatalf("membership rows %d, %v; want the membership removed", members, err)
	}
	var kind string
	if err := pool.QueryRow(ctx, `SELECT jsonb_typeof(groups) FROM principal_groups WHERE principal = 'sub-ann'`).Scan(&kind); err != nil || kind != "null" {
		t.Fatalf("record groups are %q, %v; want JSON null left as it was", kind, err)
	}
}

// The sweeper's batch is chosen among the configured issuer's people, and tenant when one is configured, before
// its limit: a full batch's worth of earlier pending removals of another issuer or of another tenant under the
// same issuer, whose identity ids sort first, does not hide the own or the legacy (no tenant recorded) ones. The
// predicate is the one the API applies to a user by id: the issuer, and the tenant unless either side has none.
func TestPG_PendingGroupRemovals_AreFilteredToTheIssuerAndTenantBeforeTheLimit(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now()
	const issuer, otherIssuer, tenant, otherTenant = "https://idp.example", "https://idp.other.example", "tenant-a", "tenant-b"
	grp, err := st.CreateScimGroup(ctx, "eng", "Engineering", now)
	if err != nil {
		t.Fatal(err)
	}
	pending := func(id uuid.UUID, principal, iss, ten string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO principal_identities (id, principal, issuer, tenant_id, email_lower) VALUES ($1, $2, $3, $4, $5)`,
			id, principal, iss, ten, principal+"@corp.example"); err != nil {
			t.Fatal(err)
		}
		if err := st.StartGroupRemoval(ctx, grp.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	const limit = 50
	var foreignTenant []uuid.UUID
	for i := range limit + 10 {
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		if i%2 == 0 {
			pending(id, fmt.Sprintf("sub-fi-%d", i), otherIssuer, tenant)
		} else {
			pending(id, fmt.Sprintf("sub-ft-%d", i), issuer, otherTenant)
			foreignTenant = append(foreignTenant, id)
		}
	}
	own := uuid.MustParse("ffffffff-ffff-4fff-8fff-fffffffffffe")
	legacy := uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")
	pending(own, "sub-own", issuer, tenant)
	pending(legacy, "sub-legacy", issuer, "")

	ids := func(issuer, tenant string) []uuid.UUID {
		t.Helper()
		got, err := st.PendingGroupRemovals(ctx, issuer, tenant, 0, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []uuid.UUID
		for _, p := range got {
			if p.GroupID != grp.ID {
				t.Errorf("pending removal of group %v, want %v", p.GroupID, grp.ID)
			}
			out = append(out, p.IdentityID)
		}
		return out
	}
	if got := ids(issuer, tenant); !slices.Equal(got, []uuid.UUID{own, legacy}) {
		t.Errorf("issuer and tenant configured: %v, want the own and the legacy row alone", got)
	}
	// With no tenant configured the tenant is not asked, as in the API: every row of the issuer is listed, none of another's.
	want := append(slices.Clone(foreignTenant), own, legacy)
	if got := ids(issuer, ""); len(got) != len(want) || !slices.Equal(got, want) {
		t.Errorf("issuer alone: %d rows %v, want the issuer's %d", len(got), got, len(want))
	}
	if got := ids("https://idp.nobody.example", tenant); len(got) != 0 {
		t.Errorf("an issuer with no people: %v", got)
	}
}
