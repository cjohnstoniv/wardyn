// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Phase 2 lane B: the console-managed (Getting Started -> People) role
// mappings seam — mergeRoleMaps' pure merge rules, the CallbackHandler arm
// that reads Config.RoleMappings once per login, and PreviewRole, the
// console's own-derivation preview. See derive.go for the implementation
// this file pins.
package oidc_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"testing"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// fakeRoleMappingSource is an in-memory writoidc.RoleMappingSource double —
// mirrors fakeSessionRevocations' canned rows/err shape, since
// Config.RoleMappings follows the exact same fail-closed-on-error contract
// D16's Revocations already pins.
type fakeRoleMappingSource struct {
	rows []writoidc.RoleMapping
	err  error
}

func (f *fakeRoleMappingSource) ListRoleMappings(context.Context) ([]writoidc.RoleMapping, error) {
	return f.rows, f.err
}

// newRoleMappingAuth is newRoleAuth (see oidc_test.go) plus a
// Config.RoleMappings source — a local variant rather than widening
// newRoleAuth's signature, which every other role-derivation test also calls.
func (e *idpEnv) newRoleMappingAuth(t *testing.T, roleMap map[string]string, defaultRole string, legacyAdminEmails []string, mappings writoidc.RoleMappingSource) *writoidc.Authenticator {
	t.Helper()
	rt := &rewriteTokenRT{
		base:          http.DefaultTransport,
		originalToken: e.httpSrv.URL + "/token",
		replacedToken: e.tokenSrv.URL + "/",
	}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: rt})
	cfg := writoidc.Config{
		IssuerURL:         e.httpSrv.URL,
		ClientID:          e.clientID,
		ClientSecret:      "secret",
		RedirectURL:       "http://localhost/auth/callback",
		RoleMap:           roleMap,
		DefaultRole:       defaultRole,
		LegacyAdminEmails: legacyAdminEmails,
		RoleMappings:      mappings,
	}
	auth, err := writoidc.New(ctx, cfg, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}
	return auth
}

// ─── mergeRoleMaps: pure-function table tests ─────────────────────────────────

func TestMergeRoleMapsDisjointUnion(t *testing.T) {
	chart := map[string]string{"wardyn.admin": writoidc.RoleAdmin}
	rows := []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}

	merged, shadowed := writoidc.MergeRoleMapsForTest(chart, nil, rows)

	want := map[string]string{"wardyn.admin": writoidc.RoleAdmin, "eng-team": writoidc.RoleMember}
	if !maps.Equal(merged, want) {
		t.Errorf("merged = %v, want %v", merged, want)
	}
	if len(shadowed) != 0 {
		t.Errorf("shadowed = %v, want none (disjoint keys)", shadowed)
	}
}

// TestMergeRoleMapsConsoleKeyShadowedByChart: a console row keyed the same as
// a chart entry never overwrites it — the chart always wins a collision — and
// the loss is reported, not silent.
func TestMergeRoleMapsConsoleKeyShadowedByChart(t *testing.T) {
	chart := map[string]string{"eng-team": writoidc.RoleMember}
	rows := []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleAdmin}}

	merged, shadowed := writoidc.MergeRoleMapsForTest(chart, nil, rows)

	if merged["eng-team"] != writoidc.RoleMember {
		t.Errorf("merged[eng-team] = %q, want %q (chart wins the collision)", merged["eng-team"], writoidc.RoleMember)
	}
	if len(shadowed) != 1 || shadowed[0] != "eng-team" {
		t.Errorf("shadowed = %v, want [eng-team]", shadowed)
	}
}

// TestMergeRoleMapsConsoleRowShadowedByOperatorEmail: a console row whose
// value is on the operator allowlist is shadowed the same way as a chart
// collision — the allowlist is a stronger, harder-to-edit admin source, and
// a differing console role for that email would be a confusing no-op.
func TestMergeRoleMapsConsoleRowShadowedByOperatorEmail(t *testing.T) {
	legacyAdminEmails := []string{"Ops@Corp.Example"} // mixed case, as an operator would type it
	rows := []writoidc.RoleMapping{{Value: "ops@corp.example", Role: writoidc.RoleMember}}

	merged, shadowed := writoidc.MergeRoleMapsForTest(nil, legacyAdminEmails, rows)

	if _, ok := merged["ops@corp.example"]; ok {
		t.Errorf("merged = %v, want the operator-email row dropped, not added as member", merged)
	}
	if len(shadowed) != 1 || shadowed[0] != "ops@corp.example" {
		t.Errorf("shadowed = %v, want [ops@corp.example]", shadowed)
	}
}

// TestMergeRoleMapsUsesRowsVerbatim pins the canonicalization contract on
// RoleMapping.Value: mergeRoleMaps stores it AS GIVEN, never re-lowering — a
// non-canonical (mixed-case) row becomes a merged key that the ASCII-lowered
// claim lookup in deriveRole can never hit, proving the API layer — not this
// function — owns canonicalization.
func TestMergeRoleMapsUsesRowsVerbatim(t *testing.T) {
	rows := []writoidc.RoleMapping{{Value: "Eng-Team", Role: writoidc.RoleMember}}

	merged, _ := writoidc.MergeRoleMapsForTest(nil, nil, rows)

	if _, ok := merged["Eng-Team"]; !ok {
		t.Fatalf("merged = %v, want the row's exact (non-lowered) value as the key", merged)
	}
	if _, ok := merged["eng-team"]; ok {
		t.Error("merged holds a lowercased key mergeRoleMaps never wrote — it must not silently canonicalize")
	}
}

// TestMergeRoleMapsCanonicalRowMatchesMixedCaseClaim is the flip side of
// TestMergeRoleMapsUsesRowsVerbatim: a PROPERLY canonical (already-lowercase)
// console row DOES match a mixed-case claim end to end, because deriveRole
// lowers the claim before lookup — the matching case-insensitivity lives
// entirely in deriveRole, not in the merge.
func TestMergeRoleMapsCanonicalRowMatchesMixedCaseClaim(t *testing.T) {
	rows := []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}
	merged, _ := writoidc.MergeRoleMapsForTest(nil, nil, rows)

	role, matches, ok := writoidc.DeriveRoleForTest(nil, []string{"ENG-Team"}, "", merged, nil, "")
	if !ok || role != writoidc.RoleMember {
		t.Fatalf("role = %q, ok = %v, want %q (a canonical row matches through deriveRole's lowering)", role, ok, writoidc.RoleMember)
	}
	if len(matches) != 1 || matches[0].Source != writoidc.MatchSourceMapRow {
		t.Errorf("matches = %+v, want one MatchSourceMapRow match", matches)
	}
}

// TestMergeRoleMapsPostureFlip pins the two transitions the console's
// acknowledgement-guard UI depends on as PURE-FUNCTION facts: adding the
// first row flips an empty merged map to non-empty, and deleting it flips
// back. The guard itself is API-layer and is deliberately NOT built here.
func TestMergeRoleMapsPostureFlip(t *testing.T) {
	before, _ := writoidc.MergeRoleMapsForTest(nil, nil, nil)
	if len(before) != 0 {
		t.Fatalf("empty chart + no console rows: merged = %v, want empty", before)
	}

	after, _ := writoidc.MergeRoleMapsForTest(nil, nil, []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}})
	if len(after) == 0 {
		t.Error("empty chart + first console row: merged is still empty, want non-empty")
	}

	deleted, _ := writoidc.MergeRoleMapsForTest(nil, nil, nil)
	if len(deleted) != 0 {
		t.Errorf("deleting the only console row: merged = %v, want empty again", deleted)
	}
}

// TestMergeRoleMapsChartNonEmptyNeverEmpty: once the chart itself has an
// entry, the merged map can never come back empty regardless of what the
// console holds — the chart is a boot-time floor the console can only add on
// top of.
func TestMergeRoleMapsChartNonEmptyNeverEmpty(t *testing.T) {
	chart := map[string]string{"wardyn.admin": writoidc.RoleAdmin}
	for _, rows := range [][]writoidc.RoleMapping{nil, {}, {{Value: "eng-team", Role: writoidc.RoleMember}}} {
		merged, _ := writoidc.MergeRoleMapsForTest(chart, nil, rows)
		if len(merged) == 0 {
			t.Errorf("rows = %v: merged is empty, want the chart entry to always survive", rows)
		}
	}
}

// ─── CallbackHandler: Config.RoleMappings wired ───────────────────────────────

// TestCallbackRoleMappingsStoreErrorDeniesLogin: a wired store that errors
// must deny the login with the distinct role_check_unavailable code and clear
// any pre-existing session cookie — never fall back to the env-only map.
func TestCallbackRoleMappingsStoreErrorDeniesLogin(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleMappingAuth(t, nil, "", nil, &fakeRoleMappingSource{err: errors.New("pg: connection refused")})

	w, sess := doRoleCallback(t, env, auth, "anyone@corp.example", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if loc := w.Result().Header.Get("Location"); loc == "" || !containsAuthError(loc, "role_check_unavailable") {
		t.Errorf("Location = %q, want auth_error=role_check_unavailable", loc)
	}
	if sess.Role != "" {
		t.Errorf("role = %q, want no session issued (denied)", sess.Role)
	}
	assertSessionCookieCleared(t, w)
}

// TestCallbackRoleMappingsRowInfluencesRole: a console row reaches the merged
// map CallbackHandler actually derives against — a groups-claim value with no
// chart entry maps to member purely via the wired store.
func TestCallbackRoleMappingsRowInfluencesRole(t *testing.T) {
	env := newIdPEnv(t)
	store := &fakeRoleMappingSource{rows: []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}}
	auth := env.newRoleMappingAuth(t, nil, "", nil, store)

	w, sess := doRoleCallback(t, env, auth, "carol@corp.example", nil, []string{"eng-team"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleMember {
		t.Errorf("role = %q, want %q (console row alone, no chart entry)", sess.Role, writoidc.RoleMember)
	}
}

// TestCallbackRoleMappingsChartWinsCollision: end to end through the
// callback, a chart entry beats a console row keyed the same, and the login
// still succeeds (the shadowing is logged, never a denial).
func TestCallbackRoleMappingsChartWinsCollision(t *testing.T) {
	env := newIdPEnv(t)
	store := &fakeRoleMappingSource{rows: []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleAdmin}}}
	auth := env.newRoleMappingAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, "", nil, store)

	w, sess := doRoleCallback(t, env, auth, "dana@corp.example", nil, []string{"eng-team"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleMember {
		t.Errorf("role = %q, want %q (chart wins the collision, even end to end)", sess.Role, writoidc.RoleMember)
	}
}

// ─── PreviewRole ───────────────────────────────────────────────────────────────

// TestPreviewRoleParityWithCallback: PreviewRole must derive the SAME role a
// real login with identical claims would, including a real store read.
func TestPreviewRoleParityWithCallback(t *testing.T) {
	env := newIdPEnv(t)
	store := &fakeRoleMappingSource{rows: []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}}
	auth := env.newRoleMappingAuth(t, nil, "", nil, store)

	previewRole, _, previewOK, err := auth.PreviewRole(context.Background(), nil, []string{"eng-team"}, "carol@corp.example")
	if err != nil {
		t.Fatalf("PreviewRole: %v", err)
	}

	_, sess := doRoleCallback(t, env, auth, "carol@corp.example", nil, []string{"eng-team"})
	if !previewOK || previewRole != sess.Role {
		t.Errorf("PreviewRole = (%q, ok=%v), want parity with the callback's derived role %q", previewRole, previewOK, sess.Role)
	}
}

// TestPreviewRoleErrorPropagation: a store error surfaces as PreviewRole's
// err (couldn't-check), distinct from ok=false (checked, nothing matched) —
// the console preview must be able to tell those apart.
func TestPreviewRoleErrorPropagation(t *testing.T) {
	env := newIdPEnv(t)
	wantErr := errors.New("pg: connection refused")
	auth := env.newRoleMappingAuth(t, nil, "", nil, &fakeRoleMappingSource{err: wantErr})

	role, matched, ok, err := auth.PreviewRole(context.Background(), nil, nil, "x@corp.example")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if ok {
		t.Error("ok = true on a store error, want false")
	}
	if role != "" || matched != nil {
		t.Errorf("role = %q, matched = %v, want zero values on error", role, matched)
	}
}

// TestPreviewRoleProvenance pins that PreviewRole's Match slice correctly
// names the source that produced the derived role — the console preview's
// whole point.
func TestPreviewRoleProvenance(t *testing.T) {
	env := newIdPEnv(t)

	t.Run("operator email", func(t *testing.T) {
		auth := env.newRoleMappingAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, "", []string{"ops@corp.example"}, nil)
		role, matched, ok, err := auth.PreviewRole(context.Background(), nil, []string{"eng-team"}, "ops@corp.example")
		if err != nil || !ok || role != writoidc.RoleAdmin {
			t.Fatalf("role = %q, ok = %v, err = %v, want (%q, true, nil)", role, ok, err, writoidc.RoleAdmin)
		}
		if !hasMatchSource(matched, writoidc.MatchSourceOperatorAllowlist) {
			t.Errorf("matched = %+v, want a MatchSourceOperatorAllowlist entry", matched)
		}
	})

	t.Run("map row", func(t *testing.T) {
		auth := env.newRoleMappingAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, "", nil, nil)
		role, matched, ok, err := auth.PreviewRole(context.Background(), nil, []string{"eng-team"}, "eng@corp.example")
		if err != nil || !ok || role != writoidc.RoleMember {
			t.Fatalf("role = %q, ok = %v, err = %v, want (%q, true, nil)", role, ok, err, writoidc.RoleMember)
		}
		if !hasMatchSource(matched, writoidc.MatchSourceMapRow) {
			t.Errorf("matched = %+v, want a MatchSourceMapRow entry", matched)
		}
	})

	t.Run("default role fallthrough", func(t *testing.T) {
		auth := env.newRoleMappingAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, writoidc.RoleMember, nil, nil)
		role, matched, ok, err := auth.PreviewRole(context.Background(), nil, nil, "nobody@corp.example")
		if err != nil || !ok || role != writoidc.RoleMember {
			t.Fatalf("role = %q, ok = %v, err = %v, want (%q, true, nil)", role, ok, err, writoidc.RoleMember)
		}
		if !hasMatchSource(matched, writoidc.MatchSourceDefaultRole) {
			t.Errorf("matched = %+v, want a MatchSourceDefaultRole entry", matched)
		}
	})
}

// hasMatchSource reports whether any Match in ms carries source.
func hasMatchSource(ms []writoidc.Match, source writoidc.MatchSource) bool {
	for _, m := range ms {
		if m.Source == source {
			return true
		}
	}
	return false
}

// containsAuthError reports whether a "/?auth_error=<code>" redirect
// Location carries the given code.
func containsAuthError(location, code string) bool {
	u, err := url.Parse(location)
	if err != nil {
		return false
	}
	return u.Query().Get("auth_error") == code
}

// ─── read-only accessors ────────────────────────────────────────────────────

// TestChartRoleMapCopiesNotTheLiveMap: the returned map is a copy — mutating
// it must not affect a subsequent call, since it is read straight off boot
// Config with no defensive copy otherwise.
func TestChartRoleMapCopiesNotTheLiveMap(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"wardyn.admin": writoidc.RoleAdmin}, "", nil)

	got := auth.ChartRoleMap()
	got["injected"] = writoidc.RoleAdmin // mutate the caller's copy

	again := auth.ChartRoleMap()
	if _, ok := again["injected"]; ok {
		t.Error("mutating a returned ChartRoleMap leaked into a later call — it is not a copy")
	}
	if again["wardyn.admin"] != writoidc.RoleAdmin {
		t.Errorf("ChartRoleMap() = %v, want the boot chart map preserved", again)
	}
}

func TestDefaultRoleAndHasOperatorEmailsAccessors(t *testing.T) {
	env := newIdPEnv(t)

	unset := env.newRoleAuth(t, nil, "", nil)
	if unset.DefaultRole() != "" {
		t.Errorf("DefaultRole() = %q, want empty", unset.DefaultRole())
	}
	if unset.HasOperatorEmails() {
		t.Error("HasOperatorEmails() = true, want false (no LegacyAdminEmails configured)")
	}

	set := env.newRoleAuth(t, nil, writoidc.RoleMember, []string{"ops@corp.example"})
	if set.DefaultRole() != writoidc.RoleMember {
		t.Errorf("DefaultRole() = %q, want %q", set.DefaultRole(), writoidc.RoleMember)
	}
	if !set.HasOperatorEmails() {
		t.Error("HasOperatorEmails() = false, want true")
	}
}

