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
	"bytes"
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
// opts, when given, can tweak the Config before it is built — e.g. setting
// AllowedEmailDomains (A-4's HasEmailDomains) without a new positional param
// on every existing call site.
func newAccessAuth(t *testing.T, roleMap map[string]string, defaultRole string, legacyAdminEmails []string, st *roleMapStore, opts ...func(*oidc.Config)) *oidc.Authenticator {
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
	cfg := oidc.Config{
		IssuerURL:         httpSrv.URL,
		ClientID:          "wardyn-client",
		ClientSecret:      "secret",
		RedirectURL:       "http://localhost/auth/callback",
		RoleMap:           roleMap,
		DefaultRole:       defaultRole,
		LegacyAdminEmails: legacyAdminEmails,
		RoleMappings:      mappings,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	auth, err := oidc.New(context.Background(), cfg, accessTestHMACKey)
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
// oidcGroupsFromContext — NIL IS REPRESENTABLE here, deliberately not
// coerced to []string{}: nil is a distinct, real state (a pre-0.6 cookie
// that predates the groups snapshot field, or a group list truncated away
// entirely, see sessionGroups' own "NEVER returns nil ... except" doc) that
// A-1's stale-snapshot tests need to construct, not just the empty-snapshot
// state a genuinely group-less human produces.
func accessSession(t *testing.T, sub, email, role string, groups []string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		// Hand-rolled payload: stamp the codec version or decodeSession reads it
		// as a pre-0.7 cookie and refuses it (see ssoSession in rbac_test.go).
		V:   oidc.SessionCodecVersion,
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
	// A-2: structured cause, not prose-only.
	var body accessCollisionBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Cause != "chart" || body.Value != "eng-team" {
		t.Errorf("body = %+v, want cause=chart value=eng-team", body)
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
	// A-2: structured cause, distinct from the chart arm above.
	var body accessCollisionBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Cause != "operator_allowlist" || body.Value != "ops@corp.example" {
		t.Errorf("body = %+v, want cause=operator_allowlist value=ops@corp.example", body)
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

// TestAccess_GetIncludesOperatorEmailAddresses (A-3): the Defaults block
// needs the ADDRESSES themselves (api.Config.OperatorEmails), not just the
// operator_emails_present bool the guard-note logic already had.
func TestAccess_GetIncludesOperatorEmailAddresses(t *testing.T) {
	auth := newAccessAuth(t, nil, "", []string{"ops@corp.example", "root@corp.example"}, nil)
	cfg := baseTestConfig(newHarness(t), &roleMapStore{})
	cfg.OIDC = auth
	cfg.OperatorEmails = []string{"ops@corp.example", "root@corp.example"}
	srv := New(cfg)

	w := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp accessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OperatorEmailsPresent {
		t.Error("operator_emails_present = false, want true")
	}
	want := []string{"ops@corp.example", "root@corp.example"}
	if !slices.Equal(resp.OperatorEmails, want) {
		t.Errorf("operator_emails = %v, want %v", resp.OperatorEmails, want)
	}
}

// TestAccess_GetOperatorEmailsNeverNull (W-4): with no operator emails
// configured, operator_emails must still be a JSON array, never null — a nil
// slice marshals to null, and the console reads its .length, so null crashes
// the whole /setup page (found by the live Entra walk, not CI: the e2e
// fixture had sent [] and so never reproduced the real nil-slice shape).
func TestAccess_GetOperatorEmailsNeverNull(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"wardyn.admin": "admin"}, "", nil, nil)
	cfg := baseTestConfig(newHarness(t), &roleMapStore{})
	cfg.OIDC = auth
	cfg.OperatorEmails = nil
	srv := New(cfg)

	w := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"operator_emails":null`)) {
		t.Errorf("operator_emails serialized as null; want []: %s", w.Body.String())
	}
	var resp accessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OperatorEmails == nil {
		t.Error("OperatorEmails is nil; want non-nil empty slice")
	}
}

// TestAccess_GetReflectsEmailDomainsConfigured (A-4): the EMAIL_KEY badge
// copy needs to tell "no WARDYN_OIDC_EMAIL_DOMAINS" apart from "configured".
func TestAccess_GetReflectsEmailDomainsConfigured(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured=%v", configured), func(t *testing.T) {
			var opts []func(*oidc.Config)
			if configured {
				opts = append(opts, func(c *oidc.Config) { c.AllowedEmailDomains = []string{"corp.example"} })
			}
			auth := newAccessAuth(t, nil, "", nil, nil, opts...)
			srv := accessServer(t, auth, &roleMapStore{})

			w := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			var resp accessResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.EmailDomainsConfigured != configured {
				t.Errorf("email_domains_configured = %v, want %v", resp.EmailDomainsConfigured, configured)
			}
		})
	}
}

// ─── A-5: guard matrix over a SHADOWED row (merged map, not raw counts) ────

// TestAccess_ShadowedRowGuards: chart is EMPTY; the console's ONLY row
// collides with the OPERATOR ALLOWLIST and is shadowed (mergeRoleMaps drops
// it, contributing nothing) — the merged map is genuinely empty even though
// len(existing)==1, the exact divergence A-5 fixes (chart[value]!="" would
// always keep merged non-empty on its own, so a chart collision can never
// reproduce "merged empty, len(existing)==1" — only an allowlist collision
// can). Both the GET posture and the DELETE guard must key on the real
// merged emptiness, not the raw row count.
func TestAccess_ShadowedRowGuards(t *testing.T) {
	auth := newAccessAuth(t, nil, "", []string{"ops@corp.example"}, nil)
	shadowedID := uuid.New()
	st := &roleMapStore{rows: []types.RoleMapping{{ID: shadowedID, Value: "ops@corp.example", Role: oidc.RoleAdmin}}}
	srv := accessServer(t, auth, st)

	// GET: map_empty must be TRUE (merged-effective) — under the OLD raw
	// count (len(chart)==0 && len(rows)==0) it would read false, since
	// len(rows)==1, even though this row contributes nothing to the map
	// deriveRole actually looks values up in.
	getResp := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
	var resp accessResponse
	if err := json.Unmarshal(getResp.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Posture.MapEmpty {
		t.Errorf("posture.map_empty = false, want true (the only console row is shadowed by the operator allowlist)")
	}

	// DELETE the shadowed row: removing it changes NOTHING about the merged
	// map (it contributed nothing to begin with) — before==after, so no
	// acknowledgement should be required. Under the OLD guard
	// (len(chart)==0 && len(existing)==1 && existing[0].ID==id) this would
	// have been misread as the last-row transition and demanded one.
	w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+shadowedID.String(), adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (deleting a shadowed row changes nothing, no ack needed); body=%s", w.Code, w.Body.String())
	}
}

// TestAccess_ShadowedRowGuard_AddGuardFiresOnRealFlip: the mirror case — the
// existing console row is shadowed by the OPERATOR ALLOWLIST (not the
// chart), so the merged map starts empty; adding an unshadowed row is the
// real transition from arm 1 to arm 2 and MUST require acknowledgement even
// though len(existing)==1 already before this write (old count-based guard
// would have stayed silent here, since its precondition only fired when the
// store held ZERO rows).
func TestAccess_ShadowedRowGuard_AddGuardFiresOnRealFlip(t *testing.T) {
	auth := newAccessAuth(t, nil, "", []string{"ops@corp.example"}, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "ops@corp.example", Role: oidc.RoleMember}, // shadowed by the allowlist
	}}
	srv := accessServer(t, auth, st)

	blocked := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"design-team","role":"member"}`)
	if blocked.Code != http.StatusBadRequest {
		t.Fatalf("without acknowledge: status = %d, want 400 (real arm flip, masked by the shadowed row under the old count-based guard); body=%s", blocked.Code, blocked.Body.String())
	}
	var flip accessPostureFlipBody
	if err := json.Unmarshal(blocked.Body.Bytes(), &flip); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !flip.RequiredAcknowledgement {
		t.Errorf("flip body = %+v, want required_acknowledgement=true", flip)
	}

	allowed := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"design-team","role":"member","acknowledge_access_change":true}`)
	if allowed.Code != http.StatusCreated {
		t.Fatalf("with acknowledge: status = %d, want 201; body=%s", allowed.Code, allowed.Body.String())
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
		// canonicalRoleMapValue's ASCII guard is oidc.ASCIIOnly, the SAME
		// function the login path applies to claim values — these two arms pin
		// the ones that distinguish a real ASCII test from a lazy one.
		// U+017F (LATIN SMALL LETTER LONG S) survives ToLower unchanged and
		// must stay refused: it case-folds onto ASCII "s", the escalating
		// direction the guard exists for.
		{"fold-escalating rune", `{"value":"roſs","role":"member","acknowledge_access_change":true}`},
		// Invalid UTF-8 decodes to RuneError (U+FFFD), which is above ASCII —
		// so a byte-level and a rune-level check agree here, and refusing is
		// the fail-closed answer either way.
		{"invalid UTF-8 value", `{"value":"\uFFFDeng","role":"member","acknowledge_access_change":true}`},
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

// ─── A-1: stale-snapshot guard, distinct from a genuine lockout ────────────

// TestAccess_StaleSnapshot_NilGroupsNeverReadsAsLockout: an admin session
// whose groups snapshot is nil (a pre-0.6 cookie, or a group that fell off
// the 2048-byte truncation) cannot re-derive the admin access the caller
// demonstrably holds — PreviewRoleAgainst against the snapshot alone comes
// out non-admin regardless of the write. Before the fix this 400'd with the
// LOCKOUT message on every such write (false positive); now it must get the
// distinct accessStaleSnapshot refusal instead, and the row must survive.
func TestAccess_StaleSnapshot_NilGroupsNeverReadsAsLockout(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "admins", Role: oidc.RoleAdmin},
		{ID: uuid.New(), Value: "other-team", Role: oidc.RoleMember},
	}}
	srv := accessServer(t, auth, st)
	// The cookie's own Role is admin (a real prior login derived it), but its
	// Groups snapshot is nil — accessSession now leaves nil representable
	// rather than coercing it to []string{} (the test seam A-1 also fixes).
	admin := accessSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin, nil)

	// A genuinely flipping write (deletes the admin's own row) — would be
	// the real lockout IF the snapshot could verify roleBefore.
	w := doSSO(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+st.rows[0].ID.String(), admin, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != accessStaleSnapshot {
		t.Errorf("body.Error = %q, want the stale-snapshot message %q (never the lockout message)", body.Error, accessStaleSnapshot)
	}
	if strings.Contains(body.Error, "remove your own admin access") {
		t.Errorf("body.Error = %q, must not read as the lockout refusal", body.Error)
	}
	if len(st.rows) != 2 {
		t.Fatalf("blocked delete must not have removed anything; rows = %+v", st.rows)
	}
}

// TestAccess_StaleSnapshot_NonFlippingWriteAlsoRefused: a stale-snapshot
// admin's write that would not even touch their own admin row must STILL be
// refused with accessStaleSnapshot, not silently allowed — the snapshot is
// too stale to verify a no-op write exactly as it is too stale to verify a
// real demotion (accessLockoutErr checks roleBefore first, unconditionally).
func TestAccess_StaleSnapshot_NonFlippingWriteAlsoRefused(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "admins", Role: oidc.RoleAdmin},
		{ID: uuid.New(), Value: "other-team", Role: oidc.RoleMember},
	}}
	srv := accessServer(t, auth, st)
	admin := accessSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin, nil)

	// Adds an UNRELATED row — does not touch "admins" at all, and the map
	// stays non-empty either way, so no posture-flip guard applies.
	w := doSSO(t, srv, http.MethodPost, "/api/v1/access/mappings", admin, `{"value":"design-team","role":"member"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (stale snapshot, even for a non-flipping write); body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != accessStaleSnapshot {
		t.Errorf("body.Error = %q, want %q", body.Error, accessStaleSnapshot)
	}
	if len(st.rows) != 2 {
		t.Fatalf("blocked write must not have applied; rows = %+v", st.rows)
	}
}

// TestAccess_LockoutGuard_GenuineLockoutStillRefused re-pins the SAME
// scenario TestAccess_LockoutGuard_SSOAdminBlockedFromDemotingSelf already
// covers (roleBefore IS admin, roleAfter is not) through the two-outcome
// A-1 rewrite, naming explicitly that it is the LOCKOUT message, not the
// stale-snapshot one, that fires when the snapshot genuinely can verify the
// caller's admin access.
func TestAccess_LockoutGuard_GenuineLockoutStillRefused(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "admins", Role: oidc.RoleAdmin},
		{ID: uuid.New(), Value: "other-team", Role: oidc.RoleMember},
	}}
	srv := accessServer(t, auth, st)
	admin := accessSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin, []string{"admins"})

	w := doSSO(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+st.rows[0].ID.String(), admin, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Error, "remove your own admin access") {
		t.Errorf("body.Error = %q, want the lockout message", body.Error)
	}
	if body.Error == accessStaleSnapshot {
		t.Errorf("body.Error = %q, must not read as the stale-snapshot refusal (the snapshot DOES verify admin here)", body.Error)
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

// TestAccess_ShadowCauseChartWinsOverAllowlist pins the ONE rule the read path
// (accessMappingsView) and the write path (POST /access/mappings, which refuses
// on the same cause) must never disagree about: a value present in BOTH the
// chart and the operator allowlist reports "chart", the more specific and more
// actionable source. Both paths now go through accessCollisionCause, and this
// is the arm that fold makes load-bearing — the other three arms are pinned by
// TestAccess_GetShapeIncludesShadowedRow above.
func TestAccess_ShadowCauseChartWinsOverAllowlist(t *testing.T) {
	const both = "ops@corp.example"
	auth := newAccessAuth(t, map[string]string{both: oidc.RoleMember}, oidc.RoleMember, []string{both}, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: both, Role: oidc.RoleAdmin, CreatedBy: "admin@corp.example", CreatedAt: time.Now().UTC()},
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
	var console *accessMappingView
	for i, m := range resp.Mappings {
		if m.Value == both && m.Source == "console" {
			console = &resp.Mappings[i]
		}
	}
	if console == nil {
		t.Fatalf("no console row for %q in %+v", both, resp.Mappings)
	}
	if !console.Shadowed || console.ShadowCause != "chart" {
		t.Errorf("row colliding with chart AND allowlist = %+v, want shadowed=true shadow_cause=chart (chart is checked first)", *console)
	}
}

// TestAccess_CreatedAtKeyIsAbsentOnChartRows pins created_at's `omitzero`
// elision at the RAW key level. A chart row has no creation time at all, and
// the TS twin declares created_at OPTIONAL (ui/src/app/lib/types/access.ts:25)
// — shipping a zero "0001-01-01T00:00:00Z" instead of omitting the key would
// decode back to a zero time.Time and slip past any struct-level assertion,
// while the console would render it as a real date.
func TestAccess_CreatedAtKeyIsAbsentOnChartRows(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"eng-team": oidc.RoleMember}, oidc.RoleMember, nil, nil)
	stamp := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "design-team", Role: oidc.RoleMember, CreatedBy: "admin@corp.example", CreatedAt: stamp},
	}}
	srv := accessServer(t, auth, st)

	w := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var raw struct {
		Mappings []map[string]any `json:"mappings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var chart, console map[string]any
	for _, m := range raw.Mappings {
		switch m["source"] {
		case "chart":
			chart = m
		case "console":
			console = m
		}
	}
	if chart == nil || console == nil {
		t.Fatalf("want one chart row and one console row, got %v", raw.Mappings)
	}
	if _, ok := chart["created_at"]; ok {
		t.Errorf("chart row = %v, want NO created_at key (a chart row has no creation time)", chart)
	}
	if got := console["created_at"]; got != stamp.Format(time.RFC3339Nano) {
		t.Errorf("console created_at = %v, want %q", got, stamp.Format(time.RFC3339Nano))
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
	if len(resp.Matched) != 1 || resp.Matched[0].Source != oidc.MatchSourceMapRow {
		t.Errorf("matched = %+v, want one map_row match", resp.Matched)
	}
}

// TestAccess_PreviewWireShape pins the RAW bytes of POST /access/preview's
// matched[] now that it marshals oidc.Match directly instead of an api-local
// twin: the three lowercase keys the TS twin declares
// (ui/src/app/lib/types/access.ts:108-112) and, for a no-match preview, [] and
// never null — the console reads matched.length, which throws on null.
func TestAccess_PreviewWireShape(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"eng-team": oidc.RoleMember}, "", nil, nil)
	srv := accessServer(t, auth, &roleMapStore{})

	w := do(t, srv, http.MethodPost, "/api/v1/access/preview", adminToken, `{"groups":["eng-team"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var raw struct {
		Matched []map[string]any `json:"matched"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Matched) != 1 {
		t.Fatalf("matched = %v, want exactly one entry; body=%s", raw.Matched, w.Body.String())
	}
	got := raw.Matched[0]
	if len(got) != 3 {
		t.Errorf("match object = %v, want exactly the 3 wire keys (a new exported field on oidc.Match must not leak onto this route)", got)
	}
	for _, k := range []string{"value", "role", "source"} {
		if _, ok := got[k]; !ok {
			t.Errorf("match object %v is missing key %q", got, k)
		}
	}
	if got["source"] != string(oidc.MatchSourceMapRow) {
		t.Errorf("source = %v, want %q as a plain string", got["source"], oidc.MatchSourceMapRow)
	}

	// No match at all: [] on the wire, never null. An EMPTY merged map is the
	// arm that actually returns a nil slice from deriveRole (derive.go:503),
	// so this is the construction that would marshal null without the guard —
	// a non-empty map falls through to the default_role match instead.
	empty := newAccessAuth(t, nil, oidc.RoleMember, nil, nil)
	esrv := accessServer(t, empty, &roleMapStore{})
	ew := do(t, esrv, http.MethodPost, "/api/v1/access/preview", adminToken, `{"groups":["nobody"]}`)
	if ew.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", ew.Code, ew.Body.String())
	}
	if !strings.Contains(ew.Body.String(), `"matched":[]`) {
		t.Errorf("body = %s, want matched:[] (a nil slice would marshal to null and break matched.length)", ew.Body.String())
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

// ─── A-6: delete audit records WHICH mapping was removed ──────────────────

// TestAccess_DeleteRecordsValueAndRoleInAudit: once a row is gone, the store
// can no longer say what it named — the audit event must carry the matched
// row's value/role at delete time, not just its (now-meaningless) id.
func TestAccess_DeleteRecordsValueAndRoleInAudit(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "eng-team", Role: oidc.RoleMember},
		{ID: uuid.New(), Value: "other-team", Role: oidc.RoleMember}, // keeps the map non-empty either way
	}}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = auth
	srv := New(cfg)

	target := st.rows[0]
	w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+target.ID.String(), adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	var found bool
	for _, ev := range h.audit.events {
		if ev.Action != "access.role_mapping.delete" {
			continue
		}
		found = true
		if ev.Target != target.ID.String() {
			t.Errorf("audit target = %q, want %q", ev.Target, target.ID.String())
		}
		var data struct {
			Value string `json:"value"`
			Role  string `json:"role"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode audit data: %v", err)
		}
		if data.Value != "eng-team" || data.Role != oidc.RoleMember {
			t.Errorf("audit data = %+v, want value=eng-team role=member", data)
		}
	}
	if !found {
		t.Fatalf("no access.role_mapping.delete audit event recorded")
	}
}

// ─── A-10: acknowledge_access_change via strconv.ParseBool ────────────────

// TestAccess_DeleteAcknowledgeAcceptsParseBoolForms: the query param used to
// accept only the literal "true" — strconv.ParseBool also takes "1"/"T"/
// "TRUE", and a garbage value must still read as false (never error the
// request), same as an absent param.
func TestAccess_DeleteAcknowledgeAcceptsParseBoolForms(t *testing.T) {
	// emails + no default: the reverse posture-flip guard fires on an
	// unacknowledged delete of the deployment's only row, which is exactly
	// what exercises ParseBool's non-"true" spellings below.
	auth := newAccessAuth(t, nil, "", []string{"ops@corp.example"}, nil)
	for _, ack := range []string{"1", "T", "TRUE"} {
		t.Run(ack, func(t *testing.T) {
			id := uuid.New()
			st := &roleMapStore{rows: []types.RoleMapping{{ID: id, Value: "eng-team", Role: oidc.RoleMember}}}
			srv := accessServer(t, auth, st)

			w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change="+ack, adminToken, "")
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (ack=%q accepted by ParseBool); body=%s", w.Code, ack, w.Body.String())
			}
		})
	}

	// A garbage value must read as false, not error the request.
	id := uuid.New()
	st := &roleMapStore{rows: []types.RoleMapping{{ID: id, Value: "eng-team", Role: oidc.RoleMember}}}
	srv := accessServer(t, auth, st)
	w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change=nonsense", adminToken, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (garbage ack value reads as false, guard still fires); body=%s", w.Code, w.Body.String())
	}
	if len(st.rows) != 1 {
		t.Fatalf("blocked delete must not have removed the row; rows = %+v", st.rows)
	}
}

// ─── the third tier: security_admin (0.7 §B, migration 0053) ──────────────

// TestAccess_SecurityAdminMappingPersists_PGBacked is the ONE test in this
// file that cannot use roleMapStore, and that is the entire point: the bug it
// pins lived exactly in the gap between the two halves. oidc.ValidRole was
// widened to accept RoleSecurityAdmin while migration 0051's
// `CHECK (role IN ('admin','member'))` still refused it, so this write passed
// every in-handler gate — canonicalization, collision, email opt-in, posture
// flip, lockout — and then 500'd at the INSERT. An in-memory double has no
// CHECK to violate and reports 201 either way; only the real schema can tell
// the two states apart. Guarded by WARDYN_TEST_PG (throwawayPGPool), which
// applies the full migration chain including 0053.
//
// Chart map non-empty so the posture-flip guard is inert (nothing here is
// about that guard), and adminToken so the lockout guard takes its
// no-OIDC-human break-glass exemption — the same setup
// TestAccess_ReAddFlipsRoleAndReturns200 uses, on a real store.
func TestAccess_SecurityAdminMappingPersists_PGBacked(t *testing.T) {
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)

	cfg := baseTestConfig(newHarness(t), pg)
	cfg.OIDC = newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, "", nil, nil)
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
		`{"value":"sec-team","role":"security_admin"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s\n"+
			"a 500 here means the role_mappings.role CHECK still refuses %q "+
			"(migration 0053 missing or not applied) while oidc.ValidRole accepts it",
			w.Code, w.Body.String(), oidc.RoleSecurityAdmin)
	}

	// The status alone would pass against a store that swallowed the role;
	// read the row back through the SAME pg store to prove the value the
	// CHECK had to admit is the value that landed.
	rows, err := pg.ListRoleMappings(context.Background())
	if err != nil {
		t.Fatalf("list role mappings: %v", err)
	}
	var got *types.RoleMapping
	for i := range rows {
		if rows[i].Value == "sec-team" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("no sec-team row persisted; rows = %+v", rows)
	}
	if got.Role != oidc.RoleSecurityAdmin {
		t.Errorf("persisted role = %q, want %q", got.Role, oidc.RoleSecurityAdmin)
	}
}

// TestAccess_InvalidRoleNamesAllThreeRoles: the 400 an unrecognized role gets
// must NAME the third tier. TestAccess_InvalidShapeRejected's "invalid role"
// case already pins the STATUS; this pins the MESSAGE, which is what an
// operator actually reads (and greps) when a write is refused — a two-role
// message would tell them security_admin is not a role, which is now false.
// Deliberately store-less-by-fake: this arm returns before the store is ever
// touched, so it needs no PG.
func TestAccess_InvalidRoleNamesAllThreeRoles(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, "", nil, nil)
	st := &roleMapStore{}
	srv := accessServer(t, auth, st)

	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
		`{"value":"eng-team","role":"securityadmin"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin, oidc.RoleMember} {
		if !strings.Contains(body.Error, role) {
			t.Errorf("error %q does not name role %q — all three valid roles must be listed", body.Error, role)
		}
	}
	if len(st.rows) != 0 {
		t.Errorf("a refused role must not reach the store; rows = %+v", st.rows)
	}
}
