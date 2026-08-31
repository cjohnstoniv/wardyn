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

// TestMergeRoleMapsRejectsNonCanonicalRow supersedes the old
// TestMergeRoleMapsUsesRowsVerbatim: an Opus review found that storing a
// non-canonical row VERBATIM (the old contract) let a bad row become either a
// permanent dead key (never hit by deriveRole's lowered claim lookup) or,
// worse, an empty/whitespace key that matches ANY empty/whitespace claim —
// admin escalation. mergeRoleMaps now REJECTS a non-canonical row instead of
// trusting the write boundary; this is the sanctioned rewrite the review
// asked for, pinning the closed contract rather than the old open one.
func TestMergeRoleMapsRejectsNonCanonicalRow(t *testing.T) {
	rows := []writoidc.RoleMapping{{Value: "Eng-Team", Role: writoidc.RoleMember}}

	merged, shadowed := writoidc.MergeRoleMapsForTest(nil, nil, rows)

	if len(merged) != 0 {
		t.Errorf("merged = %v, want empty — a non-canonical (mixed-case) row must be rejected, not stored verbatim", merged)
	}
	if len(shadowed) != 1 || shadowed[0] != "Eng-Team" {
		t.Errorf("shadowed = %v, want [Eng-Team] (rejected rows are reported, not silently dropped)", shadowed)
	}
}

// TestMergeRoleMapsRejectsInvalidRows table-tests every way a console row can
// fail the canonical/valid contract mergeRoleMaps now enforces: each case
// must be dropped into shadowed and contribute nothing to merged.
func TestMergeRoleMapsRejectsInvalidRows(t *testing.T) {
	cases := []struct {
		name string
		row  writoidc.RoleMapping
	}{
		{"empty value", writoidc.RoleMapping{Value: "", Role: writoidc.RoleAdmin}},
		{"whitespace-only value", writoidc.RoleMapping{Value: " ", Role: writoidc.RoleAdmin}},
		{"mixed-case (non-canonical) value", writoidc.RoleMapping{Value: "Eng-Team", Role: writoidc.RoleMember}},
		{"invalid role", writoidc.RoleMapping{Value: "eng-team", Role: "Admin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, shadowed := writoidc.MergeRoleMapsForTest(nil, nil, []writoidc.RoleMapping{tc.row})
			if len(merged) != 0 {
				t.Errorf("merged = %v, want empty (row rejected)", merged)
			}
			if len(shadowed) != 1 || shadowed[0] != tc.row.Value {
				t.Errorf("shadowed = %v, want [%q]", shadowed, tc.row.Value)
			}
		})
	}
}

// TestMergeRoleMapsRejectedRowNeverFlipsArm closes the HIGH finding's other
// half: an empty chart plus ONLY rejected console rows must leave merged
// empty, so deriveRole stays on its arm-1 path (no role map: legacy allowlist
// alone) rather than flipping to arm 2 (role map present, nothing matched) —
// which would deny a login arm 1 would have allowed, whenever no DefaultRole
// is configured.
func TestMergeRoleMapsRejectedRowNeverFlipsArm(t *testing.T) {
	rows := []writoidc.RoleMapping{
		{Value: "", Role: writoidc.RoleAdmin},
		{Value: "Bad-Case", Role: writoidc.RoleMember},
		{Value: "eng-team", Role: "not-a-role"},
	}
	merged, _ := writoidc.MergeRoleMapsForTest(nil, nil, rows)
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want empty — only-invalid rows must never flip deriveRole's arm", merged)
	}

	role, _, ok := writoidc.DeriveRoleForTest(nil, nil, "anyone@corp.example", merged, nil, "")
	if !ok || role != writoidc.RoleAdmin {
		t.Errorf("role = %q, ok = %v, want (%q, true) — arm 1 (no role map, no legacy allowlist) still applies", role, ok, writoidc.RoleAdmin)
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

// TestDeriveRoleDedupesMapRowMatchAcrossClaims pins the MED dedup fix: a
// value present in BOTH the roles claim and the groups claim (a real IdP
// shape — Entra can populate an App Role into both) must produce exactly ONE
// MatchSourceMapRow entry, not two, since PreviewRole's console preview
// renders the Match list verbatim and a duplicate would read as two things
// matching when only one claim value did.
func TestDeriveRoleDedupesMapRowMatchAcrossClaims(t *testing.T) {
	roleMap := map[string]string{"eng-team": writoidc.RoleMember}

	role, matches, ok := writoidc.DeriveRoleForTest([]string{"eng-team"}, []string{"eng-team"}, "", roleMap, nil, "")
	if !ok || role != writoidc.RoleMember {
		t.Fatalf("role = %q, ok = %v, want (%q, true)", role, ok, writoidc.RoleMember)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %+v, want exactly one deduped MatchSourceMapRow entry", matches)
	}
}

// TestDeriveRoleKeepsOperatorAllowlistAndMapRowDoubleEntry pins the OTHER
// half of the dedup fix: an email that is BOTH on the operator allowlist AND
// hits a map row (e.g. as the "email" value in the union deriveRole builds)
// must still produce TWO Match entries — one per Source — because that is
// genuinely distinct provenance, not a repeat of the same fact. Only same-
// value-same-source duplicates get collapsed.
func TestDeriveRoleKeepsOperatorAllowlistAndMapRowDoubleEntry(t *testing.T) {
	roleMap := map[string]string{"ops@corp.example": writoidc.RoleMember}
	legacyAdminEmails := []string{"ops@corp.example"}

	role, matches, ok := writoidc.DeriveRoleForTest(nil, nil, "ops@corp.example", roleMap, legacyAdminEmails, "")
	if !ok || role != writoidc.RoleAdmin {
		t.Fatalf("role = %q, ok = %v, want (%q, true) (allowlist admin wins the member map row)", role, ok, writoidc.RoleAdmin)
	}
	if !hasMatchSource(matches, writoidc.MatchSourceOperatorAllowlist) || !hasMatchSource(matches, writoidc.MatchSourceMapRow) {
		t.Fatalf("matches = %+v, want both an OperatorAllowlist and a MapRow entry for the same email", matches)
	}
	if len(matches) != 2 {
		t.Errorf("matches = %+v, want exactly 2 (one per source, not deduped across sources)", matches)
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

	rows := []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}
	after, _ := writoidc.MergeRoleMapsForTest(nil, nil, rows)
	if len(after) == 0 {
		t.Error("empty chart + first console row: merged is still empty, want non-empty")
	}

	// Derive the post-delete input by slicing the row OUT of rows — re-running
	// the identical nil,nil,nil call "before" already made (the old version of
	// this test) proved nothing about deletion, only that two no-rows calls
	// agree with each other.
	deleted, _ := writoidc.MergeRoleMapsForTest(nil, nil, rows[:0])
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

	t.Run("arm-1 operator allowlist (empty role map)", func(t *testing.T) {
		// PreviewRole's arm-1 path (see deriveRole's precedence doc): an empty
		// roleMap disables claim-based derivation entirely, so the operator
		// allowlist must be what decides — and provenance must still name
		// MatchSourceOperatorAllowlist, the SAME source arm 2 uses below, as
		// the ONLY Match, one entry exactly.
		auth := env.newRoleMappingAuth(t, nil, "", []string{"ops@corp.example"}, nil)
		role, matched, ok, err := auth.PreviewRole(context.Background(), nil, nil, "ops@corp.example")
		if err != nil || !ok || role != writoidc.RoleAdmin {
			t.Fatalf("role = %q, ok = %v, err = %v, want (%q, true, nil)", role, ok, err, writoidc.RoleAdmin)
		}
		if len(matched) != 1 || matched[0].Source != writoidc.MatchSourceOperatorAllowlist {
			t.Errorf("matched = %+v, want exactly one MatchSourceOperatorAllowlist entry", matched)
		}
	})

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

// ─── IsOperatorEmail ─────────────────────────────────────────────────────────

// TestIsOperatorEmail pins the case-insensitive allowlist membership check
// the console write boundary uses to name a collision — same match rule
// emailInList already applies inside deriveRole, so the two can never
// disagree about which emails are on the list.
func TestIsOperatorEmail(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, nil, "", []string{"Ops@Corp.Example"})

	if !auth.IsOperatorEmail("ops@corp.example") {
		t.Error("IsOperatorEmail(lowercased) = false, want true (case-insensitive match)")
	}
	if !auth.IsOperatorEmail("  Ops@Corp.Example  ") {
		t.Error("IsOperatorEmail(padded, exact case) = false, want true (trimmed)")
	}
	if auth.IsOperatorEmail("someone-else@corp.example") {
		t.Error("IsOperatorEmail(unlisted) = true, want false")
	}
	if auth.IsOperatorEmail("") {
		t.Error("IsOperatorEmail(\"\") = true, want false")
	}

	unset := env.newRoleAuth(t, nil, "", nil)
	if unset.IsOperatorEmail("ops@corp.example") {
		t.Error("IsOperatorEmail with no LegacyAdminEmails configured = true, want false")
	}
}

// ─── PreviewRoleAgainst ──────────────────────────────────────────────────────

// TestPreviewRoleAgainstParityWithPreviewRole: PreviewRoleAgainst, handed the
// exact rows a real Config.RoleMappings read would have returned, must derive
// the SAME role PreviewRole (and a real callback) does — it is the same
// mergeRoleMaps + deriveRole pipeline, just fed rows directly instead of
// reading them.
func TestPreviewRoleAgainstParityWithPreviewRole(t *testing.T) {
	env := newIdPEnv(t)
	rows := []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}
	store := &fakeRoleMappingSource{rows: rows}
	auth := env.newRoleMappingAuth(t, nil, "", nil, store)

	wantRole, _, wantOK, err := auth.PreviewRole(context.Background(), nil, []string{"eng-team"}, "carol@corp.example")
	if err != nil {
		t.Fatalf("PreviewRole: %v", err)
	}

	gotRole, gotOK := auth.PreviewRoleAgainst(rows, nil, []string{"eng-team"}, "carol@corp.example")
	if gotRole != wantRole || gotOK != wantOK {
		t.Errorf("PreviewRoleAgainst = (%q, %v), want parity with PreviewRole (%q, %v)", gotRole, gotOK, wantRole, wantOK)
	}
}

// TestPreviewRoleAgainstNoStoreRead: unlike PreviewRole, PreviewRoleAgainst
// must never touch Config.RoleMappings — even one wired to always error, so
// a candidate evaluation racing a real write never trips the same fail-closed
// store guard PreviewRole/CallbackHandler apply to a REAL read.
func TestPreviewRoleAgainstNoStoreRead(t *testing.T) {
	env := newIdPEnv(t)
	poisoned := &fakeRoleMappingSource{err: errors.New("must never be called")}
	auth := env.newRoleMappingAuth(t, nil, "", nil, poisoned)

	role, ok := auth.PreviewRoleAgainst(
		[]writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleAdmin}},
		nil, []string{"eng-team"}, "",
	)
	if !ok || role != writoidc.RoleAdmin {
		t.Fatalf("role = %q, ok = %v, want (%q, true) derived purely from the passed-in rows", role, ok, writoidc.RoleAdmin)
	}
}

// TestPreviewRoleAgainstLockoutScenario is the shape the console's
// POST/DELETE /access/mappings lockout guard actually needs: given the
// candidate rows AFTER a proposed write (here, a delete — the row that made
// this admin an admin has been removed, but ANOTHER row survives so the
// merged map stays non-empty — arm 2, not the "no role map at all" fallback
// to admin arm 1 would take), does the acting admin still derive admin? This
// is exactly "no" — the guard's whole reason to exist.
func TestPreviewRoleAgainstLockoutScenario(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil)

	before := []writoidc.RoleMapping{
		{Value: "admins", Role: writoidc.RoleAdmin},
		{Value: "other-team", Role: writoidc.RoleMember},
	}
	role, ok := auth.PreviewRoleAgainst(before, nil, []string{"admins"}, "admin@corp.example")
	if !ok || role != writoidc.RoleAdmin {
		t.Fatalf("before delete: role = %q, ok = %v, want (%q, true)", role, ok, writoidc.RoleAdmin)
	}

	// The candidate rows AFTER deleting the "admins" row — what the guard
	// would build before actually performing the delete. "other-team"
	// survives, so the merged map stays non-empty (arm 2) — this caller's
	// "admins" group now matches nothing in it, and no DefaultRole is set.
	after := []writoidc.RoleMapping{{Value: "other-team", Role: writoidc.RoleMember}}
	role, ok = auth.PreviewRoleAgainst(after, nil, []string{"admins"}, "admin@corp.example")
	if ok {
		t.Fatalf("after delete: role = %q, ok = %v, want ok=false (this admin would no longer derive any role — the lockout the guard exists to catch)", role, ok)
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

// TestHasEmailDomainsAccessor: the console's EMAIL_KEY badge copy needs to
// tell "no domains list configured" apart from "configured" (A-4) — pinned
// against env.newAuth's own allowedDomains param, not newRoleAuth (which
// deliberately omits domain restriction, see its doc comment).
func TestHasEmailDomainsAccessor(t *testing.T) {
	env := newIdPEnv(t)

	unset := env.newAuth(t, nil)
	if unset.HasEmailDomains() {
		t.Error("HasEmailDomains() = true, want false (no AllowedEmailDomains configured)")
	}

	set := env.newAuth(t, []string{"corp.example"})
	if !set.HasEmailDomains() {
		t.Error("HasEmailDomains() = false, want true")
	}
}

// TestMergedMapEmptyAccountsForShadowing (A-5): a stored row that collides
// with the operator allowlist is SHADOWED (mergeRoleMaps drops it,
// contributing nothing) — MergedMapEmpty must report the real merged-map
// emptiness, not a raw row count, or the console's posture display and write
// guards diverge from what deriveRole actually sees. A chart collision is
// the contrasting case: the chart's OWN entry keeps the map non-empty
// regardless of the shadowed row, so it can never produce the "merged empty,
// len(rows)>0" state this fix is actually about.
func TestMergedMapEmptyAccountsForShadowing(t *testing.T) {
	env := newIdPEnv(t)

	// Empty chart, zero rows: genuinely empty.
	empty := env.newRoleAuth(t, nil, "", nil)
	if !empty.MergedMapEmpty(nil) {
		t.Error("MergedMapEmpty(nil) = false, want true (no chart, no rows)")
	}

	// One row that collides with a NON-EMPTY chart: shadowed, contributes
	// nothing on its own — but the chart's OWN entry (chart always wins the
	// collision, by construction) already keeps the merged map non-empty
	// regardless. A chart collision can therefore never reproduce "merged
	// empty, len(rows)==1" — only an operator-allowlist collision can (below,
	// nothing about the allowlist ever enters the map itself).
	shadowedByChart := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleAdmin}, "", nil)
	rows := []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleMember}}
	if shadowedByChart.MergedMapEmpty(rows) {
		t.Error("MergedMapEmpty(rows) = true, want false (the chart's own entry keeps the map non-empty)")
	}

	// One row, but it collides with the operator allowlist: the map really
	// IS empty — len(rows)==1 but MergedMapEmpty must report true, the exact
	// divergence A-5 fixes (a raw row count would have said false here).
	shadowedByAllowlist := env.newRoleAuth(t, nil, "", []string{"ops@corp.example"})
	opsRow := []writoidc.RoleMapping{{Value: "ops@corp.example", Role: writoidc.RoleMember}}
	if !shadowedByAllowlist.MergedMapEmpty(opsRow) {
		t.Error("MergedMapEmpty(opsRow) = false, want true (the only row is shadowed by the operator allowlist)")
	}

	// A genuinely unshadowed row makes the map non-empty.
	nonEmpty := env.newRoleAuth(t, nil, "", nil)
	if nonEmpty.MergedMapEmpty([]writoidc.RoleMapping{{Value: "design-team", Role: writoidc.RoleMember}}) {
		t.Error("MergedMapEmpty([design-team]) = true, want false (unshadowed row)")
	}
}
