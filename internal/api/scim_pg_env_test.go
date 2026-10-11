// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/scim"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// Two wardynd instances over one Postgres database, the SCIM routes mounted on both, a fake Entra tenant
// as the identity provider: what the leaver tests need, because an epoch or a gate tested on one instance
// proves nothing about the replica a suspension did not land on.

const scimEnvToken = "scim-env-token-0123456789abcdef0123456789abcd"

// pgTestRevocations is the session-revocation store the production adapter is (cmd/wardynd), over the
// same tables, with the three hooks the races need.
type pgTestRevocations struct {
	pool *pgxpool.Pool
	st   store.PG
	now  func() time.Time // consistently injected app clock in the clock regressions

	mu          sync.Mutex
	failRevoke  bool
	onLateCheck func()
}

// cutoffRevoked is the cutoff check. cuts says the browser-session cuts count too, as in the production
// adapter: for IsSessionRevoked and for a credential that carries an epoch, not for a token or a key.
func (r *pgTestRevocations) cutoffRevoked(ctx context.Context, sub, email string, issuedAt time.Time, cuts bool) (bool, error) {
	return r.cutoffRevokedQ(ctx, r.pool, sub, email, issuedAt, cuts)
}

func (r *pgTestRevocations) cutoffRevokedQ(ctx context.Context, q store.Querier, sub, email string, issuedAt time.Time, cuts bool) (bool, error) {
	var cutoff sql.NullTime
	var databaseAt time.Time
	err := q.QueryRow(ctx, `SELECT MAX(revoked_at), clock_timestamp() FROM (
			SELECT sub, revoked_at FROM oidc_session_revocations
			UNION ALL SELECT sub, cut_at FROM oidc_session_cuts WHERE $3::boolean) r
		WHERE sub = $1 OR lower(sub) = lower($2) OR sub = ''`, sub, email, cuts).Scan(&cutoff, &databaseAt)
	if err != nil || !cutoff.Valid {
		return false, err
	}
	appNow := time.Now()
	if r.now != nil {
		appNow = r.now()
	}
	anchor := db.AppClockAnchor{DatabaseAt: databaseAt, AppAt: appNow}
	return issuedAt.IsZero() || !issuedAt.After(cutoff.Time) || !anchor.Translate(issuedAt).After(cutoff.Time), nil
}

// IsSessionRevoked is the cutoff check. After it has computed its answer, a pending onLateCheck hook runs
// once: a request that read "not revoked" and then lost a race with a suspension is exactly this.
func (r *pgTestRevocations) IsSessionRevoked(ctx context.Context, sub, email string, issuedAt time.Time) (bool, error) {
	revoked, err := r.cutoffRevoked(ctx, sub, email, issuedAt, true)
	r.mu.Lock()
	hook := r.onLateCheck
	r.onLateCheck = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return revoked, err
}

func (r *pgTestRevocations) SessionStatus(ctx context.Context, sub, email string, issuedAt time.Time, epoch int64) (oidc.SessionStatus, error) {
	return r.SessionStatusQ(ctx, r.pool, sub, email, issuedAt, epoch)
}

func (r *pgTestRevocations) SessionStatusQ(ctx context.Context, q store.Querier, sub, email string, issuedAt time.Time, epoch int64) (oidc.SessionStatus, error) {
	blocked, err := store.IdentityBlockedQ(ctx, q, sub, epoch)
	if err != nil {
		return oidc.SessionLive, err
	}
	if blocked {
		return oidc.SessionDeactivated, nil
	}
	revoked, err := r.cutoffRevokedQ(ctx, q, sub, email, issuedAt, epoch >= 0)
	if revoked {
		return oidc.SessionRevoked, err
	}
	return oidc.SessionLive, err
}

func (r *pgTestRevocations) RevokeSub(ctx context.Context, sub string) error {
	r.mu.Lock()
	fail := r.failRevoke
	r.mu.Unlock()
	if fail {
		return errors.New("revocation store unavailable")
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp())
		ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`, sub)
	return err
}

func (r *pgTestRevocations) CutSessions(ctx context.Context, sub string) error {
	r.mu.Lock()
	fail := r.failRevoke
	r.mu.Unlock()
	if fail {
		return errors.New("revocation store unavailable")
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO oidc_session_cuts (sub, cut_at) VALUES ($1, clock_timestamp())
		ON CONFLICT (sub) DO UPDATE SET cut_at = EXCLUDED.cut_at`, sub)
	return err
}

func (r *pgTestRevocations) RevokeAll(ctx context.Context) error { return r.RevokeSub(ctx, "") }

func (r *pgTestRevocations) setFail(v bool) { r.mu.Lock(); r.failRevoke = v; r.mu.Unlock() }
func (r *pgTestRevocations) setLateCheck(f func()) {
	r.mu.Lock()
	r.onLateCheck = f
	r.mu.Unlock()
}

// flakyAudit fails the audit write of the actions it is told to fail.
type flakyAudit struct {
	*recRecorder
	mu      sync.Mutex
	failing map[string]bool
}

func (f *flakyAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	f.mu.Lock()
	fail := f.failing[ev.Action]
	f.mu.Unlock()
	if fail {
		return errors.New("audit sink unavailable")
	}
	return f.recRecorder.Record(ctx, ev)
}

func (f *flakyAudit) fail(action string, v bool) {
	f.mu.Lock()
	if f.failing == nil {
		f.failing = map[string]bool{}
	}
	f.failing[action] = v
	f.mu.Unlock()
}

// scimRunner is a runner whose KillSandbox fails failKills times and counts every call.
type scimRunner struct {
	*fakeRunner
	mu        sync.Mutex
	failKills int
	kills     int
}

func (r *scimRunner) KillSandbox(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kills++
	if r.failKills > 0 {
		r.failKills--
		return errors.New("the runner is unreachable")
	}
	return nil
}

func (r *scimRunner) setFailKills(n int) { r.mu.Lock(); r.failKills = n; r.mu.Unlock() }
func (r *scimRunner) killCount() int     { r.mu.Lock(); defer r.mu.Unlock(); return r.kills }

// hookGate is the real identity gate with one hook: after admission answers "not refused", the hook runs
// once, before the callback goes on to issuance.
type hookGate struct {
	oidc.IdentityGate
	mu           sync.Mutex
	afterRefused func()
}

func (g *hookGate) Refused(ctx context.Context, ref oidc.IdentityRef) (bool, error) {
	refused, err := g.IdentityGate.Refused(ctx, ref)
	g.mu.Lock()
	hook := g.afterRefused
	g.afterRefused = nil
	g.mu.Unlock()
	if hook != nil && !refused && err == nil {
		hook()
	}
	return refused, err
}

func (g *hookGate) setAfterRefused(f func()) { g.mu.Lock(); g.afterRefused = f; g.mu.Unlock() }

// scimNode is one wardynd instance.
type scimNode struct {
	srv    *Server
	h      *harness
	audit  *flakyAudit
	runner *scimRunner
	auth   *oidc.Authenticator
	gate   *hookGate

	mu      sync.Mutex
	onLogin func()
}

func (n *scimNode) setOnLogin(f func()) { n.mu.Lock(); n.onLogin = f; n.mu.Unlock() }

type scimEnv struct {
	t        *testing.T
	pool     *pgxpool.Pool
	st       store.PG
	sec      *secretspg.Store
	rev      *pgTestRevocations
	fake     *entrafake.Server
	issuer   string
	tenant   string
	redirect string
	a, b     *scimNode
}

// newSCIMEnv builds the database, the fake tenant and two instances. shape edits each instance's Config
// before it is built.
func newSCIMEnv(t *testing.T, shape ...func(*Config)) *scimEnv {
	t.Helper()
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	fake := entrafake.New()
	t.Cleanup(fake.Close)
	redirect := "http://console.example.invalid/auth/callback"
	fake.SetRedirectURI(redirect)
	st := store.NewPG(pool)
	e := &scimEnv{t: t, pool: pool, st: st, sec: sec, rev: &pgTestRevocations{pool: pool, st: st}, fake: fake,
		issuer: fake.Issuer(), tenant: fake.TenantID(), redirect: redirect}
	e.a, e.b = e.node(shape...), e.node(shape...)
	return e
}

func (e *scimEnv) node(shape ...func(*Config)) *scimNode {
	e.t.Helper()
	n := &scimNode{h: newHarness(e.t), runner: &scimRunner{fakeRunner: &fakeRunner{}}}
	n.audit = &flakyAudit{recRecorder: n.h.audit}
	n.gate = &hookGate{IdentityGate: NewIdentityGate(e.st)}
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: e.issuer, ClientID: e.fake.ClientID(), RedirectURL: e.redirect, DefaultRole: oidc.RoleUser,
		Revocations: e.rev, Identities: n.gate,
		OnLogin: func(context.Context, oidc.LoginFacts) {
			n.mu.Lock()
			f := n.onLogin
			n.mu.Unlock()
			if f != nil {
				f()
			}
		},
	}, accessTestHMACKey)
	if err != nil {
		e.t.Fatalf("oidc.New: %v", err)
	}
	n.auth = auth
	cfg := baseTestConfig(n.h, e.st)
	cfg.Audit = n.audit
	cfg.OIDC = auth
	cfg.SessionRevocations = e.rev
	cfg.Secrets = e.sec
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.Runner = n.runner
	cfg.SCIM = &SCIMConfig{Token: scimEnvToken, Issuer: e.issuer, Tenant: e.tenant}
	for _, f := range shape {
		f(&cfg)
	}
	n.srv = New(cfg)
	return n
}

// scim sends one SCIM request to n with the primary bearer.
func (e *scimEnv) scim(n *scimNode, method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+scimEnvToken)
	r.Header.Set("Content-Type", scim.MediaType)
	r.RemoteAddr = "192.0.2.7:4000"
	w := httptest.NewRecorder()
	panicFails(e.t, n.srv.Handler()).ServeHTTP(w, r)
	return w
}

// scimUser is the SCIM User as a test reads it back.
type scimUser struct {
	ID         string       `json:"id"`
	ExternalID string       `json:"externalId"`
	UserName   string       `json:"userName"`
	Active     bool         `json:"active"`
	Emails     []scim.Email `json:"emails"`
	Meta       *scim.Meta   `json:"meta"`
	Schemas    []string     `json:"schemas"`
}

func decodeSCIMUser(t *testing.T, w *httptest.ResponseRecorder) scimUser {
	t.Helper()
	var u scimUser
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode SCIM user: %v: %s", err, w.Body.String())
	}
	return u
}

func scimErrorOf(t *testing.T, w *httptest.ResponseRecorder) (status, scimType string) {
	t.Helper()
	var env struct {
		Status   string `json:"status"`
		ScimType string `json:"scimType"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode SCIM error: %v: %s", err, w.Body.String())
	}
	return env.Status, env.ScimType
}

// postUser POSTs a user and returns its SCIM id.
func (e *scimEnv) postUser(n *scimNode, externalID, userName, email string, active bool) (scimUser, int) {
	e.t.Helper()
	body := map[string]any{
		"schemas": []string{scim.SchemaUser}, "externalId": externalID, "userName": userName, "active": active,
	}
	if email != "" {
		body["emails"] = []map[string]any{{"value": email, "type": "work", "primary": true}}
	}
	raw, _ := json.Marshal(body)
	w := e.scim(n, http.MethodPost, "/scim/v2/Users", string(raw))
	if w.Code != http.StatusCreated {
		return scimUser{}, w.Code
	}
	return decodeSCIMUser(e.t, w), w.Code
}

func (e *scimEnv) patch(n *scimNode, id, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.scim(n, http.MethodPatch, "/scim/v2/Users/"+id, body)
}

const patchActive = `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","path":"active","value":%s}]}`

func patchOf(active string) string { return strings.Replace(patchActive, "%s", active, 1) }

func (e *scimEnv) identity(id string) store.PrincipalIdentity {
	e.t.Helper()
	u, err := uuid.Parse(id)
	if err != nil {
		e.t.Fatal(err)
	}
	row, err := e.st.GetIdentity(context.Background(), u)
	if err != nil {
		e.t.Fatalf("get identity %s: %v", id, err)
	}
	return row
}

// seedEntra writes the identity row an Entra sign-in leaves: keyed by the object id, bound to principal.
func (e *scimEnv) seedEntra(principal, email, oid string) store.PrincipalIdentity {
	e.t.Helper()
	row, err := e.st.UpsertLoginIdentity(context.Background(), store.LoginIdentity{
		Principal: principal, Issuer: e.issuer, TenantID: e.tenant, ObjectID: oid, Email: email,
	}, time.Now().UTC())
	if err != nil {
		e.t.Fatalf("seed identity %s: %v", principal, err)
	}
	return row
}

// seedSignIn writes the row a non-Entra sign-in leaves: keyed by (issuer, principal), no object id.
func (e *scimEnv) seedSignIn(principal, email string) store.PrincipalIdentity {
	e.t.Helper()
	row, err := e.st.UpsertLoginIdentity(context.Background(), store.LoginIdentity{Principal: principal, Issuer: e.issuer, Email: email}, time.Now().UTC())
	if err != nil {
		e.t.Fatalf("seed identity %s: %v", principal, err)
	}
	return row
}

func (e *scimEnv) seedToken(principal, email string) (uuid.UUID, string) {
	e.t.Helper()
	raw := apiTokenPrefix + uuid.NewString()
	f := false
	tok, err := e.st.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: principal, Email: email, Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		GroupsTruncated: &f, Name: "t", CreatedAt: time.Now().UTC(),
	}, raw)
	if err != nil {
		e.t.Fatalf("seed token for %s: %v", principal, err)
	}
	return tok.ID, raw
}

func (e *scimEnv) seedKey(principal string) string {
	e.t.Helper()
	fp := "SHA256:" + uuid.NewString()
	if _, err := e.st.AddSSHKey(context.Background(), types.SSHPublicKey{
		Fingerprint: fp, Principal: principal, Name: "k", PublicKey: "ssh-ed25519 AAAA " + fp, Role: oidc.RoleUser, CreatedAt: time.Now().UTC(),
	}); err != nil {
		e.t.Fatalf("seed key for %s: %v", principal, err)
	}
	return fp
}

func (e *scimEnv) seedRun(principal string, state types.RunState) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	run, err := e.st.CreateRun(context.Background(), types.AgentRun{
		ID: id, CreatedBy: principal, Agent: "claude-code", ConfinementClass: types.CC1, State: state, RunnerTarget: "docker",
		Task: "t", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
	})
	if err != nil {
		e.t.Fatalf("seed run for %s: %v", principal, err)
	}
	if run.State != state {
		if ok, err := e.st.UpdateRunStateIf(context.Background(), id, run.State, state); err != nil || !ok {
			e.t.Fatalf("move run to %s: %v %v", state, ok, err)
		}
	}
	if err := e.st.SetSandboxRef(context.Background(), id, "wardyn-agent-"+id.String()); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *scimEnv) runState(id uuid.UUID) types.RunState {
	e.t.Helper()
	run, err := e.st.GetRun(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return run.State
}

// cookie is a session cookie for sub as the callback would have issued it: issued now, at epoch.
func scimCookie(t *testing.T, sub, email string, epoch int64) *http.Cookie {
	t.Helper()
	return scimCookieAt(t, sub, email, epoch, time.Now().UTC())
}

func scimCookieAt(t *testing.T, sub, email string, epoch int64, issuedAt time.Time) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: email, Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		Expiry: time.Now().UTC().Add(time.Hour), IssuedAt: issuedAt, AuthorityEpoch: epoch, Groups: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, accessTestHMACKey)
	mac.Write(payload)
	return &http.Cookie{
		Name:  "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// cookieWorks reports whether the cookie authenticates a request on n.
func (e *scimEnv) cookieWorks(n *scimNode, c *http.Cookie) bool {
	e.t.Helper()
	return doSSO(e.t, n.srv, http.MethodGet, "/api/v1/me", c, "").Code == http.StatusOK
}

// tokenWorks reports whether the API token authenticates a request on n.
func (e *scimEnv) tokenWorks(n *scimNode, raw string) bool {
	e.t.Helper()
	return do(e.t, n.srv, http.MethodGet, "/api/v1/me", raw, "").Code == http.StatusOK
}

func (e *scimEnv) tokenRevoked(id uuid.UUID) bool {
	e.t.Helper()
	var at sql.NullTime
	if err := e.pool.QueryRow(context.Background(), `SELECT revoked_at FROM api_tokens WHERE id = $1`, id).Scan(&at); err != nil {
		e.t.Fatal(err)
	}
	return at.Valid
}

func (e *scimEnv) keyCount(principal string) int {
	e.t.Helper()
	keys, err := e.st.ListSSHKeysByPrincipal(context.Background(), principal)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(keys)
}

func (e *scimEnv) rows(n *scimNode, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range n.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func dataOf(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("row %s data %s: %v", ev.Action, ev.Data, err)
	}
	return d
}

// signIn drives a real sign-in on n as the person the fake tenant offers, returning the callback response.
func (e *scimEnv) signIn(n *scimNode, sub, email string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.signInAs(n, entrafake.Identity{Username: email, Subject: sub})
}

// signInAs is signIn for a person whose id_token carries the claims id names.
func (e *scimEnv) signInAs(n *scimNode, id entrafake.Identity) *httptest.ResponseRecorder {
	e.t.Helper()
	email := id.Username
	e.fake.SetIdentities(id)
	handler := panicFails(e.t, n.srv.Handler())
	lw := httptest.NewRecorder()
	handler.ServeHTTP(lw, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if lw.Code != http.StatusFound {
		e.t.Fatalf("/auth/login = %d", lw.Code)
	}
	authURL := lw.Header().Get("Location") + "&login_hint=" + url.QueryEscape(email)
	q := follow(e.t, authURL)
	params := url.Values{"state": {q.Get("state")}}
	for _, k := range []string{"code", "error", "error_description"} {
		if v := q.Get(k); v != "" {
			params.Set(k, v)
		}
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?"+params.Encode(), nil)
	for _, c := range lw.Result().Cookies() {
		cb.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, cb)
	return w
}

func sessionCookieOf(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			return c
		}
	}
	return nil
}

func authErrorOf(w *httptest.ResponseRecorder) string {
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		return ""
	}
	return loc.Query().Get("auth_error")
}
