// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// permissionsDoc is GET /permissions' body (api.permissionsResponse is
// unexported). GET /me/capabilities returns a superset of these two fields;
// the extra members it carries are asserted separately below.
type permissionsDoc struct {
	Grants      []types.CapabilityGrant `json:"grants"`
	Enforcement map[string]bool         `json:"enforcement"`
}

// TestPermissions_AdminRoundTrip drives the whole permissioning surface against
// the REAL booted server with the admin bearer (stage A-C's third verify
// criterion). pkg/client has no permissions methods, so this goes over raw HTTP
// (doAdmin) rather than growing the SDK for one test:
//
//	POST /permissions/grants        201 on a new natural key, 200 on re-grant
//	                                (same row id, effect flipped in place)
//	GET  /permissions               the admin table + enforcement map
//	PUT  /permissions/enforcement   full-map replace, unknown kind rejected
//	GET  /me/capabilities           the caller's OWN set (the `all` row, not
//	                                the other subject's user row)
//	DELETE /permissions/grants/{id} 204, then 404 for the same id
func TestPermissions_AdminRoundTrip(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	perms := h.srv.URL + "/api/v1/permissions"
	grants := perms + "/grants"

	// The enforcement map is deployment-global and this PUT replaces it whole,
	// so snapshot it first and put it back — the shared apie2e database outlives
	// this test.
	baseline := getPermissions(t, perms)
	t.Cleanup(func() {
		body, _ := json.Marshal(baseline.Enforcement)
		doAdmin(t, http.MethodPut, perms+"/enforcement", body)
	})

	// ─── POST: 201 on a genuinely new natural key ────────────────────────────
	// Subject is deliberately mixed-case: the handler lowercases it so the row
	// matches the lowercased sub/email the resolver compares against.
	subject := "Alice+" + uuid.NewString() + "@Corp.example"
	host := "apie2e-" + uuid.NewString() + ".example.com"
	created := postGrant(t, grants, http.StatusCreated, map[string]any{
		"subject_type": "user", "subject": subject,
		"capability": "egress_host", "value": host, "effect": "allow",
	})
	t.Cleanup(func() { doAdmin(t, http.MethodDelete, grants+"/"+created.ID.String(), nil) })
	if created.ID == uuid.Nil {
		t.Fatalf("created grant has nil id: %+v", created)
	}
	if want := strings.ToLower(subject); created.Subject != want {
		t.Errorf("created subject = %q, want lowercased %q", created.Subject, want)
	}
	if created.Effect != types.CapabilityAllow || created.CreatedBy == "" {
		t.Errorf("created grant = %+v (want allow + server-assigned created_by)", created)
	}

	// ─── POST again, same natural key: 200 and the SAME row, effect flipped ──
	regranted := postGrant(t, grants, http.StatusOK, map[string]any{
		"subject_type": "user", "subject": subject,
		"capability": "egress_host", "value": host, "effect": "deny",
	})
	if regranted.ID != created.ID {
		t.Errorf("re-grant id = %s, want the existing row %s", regranted.ID, created.ID)
	}
	if regranted.Effect != types.CapabilityDeny {
		t.Errorf("re-grant effect = %q, want deny (flipped in place)", regranted.Effect)
	}

	// An `all` row: this one DOES describe the caller, so /me/capabilities must
	// return it while the user row above stays invisible there.
	secret := "apie2e-secret-" + uuid.NewString()
	allGrant := postGrant(t, grants, http.StatusCreated, map[string]any{
		"subject_type": "all", "subject": "ignored-for-all",
		"capability": "secret", "value": secret, "effect": "allow",
	})
	t.Cleanup(func() { doAdmin(t, http.MethodDelete, grants+"/"+allGrant.ID.String(), nil) })
	if allGrant.Subject != "" {
		t.Errorf("all-grant subject = %q, want dropped to empty", allGrant.Subject)
	}

	// A closed-set violation is rejected at the write boundary (the schema
	// deliberately carries no CHECK on capability).
	status, raw := doAdmin(t, http.MethodPost, grants, mustMarshal(map[string]any{
		"subject_type": "all", "capability": "devcontainer_repo", "value": "*", "effect": "allow",
	}))
	if status != http.StatusBadRequest {
		t.Errorf("POST grant with unknown kind status = %d, want 400 (body=%s)", status, raw)
	}

	// ─── GET /permissions: the admin table carries both rows ─────────────────
	all := getPermissions(t, perms)
	if !slices.ContainsFunc(all.Grants, func(g types.CapabilityGrant) bool {
		return g.ID == created.ID && g.Effect == types.CapabilityDeny
	}) {
		t.Errorf("GET /permissions missing the re-granted deny row %s", created.ID)
	}
	if !slices.ContainsFunc(all.Grants, func(g types.CapabilityGrant) bool { return g.ID == allGrant.ID }) {
		t.Errorf("GET /permissions missing the all-row %s", allGrant.ID)
	}

	// ─── PUT /permissions/enforcement ────────────────────────────────────────
	status, raw = doAdmin(t, http.MethodPut, perms+"/enforcement",
		mustMarshal(map[string]bool{"egress_host": true, "secret": false}))
	if status != http.StatusOK {
		t.Fatalf("PUT /permissions/enforcement status = %d, want 200 (body=%s)", status, raw)
	}
	var enf map[string]bool
	if err := json.Unmarshal(raw, &enf); err != nil {
		t.Fatalf("decode enforcement: %v (body=%s)", err, raw)
	}
	if !enf["egress_host"] || enf["secret"] {
		t.Errorf("enforcement echo = %v, want egress_host on / secret off", enf)
	}
	if got := getPermissions(t, perms).Enforcement; !got["egress_host"] {
		t.Errorf("enforcement did not persist: %v", got)
	}
	status, raw = doAdmin(t, http.MethodPut, perms+"/enforcement",
		mustMarshal(map[string]bool{"devcontainer_repo": true}))
	if status != http.StatusBadRequest {
		t.Errorf("PUT enforcement with unknown kind status = %d, want 400 (body=%s)", status, raw)
	}

	// ─── GET /me/capabilities: the caller's OWN set ──────────────────────────
	status, raw = doAdmin(t, http.MethodGet, h.srv.URL+"/api/v1/me/capabilities", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /me/capabilities status = %d, want 200 (body=%s)", status, raw)
	}
	var me struct {
		permissionsDoc
		SessionGroups       []string `json:"session_groups"`
		GroupsSnapshotStale bool     `json:"groups_snapshot_stale"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		t.Fatalf("decode /me/capabilities: %v (body=%s)", err, raw)
	}
	if !slices.ContainsFunc(me.Grants, func(g types.CapabilityGrant) bool { return g.ID == allGrant.ID }) {
		t.Errorf("/me/capabilities missing the all-row %s: %+v", allGrant.ID, me.Grants)
	}
	if slices.ContainsFunc(me.Grants, func(g types.CapabilityGrant) bool { return g.ID == created.ID }) {
		t.Errorf("/me/capabilities leaked another subject's user row %s", created.ID)
	}
	if !me.Enforcement["egress_host"] {
		t.Errorf("/me/capabilities enforcement = %v, want egress_host on", me.Enforcement)
	}
	// An admin-token caller carries no OIDC session, so the group snapshot reads
	// as "can't tell yet" rather than as an empty group list.
	if !me.GroupsSnapshotStale || len(me.SessionGroups) != 0 {
		t.Errorf("/me/capabilities groups = %v stale = %v, want none + stale for a token caller",
			me.SessionGroups, me.GroupsSnapshotStale)
	}

	// ─── DELETE: 204, then 404 for the same id ───────────────────────────────
	if status, raw = doAdmin(t, http.MethodDelete, grants+"/"+created.ID.String(), nil); status != http.StatusNoContent {
		t.Fatalf("DELETE grant status = %d, want 204 (body=%s)", status, raw)
	}
	if status, raw = doAdmin(t, http.MethodDelete, grants+"/"+created.ID.String(), nil); status != http.StatusNotFound {
		t.Errorf("DELETE grant (already gone) status = %d, want 404 (body=%s)", status, raw)
	}
	if after := getPermissions(t, perms); slices.ContainsFunc(after.Grants,
		func(g types.CapabilityGrant) bool { return g.ID == created.ID }) {
		t.Errorf("deleted grant %s still listed", created.ID)
	}
}

// getPermissions GETs the admin permissions document.
func getPermissions(t *testing.T, url string) permissionsDoc {
	t.Helper()
	status, raw := doAdmin(t, http.MethodGet, url, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /permissions status = %d, want 200 (body=%s)", status, raw)
	}
	var doc permissionsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode permissions: %v (body=%s)", err, raw)
	}
	return doc
}

// postGrant POSTs one grant body, asserts the status, and returns the saved row.
func postGrant(t *testing.T, url string, want int, body map[string]any) types.CapabilityGrant {
	t.Helper()
	status, raw := doAdmin(t, http.MethodPost, url, mustMarshal(body))
	if status != want {
		t.Fatalf("POST %v status = %d, want %d (body=%s)", body, status, want, raw)
	}
	var g types.CapabilityGrant
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("decode grant: %v (body=%s)", err, raw)
	}
	return g
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
