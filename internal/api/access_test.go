// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Tests for access.go's People-step CRUD (Phase 2 lane A, migration 0051).
// The member/admin/unauthenticated admit-refuse boundary for all four routes
// is already pinned generically by authz_test.go's routeMatrix (classAdmin)
// and rbac_test.go's gatedRoutes — this file covers what is UNIQUE to
// access.go: canonicalization, the collision check, the posture-flip and
// lockout guards, the preview endpoint, and the 503-unconfigured posture.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	gooidctest "github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the store double ───────────────────────────────────────────────────────

// roleMapStore holds role_mappings rows in memory, mirroring permStore's
// shape for capability grants: UpsertRoleMapping flips an existing (value)
// row in place (the store's own natural-key UNIQUE), DeleteRoleMapping is
// ErrNotFound on a miss.
type roleMapStore struct {
	store.Store
	rows    []types.RoleMapping
	listErr error
}

func (s *roleMapStore) ListRoleMappings(context.Context) ([]types.RoleMapping, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return slices.Clone(s.rows), nil
}

func (s *roleMapStore) UpsertRoleMapping(_ context.Context, m types.RoleMapping) (types.RoleMapping, error) {
	for i, ex := range s.rows {
		if ex.Value == m.Value {
			s.rows[i].Role = m.Role
			s.rows[i].CreatedBy = m.CreatedBy
			return s.rows[i], nil
		}
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	s.rows = append(s.rows, m)
	return m, nil
}

func (s *roleMapStore) DeleteRoleMapping(_ context.Context, id uuid.UUID) error {
	for i, ex := range s.rows {
		if ex.ID == id {
			s.rows = slices.Delete(s.rows, i, i+1)
			return nil
		}
	}
	return store.ErrNotFound
}

// accessOIDCBridge adapts a *roleMapStore to oidc.RoleMappingSource — the
// exact conversion cmd/wardynd's pgRoleMappings performs in production, so
// PreviewRole (which reads Config.RoleMappings) sees the SAME rows
// access.go's own handlers read via s.cfg.Store.
type accessOIDCBridge struct{ st *roleMapStore }

func (b accessOIDCBridge) ListRoleMappings(ctx context.Context) ([]oidc.RoleMapping, error) {
	rows, err := b.st.ListRoleMappings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]oidc.RoleMapping, len(rows))
	for i, m := range rows {
		out[i] = oidc.RoleMapping{Value: m.Value, Role: m.Role}
	}
	return out, nil
}

// ─── a REAL *oidc.Authenticator, not the zero-value stub every other API
// test uses ───────────────────────────────────────────────────────────────
//
// Every other internal/api test authenticates through a session cookie whose
// ROLE the router trusts directly (ssoSession) — it never needs the
// Authenticator's own derivation. access.go's handlers call INSTANCE METHODS
// (ChartRoleMap, HasOperatorEmails, DefaultRole, IsOperatorEmail,
// PreviewRole/PreviewRoleAgainst) that read the Authenticator's private
// Config, which a zero-value &oidc.Authenticator{} can never carry (that
// field has no exported setter — oidc.New is the only constructor). A fake
// discovery server (no ID-token signing needed; nothing here performs a
// login) is the smallest way to get a REAL Config wired.

// accessTestHMACKey signs every session cookie accessSession mints — 32
// bytes, oidc.New's minimum, distinct from other test files' testHMACKey so
// nothing here can accidentally validate against a fixture from another file.
var accessTestHMACKey = []byte("access-test-hmac-key-32-bytes!!!")

// newAccessAuth builds a real *oidc.Authenticator against a throwaway fake
// discovery server. st, when non-nil, is wired as Config.RoleMappings via
// accessOIDCBridge; nil leaves RoleMappings unset (env-only derivation).
func newAccessAuth(t *testing.T, roleMap map[string]string, defaultRole string, legacyAdminEmails []string, st *roleMapStore) *oidc.Authenticator {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	oidcSrv := &gooidctest.Server{PublicKeys: []gooidctest.PublicKey{{
		PublicKey: priv.Public(), KeyID: "access-test-key", Algorithm: "RS256",
	}}}
	httpSrv := httptest.NewServer(oidcSrv)
	oidcSrv.SetIssuer(httpSrv.URL)
	t.Cleanup(httpSrv.Close)

	var mappings oidc.RoleMappingSource
	if st != nil {
		mappings = accessOIDCBridge{st: st}
	}
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:         httpSrv.URL,
		ClientID:          "wardyn-client",
		ClientSecret:      "secret",
		RedirectURL:       "http://localhost/auth/callback",
		RoleMap:           roleMap,
		DefaultRole:       defaultRole,
		LegacyAdminEmails: legacyAdminEmails,
		RoleMappings:      mappings,
	}, accessTestHMACKey)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	return auth
}

// accessSession mints a wardyn_session cookie signed with accessTestHMACKey —
// the real key newAccessAuth's Authenticator verifies against (unlike
// ssoSession elsewhere in this package, which is keyed for the ZERO-VALUE
// Authenticator every other test file uses). groups is the login-time claim
// snapshot the lockout guard's PreviewRoleAgainst call reads back via
// oidcGroupsFromContext.
func accessSession(t *testing.T, sub, email, role string, groups []string) *http.Cookie {
	t.Helper()
	if groups == nil {
		groups = []string{}
	}
	payload, err := json.Marshal(oidc.Session{
		Sub: sub, Email: email, Role: role, Expiry: time.Now().UTC().Add(time.Hour), Groups: groups,
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	mac := hmac.New(sha256.New, accessTestHMACKey)
	mac.Write(payload)
	return &http.Cookie{
		Name:  "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

func accessServer(t *testing.T, auth *oidc.Authenticator, st *roleMapStore) *Server {
	t.Helper()
	if st == nil {
		st = &roleMapStore{}
	}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = auth
	return New(cfg)
}

// ─── 503 when OIDC is not configured ───────────────────────────────────────

func TestAccess_Unconfigured503(t *testing.T) {
	cfg := baseTestConfig(newHarness(t), &roleMapStore{})
	cfg.OIDC = nil
	srv := New(cfg)

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/access", ""},
		{http.MethodPost, "/api/v1/access/mappings", `{"value":"eng","role":"member"}`},
		{http.MethodDelete, "/api/v1/access/mappings/" + uuid.NewString(), ""},
		{http.MethodPost, "/api/v1/access/preview", `{}`},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := do(t, srv, rt.method, rt.path, adminToken, rt.body)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
			}
			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != "SSO is not configured" {
				t.Errorf("body.Error = %q, want %q", body.Error, "SSO is not configured")
			}
		})
	}
}

// ─── canonicalization + collision ──────────────────────────────────────────

func TestAccess_CanonicalizesValueOnWrite(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{}
	srv := accessServer(t, auth, st)

	// Empty chart + zero existing rows trips the posture-flip guard (before
	// admin, after denied) — acknowledge it so this test isolates
	// canonicalization, not the guard.
	body := `{"value":"  ENG-Team  ","role":"member","acknowledge_access_change":true}`
	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if len(st.rows) != 1 || st.rows[0].Value != "eng-team" {
		t.Fatalf("stored rows = %+v, want exactly one row with value %q", st.rows, "eng-team")
	}

	var saved types.RoleMapping
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if saved.Value != "eng-team" {
		t.Errorf("response value = %q, want %q (mixed-case input stored lowered)", saved.Value, "eng-team")
	}
}

func TestAccess_ReAddFlipsRoleAndReturns200(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, "", nil, nil)
	st := &roleMapStore{}
	srv := accessServer(t, auth, st)

	// Chart is non-empty, so the posture-flip guard never applies here — no
	// acknowledge needed.
	first := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"eng-team","role":"member"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first add: status = %d, want 201; body=%s", first.Code, first.Body.String())
	}
	second := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"ENG-TEAM","role":"admin"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("re-add: status = %d, want 200 (existing row flipped, not created); body=%s", second.Code, second.Body.String())
	}
	if len(st.rows) != 1 || st.rows[0].Role != oidc.RoleAdmin {
		t.Fatalf("stored rows = %+v, want exactly one row now admin", st.rows)
	}
}

func TestAccess_CollisionWithChart400(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"eng-team": oidc.RoleMember}, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{})

	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"Eng-Team","role":"admin"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "chart config") {
		t.Errorf("body = %q, want it to name the chart source", w.Body.String())
	}
}

func TestAccess_CollisionWithOperatorAllowlist400(t *testing.T) {
	auth := newAccessAuth(t, nil, "", []string{"ops@corp.example"}, nil)
	srv := accessServer(t, auth, &roleMapStore{})

	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"Ops@Corp.Example","role":"member"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "chart config") {
		t.Errorf("body = %q, want it to name the chart source (allowlist collision)", w.Body.String())
	}
}

// ─── Q7 adjudication: email-shaped console mappings ────────────────────────

// TestAccess_EmailMappingRefusedByDefault pins EMAIL_KEY_REFUSED
// byte-for-byte (docs/design/people-access-prompt.md's Adjudication §Q7) —
// this is a frozen console-facing string, not a message this test may
// loosely substring-match.
func TestAccess_EmailMappingRefusedByDefault(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{}) // Config.AllowEmailMappings unset -> false

	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"carol@corp.example","role":"member","acknowledge_access_change":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	const want = `Email mappings are disabled on this install. Map an App Role or group instead, or opt in with WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS in your chart.`
	if body.Error != want {
		t.Errorf("body.Error = %q, want the frozen EMAIL_KEY_REFUSED string %q", body.Error, want)
	}
}

// TestAccess_EmailMappingAllowedWhenOptedIn: the identical write succeeds
// once Config.AllowEmailMappings is set.
func TestAccess_EmailMappingAllowedWhenOptedIn(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = auth
	cfg.AllowEmailMappings = true
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"Carol@Corp.Example","role":"member","acknowledge_access_change":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if len(st.rows) != 1 || st.rows[0].Value != "carol@corp.example" {
		t.Fatalf("stored rows = %+v, want exactly one row with value %q", st.rows, "carol@corp.example")
	}
}

// TestAccess_EnvRoleMapEmailKeysUnaffected: an env WARDYN_OIDC_ROLE_MAP entry
// keyed on an email is a CHART row, not a console write — AllowEmailMappings
// gates POST /access/mappings only, so a chart email key still shows up (and
// still shadows a colliding console row) with the opt-in left off.
func TestAccess_EnvRoleMapEmailKeysUnaffected(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"carol@corp.example": oidc.RoleAdmin}, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{}) // AllowEmailMappings unset -> false

	w := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp accessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, m := range resp.Mappings {
		if m.Value == "carol@corp.example" && m.Source == "chart" {
			found = true
		}
	}
	if !found {
		t.Errorf("mappings = %+v, want the chart's email-keyed row present regardless of allow_email_mappings", resp.Mappings)
	}
}

// TestAccess_GetReflectsAllowEmailMappings covers both states of the flag.
func TestAccess_GetReflectsAllowEmailMappings(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%v", allow), func(t *testing.T) {
			auth := newAccessAuth(t, nil, "", nil, nil)
			cfg := baseTestConfig(newHarness(t), &roleMapStore{})
			cfg.OIDC = auth
			cfg.AllowEmailMappings = allow
			srv := New(cfg)

			w := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			var resp accessResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.AllowEmailMappings != allow {
				t.Errorf("allow_email_mappings = %v, want %v", resp.AllowEmailMappings, allow)
			}
		})
	}
}

func TestAccess_InvalidShapeRejected(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{})

	cases := []struct {
		name string
		body string
	}{
		{"empty value", `{"value":"","role":"member","acknowledge_access_change":true}`},
		{"whitespace-only value", `{"value":"   ","role":"member","acknowledge_access_change":true}`},
		{"invalid role", `{"value":"eng-team","role":"superadmin","acknowledge_access_change":true}`},
		{"non-ASCII value", `{"value":"café","role":"member","acknowledge_access_change":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// ─── posture-flip guard: pure-function matrix over accessRolePosture ───────

func TestAccessRolePosture_Matrix(t *testing.T) {
	cases := []struct {
		name              string
		hasOperatorEmails bool
		defaultRole       string
		wantBefore        string
		wantAfter         string
		wantChanges       bool
	}{
		{"emails + no default -> denied (fires)", true, "", oidc.RoleMember, accessDeniedRole, true},
		{"emails + default admin -> widening (fires)", true, oidc.RoleAdmin, oidc.RoleMember, oidc.RoleAdmin, true},
		{"emails + default member (suppressed)", true, oidc.RoleMember, oidc.RoleMember, oidc.RoleMember, false},
		{"no emails + default admin -> admin (suppressed)", false, oidc.RoleAdmin, oidc.RoleAdmin, oidc.RoleAdmin, false},
		{"no emails + no default -> denied (fires)", false, "", oidc.RoleAdmin, accessDeniedRole, true},
		{"no emails + default member -> member (fires)", false, oidc.RoleMember, oidc.RoleAdmin, oidc.RoleMember, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var emails []string
			if tc.hasOperatorEmails {
				emails = []string{"ops@corp.example"}
			}
			auth := newAccessAuth(t, nil, tc.defaultRole, emails, nil)
			before, after, changes := accessRolePosture(auth)
			if before != tc.wantBefore || after != tc.wantAfter || changes != tc.wantChanges {
				t.Errorf("accessRolePosture = (%q, %q, %v), want (%q, %q, %v)",
					before, after, changes, tc.wantBefore, tc.wantAfter, tc.wantChanges)
			}
		})
	}
}

// TestAccess_PostureFlipGuard_FiresAndAcknowledges: end to end through
// POST /access/mappings — the pure-function matrix above proves the
// before/after/changes computation; this proves the HANDLER actually gates
// the write on it.
func TestAccess_PostureFlipGuard_FiresAndAcknowledges(t *testing.T) {
	auth := newAccessAuth(t, nil, "", []string{"ops@corp.example"}, nil) // emails + no default -> denied: fires
	srv := accessServer(t, auth, &roleMapStore{})

	blocked := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"eng-team","role":"member"}`)
	if blocked.Code != http.StatusBadRequest {
		t.Fatalf("without acknowledge: status = %d, want 400; body=%s", blocked.Code, blocked.Body.String())
	}
	var flip accessPostureFlipBody
	if err := json.Unmarshal(blocked.Body.Bytes(), &flip); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !flip.RequiredAcknowledgement || flip.Before != oidc.RoleMember || flip.After != accessDeniedRole {
		t.Errorf("flip body = %+v, want required_acknowledgement=true before=member after=denied", flip)
	}

	allowed := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"eng-team","role":"member","acknowledge_access_change":true}`)
	if allowed.Code != http.StatusCreated {
		t.Fatalf("with acknowledge: status = %d, want 201; body=%s", allowed.Code, allowed.Body.String())
	}
}

// TestAccess_PostureFlipGuard_InertWhenChartNonEmpty: the guard's "iff chart
// map is empty" condition — a non-empty chart means the role map was ALREADY
// non-empty (arm 2 already applies) before this write, so adding the
// deployment's first CONSOLE row cannot be the transition that flips arm.
func TestAccess_PostureFlipGuard_InertWhenChartNonEmpty(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"chart-row": oidc.RoleMember}, "", []string{"ops@corp.example"}, nil)
	srv := accessServer(t, auth, &roleMapStore{})

	// Same operator-emails + no-default combination TestAccess_PostureFlipGuard_FiresAndAcknowledges
	// used to trip the guard — but the chart is non-empty here, so it must not.
	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"eng-team","role":"member"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (guard inert when the chart map is non-empty); body=%s", w.Code, w.Body.String())
	}
}

// TestAccess_ReversePostureFlipGuard: DELETE's mirror-image guard, over the
// query-param acknowledgement (?acknowledge_access_change=true) — the repo's
// existing DELETE handlers (handleDeleteSource, handleDeleteBaseImage) use a
// query param for this shape, not a body.
func TestAccess_ReversePostureFlipGuard(t *testing.T) {
	auth := newAccessAuth(t, nil, oidc.RoleAdmin, nil, nil) // no emails + default admin: reverse fires (admin -> admin is NOT this case's before/after — see below)
	st := &roleMapStore{}
	srv := accessServer(t, auth, st)

	// Seed the deployment's ONLY console row directly (bypassing the add
	// guard, which is not what this test is about).
	id := uuid.New()
	st.rows = append(st.rows, types.RoleMapping{ID: id, Value: "eng-team", Role: oidc.RoleMember})

	// Reverse posture: before = DefaultRole() (admin, since it's set) =
	// admin; after = admin/member per allowlist -> no operator emails ->
	// admin. before == after here, so THIS combination is actually
	// suppressed — switch to a combination that fires: default role unset,
	// no operator emails (before=denied, after=admin).
	authFires := newAccessAuth(t, nil, "", nil, nil)
	stFires := &roleMapStore{rows: []types.RoleMapping{{ID: id, Value: "eng-team", Role: oidc.RoleMember}}}
	srvFires := accessServer(t, authFires, stFires)

	blocked := do(t, srvFires, http.MethodDelete, "/api/v1/access/mappings/"+id.String(), adminToken, "")
	if blocked.Code != http.StatusBadRequest {
		t.Fatalf("without acknowledge: status = %d, want 400; body=%s", blocked.Code, blocked.Body.String())
	}
	var flip accessPostureFlipBody
	if err := json.Unmarshal(blocked.Body.Bytes(), &flip); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if flip.Before != accessDeniedRole || flip.After != oidc.RoleAdmin {
		t.Errorf("flip body = %+v, want before=denied after=admin", flip)
	}
	if len(stFires.rows) != 1 {
		t.Fatalf("blocked delete must not have removed the row; rows = %+v", stFires.rows)
	}

	allowed := do(t, srvFires, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change=true", adminToken, "")
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("with acknowledge: status = %d, want 204; body=%s", allowed.Code, allowed.Body.String())
	}
	if len(stFires.rows) != 0 {
		t.Errorf("rows after acknowledged delete = %+v, want empty", stFires.rows)
	}

	// The suppressed (admin -> admin) combination from the top of this test
	// must delete WITHOUT acknowledgement.
	suppressed := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String(), adminToken, "")
	if suppressed.Code != http.StatusNoContent {
		t.Fatalf("suppressed case (admin -> admin): status = %d, want 204 with no acknowledge needed; body=%s", suppressed.Code, suppressed.Body.String())
	}
}

// ─── lockout guard ──────────────────────────────────────────────────────────

// TestAccess_LockoutGuard_SSOAdminBlockedFromDemotingSelf: an SSO admin
// deleting the console row that is their ONLY source of admin — with a
// second row remaining so the merged map stays non-empty (arm 2, not the
// "no role map at all" fallback-to-admin arm 1) — is refused.
func TestAccess_LockoutGuard_SSOAdminBlockedFromDemotingSelf(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "admins", Role: oidc.RoleAdmin},
		{ID: uuid.New(), Value: "other-team", Role: oidc.RoleMember},
	}}
	srv := accessServer(t, auth, st)
	admin := accessSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin, []string{"admins"})

	w := doSSO(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+st.rows[0].ID.String(), admin, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (would remove the caller's own admin access); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "remove your own admin access") {
		t.Errorf("body = %q, want the lockout message", w.Body.String())
	}
	if len(st.rows) != 2 {
		t.Fatalf("blocked delete must not have removed anything; rows = %+v", st.rows)
	}
}

// TestAccess_LockoutGuard_AdminTokenExempt: the SAME delete an SSO admin was
// just refused succeeds for the admin-token caller — no per-human role to
// demote, and this exemption IS the recovery path once every SSO admin has
// locked themselves out.
func TestAccess_LockoutGuard_AdminTokenExempt(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "admins", Role: oidc.RoleAdmin},
		{ID: uuid.New(), Value: "other-team", Role: oidc.RoleMember},
	}}
	srv := accessServer(t, auth, st)

	w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+st.rows[0].ID.String(), adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin-token delete: status = %d, want 204 (lockout-exempt); body=%s", w.Code, w.Body.String())
	}

	// The admin-token caller can also demote EVERY SSO human via POST — the
	// task's other named exemption case.
	w2 := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"other-team","role":"member","acknowledge_access_change":true}`)
	if w2.Code != http.StatusOK && w2.Code != http.StatusCreated {
		t.Fatalf("admin-token re-add: status = %d, want 200/201; body=%s", w2.Code, w2.Body.String())
	}
}

// TestAccess_LockoutGuard_POST: the add-side of the same guard — an SSO
// admin re-mapping the ONLY row that grants them admin to "member" (a
// second row keeps the map non-empty) must be refused, exactly like the
// delete case above.
func TestAccess_LockoutGuard_POST(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "admins", Role: oidc.RoleAdmin},
		{ID: uuid.New(), Value: "other-team", Role: oidc.RoleMember},
	}}
	srv := accessServer(t, auth, st)
	admin := accessSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin, []string{"admins"})

	w := doSSO(t, srv, http.MethodPost, "/api/v1/access/mappings", admin, `{"value":"admins","role":"member"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (would remove the caller's own admin access); body=%s", w.Code, w.Body.String())
	}
	if st.rows[0].Role != oidc.RoleAdmin {
		t.Fatalf("blocked write must not have applied; rows[0] = %+v", st.rows[0])
	}
}

// ─── GET /access shape ──────────────────────────────────────────────────────

func TestAccess_GetShapeIncludesShadowedRow(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"eng-team": oidc.RoleMember}, oidc.RoleMember, []string{"ops@corp.example"}, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		// Shadowed by the chart entry above.
		{ID: uuid.New(), Value: "eng-team", Role: oidc.RoleAdmin, CreatedBy: "admin@corp.example", CreatedAt: time.Now().UTC()},
		// Shadowed by the operator allowlist.
		{ID: uuid.New(), Value: "ops@corp.example", Role: oidc.RoleMember, CreatedBy: "admin@corp.example", CreatedAt: time.Now().UTC()},
		// Not shadowed.
		{ID: uuid.New(), Value: "design-team", Role: oidc.RoleMember, CreatedBy: "admin@corp.example", CreatedAt: time.Now().UTC()},
	}}
	srv := accessServer(t, auth, st)

	w := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp accessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DefaultRole != oidc.RoleMember || !resp.OperatorEmailsPresent {
		t.Errorf("response = %+v, want default_role=member operator_emails_present=true", resp)
	}
	if len(resp.Mappings) != 4 { // 1 chart + 3 console
		t.Fatalf("mappings = %+v, want 4 rows (1 chart + 3 console)", resp.Mappings)
	}
	byValueSource := func(value, source string) accessMappingView {
		for _, m := range resp.Mappings {
			if m.Value == value && m.Source == source {
				return m
			}
		}
		t.Fatalf("no mapping found for value=%q source=%q in %+v", value, source, resp.Mappings)
		return accessMappingView{}
	}
	if chartRow := byValueSource("eng-team", "chart"); chartRow.Shadowed {
		t.Errorf("chart row = %+v, chart rows are never shadowed", chartRow)
	}
	if consoleShadowed := byValueSource("eng-team", "console"); !consoleShadowed.Shadowed || consoleShadowed.ShadowCause != "chart" {
		t.Errorf("console row shadowed by chart = %+v, want shadowed=true shadow_cause=chart", consoleShadowed)
	}
	if opsShadowed := byValueSource("ops@corp.example", "console"); !opsShadowed.Shadowed || opsShadowed.ShadowCause != "operator_allowlist" {
		t.Errorf("console row shadowed by allowlist = %+v, want shadowed=true shadow_cause=operator_allowlist", opsShadowed)
	}
	if clean := byValueSource("design-team", "console"); clean.Shadowed || clean.ShadowCause != "" {
		t.Errorf("unshadowed console row = %+v, want shadowed=false shadow_cause=\"\"", clean)
	}
}

// ─── preview ────────────────────────────────────────────────────────────────

func TestAccess_PreviewExplicitClaims(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"eng-team": oidc.RoleMember}, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{})

	w := do(t, srv, http.MethodPost, "/api/v1/access/preview", adminToken, `{"groups":["eng-team"],"email":"carol@corp.example"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp accessPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Role != oidc.RoleMember || resp.Error != "" {
		t.Errorf("preview = %+v, want ok=true role=member no error", resp)
	}
	if len(resp.Matched) != 1 || resp.Matched[0].Source != string(oidc.MatchSourceMapRow) {
		t.Errorf("matched = %+v, want one map_row match", resp.Matched)
	}
}

func TestAccess_PreviewUseSession(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"eng-team": oidc.RoleMember}, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{})
	// /access/preview is operatorOnly like every other route here, so the
	// calling session must itself be admin — this pins use_session pulling
	// carol's GROUPS claim ("eng-team") from context, not that her own
	// derived role happens to be member.
	admin := accessSession(t, "sub-carol", "carol@corp.example", oidc.RoleAdmin, []string{"eng-team"})

	w := doSSO(t, srv, http.MethodPost, "/api/v1/access/preview", admin, `{"use_session":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp accessPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Role != oidc.RoleMember {
		t.Errorf("preview = %+v, want ok=true role=member (derived from the session's own groups claim)", resp)
	}
}

func TestAccess_PreviewUseSessionWithoutSessionIs400(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{})

	w := do(t, srv, http.MethodPost, "/api/v1/access/preview", adminToken, `{"use_session":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no session claims to preview") {
		t.Errorf("body = %q, want the no-session message", w.Body.String())
	}
}

// TestAccess_PreviewStoreErrorIsOutcomeNot500: a role-mapping store read
// failure is a PREVIEW OUTCOME (200 + error field), not a 500 — "couldn't
// check" is exactly what this endpoint exists to let an admin see and retry.
func TestAccess_PreviewStoreErrorIsOutcomeNot500(t *testing.T) {
	st := &roleMapStore{listErr: context.DeadlineExceeded}
	auth := newAccessAuth(t, nil, "", nil, st)
	srv := accessServer(t, auth, st)

	w := do(t, srv, http.MethodPost, "/api/v1/access/preview", adminToken, `{"email":"x@corp.example"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (store error is an outcome, not a 500); body=%s", w.Code, w.Body.String())
	}
	var resp accessPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != "role_check_unavailable" || resp.OK {
		t.Errorf("preview = %+v, want error=role_check_unavailable ok=false", resp)
	}
}

// ─── DELETE unknown id ──────────────────────────────────────────────────────

func TestAccess_DeleteUnknownID404(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{})

	w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+uuid.NewString(), adminToken, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
