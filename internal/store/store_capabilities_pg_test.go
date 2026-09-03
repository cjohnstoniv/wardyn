// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for capability grants + the per-kind enforcement switch
// (migration 0042). Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
// Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capGrant builds an allow grant for the given subject. Every test below tags
// its rows with a per-test capability name so the shared substrate's other
// rows (this table is global — there is no per-run scoping to filter on) can
// never make an assertion here pass or fail by accident.
func capGrant(kind string, st types.CapabilitySubjectType, subject, value string) types.CapabilityGrant {
	return types.CapabilityGrant{
		SubjectType: st,
		Subject:     subject,
		Capability:  kind,
		Value:       value,
		Effect:      types.CapabilityAllow,
		CreatedBy:   "admin@example.com",
	}
}

// onlyKind filters a grant list down to one test's own capability tag.
func onlyKind(grants []types.CapabilityGrant, kind string) []types.CapabilityGrant {
	out := []types.CapabilityGrant{}
	for _, g := range grants {
		if g.Capability == kind {
			out = append(out, g)
		}
	}
	return out
}

// TestPG_CapabilityGrants_UpsertFlipsInPlace: the natural-key UNIQUE is the
// whole reason a re-grant is safe. Writing allow then deny for the SAME
// (subject_type, subject, capability, value) must leave ONE row carrying the
// new effect and the ORIGINAL id — two rows would resolve as a permanent deny
// (deny beats allow) that no admin could explain, and a fresh id on the return
// would hand the console a DELETE target that names no row.
func TestPG_CapabilityGrants_UpsertFlipsInPlace(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	kind := "test_flip_" + uuid.NewString()

	g := capGrant(kind, types.CapabilitySubjectUser, "alice@example.com", "pypi.org")
	first, err := st.UpsertCapabilityGrant(ctx, g)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteCapabilityGrant(ctx, first.ID) })
	if first.ID == uuid.Nil {
		t.Fatal("insert returned the nil uuid; the console would have no delete target")
	}
	if first.CreatedAt.IsZero() || first.CreatedBy != "admin@example.com" {
		t.Errorf("insert = %+v, want a server-stamped created_at and the caller's created_by", first)
	}

	g.Effect = types.CapabilityDeny
	g.ID = uuid.New() // a fresh submit carries a fresh id; the conflict must ignore it
	flipped, err := st.UpsertCapabilityGrant(ctx, g)
	if err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if flipped.ID != first.ID {
		t.Errorf("re-grant id = %s, want the existing row's %s", flipped.ID, first.ID)
	}
	if flipped.Effect != types.CapabilityDeny {
		t.Errorf("re-grant effect = %q, want deny", flipped.Effect)
	}

	all, err := st.ListCapabilityGrants(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if mine := onlyKind(all, kind); len(mine) != 1 {
		t.Fatalf("list has %d rows for %s, want exactly 1 (a duplicate resolves as a permanent deny)", len(mine), kind)
	}
}

// TestPG_CapabilityGrants_DeleteMissing: DELETE on an unknown id is
// ErrNotFound, so the CRUD route can answer 404 instead of a silent 204 that
// lets an admin believe a grant they still hold is gone.
func TestPG_CapabilityGrants_DeleteMissing(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	if err := st.DeleteCapabilityGrant(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_CapabilityGrants_ListForSubject pins the resolver's read: the `all`
// row always applies, a `user` row applies through EITHER identity (sub or
// email), a `group` row applies through the login-time claim snapshot, and a
// row naming somebody else applies to nobody. Getting this wrong is either a
// silent lockout or a silent grant to the wrong human.
func TestPG_CapabilityGrants_ListForSubject(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	kind := "test_subj_" + uuid.NewString()

	rows := map[string]types.CapabilityGrant{
		"all":      capGrant(kind, types.CapabilitySubjectAll, "", "*"),
		"bysub":    capGrant(kind, types.CapabilitySubjectUser, "sub-alice", "a.example.com"),
		"byemail":  capGrant(kind, types.CapabilitySubjectUser, "alice@example.com", "b.example.com"),
		"bygroup":  capGrant(kind, types.CapabilitySubjectGroup, "eng", "c.example.com"),
		"stranger": capGrant(kind, types.CapabilitySubjectUser, "mallory@example.com", "d.example.com"),
		"othergrp": capGrant(kind, types.CapabilitySubjectGroup, "finance", "e.example.com"),
	}
	for name, g := range rows {
		saved, err := st.UpsertCapabilityGrant(ctx, g)
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		t.Cleanup(func() { _ = st.DeleteCapabilityGrant(ctx, saved.ID) })
	}

	got, err := st.ListCapabilityGrantsFor(ctx, []string{"sub-alice", "alice@example.com"}, []string{"eng"})
	if err != nil {
		t.Fatalf("list for subject: %v", err)
	}
	seen := map[string]bool{}
	for _, g := range onlyKind(got, kind) {
		seen[g.Value] = true
	}
	for _, want := range []string{"*", "a.example.com", "b.example.com", "c.example.com"} {
		if !seen[want] {
			t.Errorf("resolver read is missing the %q grant — that member is silently locked out", want)
		}
	}
	for _, never := range []string{"d.example.com", "e.example.com"} {
		if seen[never] {
			t.Errorf("resolver read returned %q, a grant written for somebody else", never)
		}
	}

	// A caller with NO groups and no user match still sees the `all` row and
	// nothing else — the IdP-without-groups baseline.
	bare, err := st.ListCapabilityGrantsFor(ctx, nil, nil)
	if err != nil {
		t.Fatalf("list for bare subject: %v", err)
	}
	mine := onlyKind(bare, kind)
	if len(mine) != 1 || mine[0].SubjectType != types.CapabilitySubjectAll {
		t.Errorf("bare caller saw %+v, want exactly the `all` row", mine)
	}
}

// TestPG_CapabilityEnforcement_ReplaceAndPrune: an absent row means NOT
// enforced, so the PUT is a full-map replace — a capability the admin omits
// must lose its row rather than keep an enforcement nobody can see in the
// submitted map. The empty map is the "turn everything off" case and has to
// clear the table rather than no-op.
func TestPG_CapabilityEnforcement_ReplaceAndPrune(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	t.Cleanup(func() { _, _ = st.PutCapabilityEnforcement(ctx, nil) })

	got, err := st.PutCapabilityEnforcement(ctx, map[string]bool{"egress_host": true, "secret": false})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !got["egress_host"] || got["secret"] {
		t.Errorf("put returned %v, want egress_host on and secret off", got)
	}

	// Re-read through the independent getter: the PUT's return must not be the
	// only place the state exists.
	read, err := st.GetCapabilityEnforcement(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !read["egress_host"] {
		t.Errorf("get = %v, want egress_host enforced", read)
	}

	// Omitting egress_host prunes it: a missing key reads as not enforced.
	pruned, err := st.PutCapabilityEnforcement(ctx, map[string]bool{"image": true})
	if err != nil {
		t.Fatalf("put pruning: %v", err)
	}
	if _, still := pruned["egress_host"]; still {
		t.Errorf("put = %v, want the omitted egress_host row pruned", pruned)
	}
	if !pruned["image"] {
		t.Errorf("put = %v, want image enforced", pruned)
	}

	// The empty map clears everything — "turn it all off" is a real action.
	empty, err := st.PutCapabilityEnforcement(ctx, nil)
	if err != nil {
		t.Fatalf("put empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("put(nil) = %v, want an empty map (nothing enforced)", empty)
	}
}

// TestPG_CapabilityGrants_SubjectTypeCheck: the DB CHECK is the last line under
// a caller that skipped types.CapabilitySubjectType.Valid. A garbage subject
// type must be REFUSED, not stored as a row no resolver will ever match (an
// admin would see their grant in the table and wonder why it does nothing).
func TestPG_CapabilityGrants_SubjectTypeCheck(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	bad := capGrant("test_check_"+uuid.NewString(), types.CapabilitySubjectType("everyone"), "x", "y")
	if _, err := st.UpsertCapabilityGrant(ctx, bad); err == nil {
		t.Error("stored a grant with an unknown subject_type; the CHECK is missing")
	} else if !strings.Contains(err.Error(), "capability_grants") {
		t.Errorf("refusal did not name the table: %v", err)
	}

	bad = capGrant("test_check_"+uuid.NewString(), types.CapabilitySubjectUser, "x", "y")
	bad.Effect = types.CapabilityEffect("maybe")
	if _, err := st.UpsertCapabilityGrant(ctx, bad); err == nil {
		t.Error("stored a grant with an unknown effect; the CHECK is missing")
	}
}

// TestPG_ListGroupDenyGrants_PredicateMatchesAGoSideScan pins the SQL predicate
// that replaced internal/api's whole-table scan on the unresolvable-group-deny
// FAIL-CLOSED path. The api-side equivalence test drives a Go double, so it
// cannot see this query at all — and this query is the newly written half, so
// it is where a narrowing mistake would actually live.
//
// The oracle is a Go filter applying the predicate the old code applied in
// Go (subject_type='group' AND effect='deny' AND capability=$1). Postgres must
// return exactly that set. A dropped clause here means either a deny that stops
// firing (a breach) or one that fires on rows the old path ignored (every
// pre-0.7 token refused on every deployment).
func TestPG_ListGroupDenyGrants_PredicateMatchesAGoSideScan(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	kind := "test_groupdeny_" + uuid.NewString()
	otherKind := "test_groupdeny_other_" + uuid.NewString()

	// Every combination of the three predicate fields, so each clause has a row
	// that only it excludes — plus two rows on a DIFFERENT kind, since the kind
	// filter is the one carrying a bind parameter.
	seed := []types.CapabilityGrant{
		{SubjectType: types.CapabilitySubjectGroup, Subject: "walled", Capability: kind, Value: "prod-db", Effect: types.CapabilityDeny},
		{SubjectType: types.CapabilitySubjectGroup, Subject: "contractors", Capability: kind, Value: "*.corp.example", Effect: types.CapabilityDeny},
		{SubjectType: types.CapabilitySubjectGroup, Subject: "walled", Capability: kind, Value: "allowed-db", Effect: types.CapabilityAllow},
		{SubjectType: types.CapabilitySubjectUser, Subject: "alice@example.com", Capability: kind, Value: "prod-db", Effect: types.CapabilityDeny},
		{SubjectType: types.CapabilitySubjectAll, Subject: "", Capability: kind, Value: "prod-db", Effect: types.CapabilityDeny},
		{SubjectType: types.CapabilitySubjectGroup, Subject: "walled", Capability: otherKind, Value: "prod-db", Effect: types.CapabilityDeny},
		{SubjectType: types.CapabilitySubjectUser, Subject: "bob@example.com", Capability: otherKind, Value: "x", Effect: types.CapabilityAllow},
	}
	for _, g := range seed {
		g.CreatedBy = "admin@example.com"
		if _, err := st.UpsertCapabilityGrant(ctx, g); err != nil {
			t.Fatalf("seed %+v: %v", g, err)
		}
	}

	// The oracle: the predicate the pre-fix Go loop applied.
	wantSet := map[string]bool{}
	for _, g := range seed {
		if g.SubjectType == types.CapabilitySubjectGroup && g.Effect == types.CapabilityDeny && g.Capability == kind {
			wantSet[g.Subject+"|"+g.Value] = true
		}
	}
	if len(wantSet) == 0 {
		t.Fatal("the oracle selected nothing; the fixture proves nothing")
	}

	got, err := st.ListGroupDenyGrants(ctx, kind)
	if err != nil {
		t.Fatalf("ListGroupDenyGrants: %v", err)
	}
	gotSet := map[string]bool{}
	for _, g := range got {
		if g.SubjectType != types.CapabilitySubjectGroup {
			t.Errorf("returned a %s-tier row (%+v) — the subject_type clause is not holding", g.SubjectType, g)
		}
		if g.Effect != types.CapabilityDeny {
			t.Errorf("returned an %s row (%+v) — the effect clause is not holding; every pre-0.7 token would be refused on an ALLOW row", g.Effect, g)
		}
		if g.Capability != kind {
			t.Errorf("returned a %q row while asking for %q — the kind clause is not holding", g.Capability, kind)
		}
		gotSet[g.Subject+"|"+g.Value] = true
	}
	for k := range wantSet {
		if !gotSet[k] {
			t.Errorf("row %q missing from the result — a group DENY that no longer reaches the refusal is a breach, not a slow path", k)
		}
	}
	for k := range gotSet {
		if !wantSet[k] {
			t.Errorf("row %q returned but the Go-side scan excludes it", k)
		}
	}

	// And the empty case, which is the one nearly every deployment hits: no
	// group deny rows of that kind => no rows, not "all of them".
	empty, err := st.ListGroupDenyGrants(ctx, "test_groupdeny_absent_"+uuid.NewString())
	if err != nil {
		t.Fatalf("ListGroupDenyGrants(absent kind): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an unknown kind returned %d rows, want 0 — the refusal must stay SCOPED or it denies every pre-0.7 token", len(empty))
	}
}
