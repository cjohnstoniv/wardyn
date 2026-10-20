// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CLI sign-in against a real Postgres: the request, the approval, the token call, the session's refresh,
// rotation and reuse, every cut, the admin step-up and the clamp, through the real router.
//
// Guarded by WARDYN_TEST_PG (throwawayBarePGPool): skipped cleanly when unset, must PASS when set.

const cliTestOrigin = "https://wardyn.test"

// cliEnv is a server with CLI sign-in on, the PG session revocations, and a clock a test moves.
type cliEnv struct {
	*govEnv
	rev   *pgTestRevocations
	clock time.Time
}

func newCLIEnv(t *testing.T, mutate ...func(*Config)) *cliEnv {
	t.Helper()
	pool := throwawayBarePGPool(t)
	e := &cliEnv{rev: &pgTestRevocations{pool: pool, st: store.NewPG(pool)}, clock: time.Now().UTC().Truncate(time.Second)}
	all := append([]func(*Config){func(c *Config) {
		c.CLISignIn = CLISignInConfig{Enabled: true, Origin: cliTestOrigin}
		c.SessionRevocations = e.rev
		c.Now = func() time.Time { return e.clock }
		c.Runner = &fakeRunner{}
		c.RunnerOrgURL = poolTestOrgURL
	}}, mutate...)
	e.govEnv = newGovEnvOn(t, pool, all...)
	return e
}

func (e *cliEnv) advance(d time.Duration) { e.clock = e.clock.Add(d) }

// req sends one request from remote (host:port), with an optional bearer, cookie and X-Forwarded-For.
func (e *cliEnv) req(method, path, bearer string, c *http.Cookie, remote, xff, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if remote != "" {
		r.RemoteAddr = remote
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	panicFails(e.t, e.srv.Handler()).ServeHTTP(w, r)
	return w
}

// cliClient is one computer's CLI: its device key and PKCE verifier.
type cliClient struct {
	e        *cliEnv
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	verifier string
}

func (e *cliEnv) client() *cliClient {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	return &cliClient{e: e, pub: pub, priv: priv, verifier: uuid.NewString() + uuid.NewString()}
}

func (c *cliClient) challenge() string {
	sum := sha256.Sum256([]byte(c.verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// createBody is a sign-in request body for this client; edit tweaks it.
func (c *cliClient) createBody(edit func(*types.CLISignInCreateRequest)) string {
	req := types.CLISignInCreateRequest{Challenge: c.challenge(), Port: 4711, Machine: "pm-laptop",
		PublicKey: base64.RawURLEncoding.EncodeToString(c.pub), Mode: types.CLISignInModeLoopback, Store: types.CLIStoreKeychain}
	if edit != nil {
		edit(&req)
	}
	b, _ := json.Marshal(req)
	return string(b)
}

func (c *cliClient) create(edit func(*types.CLISignInCreateRequest)) types.CLISignInCreateResponse {
	c.e.t.Helper()
	w := c.e.req(http.MethodPost, "/api/v1/cli/sign-ins", "", nil, "", "", c.createBody(edit))
	if w.Code != http.StatusCreated {
		c.e.t.Fatalf("create sign-in = %d %s", w.Code, w.Body)
	}
	return decodeJSON[types.CLISignInCreateResponse](c.e.t, w)
}

func (c *cliClient) proofAt(route string, id uuid.UUID, secret string, at time.Time, key ed25519.PrivateKey) types.CLIProof {
	sig := ed25519.Sign(key, types.CLIProofMessage(cliTestOrigin, route, id, secret, at.Unix()))
	return types.CLIProof{Time: at.Unix(), Signature: base64.RawURLEncoding.EncodeToString(sig)}
}

func (c *cliClient) tokenWith(id uuid.UUID, code, verifier string, proof types.CLIProof) *httptest.ResponseRecorder {
	b, _ := json.Marshal(types.CLISignInTokenRequest{Code: code, Verifier: verifier, Proof: proof})
	return c.e.req(http.MethodPost, "/api/v1/cli/sign-ins/"+id.String()+"/token", "", nil, "", "", string(b))
}

func (c *cliClient) token(id uuid.UUID, code string) *httptest.ResponseRecorder {
	route := "/api/v1/cli/sign-ins/" + id.String() + "/token"
	return c.tokenWith(id, code, c.verifier, c.proofAt(route, id, code, c.e.clock, c.priv))
}

func (c *cliClient) refreshWith(id uuid.UUID, secret string, proof types.CLIProof) *httptest.ResponseRecorder {
	b, _ := json.Marshal(types.CLISessionRefreshRequest{Secret: secret, Proof: proof})
	return c.e.req(http.MethodPost, "/api/v1/cli/sessions/"+id.String()+"/refresh", "", nil, "", "", string(b))
}

func (c *cliClient) refresh(id uuid.UUID, secret string) *httptest.ResponseRecorder {
	route := "/api/v1/cli/sessions/" + id.String() + "/refresh"
	return c.refreshWith(id, secret, c.proofAt(route, id, secret, c.e.clock, c.priv))
}

func (e *cliEnv) approve(cookie *http.Cookie, id uuid.UUID) *httptest.ResponseRecorder {
	return e.req(http.MethodPost, "/api/v1/me/cli-sign-ins/"+id.String()+"/approve", "", cookie, "", "", "")
}

// signIn runs a whole sign-in for cookie's person and returns the client and its grant.
func (e *cliEnv) signIn(cookie *http.Cookie, edit func(*types.CLISignInCreateRequest)) (*cliClient, types.CLISessionGrant) {
	e.t.Helper()
	c := e.client()
	si := c.create(edit)
	w := e.approve(cookie, si.ID)
	if w.Code != http.StatusOK {
		e.t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	ap := decodeJSON[types.CLISignInApproveResponse](e.t, w)
	w = c.token(si.ID, ap.Code)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("token = %d %s", w.Code, w.Body)
	}
	return c, decodeJSON[types.CLISessionGrant](e.t, w)
}

// me is GET /api/v1/me with bearer: its status and the role it publishes.
func (e *cliEnv) me(bearer string) (int, string) {
	w := e.req(http.MethodGet, "/api/v1/me", bearer, nil, "", "", "")
	var body struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.Role
}

func wantCLIReason(t *testing.T, w *httptest.ResponseRecorder, code int, reason string) {
	t.Helper()
	if w.Code != code || !strings.Contains(w.Body.String(), `"reason":"`+reason+`"`) {
		t.Fatalf("got %d %s, want %d %s", w.Code, w.Body, code, reason)
	}
}

func TestPG_CLISignIn_FlowAndApprovePageData(t *testing.T) {
	e := newCLIEnv(t)
	if w := e.req(http.MethodGet, "/healthz", "", nil, "", "", ""); !strings.Contains(w.Body.String(), `"cli_sign_in":true`) ||
		!strings.Contains(w.Body.String(), `"cli_paste_sign_in":false`) {
		t.Fatalf("/healthz = %s, want cli_sign_in true and paste off", w.Body)
	}
	c := e.client()
	si := c.create(nil)
	if si.ExpiresIn != 600 || len(si.UserCode) != 9 || si.UserCode[4] != '-' {
		t.Fatalf("create = %+v, want a 10-minute request with an XXXX-XXXX code", si)
	}
	page := e.req(http.MethodGet, "/api/v1/me/cli-sign-ins/"+si.ID.String(), "", e.user, "", "", "")
	view := decodeJSON[types.CLISignInView](t, page)
	if page.Code != http.StatusOK || view.Machine != "pm-laptop" || view.UserCode != si.UserCode || !view.AddressMatch ||
		view.SessionTTLSeconds != int64(30*24*3600) || view.SessionIdleSeconds != int64(7*24*3600) || view.AccessTTLSeconds != 3600 {
		t.Fatalf("approve page = %d %+v, want the machine, the code, a matching address and 30 d / 7 d / 1 h", page.Code, view)
	}
	other := decodeJSON[types.CLISignInView](t, e.req(http.MethodGet, "/api/v1/me/cli-sign-ins/"+si.ID.String(), "", e.user, "203.0.113.9:443", "", ""))
	if other.AddressMatch || other.RequestAddress != "192.0.2.1" || other.ApproverAddress != "203.0.113.9" {
		t.Fatalf("approve page from another address = %+v, want the mismatch with both addresses", other)
	}
	w := e.approve(e.user, si.ID)
	ap := decodeJSON[types.CLISignInApproveResponse](t, w)
	if w.Code != http.StatusOK || ap.CodeExpiresIn != 60 || ap.RedirectURL != "http://127.0.0.1:4711/callback?id="+si.ID.String()+"&code="+ap.Code {
		t.Fatalf("approve = %d %+v, want K for 60 s and the loopback redirect", w.Code, ap)
	}
	wantCLIReason(t, e.req(http.MethodGet, "/api/v1/me/cli-sign-ins/"+si.ID.String(), "", e.user, "", "", ""), http.StatusGone, reasonCLISignInUsed)
	approvedAt := e.clock
	w = c.token(si.ID, ap.Code)
	g := decodeJSON[types.CLISessionGrant](t, w)
	if w.Code != http.StatusCreated || !strings.HasPrefix(g.Secret, "wdc_") || !strings.HasPrefix(g.AccessToken, "wdn_") || g.Kind != types.APITokenKindCLIAccess ||
		!g.SessionExpiresAt.Equal(approvedAt.Add(30*24*time.Hour)) || !g.IdleExpiresAt.Equal(approvedAt.Add(7*24*time.Hour)) ||
		g.AccessExpiresAt.Sub(approvedAt) != time.Hour || g.Store != types.CLIStoreKeychain {
		t.Fatalf("token = %d %+v, want a 30-day session, 7 days idle, and a 1-hour cli_access token", w.Code, g)
	}
	for _, action := range []string{"cli.sign_in.request", "cli.sign_in.approve", "cli.session.create"} {
		lastAuditEvent(t, e.h.audit.snapshot(), action)
	}
	if ev := lastAuditEvent(t, e.h.audit.snapshot(), "token.create"); !strings.Contains(string(ev.Data), `"kind":"cli_access"`) ||
		strings.Contains(string(ev.Data), g.AccessToken) {
		t.Errorf("token.create = %s, want kind cli_access and never the token", ev.Data)
	}
	list := decodeJSON[[]types.CLISession](t, e.req(http.MethodGet, "/api/v1/me/cli-sessions", "", e.user, "", "", ""))
	if len(list) != 1 || list[0].ID != g.SessionID || list[0].Machine != "pm-laptop" || list[0].Store != types.CLIStoreKeychain {
		t.Fatalf("Signed-in CLIs = %+v, want this computer", list)
	}
	if code, _ := e.me(g.AccessToken); code != http.StatusOK {
		t.Fatalf("GET /me with the access token = %d", code)
	}
}

// The address the request came from is read as realip.go reads it: the TCP peer, or the client a listed
// proxy reports. Behind an unlisted proxy both sides read the proxy and always match (advisory only).
func TestPG_CLISignIn_AddressMatchBehindTrustedProxy(t *testing.T) {
	proxied := newCLIEnv(t, func(c *Config) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} })
	c := proxied.client()
	w := proxied.req(http.MethodPost, "/api/v1/cli/sign-ins", "", nil, "10.1.1.1:5000", "198.51.100.7", c.createBody(nil))
	si := decodeJSON[types.CLISignInCreateResponse](t, w)
	view := func(e *cliEnv, xff string) types.CLISignInView {
		return decodeJSON[types.CLISignInView](t, e.req(http.MethodGet, "/api/v1/me/cli-sign-ins/"+si.ID.String(), "", e.user, "10.1.1.1:6000", xff, ""))
	}
	if v := view(proxied, "198.51.100.7"); !v.AddressMatch || v.RequestAddress != "198.51.100.7" {
		t.Fatalf("same client behind a listed proxy = %+v, want a match on the forwarded address", v)
	}
	if v := view(proxied, "198.51.100.8"); v.AddressMatch || v.ApproverAddress != "198.51.100.8" {
		t.Fatalf("another client behind a listed proxy = %+v, want the mismatch", v)
	}
	plain := newCLIEnv(t)
	c = plain.client()
	w = plain.req(http.MethodPost, "/api/v1/cli/sign-ins", "", nil, "10.1.1.1:5000", "198.51.100.7", c.createBody(nil))
	si = decodeJSON[types.CLISignInCreateResponse](t, w)
	if v := view(plain, "198.51.100.8"); !v.AddressMatch || v.RequestAddress != "10.1.1.1" {
		t.Fatalf("behind an unlisted proxy = %+v, want both read as the proxy (X-Forwarded-For ignored)", v)
	}
}

func TestPG_CLISignIn_ApproveRunsMintGuards(t *testing.T) {
	e := newCLIEnv(t)
	c := e.client()
	si := c.create(nil)
	wantCLIReason(t, e.req(http.MethodPost, "/api/v1/me/cli-sign-ins/"+si.ID.String()+"/approve", adminToken, nil, "", "", ""),
		http.StatusForbidden, reasonAPITokenNoHuman)
	personal := decodeJSON[types.APIToken](t, e.call(e.user, http.MethodPost, "/api/v1/me/tokens", `{"name":"ci"}`))
	wantCLIReason(t, e.req(http.MethodPost, "/api/v1/me/cli-sign-ins/"+si.ID.String()+"/approve", personal.Token, nil, "", "", ""),
		http.StatusForbidden, reasonAPITokenFromAPIToken)
	_, g := e.signIn(e.user, nil)
	wantCLIReason(t, e.req(http.MethodPost, "/api/v1/me/cli-sign-ins/"+si.ID.String()+"/approve", g.AccessToken, nil, "", "", ""),
		http.StatusForbidden, reasonAPITokenFromAPIToken)
	payload, _ := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: "sub-super", Email: "super@corp.example", Role: oidc.RoleAdmin,
		UserType: types.UserTypeStandard, MemberMode: true, Expiry: time.Now().UTC().Add(time.Hour)})
	wantCLIReason(t, e.approve(signedSessionCookie(payload), si.ID), http.StatusConflict, reasonAPITokenMemberModeMint)
	if _, err := e.pg.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{SubjectType: types.CapabilitySubjectAll,
		Capability: capFeature, Value: featureAPIToken, Effect: types.CapabilityDeny}); err != nil {
		t.Fatal(err)
	}
	if w := e.approve(e.user, si.ID); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "capability_feature") {
		t.Fatalf("approve without the api_token feature = %d %s, want 403 capability_feature", w.Code, w.Body)
	}
	if got, _ := e.pg.GetCLISignIn(context.Background(), si.ID); got.Decision != "" {
		t.Fatalf("a refused approval decided the request: %+v", got)
	}
}

func TestPG_CLISignIn_TokenRefusals(t *testing.T) {
	e := newCLIEnv(t)
	c := e.client()
	si := c.create(nil)
	ap := decodeJSON[types.CLISignInApproveResponse](t, e.approve(e.user, si.ID))
	route := "/api/v1/cli/sign-ins/" + si.ID.String() + "/token"
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"a wrong verifier":        c.tokenWith(si.ID, ap.Code, "not-the-verifier", c.proofAt(route, si.ID, ap.Code, e.clock, c.priv)),
		"a proof by another key":  c.tokenWith(si.ID, ap.Code, c.verifier, c.proofAt(route, si.ID, ap.Code, e.clock, stranger)),
		"a proof 6 minutes old":   c.tokenWith(si.ID, ap.Code, c.verifier, c.proofAt(route, si.ID, ap.Code, e.clock.Add(-6*time.Minute), c.priv)),
		"a proof 6 minutes ahead": c.tokenWith(si.ID, ap.Code, c.verifier, c.proofAt(route, si.ID, ap.Code, e.clock.Add(6*time.Minute), c.priv)),
		"a wrong code":            c.token(si.ID, "0123456789abcdef0123456789abcdef"),
	} {
		t.Run(name, func(t *testing.T) { wantCLIReason(t, w, http.StatusUnauthorized, reasonCLISignInInvalid) })
	}
	// None of those burned K: the right code, verifier and proof still redeem it, once.
	if w := c.token(si.ID, ap.Code); w.Code != http.StatusCreated {
		t.Fatalf("token after refused attempts = %d %s", w.Code, w.Body)
	}
	wantCLIReason(t, c.token(si.ID, ap.Code), http.StatusUnauthorized, reasonCLISignInInvalid)
}

// A revoke or a sessions-only cut that lands between Approve and the token call refuses the mint: the
// token call re-checks the approving session as prepareMint's late check does.
func TestPG_CLISignIn_RevokeBetweenApproveAndToken(t *testing.T) {
	for _, body := range []string{`{"sub":"sub-user"}`, `{"sub":"sub-user","sessions_only":true}`} {
		t.Run(body, func(t *testing.T) {
			e := newCLIEnv(t)
			c := e.client()
			si := c.create(nil)
			ap := decodeJSON[types.CLISignInApproveResponse](t, e.approve(e.user, si.ID))
			if w := e.admin(http.MethodPost, "/api/v1/sessions/revoke", body); w.Code != http.StatusNoContent {
				t.Fatalf("revoke = %d %s", w.Code, w.Body)
			}
			if w := c.token(si.ID, ap.Code); w.Code != http.StatusForbidden {
				t.Fatalf("token after the revoke = %d %s, want refused", w.Code, w.Body)
			}
			if n := countCLISessions(t, e, "sub-user"); n != 0 {
				t.Fatalf("%d CLI sessions written after the revoke, want 0", n)
			}
		})
	}
}

func countCLISessions(t *testing.T, e *cliEnv, principal string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM cli_sessions WHERE principal = $1`, principal).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// K is single use and lives 60 s for loopback and 5 minutes for paste-back; a request lives 10 minutes.
func TestPG_CLISignIn_CodeAndRequestLifetimes(t *testing.T) {
	e := newCLIEnv(t, func(c *Config) { c.CLISignIn.Paste = true })
	for _, c := range []struct {
		mode      string
		ok, stale time.Duration
		life      int
	}{
		{types.CLISignInModeLoopback, 59 * time.Second, 61 * time.Second, 60},
		{types.CLISignInModePaste, 299 * time.Second, 301 * time.Second, 300},
	} {
		t.Run(c.mode, func(t *testing.T) {
			for _, wait := range []time.Duration{c.ok, c.stale} {
				cl := e.client()
				si := cl.create(func(r *types.CLISignInCreateRequest) {
					r.Mode = c.mode
					if c.mode == types.CLISignInModePaste {
						r.Port = 0
					}
				})
				ap := decodeJSON[types.CLISignInApproveResponse](t, e.approve(e.user, si.ID))
				if ap.CodeExpiresIn != c.life || (c.mode == types.CLISignInModePaste) != (ap.RedirectURL == "") {
					t.Fatalf("approve = %+v, want K for %d s, a redirect only for loopback", ap, c.life)
				}
				e.advance(wait)
				w := cl.token(si.ID, ap.Code)
				if wait == c.ok && w.Code != http.StatusCreated {
					t.Fatalf("token %s after approval = %d %s, want 201", wait, w.Code, w.Body)
				}
				if wait == c.stale {
					wantCLIReason(t, w, http.StatusUnauthorized, reasonCLISignInInvalid)
				}
			}
		})
	}
	cl := e.client()
	si := cl.create(nil)
	e.advance(10 * time.Minute)
	wantCLIReason(t, e.req(http.MethodGet, "/api/v1/me/cli-sign-ins/"+si.ID.String(), "", e.user, "", "", ""), http.StatusGone, reasonCLISignInExpired)
	wantCLIReason(t, e.approve(e.user, si.ID), http.StatusGone, reasonCLISignInExpired)
}

func TestPG_CLISignIn_PasteAndFileStoreSwitches(t *testing.T) {
	e := newCLIEnv(t)
	paste := e.client().createBody(func(r *types.CLISignInCreateRequest) { r.Mode, r.Port = types.CLISignInModePaste, 0 })
	wantCLIReason(t, e.req(http.MethodPost, "/api/v1/cli/sign-ins", "", nil, "", "", paste), http.StatusForbidden, reasonCLIPasteSignInOff)
	file := e.client().createBody(func(r *types.CLISignInCreateRequest) { r.Store = types.CLIStoreFile })
	if w := e.req(http.MethodPost, "/api/v1/cli/sign-ins", "", nil, "", "", file); w.Code != http.StatusCreated {
		t.Fatalf("file store with the default setting = %d %s, want allowed", w.Code, w.Body)
	}
	off := newCLIEnv(t, func(c *Config) { c.CLISignIn.FileStoreOff = true })
	wantCLIReason(t, off.req(http.MethodPost, "/api/v1/cli/sign-ins", "", nil, "", "", file), http.StatusForbidden, reasonCLIFileStoreOff)
}

// The file store is capped at 7 days whatever the operator's TTL, and an operator TTL bounds the idle
// lifetime too; the approve page shows the lifetimes that apply.
func TestPG_CLISession_FileStoreCappedAt7Days(t *testing.T) {
	e := newCLIEnv(t)
	start := e.clock
	c, g := e.signIn(e.user, func(r *types.CLISignInCreateRequest) { r.Store = types.CLIStoreFile })
	if !g.SessionExpiresAt.Equal(start.Add(7*24*time.Hour)) || g.Store != types.CLIStoreFile {
		t.Fatalf("file-store session = %+v, want it to end 7 days after approval", g)
	}
	e.advance(6 * 24 * time.Hour)
	w := c.refresh(g.SessionID, g.Secret)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh at day 6 = %d %s", w.Code, w.Body)
	}
	g = decodeJSON[types.CLISessionGrant](t, w)
	e.advance(24*time.Hour + time.Second)
	wantCLIReason(t, c.refresh(g.SessionID, g.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
	if ev := lastAuditEvent(t, e.h.audit.snapshot(), "cli.session.refresh_refused"); !strings.Contains(string(ev.Data), `"reason":"expired"`) {
		t.Fatalf("refresh_refused = %s, want reason expired", ev.Data)
	}

	short := newCLIEnv(t, func(c *Config) { c.CLISignIn.SessionTTL = 48 * time.Hour })
	si := short.client().create(nil)
	v := decodeJSON[types.CLISignInView](t, short.req(http.MethodGet, "/api/v1/me/cli-sign-ins/"+si.ID.String(), "", short.user, "", "", ""))
	if v.SessionTTLSeconds != 48*3600 || v.SessionIdleSeconds != 48*3600 {
		t.Fatalf("approve page under a 48 h TTL = %+v, want 48 h and an idle lifetime no longer than it", v)
	}
}

func TestPG_CLISession_RefreshRotatesAndReuseEndsTheSession(t *testing.T) {
	e := newCLIEnv(t)
	c, g1 := e.signIn(e.user, nil)
	e.advance(time.Minute)
	w := c.refresh(g1.SessionID, g1.Secret)
	g2 := decodeJSON[types.CLISessionGrant](t, w)
	if w.Code != http.StatusOK || g2.Secret == g1.Secret || g2.AccessToken == g1.AccessToken || g2.SessionID != g1.SessionID {
		t.Fatalf("refresh = %d %+v, want a new secret and a new access token on the same session", w.Code, g2)
	}
	// An old secret with a proof by a key that is not the device's changes nothing.
	route := "/api/v1/cli/sessions/" + g1.SessionID.String() + "/refresh"
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	wantCLIReason(t, c.refreshWith(g1.SessionID, g1.Secret, c.proofAt(route, g1.SessionID, g1.Secret, e.clock, stranger)),
		http.StatusUnauthorized, reasonCLISessionInvalid)
	if code, _ := e.me(g2.AccessToken); code != http.StatusOK {
		t.Fatalf("an invalid proof ended the session: GET /me = %d", code)
	}
	// The old secret with the device's own proof is a reuse: the session ends with every token it holds.
	wantCLIReason(t, c.refresh(g1.SessionID, g1.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
	ev := lastAuditEvent(t, e.h.audit.snapshot(), "cli.session.reuse_detected")
	if ev.Target != g1.SessionID.String() || !strings.Contains(string(ev.Data), `"machine":"pm-laptop"`) || !strings.Contains(string(ev.Data), `"address"`) {
		t.Fatalf("reuse_detected = %+v %s, want the session, the machine and the address", ev, ev.Data)
	}
	for _, tok := range []string{g1.AccessToken, g2.AccessToken} {
		if code, _ := e.me(tok); code != http.StatusUnauthorized {
			t.Fatalf("an access token of the reused session = %d, want 401", code)
		}
	}
	wantCLIReason(t, c.refresh(g2.SessionID, g2.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
	if got, _ := e.pg.GetCLISession(context.Background(), g1.SessionID); got.EndReason != types.CLISessionEndReuseDetected {
		t.Fatalf("session = %+v, want ended as reuse_detected", got)
	}
}

// Each cut refuses at refresh, also one written by a path that did not end the CLI session itself.
func TestPG_CLISession_EveryCutRefusesAtRefresh(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, reason string
		cut          func(e *cliEnv, g types.CLISessionGrant)
		wantCode     int
		ended        bool
	}{
		{"a revoke written elsewhere", "revoked", func(e *cliEnv, _ types.CLISessionGrant) {
			if _, err := e.pool.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ('sub-user', clock_timestamp())`); err != nil {
				e.t.Fatal(err)
			}
		}, http.StatusUnauthorized, true},
		{"a sessions-only cut written elsewhere", "revoked", func(e *cliEnv, _ types.CLISessionGrant) {
			if _, err := e.pool.Exec(ctx, `INSERT INTO oidc_session_cuts (sub, cut_at) VALUES ('user@corp.example', clock_timestamp())`); err != nil {
				e.t.Fatal(err)
			}
		}, http.StatusUnauthorized, true},
		{"deactivation", "deactivated", func(e *cliEnv, _ types.CLISessionGrant) {
			if _, err := e.pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "sub-user", Issuer: "https://idp.example", Email: "user@corp.example"}, time.Now().UTC()); err != nil {
				e.t.Fatal(err)
			}
			if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = now() WHERE principal = 'sub-user'`); err != nil {
				e.t.Fatal(err)
			}
		}, http.StatusUnauthorized, true},
		{"a stale role stamp", "role_stamp_stale", func(e *cliEnv, g types.CLISessionGrant) {
			e.srv.cfg.RoleStampTTL = time.Hour
			if _, err := e.pool.Exec(ctx, `UPDATE cli_sessions SET identity_stamped_at = now() - interval '2 hours' WHERE id = $1`, g.SessionID); err != nil {
				e.t.Fatal(err)
			}
		}, http.StatusUnauthorized, false},
		{"a lost api_token feature", "capability_feature", func(e *cliEnv, _ types.CLISessionGrant) {
			if _, err := e.pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{SubjectType: types.CapabilitySubjectAll, Capability: capFeature,
				Value: featureAPIToken, Effect: types.CapabilityDeny}); err != nil {
				e.t.Fatal(err)
			}
		}, http.StatusForbidden, false},
		{"Sign out on this computer's row", "ended", func(e *cliEnv, g types.CLISessionGrant) {
			if w := e.req(http.MethodDelete, "/api/v1/me/cli-sessions/"+g.SessionID.String(), "", e.user, "", "", ""); w.Code != http.StatusNoContent {
				e.t.Fatalf("sign out = %d %s", w.Code, w.Body)
			}
		}, http.StatusUnauthorized, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCLIEnv(t)
			cl, g := e.signIn(e.user, nil)
			e.advance(time.Minute)
			c.cut(e, g)
			w := cl.refresh(g.SessionID, g.Secret)
			if w.Code != c.wantCode {
				t.Fatalf("refresh after %s = %d %s, want %d", c.name, w.Code, w.Body, c.wantCode)
			}
			ev := lastAuditEvent(t, e.h.audit.snapshot(), "cli.session.refresh_refused")
			if !strings.Contains(string(ev.Data), `"reason":"`+c.reason+`"`) {
				t.Fatalf("refresh_refused = %s, want reason %s", ev.Data, c.reason)
			}
			got, _ := e.pg.GetCLISession(ctx, g.SessionID)
			if (got.EndedAt != nil) != c.ended {
				t.Fatalf("session after %s = %+v, want ended %v", c.name, got, c.ended)
			}
			if !c.ended && c.reason == "role_stamp_stale" {
				// A browser sign-in re-stamps the session, and the next refresh passes.
				if err := e.pg.RefreshAPITokenIdentity(ctx, "sub-user", oidc.RoleUser, types.UserTypeStandard, nil, false); err != nil {
					t.Fatal(err)
				}
				if w := cl.refresh(g.SessionID, g.Secret); w.Code != http.StatusOK {
					t.Fatalf("refresh after a browser sign-in = %d %s", w.Code, w.Body)
				}
			}
		})
	}
}

func TestPG_CLISession_IdleAndAbsoluteExpiry(t *testing.T) {
	idle := newCLIEnv(t, func(c *Config) { c.CLISignIn.SessionIdle = time.Hour })
	c, g := idle.signIn(idle.user, nil)
	idle.advance(59 * time.Minute)
	w := c.refresh(g.SessionID, g.Secret)
	if w.Code != http.StatusOK {
		t.Fatalf("refresh inside the idle lifetime = %d %s", w.Code, w.Body)
	}
	g = decodeJSON[types.CLISessionGrant](t, w)
	idle.advance(time.Hour)
	wantCLIReason(t, c.refresh(g.SessionID, g.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
	if got, _ := idle.pg.GetCLISession(context.Background(), g.SessionID); got.EndReason != types.CLISessionEndIdle {
		t.Fatalf("session = %+v, want ended as idle", got)
	}

	abs := newCLIEnv(t, func(c *Config) { c.CLISignIn.SessionTTL, c.CLISignIn.SessionIdle = 2*time.Hour, 2*time.Hour })
	c, g = abs.signIn(abs.user, nil)
	abs.advance(90 * time.Minute)
	w = c.refresh(g.SessionID, g.Secret)
	g2 := decodeJSON[types.CLISessionGrant](t, w)
	if w.Code != http.StatusOK || !g2.AccessExpiresAt.Equal(g.SessionExpiresAt) {
		t.Fatalf("refresh 30 min before the end = %d %+v, want an access token that ends with the session", w.Code, g2)
	}
	abs.advance(31 * time.Minute)
	wantCLIReason(t, c.refresh(g2.SessionID, g2.Secret), http.StatusUnauthorized, reasonCLISessionInvalid)
	if got, _ := abs.pg.GetCLISession(context.Background(), g.SessionID); got.EndReason != types.CLISessionEndExpired {
		t.Fatalf("session = %+v, want ended as expired", got)
	}
}

// Every session cut ends the person's CLI sessions and revokes their access and admin tokens at once:
// the next request with the access token is a 401 before any refresh. The sessions-only cut leaves the
// person's personal API token working.
func TestPG_CLISession_SessionCutsEndCLISessionsAtOnce(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		cut  func(e *cliEnv)
	}{
		{"revoke", func(e *cliEnv) { e.admin(http.MethodPost, "/api/v1/sessions/revoke", `{"sub":"sub-super"}`) }},
		{"Sign out everywhere", func(e *cliEnv) {
			e.admin(http.MethodPost, "/api/v1/sessions/revoke", `{"sub":"super@corp.example","sessions_only":true}`)
		}},
		{"revoke everyone", func(e *cliEnv) { e.admin(http.MethodPost, "/api/v1/sessions/revoke", `{"all":true}`) }},
		{"SCIM suspension", func(e *cliEnv) {
			if _, err := e.srv.revokePersonCredentialsBy(withActor(ctx, types.ActorSystem, "scim"), "sub-super", false); err != nil {
				e.t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCLIEnv(t)
			personal := decodeJSON[types.APIToken](t, e.call(e.super, http.MethodPost, "/api/v1/me/tokens", `{"name":"ci"}`))
			cl, g := e.signIn(e.super, nil)
			admin := cl.stepUp(e.super, g.SessionID)
			c.cut(e)
			for _, tok := range []string{g.AccessToken, admin.AccessToken} {
				if code, _ := e.me(tok); code != http.StatusUnauthorized {
					t.Fatalf("a CLI token after %s = %d, want 401 at once", c.name, code)
				}
			}
			if got, _ := e.pg.GetCLISession(ctx, g.SessionID); got.EndedAt == nil {
				t.Fatalf("CLI session after %s = %+v, want ended", c.name, got)
			}
			if c.name == "Sign out everywhere" {
				if code, _ := e.me(personal.Token); code != http.StatusOK {
					t.Fatalf("the personal API token after Sign out everywhere = %d, want it to keep working", code)
				}
				ev := lastAuditEvent(t, e.h.audit.snapshot(), "session.revoke")
				if !strings.Contains(string(ev.Data), `"cli_sessions_ended":1`) {
					t.Fatalf("session.revoke = %s, want cli_sessions_ended 1", ev.Data)
				}
			}
		})
	}
}

// stepUp runs wardyn login --admin on this computer's session and returns the cli_admin grant.
func (c *cliClient) stepUp(cookie *http.Cookie, session uuid.UUID) types.CLISessionGrant {
	c.e.t.Helper()
	si := c.create(func(r *types.CLISignInCreateRequest) { r.Admin, r.SessionID = true, &session })
	ap := decodeJSON[types.CLISignInApproveResponse](c.e.t, c.e.approve(cookie, si.ID))
	w := c.token(si.ID, ap.Code)
	if w.Code != http.StatusCreated {
		c.e.t.Fatalf("admin token = %d %s", w.Code, w.Body)
	}
	return decodeJSON[types.CLISessionGrant](c.e.t, w)
}

// A cli_access token publishes the role user for an admin; the step-up's cli_admin token publishes the
// real role, has no refresh, and stops when its session ends by any path.
func TestPG_CLISession_AdminStepUpAndClamp(t *testing.T) {
	e := newCLIEnv(t)
	cl, g := e.signIn(e.super, nil)
	if _, role := e.me(g.AccessToken); role != oidc.RoleUser {
		t.Fatalf("an admin's cli_access token publishes %q, want user", role)
	}
	admin := cl.stepUp(e.super, g.SessionID)
	if admin.Kind != types.APITokenKindCLIAdmin || admin.Secret != "" || admin.SessionID != g.SessionID || admin.AccessExpiresAt.Sub(e.clock) != time.Hour {
		t.Fatalf("step-up = %+v, want a 1-hour cli_admin token of the same session and no secret", admin)
	}
	if _, role := e.me(admin.AccessToken); role != oidc.RoleAdmin {
		t.Fatalf("the cli_admin token publishes %q, want admin", role)
	}
	list := decodeJSON[[]types.CLISession](t, e.req(http.MethodGet, "/api/v1/me/cli-sessions", "", e.super, "", "", ""))
	if len(list) != 1 || list[0].AdminExpiresAt == nil {
		t.Fatalf("Signed-in CLIs = %+v, want the step-up on its computer's row", list)
	}
	// Another computer's key cannot step up on this session, and an ended session has none.
	other := e.client()
	si := other.create(func(r *types.CLISignInCreateRequest) { r.Admin, r.SessionID = true, &g.SessionID })
	wantCLIReason(t, e.approve(e.super, si.ID), http.StatusConflict, reasonCLIAdminNeedsSession)

	for _, end := range []struct {
		name string
		do   func(c *cliClient, g types.CLISessionGrant)
	}{
		{"wardyn logout", func(c *cliClient, g types.CLISessionGrant) {
			e.req(http.MethodDelete, "/api/v1/me/cli-sessions/"+g.SessionID.String(), g.AccessToken, nil, "", "", "")
		}},
		{"per-device sign-out", func(c *cliClient, g types.CLISessionGrant) {
			e.req(http.MethodDelete, "/api/v1/me/cli-sessions/"+g.SessionID.String(), "", e.super, "", "", "")
		}},
		{"reuse detection", func(c *cliClient, g types.CLISessionGrant) {
			if w := c.refresh(g.SessionID, g.Secret); w.Code != http.StatusOK {
				e.t.Fatalf("refresh = %d %s", w.Code, w.Body)
			}
			c.refresh(g.SessionID, g.Secret)
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			e.advance(2 * time.Minute) // refills the per-address bucket (10, one per 10 s) this test spends
			c, g := e.signIn(e.super, nil)
			admin := c.stepUp(e.super, g.SessionID)
			end.do(c, g)
			if code, _ := e.me(admin.AccessToken); code != http.StatusUnauthorized {
				t.Fatalf("the cli_admin token after %s = %d, want 401", end.name, code)
			}
			ended, _ := e.pg.GetCLISession(context.Background(), g.SessionID)
			si := c.create(func(r *types.CLISignInCreateRequest) { r.Admin, r.SessionID = true, &ended.ID })
			wantCLIReason(t, e.approve(e.super, si.ID), http.StatusConflict, reasonCLIAdminNeedsSession)
		})
	}
}

// The clamp reaches every door: an admin's cli_access token is refused at POST /me/runners/tokens with
// capability_feature while no allow names self_host_runner (migration 0178), and their cli_admin token is not.
func TestPG_CLISession_AccessTokenClampedAtSelfHostRunner(t *testing.T) {
	e := newCLIEnv(t)
	if _, err := e.pg.PutSiteConfig(context.Background(), types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	cl, g := e.signIn(e.super, nil)
	w := e.req(http.MethodPost, "/api/v1/me/runners/tokens", g.AccessToken, nil, "", "", "{}")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "capability_feature") {
		t.Fatalf("an admin's cli_access token at /me/runners/tokens = %d %s, want 403 capability_feature", w.Code, w.Body)
	}
	admin := cl.stepUp(e.super, g.SessionID)
	if w := e.req(http.MethodPost, "/api/v1/me/runners/tokens", admin.AccessToken, nil, "", "", "{}"); w.Code != http.StatusCreated {
		t.Fatalf("the same admin's cli_admin token = %d %s, want 201", w.Code, w.Body)
	}
}

// The access token lasts 1 hour, or WARDYN_API_TOKEN_MAX_TTL when that is shorter, so a role or feature
// change reaches a live CLI within the hour.
func TestPG_CLISession_AccessTokenLivesAnHour(t *testing.T) {
	e := newCLIEnv(t, func(c *Config) { c.APITokenMaxTTL = 30 * time.Minute })
	start := e.clock
	cl, g := e.signIn(e.user, nil)
	if g.AccessExpiresAt.Sub(start) != 30*time.Minute {
		t.Fatalf("access token under a 30 min cap ends %s after approval, want 30m", g.AccessExpiresAt.Sub(start))
	}
	e.advance(10 * time.Minute)
	g2 := decodeJSON[types.CLISessionGrant](t, cl.refresh(g.SessionID, g.Secret))
	if g2.AccessExpiresAt.Sub(e.clock) != 30*time.Minute {
		t.Fatalf("refreshed access token = %+v, want 30 min from the refresh", g2)
	}
}
