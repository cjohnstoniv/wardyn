// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── fake ──────────────────────────────────────────────────────────────────

// tokenMemStore is a minimal in-memory store.Store for the api-token tests,
// following the sshMemStore/notFoundStore convention in this package (every
// unimplemented method panics via the embedded nil store.Store).
//
// It HASHES exactly the way store_apitokens.go does — rows are keyed by
// hex(sha256(raw)), never by the raw string — so "a token whose plaintext does
// not hash to a stored row is refused" is a property this double can actually
// express rather than one it fakes away.
type tokenMemStore struct {
	// noGovernanceStore rather than a bare store.Store: these tests drive GET
	// /me through the real router, which now resolves the caller's user drive as
	// well as their ceiling, and the empty-deployment answer is exactly what an
	// api-token test means to model.
	noGovernanceStore
	mu      sync.Mutex
	byID    map[uuid.UUID]types.APIToken
	byHash  map[string]uuid.UUID
	touched map[uuid.UUID]time.Time
}

func newTokenMemStore() *tokenMemStore {
	return &tokenMemStore{
		byID:    map[uuid.UUID]types.APIToken{},
		byHash:  map[string]uuid.UUID{},
		touched: map[uuid.UUID]time.Time{},
	}
}

func memHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *tokenMemStore) CreateAPIToken(_ context.Context, t types.APIToken, raw string) (types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := memHash(raw)
	if _, dup := s.byHash[h]; dup {
		return types.APIToken{}, store.ErrConflict
	}
	t.Token = "" // the store never persists the plaintext
	s.byID[t.ID] = t
	s.byHash[h] = t.ID
	return t, nil
}

// GetAPITokenByRaw mirrors the PG query's `revoked_at IS NULL`: unknown,
// mismatched and revoked all collapse to ErrNotFound.
func (s *tokenMemStore) GetAPITokenByRaw(_ context.Context, raw string) (types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byHash[memHash(raw)]
	if !ok {
		return types.APIToken{}, store.ErrNotFound
	}
	t := s.byID[id]
	if t.RevokedAt != nil {
		return types.APIToken{}, store.ErrNotFound
	}
	return t, nil
}

func (s *tokenMemStore) TouchAPIToken(_ context.Context, id uuid.UUID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched[id] = now
	return nil
}

func (s *tokenMemStore) ListAPITokensByPrincipal(_ context.Context, principal string) ([]types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []types.APIToken{}
	for _, t := range s.byID {
		if t.Principal == principal {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *tokenMemStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []types.APIToken{}
	for _, t := range s.byID {
		out = append(out, t)
	}
	return out, nil
}

func (s *tokenMemStore) RevokeAPIToken(_ context.Context, id uuid.UUID, principal string, now time.Time) (types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok || t.RevokedAt != nil || (principal != "" && t.Principal != principal) {
		return types.APIToken{}, store.ErrNotFound
	}
	t.RevokedAt = &now
	s.byID[id] = t
	return t, nil
}

// ─── harness ───────────────────────────────────────────────────────────────

const (
	tokenAdminSub   = "sub-admin"
	tokenAdminEmail = "admin@corp.example"
	tokenMemberSub  = "sub-member"
	tokenMemberMail = "member@corp.example"
)

// apiTokenTestServer builds a Server with SSO configured (so the session branch
// is reachable and tokens can be minted the only way they ever can be) and the
// in-memory token store wired.
func apiTokenTestServer(t *testing.T) (*Server, *tokenMemStore, *harness) {
	t.Helper()
	h := newHarness(t)
	st := newTokenMemStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), st, h
}

// mintToken creates a token through the real self-service route as sess and
// returns the plaintext plus the created row.
func mintToken(t *testing.T, srv *Server, sess *http.Cookie, name string) (string, types.APIToken) {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/tokens", sess, `{"name":"`+name+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create token: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created token: %v", err)
	}
	if !strings.HasPrefix(created.Token, apiTokenPrefix) {
		t.Fatalf("plaintext %q does not carry the %q prefix", created.Token, apiTokenPrefix)
	}
	return created.Token, created
}

// ─── the auth branch ───────────────────────────────────────────────────────

// TestAPITokenAuth_ContextParityWithSession is the load-bearing test of this
// feature: a request authenticated by a token must publish the IDENTICAL
// identity context a request authenticated by that human's SSO session
// publishes. If it does not, ownership, the admin gate and — worst — capability
// DENY grants resolve differently depending on which credential the caller
// reached for, which is a breach rather than a degradation.
//
// It compares against withHumanIdentity's own output, which is what the session
// branch of humanOrAdminAuth calls, so this pins BOTH that apiTokenAuth resolves
// every key and that it resolves them to the snapshot on the row.
func TestAPITokenAuth_ContextParityWithSession(t *testing.T) {
	srv, st, _ := apiTokenTestServer(t)
	row := types.APIToken{
		ID:        uuid.New(),
		Principal: tokenMemberSub,
		Email:     tokenMemberMail,
		Role:      oidc.RoleMember,
		Groups:    []string{"eng", "oncall"},
		Name:      "ci",
	}
	const raw = apiTokenPrefix + "parity"
	if _, err := st.CreateAPIToken(context.Background(), row, raw); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	var got context.Context
	capture := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })
	deny := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("fell through to the admin path with a valid token")
		w.WriteHeader(http.StatusUnauthorized)
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	srv.apiTokenAuth(capture, deny).ServeHTTP(httptest.NewRecorder(), r)
	if got == nil {
		t.Fatal("token auth never reached the next handler")
	}

	want := withHumanIdentity(context.Background(), row.Principal, row.Email, row.Role, row.Groups, false)
	if a, b := oidcHumanFromContext(got), oidcHumanFromContext(want); a != b {
		t.Errorf("sub = %q, want %q", a, b)
	}
	if a, b := oidcEmailFromContext(got), oidcEmailFromContext(want); a != b {
		t.Errorf("email = %q, want %q", a, b)
	}
	if a, b := oidcRoleFromContext(got), oidcRoleFromContext(want); a != b {
		t.Errorf("role = %q, want %q", a, b)
	}
	if a, b := oidcGroupsFromContext(got), oidcGroupsFromContext(want); !slices.Equal(a, b) {
		t.Errorf("groups = %v, want %v", a, b)
	}
	// capabilitySubjects is the resolver's actual entry point — pinning it
	// directly proves grants bind to the owning human, not merely that the keys
	// are populated.
	users, groups, stale := capabilitySubjects(got)
	if !slices.Contains(users, tokenMemberSub) || !slices.Contains(users, tokenMemberMail) {
		t.Errorf("capability users = %v, want both the sub and the email", users)
	}
	if !slices.Equal(groups, row.Groups) || stale {
		t.Errorf("capability groups = %v stale=%v, want %v stale=false", groups, stale, row.Groups)
	}
	// A token has no session, so there is nothing for the expiry warning to
	// report — deliberately NOT part of withHumanIdentity.
	if exp := oidcExpiryFromContext(got); !exp.IsZero() {
		t.Errorf("expiry = %v, want zero for a token (no session to expire)", exp)
	}
	// The token-provenance marker is separate from identity and is what stops a
	// token minting a successor.
	if apiTokenIDFromContext(got) != row.ID {
		t.Errorf("token id marker = %v, want %v", apiTokenIDFromContext(got), row.ID)
	}
}

// TestAPITokenAuth_NilGroupsStaySnapshotUnavailable pins the nil-vs-empty
// distinction across the store round trip. A token minted from a session with no
// answerable group identity must report stale, never "this human has no groups"
// — the latter would silently withhold every group grant the holder has.
func TestAPITokenAuth_NilGroupsStaySnapshotUnavailable(t *testing.T) {
	srv, st, _ := apiTokenTestServer(t)
	const raw = apiTokenPrefix + "nilgroups"
	if _, err := st.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: tokenMemberSub, Role: oidc.RoleMember, Groups: nil,
	}, raw); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	var got context.Context
	capture := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	srv.apiTokenAuth(capture, http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), r)
	if got == nil {
		t.Fatal("token auth never reached the next handler")
	}
	if _, groups, stale := capabilitySubjects(got); groups != nil || !stale {
		t.Errorf("groups = %v stale = %v, want nil/true (snapshot unavailable)", groups, stale)
	}
}

// TestAPITokenAuth_RevokedIsRefused: a revoked token authenticates nothing. The
// refusal is a 401 identical to the one an unknown token gets — the store
// collapses both to ErrNotFound, so the boundary is not an oracle for "this
// token used to exist".
func TestAPITokenAuth_RevokedIsRefused(t *testing.T) {
	srv, _, _ := apiTokenTestServer(t)
	sess := ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleMember)
	raw, created := mintToken(t, srv, sess, "ci")

	if w := do(t, srv, http.MethodGet, "/api/v1/me", raw, ""); w.Code != http.StatusOK {
		t.Fatalf("before revoke: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/tokens/"+created.ID.String(), sess, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	w := do(t, srv, http.MethodGet, "/api/v1/me", raw, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("after revoke: code = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	// Byte-identical to an unknown token: no "was revoked" tell.
	unknown := do(t, srv, http.MethodGet, "/api/v1/me", apiTokenPrefix+"never-issued", "")
	if w.Body.String() != unknown.Body.String() {
		t.Errorf("revoked body %q differs from unknown-token body %q — an existence oracle",
			w.Body.String(), unknown.Body.String())
	}
	// Revoking twice is a 404, not a second successful revoke (and so emits no
	// second token.revoke audit row for an act that did not happen).
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/tokens/"+created.ID.String(), sess, ""); w.Code != http.StatusNotFound {
		t.Errorf("second revoke: code = %d, want 404", w.Code)
	}
}

// TestAPITokenAuth_WrongHashIsRefused: a `wdn_`-prefixed bearer that hashes to
// no stored row is refused, including one that differs from a LIVE token by a
// single character — the lookup is over the hash, never a prefix or a name.
func TestAPITokenAuth_WrongHashIsRefused(t *testing.T) {
	srv, _, _ := apiTokenTestServer(t)
	sess := ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleMember)
	raw, _ := mintToken(t, srv, sess, "ci")

	nearMiss := raw[:len(raw)-1] + map[bool]string{true: "0", false: "1"}[strings.HasSuffix(raw, "1")]
	for _, bearer := range []string{
		apiTokenPrefix + "totally-unknown",
		nearMiss,
		strings.ToUpper(raw), // the stored form is a hash: case is not a near-match
	} {
		if w := do(t, srv, http.MethodGet, "/api/v1/me", bearer, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("bearer %q: code = %d, want 401; body=%s", bearer, w.Code, w.Body.String())
		}
	}
	// Control: the real one still works, so the assertions above are not passing
	// because the whole branch is broken.
	if w := do(t, srv, http.MethodGet, "/api/v1/me", raw, ""); w.Code != http.StatusOK {
		t.Errorf("the real token: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestAPITokenAuth_AdminTokenStillWorks: the token branch sits IN FRONT of the
// admin bearer compare, so it must not shadow it. An unresolvable `wdn_` bearer
// falls through to the admin path (which is also why an operator whose
// WARDYN_ADMIN_TOKEN happens to start with `wdn_` still authenticates).
func TestAPITokenAuth_AdminTokenStillWorks(t *testing.T) {
	srv, _, _ := apiTokenTestServer(t)
	if w := do(t, srv, http.MethodGet, "/api/v1/me", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("admin token: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// ─── RBAC: the token can never out-rank its human ──────────────────────────

// TestAPIToken_CannotReachAdminRouteUnlessAdminPrincipal is the RBAC pin the
// brief names: a token is exactly as powerful as the human it belongs to. A
// member's token is refused an admin-gated route with the same 403 their session
// gets; an admin's token reaches it. Crucially, NEITHER inherits the admin-token
// tier — the identity on the context is the human, so isOperator reads the
// stamped role instead of falling through to "no session role to demote".
func TestAPIToken_CannotReachAdminRouteUnlessAdminPrincipal(t *testing.T) {
	srv, _, _ := apiTokenTestServer(t)
	memberRaw, _ := mintToken(t, srv, ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleMember), "member-ci")
	adminRaw, _ := mintToken(t, srv, ssoSession(t, tokenAdminSub, tokenAdminEmail, oidc.RoleAdmin), "admin-ci")

	// GET /tokens and DELETE /tokens/{id} are the two admin-gated routes this
	// lane adds; /site-config's PUT is a pre-existing one, proving the gate is
	// the shared requireOperator rather than anything token-specific.
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/tokens"},
		{http.MethodDelete, "/api/v1/tokens/" + uuid.New().String()},
		{http.MethodPut, "/api/v1/site-config"},
	} {
		body := ""
		if probe.method == http.MethodPut {
			body = "{}"
		}
		if w := do(t, srv, probe.method, probe.path, memberRaw, body); w.Code != http.StatusForbidden {
			t.Errorf("member token on %s %s: code = %d, want 403; body=%s",
				probe.method, probe.path, w.Code, w.Body.String())
		}
		if w := do(t, srv, probe.method, probe.path, adminRaw, body); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
			t.Errorf("admin token on %s %s: code = %d, want NOT 401/403; body=%s",
				probe.method, probe.path, w.Code, w.Body.String())
		}
	}

	// /me reports the human, not "admin-token", and the member's token is not an
	// operator.
	w := do(t, srv, http.MethodGet, "/api/v1/me", memberRaw, "")
	var me map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	if me["principal"] != tokenMemberSub || me["operator"] != false || me["role"] != oidc.RoleMember {
		t.Errorf("/me = %v, want principal=%s operator=false role=%s", me, tokenMemberSub, oidc.RoleMember)
	}
	if me["principal"] == adminTokenPrincipal {
		t.Error("a token resolved to the ADMIN identity — the whole point is that it never can")
	}
}

// ─── self-service CRUD ─────────────────────────────────────────────────────

func TestAPITokens_CreateListRevoke(t *testing.T) {
	srv, _, h := apiTokenTestServer(t)
	sess := ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleMember)
	raw, created := mintToken(t, srv, sess, "laptop")

	if created.Principal != tokenMemberSub || created.Role != oidc.RoleMember || created.Name != "laptop" {
		t.Errorf("created = %+v, want the caller's own principal/role and the given name", created)
	}
	ev := lastAuditEvent(t, h.audit.events, "token.create")
	if ev.Actor != tokenMemberSub || ev.Target != created.ID.String() {
		t.Errorf("token.create audit = actor %q target %q, want %q / %q",
			ev.Actor, ev.Target, tokenMemberSub, created.ID)
	}
	if strings.Contains(string(ev.Data), raw) {
		t.Error("the plaintext token leaked into the audit Data")
	}

	// The list carries the row but NEVER the plaintext — the create response was
	// the one and only chance to read it.
	w := doSSO(t, srv, http.MethodGet, "/api/v1/me/tokens", sess, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: code = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), raw) || strings.Contains(w.Body.String(), `"token"`) {
		t.Errorf("list body carries the plaintext token: %s", w.Body.String())
	}
	var listed []types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("list = %+v, want exactly the created token", listed)
	}

	// Another human's list does not see it, and their revoke of it 404s (the
	// store scopes both to the caller's principal).
	other := ssoSession(t, "sub-other", "other@corp.example", oidc.RoleMember)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/me/tokens", other, ""); strings.Contains(w.Body.String(), created.ID.String()) {
		t.Errorf("another human's list leaks the token: %s", w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/tokens/"+created.ID.String(), other, ""); w.Code != http.StatusNotFound {
		t.Errorf("foreign revoke: code = %d, want 404 (no existence oracle)", w.Code)
	}

	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/tokens/"+created.ID.String(), sess, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	rev := lastAuditEvent(t, h.audit.events, "token.revoke")
	if rev.Target != created.ID.String() {
		t.Errorf("token.revoke target = %q, want %q", rev.Target, created.ID)
	}

	// The revoked row SURVIVES the list, marked — a human has to be able to see
	// that the credential they retired is retired.
	w = doSSO(t, srv, http.MethodGet, "/api/v1/me/tokens", sess, "")
	listed = nil
	_ = json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].RevokedAt == nil {
		t.Errorf("list after revoke = %+v, want the row present with revoked_at set", listed)
	}
}

// TestAPITokens_AdminInventoryAndRevokeAny: the admin lane sees every token and
// revokes anyone's — the remediation path for the stamp ceiling (a demoted
// admin's outstanding tokens) and for a departed owner's credential.
func TestAPITokens_AdminInventoryAndRevokeAny(t *testing.T) {
	srv, _, _ := apiTokenTestServer(t)
	memberSess := ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleMember)
	adminSess := ssoSession(t, tokenAdminSub, tokenAdminEmail, oidc.RoleAdmin)
	raw, created := mintToken(t, srv, memberSess, "member-ci")

	w := doSSO(t, srv, http.MethodGet, "/api/v1/tokens", adminSess, "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin list: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), created.ID.String()) {
		t.Errorf("admin list does not carry the member's token: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), raw) {
		t.Error("admin list leaks the plaintext token")
	}
	// A member is refused the inventory outright — it names other humans.
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/tokens", memberSess, ""); w.Code != http.StatusForbidden {
		t.Errorf("member on the admin inventory: code = %d, want 403", w.Code)
	}

	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/tokens/"+created.ID.String(), adminSess, ""); w.Code != http.StatusNoContent {
		t.Fatalf("admin revoke-any: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodGet, "/api/v1/me", raw, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("after admin revoke: code = %d, want 401", w.Code)
	}
}

// TestAPITokens_MintRequiresAVerifiedHuman: the identity invariant, enforced at
// the only place a token is born. The admin token is not a person, so it cannot
// mint a "per-user" token — otherwise this feature would manufacture a second
// admin-tier credential attributable to nobody, which is the exact problem it
// exists to remove. A TOKEN cannot mint a successor either, or revoking a leaked
// one would not end the compromise.
func TestAPITokens_MintRequiresAVerifiedHuman(t *testing.T) {
	srv, _, _ := apiTokenTestServer(t)
	if w := do(t, srv, http.MethodPost, "/api/v1/me/tokens", adminToken, `{"name":"ci"}`); w.Code != http.StatusForbidden {
		t.Errorf("admin-token mint: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	raw, _ := mintToken(t, srv, ssoSession(t, tokenAdminSub, tokenAdminEmail, oidc.RoleAdmin), "ci")
	w := do(t, srv, http.MethodPost, "/api/v1/me/tokens", raw, `{"name":"child"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("token-mints-token: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestAPITokens_PerPrincipalCap bounds an unbounded mint loop. Revoking frees a
// slot, so the cap counts LIVE tokens, not rows.
func TestAPITokens_PerPrincipalCap(t *testing.T) {
	srv, _, _ := apiTokenTestServer(t)
	sess := ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleMember)
	var first types.APIToken
	for i := 0; i < apiTokenMaxPerPrincipal; i++ {
		_, created := mintToken(t, srv, sess, "t")
		if i == 0 {
			first = created
		}
	}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/tokens", sess, `{"name":"one-too-many"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("over the cap: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/tokens/"+first.ID.String(), sess, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke to free a slot: code = %d, want 204", w.Code)
	}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/tokens", sess, `{"name":"replacement"}`); w.Code != http.StatusCreated {
		t.Errorf("after freeing a slot: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}
