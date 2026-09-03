// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the permissioning CRUD surface ──────────────────────────────────────────
//
// permissions.go is the ONLY write boundary for capability_grants +
// capability_enforcement, and until this file nothing pinned it: the resolver
// had capabilities_test.go, the seams had capability_seams_test.go, and the
// routes had authz_test.go's admit/refuse matrix — but what the handlers
// ACCEPT was uncovered, which is where a fail-closed validator quietly stops
// being fail-closed.

// permStore is capStore (the resolver double) plus the four writes, with the
// PG store's OWN natural-key semantics: an upsert on an existing
// (subject_type, subject, capability, value) returns THAT row's id, which is
// exactly how the handler tells a create (201) from a re-grant (200).
type permStore struct{ *capStore }

func (s *permStore) UpsertCapabilityGrant(_ context.Context, g types.CapabilityGrant) (types.CapabilityGrant, error) {
	for i, ex := range s.grants {
		if ex.SubjectType == g.SubjectType && ex.Subject == g.Subject &&
			ex.Capability == g.Capability && ex.Value == g.Value {
			s.grants[i].Effect = g.Effect
			return s.grants[i], nil
		}
	}
	s.grants = append(s.grants, g)
	return g, nil
}

func (s *permStore) DeleteCapabilityGrant(_ context.Context, id uuid.UUID) error {
	for i, ex := range s.grants {
		if ex.ID == id {
			s.grants = slices.Delete(s.grants, i, i+1)
			return nil
		}
	}
	return store.ErrNotFound
}

func (s *permStore) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	return s.grants, nil
}

// PutCapabilityEnforcement REPLACES the map, exactly as the PG statement does —
// an omitted key is a real "turn it off", which is the round-trip the handler's
// doc comment stakes its upgrade story on.
func (s *permStore) PutCapabilityEnforcement(_ context.Context, enabled map[string]bool) (map[string]bool, error) {
	s.enf = maps.Clone(enabled)
	if s.enf == nil {
		s.enf = map[string]bool{}
	}
	return s.enf, nil
}

// permServer builds an OIDC-configured server over permStore, so admin and
// member sessions drive the REAL operatorOnly gate through the router.
func permServer(t *testing.T) (*Server, *permStore) {
	t.Helper()
	st := &permStore{capStore: &capStore{}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), st
}

func permAdmin(t *testing.T) *http.Cookie {
	t.Helper()
	return ssoSession(t, "sub-perm-admin", "admin@corp.example", oidc.RoleAdmin)
}

func permMember(t *testing.T) *http.Cookie {
	t.Helper()
	return ssoSession(t, "sub-perm-member", "dev@corp.example", oidc.RoleMember)
}

// ─── validateCapabilityGrant ─────────────────────────────────────────────────

// TestValidateCapabilityGrant is the write-boundary matrix. Each rejected row
// is a grant that would otherwise be STORED INERT — a deny that protects
// nothing reads identical to a deny that works, on the one screen an operator
// checks after a breach.
func TestValidateCapabilityGrant(t *testing.T) {
	base := func() types.CapabilityGrant {
		return types.CapabilityGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: "dev@corp.example",
			Capability: capSecret, Value: "prod-db", Effect: types.CapabilityAllow,
		}
	}
	tests := []struct {
		name    string
		mutate  func(*types.CapabilityGrant)
		wantErr string // substring; "" means accepted
	}{
		{"valid user grant", func(*types.CapabilityGrant) {}, ""},
		{"unknown subject_type", func(g *types.CapabilityGrant) { g.SubjectType = "everyone" }, "subject_type"},
		{"unknown kind", func(g *types.CapabilityGrant) { g.Capability = "devcontainer_repo" }, "capability"},
		{"unknown effect", func(g *types.CapabilityGrant) { g.Effect = "maybe" }, "effect"},
		{"empty value", func(g *types.CapabilityGrant) { g.Value = "   " }, "value: required"},
		{"oversized value", func(g *types.CapabilityGrant) { g.Value = strings.Repeat("x", maxCapabilityGrantFieldLen+1) }, "value: invalid"},
		{"control char in value", func(g *types.CapabilityGrant) { g.Value = "prod\ndb" }, "value: invalid"},
		{"empty subject on a user grant", func(g *types.CapabilityGrant) { g.Subject = " " }, "subject: required"},
		{"empty subject on a group grant", func(g *types.CapabilityGrant) {
			g.SubjectType, g.Subject = types.CapabilitySubjectGroup, ""
		}, "subject: required"},
		{"control char in subject", func(g *types.CapabilityGrant) { g.Subject = "dev\x00@corp" }, "subject: invalid"},
		// DEL and the C1 range are controls too. C1 is the arm the hand-rolled
		// loop used to MISS (it only tested r < 0x20 || r == 0x7f);
		// unicode.IsControl covers U+0080–U+009F, matching the stance
		// repoFieldSafe already takes (repoclone_test.go rejects a NEL).
		{"DEL in value", func(g *types.CapabilityGrant) { g.Value = "prod\x7fdb" }, "value: invalid"},
		{"C1 control in value", func(g *types.CapabilityGrant) { g.Value = "prod\u0085db" }, "value: invalid"},
		{"C1 control in subject", func(g *types.CapabilityGrant) { g.Subject = "dev\u009f@corp" }, "subject: invalid"},

		// egress_host values are hosts, so they get the proxy's own entry
		// shape check — the same one every allowed_domains ingest runs.
		{"egress: mid-label wildcard", func(g *types.CapabilityGrant) {
			g.Capability, g.Value = capEgressHost, "*example.com"
		}, "never matches"},
		{"egress: URL, not a host", func(g *types.CapabilityGrant) {
			g.Capability, g.Value = capEgressHost, "https://example.com/x"
		}, "never matches"},
		{"egress: malformed port", func(g *types.CapabilityGrant) {
			g.Capability, g.Value = capEgressHost, "example.com:0"
		}, "never matches"},
		{"egress: leading wildcard is fine", func(g *types.CapabilityGrant) {
			g.Capability, g.Value = capEgressHost, "*.example.com"
		}, ""},
		{"egress: port qualifier is fine", func(g *types.CapabilityGrant) {
			g.Capability, g.Value = capEgressHost, "example.com:443"
		}, ""},
		{"egress: the kind wildcard is not a domain", func(g *types.CapabilityGrant) {
			g.Capability, g.Value = capEgressHost, capWildcard
		}, ""},

		// A GROUP subject is matched by exact equality against the login-time
		// snapshot, which carries printable ASCII only — so a subject that
		// snapshot can never produce is a stored row that matches nobody: the
		// SUBJECT half of the failure the egress_host arms above close on the
		// value half. See TestGroupSubjectWriteBoundariesShareTheSnapshotRule
		// for the shared-helper pin across both 0.7 tables.
		{"group subject with a non-ASCII name", func(g *types.CapabilityGrant) {
			g.SubjectType, g.Subject = types.CapabilitySubjectGroup, "Entwickler-Büro"
		}, "printable ASCII"},
		{"group subject that folds onto an ASCII group", func(g *types.CapabilityGrant) {
			g.SubjectType, g.Subject = types.CapabilitySubjectGroup, "\u212Aubernetes-admins"
		}, "printable ASCII"},
		{"group subject that is plain ASCII is fine", func(g *types.CapabilityGrant) {
			g.SubjectType, g.Subject = types.CapabilitySubjectGroup, "  Eng-Team "
		}, ""},
		// A USER subject is a sub/email, normalized by capabilitySubjects with
		// a bare ToLower — the group rule must not leak onto it.
		{"user subject is not held to the group rule", func(g *types.CapabilityGrant) {
			g.SubjectType, g.Subject = types.CapabilitySubjectUser, "renée@corp.example"
		}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := base()
			tc.mutate(&g)
			err := validateCapabilityGrant(&g)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateCapabilityGrant = %v, want accepted", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validateCapabilityGrant accepted %+v, want error containing %q", g, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("validateCapabilityGrant = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateCapabilityGrantNormalizes: the two normalizations the resolver
// DEPENDS on. capabilitySubjects lowercases the caller's own sub/email, so a
// grant written "Alice@Corp.com" has to be lowercased here or it never hits;
// and the "all" subject_type carries no subject at all (migration 0042), so a
// caller-supplied one is dropped rather than stored.
func TestValidateCapabilityGrantNormalizes(t *testing.T) {
	g := types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: "  Alice@Corp.Example ",
		Capability: capSecret, Value: "  prod-db  ", Effect: types.CapabilityAllow,
	}
	if err := validateCapabilityGrant(&g); err != nil {
		t.Fatalf("validateCapabilityGrant: %v", err)
	}
	if g.Subject != "alice@corp.example" || g.Value != "prod-db" {
		t.Fatalf("normalized to subject %q value %q", g.Subject, g.Value)
	}

	all := types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectAll, Subject: "everyone-ish",
		Capability: capImage, Value: "ghcr.io/x:1", Effect: types.CapabilityAllow,
	}
	if err := validateCapabilityGrant(&all); err != nil {
		t.Fatalf("validateCapabilityGrant(all): %v", err)
	}
	if all.Subject != "" {
		t.Fatalf("subject = %q, want blanked for subject_type=all", all.Subject)
	}
}

// ─── the handlers, through the router ────────────────────────────────────────

// TestUpsertCapabilityGrantCreatedThenUpdated: a new natural key is 201, the
// SAME key again is 200 with the original row's id — the distinction the
// console's DUPLICATE copy is written against.
func TestUpsertCapabilityGrantCreatedThenUpdated(t *testing.T) {
	srv, st := permServer(t)
	admin := permAdmin(t)
	body := `{"subject_type":"group","subject":"Eng","capability":"egress_host","value":"*.example.com","effect":"allow"}`

	w := doSSO(t, srv, http.MethodPost, "/api/v1/permissions/grants", admin, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("first POST = %d, want 201: %s", w.Code, w.Body.String())
	}
	var created types.CapabilityGrant
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Subject != "eng" {
		t.Fatalf("subject = %q, want lowercased", created.Subject)
	}

	// Same key, flipped effect: an UPDATE in place, never a second
	// contradictory row.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/permissions/grants", admin,
		`{"subject_type":"group","subject":"eng","capability":"egress_host","value":"*.example.com","effect":"deny"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-grant = %d, want 200: %s", w.Code, w.Body.String())
	}
	var updated types.CapabilityGrant
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.ID != created.ID || updated.Effect != types.CapabilityDeny {
		t.Fatalf("re-grant = %+v, want id %s flipped to deny", updated, created.ID)
	}
	if len(st.grants) != 1 {
		t.Fatalf("stored %d rows, want 1", len(st.grants))
	}

	// A shape the proxy could never match is refused at the boundary, not
	// stored inert.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/permissions/grants", admin,
		`{"subject_type":"group","subject":"eng","capability":"egress_host","value":"*example.com","effect":"allow"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad egress shape = %d, want 400: %s", w.Code, w.Body.String())
	}
	if len(st.grants) != 1 {
		t.Fatalf("stored %d rows after the rejected write, want 1", len(st.grants))
	}
}

// TestDeleteCapabilityGrant: 204 for a row that existed, 404 for one that never
// did (an admin already sees the whole table, so there is no existence oracle
// to protect here).
func TestDeleteCapabilityGrant(t *testing.T) {
	srv, st := permServer(t)
	admin := permAdmin(t)
	row := grant(types.CapabilitySubjectAll, "", capSecret, "prod-db", types.CapabilityAllow)
	st.grants = []types.CapabilityGrant{row}

	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/permissions/grants/"+row.ID.String(), admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body.String())
	}
	if len(st.grants) != 0 {
		t.Fatalf("row survived the delete: %+v", st.grants)
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/permissions/grants/"+row.ID.String(), admin, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404: %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/permissions/grants/not-a-uuid", admin, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed id = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestPutCapabilityEnforcementReplaces: the PUT is a FULL REPLACE, so a key the
// body omits comes back off. That is the whole contract the console's switch
// board round-trips through, and an accidental merge here would make a switch
// impossible to turn back off.
func TestPutCapabilityEnforcementReplaces(t *testing.T) {
	srv, st := permServer(t)
	admin := permAdmin(t)

	w := doSSO(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", admin,
		`{"egress_host":true,"secret":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !st.enf[capEgressHost] || !st.enf[capSecret] {
		t.Fatalf("enforcement = %v, want both on", st.enf)
	}

	// secret omitted: it is OFF again, not carried over.
	w = doSSO(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", admin, `{"egress_host":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("replace = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got[capSecret] || !got[capEgressHost] {
		t.Fatalf("enforcement = %v, want only egress_host", got)
	}

	// An unknown kind is refused rather than stored as a switch nothing reads.
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", admin, `{"devcontainer_repo":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !st.enf[capEgressHost] || len(st.enf) != 1 {
		t.Fatalf("enforcement = %v after the rejected write, want the previous state untouched", st.enf)
	}
}

// doSSOIfMatch is doSSO (rbac_test.go) plus an If-Match header.
func doSSOIfMatch(t *testing.T, srv *Server, method, path string, cookie *http.Cookie, ifMatch, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// TestPutCapabilityEnforcementIfMatch is the #7 optimistic-concurrency
// contract on this endpoint's own document (the enforcement map, distinct
// from the grant table GET /permissions also returns): no If-Match keeps
// working, a stale one 412s before the store is touched, and a fresh one
// (re-read via GET /permissions) succeeds.
func TestPutCapabilityEnforcementIfMatch(t *testing.T) {
	srv, st := permServer(t)
	admin := permAdmin(t)
	st.enf = map[string]bool{capEgressHost: true}

	get := doSSO(t, srv, http.MethodGet, "/api/v1/permissions", admin, "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET /permissions = %d: %s", get.Code, get.Body.String())
	}
	etag := get.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET /permissions did not set an ETag")
	}

	stale := `"0000000000000000000000000000000000000000000000000000000000000000"`
	w := doSSOIfMatch(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", admin, stale, `{"secret":true}`)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: code = %d, want 412: %s", w.Code, w.Body.String())
	}
	if st.enf[capSecret] {
		t.Fatal("a refused If-Match must never reach the store")
	}

	w = doSSOIfMatch(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", admin, etag, `{"secret":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("fresh If-Match: code = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !st.enf[capSecret] {
		t.Fatal("fresh If-Match write did not reach the store")
	}
	if got := w.Header().Get("ETag"); got == "" || got == etag {
		t.Fatalf("PUT ETag = %q, want a NEW value distinct from the pre-write one %q", got, etag)
	}

	// No If-Match at all: unconditional, exactly as before this feature.
	w = doSSO(t, srv, http.MethodPut, "/api/v1/permissions/enforcement", admin, `{"secret":true,"egress_host":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("no If-Match: code = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestPermissionsWritesRefuseMembers: this table bounds every member's blast
// radius, so a member may not read OR write it — their own effective set is
// GET /me/capabilities, which stays open to them.
func TestPermissionsWritesRefuseMembers(t *testing.T) {
	srv, st := permServer(t)
	member := permMember(t)
	row := grant(types.CapabilitySubjectAll, "", capSecret, "prod-db", types.CapabilityAllow)
	st.grants = []types.CapabilityGrant{row}

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/permissions", ""},
		{http.MethodPost, "/api/v1/permissions/grants", `{"subject_type":"all","capability":"secret","value":"prod-db","effect":"allow"}`},
		{http.MethodDelete, "/api/v1/permissions/grants/" + row.ID.String(), ""},
		{http.MethodPut, "/api/v1/permissions/enforcement", `{"secret":true}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if w := doSSO(t, srv, tc.method, tc.path, member, tc.body); w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
			}
		})
	}
	if len(st.grants) != 1 || len(st.enf) != 0 {
		t.Fatalf("a refused member still moved state: grants=%v enf=%v", st.grants, st.enf)
	}
	// The member-safe twin still answers.
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/me/capabilities", member, ""); w.Code != http.StatusOK {
		t.Fatalf("GET /me/capabilities = %d, want 200: %s", w.Code, w.Body.String())
	}
}
