// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCapExplainDefaults: a kind with no row at all for a subject (or "all")
// reports the kind's own direction default — Everyone for the six narrowing
// kinds, Admins only for the one widening kind (image) — exactly the posture
// capBatch.decide falls back to on an absent row.
func TestCapExplainDefaults(t *testing.T) {
	got := capExplain(nil, types.CapabilitySubjectGroup, "eng", capabilityKinds)
	if len(got) != len(capabilityKinds) {
		t.Fatalf("capExplain(no grants) = %d rows, want one per kind (%d)", len(got), len(capabilityKinds))
	}
	for _, row := range got {
		want := capExplainEveryone
		if capKinds[row.Kind].direction == capWidening {
			want = capExplainAdminsOnly
		}
		if row.Value != capWildcard || row.State != want {
			t.Errorf("%s: got {%s %s}, want {%s %s}", row.Kind, row.Value, row.State, capWildcard, want)
		}
	}
}

// TestCapExplainWildcardRows: a blanket allow or deny for the subject (or
// "all") replaces the synthesized default with the real row — This type or
// Blocked — and is not duplicated.
func TestCapExplainWildcardRows(t *testing.T) {
	tests := []struct {
		name  string
		grant types.CapabilityGrant
		want  capExplainState
	}{
		{"subject wildcard allow", grant(types.CapabilitySubjectGroup, "eng", capSecret, capWildcard, types.CapabilityAllow), capExplainThisType},
		{"subject wildcard deny", grant(types.CapabilitySubjectGroup, "eng", capSecret, capWildcard, types.CapabilityDeny), capExplainBlocked},
		{"all wildcard allow", grant(types.CapabilitySubjectAll, "", capSecret, capWildcard, types.CapabilityAllow), capExplainThisType},
		{"all wildcard deny", grant(types.CapabilitySubjectAll, "", capSecret, capWildcard, types.CapabilityDeny), capExplainBlocked},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := capExplain([]types.CapabilityGrant{tc.grant}, types.CapabilitySubjectGroup, "eng", []string{capSecret})
			if len(rows) != 1 || rows[0].Value != capWildcard || rows[0].State != tc.want {
				t.Fatalf("capExplain = %+v, want one row {%s %s %s}", rows, capSecret, capWildcard, tc.want)
			}
		})
	}
}

// TestCapExplainPerValueRows: a specific-value grant renders as its own row
// alongside the synthesized default, at the exact value an admin wrote —
// "Available to" granularity, not kind granularity.
func TestCapExplainPerValueRows(t *testing.T) {
	grants := []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capWorkspace, "ws-allowed", types.CapabilityAllow),
		grant(types.CapabilitySubjectGroup, "eng", capWorkspace, "ws-blocked", types.CapabilityDeny),
		grant(types.CapabilitySubjectUser, "someone-else", capWorkspace, "ws-not-mine", types.CapabilityAllow),
	}
	rows := capExplain(grants, types.CapabilitySubjectGroup, "eng", []string{capWorkspace})
	want := map[string]capExplainState{
		capWildcard:  capExplainEveryone,
		"ws-allowed": capExplainThisType,
		"ws-blocked": capExplainBlocked,
	}
	if len(rows) != len(want) {
		t.Fatalf("capExplain = %+v, want %d rows (a grant on another subject must not appear)", rows, len(want))
	}
	for _, row := range rows {
		if got, ok := want[row.Value]; !ok || got != row.State {
			t.Errorf("value %q: state = %s, want %s (ok=%v)", row.Value, row.State, want[row.Value], ok)
		}
	}
}

// TestCapExplainDenyWinsAcrossSubjects: a subject-scoped allow and an
// "all"-scoped deny on the SAME value disagree only because they come from
// different subject scopes (the natural key forbids two rows for one
// subject); deny wins, exactly as capBatch.scan reads two subjects for one
// caller — no user-over-group, no type-over-all precedence.
func TestCapExplainDenyWinsAcrossSubjects(t *testing.T) {
	grants := []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capEgressHost, "api.example.com", types.CapabilityAllow),
		grant(types.CapabilitySubjectAll, "", capEgressHost, "api.example.com", types.CapabilityDeny),
	}
	for _, order := range [][]types.CapabilityGrant{grants, {grants[1], grants[0]}} {
		rows := capExplain(order, types.CapabilitySubjectGroup, "eng", []string{capEgressHost})
		var got capExplainState
		for _, r := range rows {
			if r.Value == "api.example.com" {
				got = r.State
			}
		}
		if got != capExplainBlocked {
			t.Fatalf("deny-wins order %v: state = %s, want %s", order, got, capExplainBlocked)
		}
	}
}

// TestCapExplainWideningNoAllow: image (the one widening kind) with no allow
// row for this subject stays Admins only even when the subject holds a DENY
// at some other value — the default reflects "no grant", not "a grant of the
// opposite effect".
func TestCapExplainWideningNoAllow(t *testing.T) {
	grants := []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capImage, "ghcr.io/evil/image", types.CapabilityDeny),
	}
	rows := capExplain(grants, types.CapabilitySubjectGroup, "eng", []string{capImage})
	for _, r := range rows {
		if r.Value == capWildcard && r.State != capExplainAdminsOnly {
			t.Errorf("wildcard row = %s, want %s", r.State, capExplainAdminsOnly)
		}
	}
}

// TestCapExplainUnknownKindSkipped: an unknown kind name is dropped rather
// than panicking or producing a row — the HTTP handler is what validates
// kinds and refuses the request; the pure function stays total.
func TestCapExplainUnknownKindSkipped(t *testing.T) {
	rows := capExplain(nil, types.CapabilitySubjectGroup, "eng", []string{"no_such_kind"})
	if len(rows) != 0 {
		t.Fatalf("capExplain(unknown kind) = %+v, want no rows", rows)
	}
}

// ─── HTTP surface ────────────────────────────────────────────────────────────

// TestHandleExplainCapabilities: securityOps only, validates its query, and
// returns kinds_version alongside the grid so a client can tell its own copy
// of the kind table is stale (the same field GET /me/capabilities gained in
// #737).
func TestHandleExplainCapabilities(t *testing.T) {
	srv, st := permServer(t)
	admin := permAdmin(t)
	st.grants = []types.CapabilityGrant{
		grant(types.CapabilitySubjectGroup, "eng", capSecret, "prod-db", types.CapabilityDeny),
	}

	w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain?subject_type=group&subject=eng&kinds=secret", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /permissions/explain = %d: %s", w.Code, w.Body.String())
	}
	var body explainResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	if body.KindsVersion != capKindsVersion {
		t.Errorf("kinds_version = %d, want %d", body.KindsVersion, capKindsVersion)
	}
	if len(body.Rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (the default + the deny)", body.Rows)
	}

	member := permMember(t)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain?subject_type=group&subject=eng", member, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member: status = %d, want 403: %s", w.Code, w.Body.String())
	}

	for _, tc := range []struct {
		name, query string
	}{
		{"missing subject_type", "?subject=eng"},
		{"bad subject_type", "?subject_type=all&subject=x"},
		{"missing subject", "?subject_type=group"},
		{"unknown kind", "?subject_type=group&subject=eng&kinds=no_such_kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain"+tc.query, admin, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}

	// user_type is accepted even though no grant can name one yet (UT-3):
	// Explain answers the defaults, not a refusal.
	w = doSSO(t, srv, http.MethodGet, "/api/v1/permissions/explain?subject_type=user_type&subject=portfolio-manager", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("user_type subject: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}
