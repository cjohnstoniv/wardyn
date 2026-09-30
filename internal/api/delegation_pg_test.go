// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gooidctest "github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The #1142 acceptance tests that need the whole path: a real identity
// provider signing the person's token, the real exchange, the Postgres
// delegate store, and a real run create. Skips without WARDYN_TEST_PG.

const (
	delegPortalClient = "portal-client"
	delegGroup        = "portal-users"
	delegPerson       = "pat-sub"
	delegPersonEmail  = "pat@corp.example"
)

// memRevocations is oidc.SessionRevocations in memory: a revoke stamps a
// cutoff, and anything issued at or before it is revoked.
type memRevocations struct {
	mu  sync.Mutex
	cut map[string]time.Time
}

func (m *memRevocations) IsSessionRevoked(_ context.Context, sub, _ string, issuedAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cut[sub]
	return ok && !issuedAt.After(c), nil
}

func (m *memRevocations) RevokeSub(_ context.Context, sub string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cut[sub] = time.Now()
	return nil
}

func (m *memRevocations) RevokeAll(ctx context.Context) error { return m.RevokeSub(ctx, "") }

// subjectRecordingIdentity records the human subject each run identity was
// minted for — what selects the run's secret namespace.
type subjectRecordingIdentity struct {
	identity.Provider
	mu   sync.Mutex
	subs map[uuid.UUID]string
}

func (p *subjectRecordingIdentity) MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string, operatorOwned bool) (identity.RunIdentity, error) {
	p.mu.Lock()
	p.subs[runID] = humanSub
	p.mu.Unlock()
	return p.Provider.MintRunIdentity(ctx, runID, humanSub, sponsor, audience, operatorOwned)
}

type delegationPG struct {
	ownerOnlyPG
	st       store.PG
	priv     *rsa.PrivateKey
	iss      string
	ids      *subjectRecordingIdentity
	secAdmin *http.Cookie
	nowMu    sync.Mutex
	shift    time.Duration
}

func newDelegationPG(t *testing.T) *delegationPG {
	t.Helper()
	e := &delegationPG{ownerOnlyPG: newOwnerOnlyPG(t)}
	e.st = store.NewPG(e.pool)
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e.priv = priv
	idp := &gooidctest.Server{PublicKeys: []gooidctest.PublicKey{{PublicKey: priv.Public(), KeyID: "k", Algorithm: "RS256"}}}
	srv := httptest.NewServer(idp)
	t.Cleanup(srv.Close)
	idp.SetIssuer(srv.URL)
	e.iss = srv.URL
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: srv.URL, ClientID: "wardyn-client", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback",
		RoleMap:     map[string]string{"root@corp.example": oidc.RoleAdmin, "sec@corp.example": oidc.RoleSecurityAdmin},
		DefaultRole: oidc.RoleUser,
	}, accessTestHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	h := e.h
	// The identity provider records on the chain wardynd builds: delegated
	// rows it writes (identity.mint during a run create) carry data.via too.
	base, err := embedded.New(nil, "wardyn.local", identitytest.NewMemRevocationStore(), audit.DelegationRecorder{Inner: h.audit})
	if err != nil {
		t.Fatal(err)
	}
	e.ids = &subjectRecordingIdentity{Provider: base, subs: map[uuid.UUID]string{}}
	h.idp = base
	h.srv.cfg.Identity = e.ids
	h.srv.cfg.Broker = broker.New(broker.NewPgxStore(e.pool), e.sec, h.audit, base, nil)
	h.srv.cfg.OIDC = auth
	h.srv.cfg.SessionRevocations = &memRevocations{cut: map[string]time.Time{}}
	h.srv.cfg.RunnerTarget = "docker"
	h.srv.cfg.Now = func() time.Time {
		e.nowMu.Lock()
		defer e.nowMu.Unlock()
		return time.Now().Add(e.shift)
	}
	h.srv.router = h.srv.routes()
	e.admin = accessSession(t, "root", "root@corp.example", oidc.RoleAdmin, []string{})
	e.secAdmin = accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})
	return e
}

func (e *delegationPG) advance(d time.Duration) {
	e.nowMu.Lock()
	defer e.nowMu.Unlock()
	e.shift += d
}

// registerPortal registers a portal as the super admin and returns its id and
// its one-time credential.
func (e *delegationPG) registerPortal(t *testing.T, group string) (uuid.UUID, string) {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/delegates", e.admin,
		`{"name":"front end","idp_client_id":"`+delegPortalClient+`","group":"`+group+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("register portal: %d %s", w.Code, w.Body.String())
	}
	var d types.Delegate
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.Credential, delegateCredentialPrefix) || d.Group != strings.ToLower(group) {
		t.Fatalf("registered portal = %+v", d)
	}
	return d.ID, d.Credential
}

// subjectToken is the person's access token for Wardyn, requested by the
// portal, as the identity provider signs it; mut edits the claims first.
func (e *delegationPG) subjectToken(t *testing.T, sub, email string, groups []string, mut func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{
		"iss": e.iss, "sub": sub, "aud": "wardyn-client", "azp": delegPortalClient,
		"email": email, "email_verified": true, "groups": groups,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
	if mut != nil {
		mut(c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return gooidctest.SignIDToken(e.priv, "k", "RS256", string(raw))
}

func (e *delegationPG) exchange(t *testing.T, id uuid.UUID, cred, subject string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token":      {subject},
		"subject_token_type": {tokenTypeAccessToken},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cred != "" {
		r.SetBasicAuth(id.String(), cred)
	}
	w := httptest.NewRecorder()
	panicFails(t, e.h.srv.Handler()).ServeHTTP(w, r)
	return w
}

// delegate runs a successful exchange and returns the delegated token.
func (e *delegationPG) delegate(t *testing.T, id uuid.UUID, cred, sub, email string, groups ...string) string {
	t.Helper()
	w := e.exchange(t, id, cred, e.subjectToken(t, sub, email, groups, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("exchange for %s: %d %s", sub, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	tok, _ := out["access_token"].(string)
	if !strings.HasPrefix(tok, delegatedTokenPrefix) {
		t.Fatalf("exchange response = %s", w.Body.String())
	}
	return tok
}

func (e *delegationPG) rows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func viaOf(t *testing.T, ev types.AuditEvent) types.DelegationVia {
	t.Helper()
	var d struct {
		Via *types.DelegationVia `json:"via"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil || d.Via == nil {
		t.Fatalf("row %s by %s carries no data.via: %s", ev.Action, ev.Actor, ev.Data)
	}
	return *d.Via
}

func (e *delegationPG) delegatedTokenCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM delegated_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func errorReason(w *httptest.ResponseRecorder) string {
	var b errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Reason
}

// TestDelegation_HappyPathActsAsThePerson is acceptance tests 1, 9 and 11: the
// portal's credential plus the person's own token buys a ten-minute token with
// no refresh; a run created with it is owned, audited and governed as the
// person — their identity subject, their secret namespace, their drive — with
// the portal on the run row and on every audit row the delegated requests
// wrote.
func TestDelegation_HappyPathActsAsThePerson(t *testing.T) {
	e := newDelegationPG(t)
	ctx := context.Background()
	portal, cred := e.registerPortal(t, "Portal-Users")

	// The person's own secret and drive, set up the way they would be.
	pat := accessSession(t, delegPerson, delegPersonEmail, oidc.RoleUser, []string{delegGroup})
	if w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/secrets/x", pat, `{"value":"pat-own-secret-value"}`); w.Code/100 != 2 {
		t.Fatalf("person writes their own secret: %d %s", w.Code, w.Body.String())
	}
	drive, err := e.st.UpsertUserDrive(ctx, *driveFixture(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertUserDriveGrant(ctx, *grantFixture(drive.ID, func(g *types.UserDriveGrant) { g.Subject = delegPerson }), false); err != nil {
		t.Fatal(err)
	}
	strict := e.storePolicy(t, "strict", "x", true)

	w := e.exchange(t, portal, cred, e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, refresh := out["refresh_token"]; refresh || out["expires_in"] != float64(600) || out["token_type"] != "Bearer" ||
		out["issued_token_type"] != tokenTypeAccessToken {
		t.Fatalf("exchange response = %s, want a ten-minute bearer and no refresh token", w.Body.String())
	}
	tok := out["access_token"].(string)
	ex := e.rows("delegation.exchange")
	if len(ex) != 1 || ex[0].Actor != delegateActor(portal) || ex[0].Target != delegPerson || ex[0].Outcome != "success" {
		t.Fatalf("delegation.exchange rows = %+v, want one success naming the portal and the person", ex)
	}
	var exData struct {
		Grant uuid.UUID `json:"grant"`
	}
	_ = json.Unmarshal(ex[0].Data, &exData)

	before := len(e.h.audit.snapshot())
	created := mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok,
		`{"agent":"claude-code","task":"t","policy_id":"`+strict+`","drive":{"enabled":true}}`))
	var run createRunResponse
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.CreatedBy != delegPerson || run.CreatedVia == nil || *run.CreatedVia != portal {
		t.Fatalf("run created_by = %q created_via = %v, want the person, via the portal", run.CreatedBy, run.CreatedVia)
	}
	e.ids.mu.Lock()
	sub := e.ids.subs[run.ID]
	e.ids.mu.Unlock()
	if sub != delegPerson {
		t.Fatalf("run identity minted for %q, want the person", sub)
	}
	rc := e.rows("run.create")
	if len(rc) != 1 || rc[0].Actor != delegPerson || rc[0].ActorType != types.ActorHuman {
		t.Fatalf("run.create rows = %+v, want one with the person as a human actor", rc)
	}
	if v := viaOf(t, rc[0]); v.Delegate != portal || v.Grant != exData.Grant {
		t.Fatalf("run.create via = %+v, want portal %s grant %s", v, portal, exData.Grant)
	}

	// The other delegable verbs, all on the person's own run.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/runs/" + run.ID.String(), ""},
		{http.MethodGet, "/api/v1/runs", ""},
		{http.MethodGet, "/api/v1/me", ""},
		{http.MethodPost, "/api/v1/runs/preflight", `{"agent":"claude-code","task":"t"}`},
		{http.MethodPatch, "/api/v1/runs/" + run.ID.String(), `{"ends_at":"` + time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339) + `"}`},
	} {
		// Reached the handler: never a 401, never the lane's own refusal. A
		// handler's own answer (PATCH's limits rule for a user) is the person's.
		if w := do(t, e.h.srv, c.method, c.path, tok, c.body); w.Code == http.StatusUnauthorized || errorReason(w) == "delegation_scope" {
			t.Fatalf("%s %s on the delegated lane: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), tok, ""); !strings.Contains(w.Body.String(), `"created_via":"`+portal.String()+`"`) {
		t.Fatalf("GET /runs/{id} = %s, want created_via", w.Body.String())
	}

	// Acceptance 11: every row the delegated requests wrote — the API's own,
	// the identity provider's, the detached launch's — names the portal.
	time.Sleep(300 * time.Millisecond) // the detached launch writes after the response
	swept := e.h.audit.snapshot()[before:]
	sweptActions := map[string]bool{}
	for _, ev := range swept {
		sweptActions[ev.Action] = true
	}
	if !sweptActions["run.create"] || !sweptActions["identity.mint"] {
		t.Fatalf("swept actions %v, want at least the API's run.create and the identity provider's identity.mint", sweptActions)
	}
	for _, ev := range swept {
		if v := viaOf(t, ev); v.Delegate != portal {
			t.Fatalf("row %s names portal %s, want %s", ev.Action, v.Delegate, portal)
		}
	}

	// The run's own identity (the sandbox's lane, not the portal's) reads the
	// person's secret namespace.
	if got := mintedToken(t, e.mint(t, delegPerson, created)); got != "pat-own-secret-value" {
		t.Fatalf("run minted %q, want the person's own secret", got)
	}

	// Stop is delegable too (after the mint: a kill revokes the run identity).
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", tok, ""); w.Code/100 != 2 {
		t.Fatalf("delegated kill of the person's own run: %d %s", w.Code, w.Body.String())
	}

	// Acceptance 9's other half: the drive resolved on the person. A person
	// the drive is not granted to gets the refusal, not the operator's drive.
	other := e.delegate(t, portal, cred, "other-sub", "other@corp.example", delegGroup)
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/runs", other, `{"agent":"claude-code","task":"t","drive":{"enabled":true}}`); w.Code == http.StatusCreated {
		t.Fatalf("a person with no drive grant launched with a drive: %s", w.Body.String())
	}

	// The credentials are stored hashed, never raw.
	var credHash, tokHash string
	if err := e.pool.QueryRow(ctx, `SELECT credential_sha256 FROM delegates WHERE id=$1`, portal).Scan(&credHash); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT token_sha256 FROM delegated_tokens WHERE id=$1`, exData.Grant).Scan(&tokHash); err != nil {
		t.Fatal(err)
	}
	if credHash != sha256Hex(cred) || tokHash != sha256Hex(tok) {
		t.Fatal("a portal credential or delegated token is not stored as its SHA-256")
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestDelegation_PortalCredentialAndRevocation is acceptance test 2 and owner
// decision Q6: a missing or wrong portal credential is 401 at the exchange; a
// revoked portal is 401 at the exchange AND on the next use of a token it
// already holds; the run it launched is left alone.
func TestDelegation_PortalCredentialAndRevocation(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	other, otherCred := e.registerPortal(t, delegGroup)
	subject := e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil)

	for name, w := range map[string]*httptest.ResponseRecorder{
		"no credential":                       e.exchange(t, portal, "", subject),
		"wrong credential":                    e.exchange(t, portal, delegateCredentialPrefix+"nope", subject),
		"another portal's id":                 e.exchange(t, other, cred, subject),
		"this portal's id, other cred":        e.exchange(t, portal, otherCred, subject),
		"a delegated token as the credential": e.exchange(t, portal, e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup), subject),
	} {
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"invalid_client"`) {
			t.Errorf("%s: %d %s, want 401 invalid_client", name, w.Code, w.Body.String())
		}
	}
	if rows := e.rows("auth.fail"); len(rows) == 0 || rows[0].Actor != delegationAuthActor {
		t.Errorf("auth.fail rows = %+v, want the refused portal authentications recorded", rows)
	}

	tok := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	created := mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok, `{"agent":"claude-code","task":"t"}`))
	var run createRunResponse
	_ = json.Unmarshal(created.Body.Bytes(), &run)
	stateBefore, _ := e.st.GetRun(context.Background(), run.ID)

	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/admin/delegates/"+portal.String(), e.secAdmin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke portal: %d %s", w.Code, w.Body.String())
	}
	if rows := e.rows("delegate.revoke"); len(rows) != 1 || rows[0].Target != portal.String() {
		t.Fatalf("delegate.revoke rows = %+v", rows)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked portal's outstanding token: %d, want 401", w.Code)
	}
	if w := e.exchange(t, portal, cred, subject); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked portal's exchange: %d, want 401", w.Code)
	}
	if after, _ := e.st.GetRun(context.Background(), run.ID); after.State != stateBefore.State {
		t.Fatalf("revoking the portal moved its run from %s to %s; runs it launched keep running", stateBefore.State, after.State)
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/admin/delegates/"+portal.String(), e.secAdmin, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second revoke: %d, want 404", w.Code)
	}
}

// TestDelegation_RegisterRefusesTheDeploymentsOwnClientID pins the registry's
// own refusal of a portal under Wardyn's OIDC client id (#1234), in any case
// or as its api:// App ID URI: a subject token issued to Wardyn would then
// pass the audience check for that portal.
// The exchange's verifier refuses it too; this is the first layer.
func TestDelegation_RegisterRefusesTheDeploymentsOwnClientID(t *testing.T) {
	e := newDelegationPG(t)
	own := e.h.srv.cfg.OIDC.ClientID()
	for _, id := range []string{own, " " + own + " ", strings.ToUpper(own), "api://" + own, "API://" + strings.ToUpper(own)} {
		w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/delegates", e.admin,
			`{"name":"front end","idp_client_id":"`+id+`","group":"`+delegGroup+`"}`)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonDelegateClientIDIsPortal {
			t.Fatalf("register under %q: %d %s, want 422 %s", id, w.Code, w.Body.String(), reasonDelegateClientIDIsPortal)
		}
	}
	if rows := e.rows("delegate.create"); len(rows) != 0 {
		t.Fatalf("delegate.create rows = %+v, want none", rows)
	}
	if list, err := e.st.ListDelegates(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("delegates = %+v (%v), want none registered", list, err)
	}
}

// TestDelegation_BadSubjectTokenMintsNothing is acceptance test 3: a subject
// token issued to another client, expired, from another issuer, alg=none, or
// badly signed is invalid_grant, and none of them mints a token.
func TestDelegation_BadSubjectTokenMintsNothing(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	tok := func(mut func(map[string]any)) string {
		return e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, mut)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	good := tok(nil)
	parts := strings.Split(good, ".")
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	cases := map[string]string{
		"wrong azp (issued to another client)": tok(func(c map[string]any) { c["azp"] = "other-client" }),
		"wrong aud":                            tok(func(c map[string]any) { c["aud"] = "other-client"; c["azp"] = "other-client" }),
		"expired":                              tok(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }),
		"wrong issuer":                         tok(func(c map[string]any) { c["iss"] = "https://elsewhere.invalid" }),
		"alg=none":                             none,
		"bad signature":                        gooidctest.SignIDToken(otherKey, "k", "RS256", string(claims)),
		"a delegated token (no chaining)":      e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup),
		"a reserved subject":                   tok(func(c map[string]any) { c["sub"] = "delegate:" + portal.String() }),
	}
	before := e.delegatedTokenCount(t)
	for name, subject := range cases {
		if w := e.exchange(t, portal, cred, subject); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_grant"`) {
			t.Errorf("%s: %d %s, want 400 invalid_grant", name, w.Code, w.Body.String())
		}
	}
	if after := e.delegatedTokenCount(t); after != before {
		t.Fatalf("delegated tokens %d -> %d: a refused exchange minted one", before, after)
	}
	denied := 0
	for _, ev := range e.rows("delegation.exchange") {
		if ev.Outcome == "denied" && ev.Actor == delegateActor(portal) {
			denied++
		}
	}
	if denied != len(cases) {
		t.Fatalf("denied delegation.exchange rows = %d, want %d", denied, len(cases))
	}
}

// TestDelegation_ScopeIsTheRegisteredGroup is acceptance test 4: a person
// outside the portal's group — or whose token names no groups at all — is 403
// with a denied row naming them.
func TestDelegation_ScopeIsTheRegisteredGroup(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	for name, groups := range map[string][]string{"another group": {"finance"}, "no groups": nil} {
		before := e.delegatedTokenCount(t)
		w := e.exchange(t, portal, cred, e.subjectToken(t, delegPerson, delegPersonEmail, groups, nil))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"access_denied"`) {
			t.Fatalf("%s: %d %s, want 403 access_denied", name, w.Code, w.Body.String())
		}
		if e.delegatedTokenCount(t) != before {
			t.Fatalf("%s: an out-of-scope exchange minted a token", name)
		}
	}
	rows := e.rows("delegation.exchange")
	if len(rows) != 2 || rows[0].Outcome != "denied" || rows[0].Target != delegPerson || !strings.Contains(string(rows[0].Data), "outside_scope") {
		t.Fatalf("delegation.exchange rows = %+v, want denied outside_scope rows naming the person", rows)
	}
}

// TestDelegation_PersonCutoffEndsDelegation is acceptance test 5: POST
// /sessions/revoke for the person ends their outstanding delegated tokens on
// the next request and refuses a new exchange of a token issued before it.
func TestDelegation_PersonCutoffEndsDelegation(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	subject := e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil)
	tok := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("delegated /me before the cutoff: %d", w.Code)
	}
	time.Sleep(10 * time.Millisecond)
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/sessions/revoke", e.secAdmin, `{"sub":"`+delegPerson+`"}`); w.Code/100 != 2 {
		t.Fatalf("revoke sessions: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("delegated token after the person's cutoff: %d, want 401", w.Code)
	}
	w := e.exchange(t, portal, cred, subject)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_grant"`) {
		t.Fatalf("exchange after the cutoff: %d %s, want 400 invalid_grant", w.Code, w.Body.String())
	}
	if rows := e.rows("delegation.exchange"); !strings.Contains(string(rows[len(rows)-1].Data), "session_revoked") {
		t.Fatalf("last exchange row = %+v, want session_revoked", rows[len(rows)-1])
	}
}

// TestDelegation_NeverDelegableRoutes is acceptance test 6 on the real stack
// (TestDelegation_AllowListRouteWalk walks every route): the routes the brief
// names answer 403 delegation_scope before any handler runs.
func TestDelegation_NeverDelegableRoutes(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	tok := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	created := mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok, `{"agent":"claude-code","task":"t"}`))
	var run createRunResponse
	_ = json.Unmarshal(created.Body.Bytes(), &run)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/v1/secrets/x", `{"value":"v"}`},
		{http.MethodPost, "/api/v1/me/tokens", `{"name":"n"}`},
		{http.MethodPost, "/api/v1/me/ssh-keys", `{}`},
		{http.MethodPost, "/api/v1/admin/delegates", `{}`},
		{http.MethodGet, "/api/v1/admin/delegates", ""},
		{http.MethodPost, "/api/v1/runs/" + run.ID.String() + "/revive", `{}`},
		{http.MethodPost, "/api/v1/approvals/" + uuid.NewString() + "/approve", `{}`},
		{http.MethodPost, "/api/v1/approvals/" + uuid.NewString() + "/deny", `{}`},
		{http.MethodGet, "/api/v1/audit", ""},
		{http.MethodPost, "/api/v1/sessions/revoke", `{"all":true}`},
	} {
		w := do(t, e.h.srv, c.method, c.path, tok, c.body)
		if w.Code != http.StatusForbidden || errorReason(w) != "delegation_scope" {
			t.Errorf("%s %s: %d %s, want 403 delegation_scope", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if n, err := e.st.ListAPITokensByPrincipal(context.Background(), delegPerson); err != nil || len(n) != 0 {
		t.Fatalf("api tokens for the person = %v (%v), want none", n, err)
	}
}

// TestDelegation_AdminPersonIsClampedToUserReach is acceptance test 7 and
// owner decision Q1: an admin acting through the portal gets ordinary user
// reach — no admin route, and a foreign run and its attach ticket are the same
// 404 a stranger gets.
func TestDelegation_AdminPersonIsClampedToUserReach(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	foreign := mustCreate(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs",
		accessSession(t, "mem", "mem@corp.example", oidc.RoleUser, []string{}), `{"agent":"claude-code","task":"t"}`))
	var run createRunResponse
	_ = json.Unmarshal(foreign.Body.Bytes(), &run)

	tok := e.delegate(t, portal, cred, "root", "root@corp.example", delegGroup)
	if ex := e.rows("delegation.exchange"); !strings.Contains(string(ex[len(ex)-1].Data), `"role":"admin"`) {
		t.Fatalf("exchange row = %s, want the person's real role recorded", ex[len(ex)-1].Data)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), tok, ""); w.Code != http.StatusNotFound {
		t.Fatalf("admin person's delegated read of a foreign run: %d, want 404", w.Code)
	}
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach-ticket", tok, ""); w.Code != http.StatusNotFound {
		t.Fatalf("admin person's delegated attach ticket on a foreign run: %d, want 404", w.Code)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/runs", tok, ""); strings.Contains(w.Body.String(), run.ID.String()) {
		t.Fatal("admin person's delegated run list shows a foreign run")
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); !strings.Contains(w.Body.String(), `"role":"user"`) {
		t.Fatalf("admin person's delegated /me = %s, want role user", w.Body.String())
	}
	// The same person's own session is an admin: the clamp is the lane's.
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), e.admin, ""); w.Code != http.StatusOK {
		t.Fatalf("admin's own session on the foreign run: %d, want 200", w.Code)
	}
}

// TestDelegation_CeilingResolvesOnThePerson is acceptance test 8: a person
// walled by a group-assigned governance profile is walled on the delegated
// lane too, and a partial group snapshot gets the refusal the cookie lane
// gives.
func TestDelegation_CeilingResolvesOnThePerson(t *testing.T) {
	e := newDelegationPG(t)
	ctx := context.Background()
	portal, cred := e.registerPortal(t, delegGroup)
	p, err := e.st.UpsertGovernanceProfile(ctx, types.GovernanceProfile{
		ID: uuid.New(), Name: "walled", Ceiling: govProfileSpec(), Limits: types.GovernanceLimits{DenyInteractive: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: "walled", ProfileID: p.ID, Priority: 1,
	}); err != nil {
		t.Fatal(err)
	}
	const interactive = `{"agent":"claude-code","task":"t","interactive":true}`
	walled := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup, "walled")
	cookie := accessSession(t, delegPerson, delegPersonEmail, oidc.RoleUser, []string{delegGroup, "walled"})
	viaPortal := do(t, e.h.srv, http.MethodPost, "/api/v1/runs", walled, interactive)
	viaCookie := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", cookie, interactive)
	if viaPortal.Code == http.StatusCreated || viaPortal.Code != viaCookie.Code || viaPortal.Body.String() != viaCookie.Body.String() {
		t.Fatalf("walled person: portal %d %s, cookie %d %s — want the same refusal", viaPortal.Code, viaPortal.Body.String(), viaCookie.Code, viaCookie.Body.String())
	}
	free := e.delegate(t, portal, cred, "free-sub", "free@corp.example", delegGroup)
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/runs", free, interactive); w.Code != http.StatusCreated {
		t.Fatalf("unwalled person on the same portal: %d %s, want 201", w.Code, w.Body.String())
	}

	// A group snapshot the IdP made partial (a claim this build cannot read).
	w := e.exchange(t, portal, cred, e.subjectToken(t, "part-sub", "part@corp.example", []string{delegGroup},
		func(c map[string]any) { c["roles"] = "not-a-list" }))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	partial, _ := out["access_token"].(string)
	if partial == "" {
		t.Fatalf("partial-snapshot exchange: %d %s", w.Code, w.Body.String())
	}
	partialCookie := signedAccessCookie(t, oidc.Session{V: oidc.SessionCodecVersion, Sub: "part-sub", Email: "part@corp.example",
		Role: oidc.RoleUser, UserType: types.UserTypeStandard, Expiry: time.Now().Add(time.Hour), Groups: []string{delegGroup}, GroupsTruncated: true})
	viaPortal = do(t, e.h.srv, http.MethodPost, "/api/v1/runs", partial, `{"agent":"claude-code","task":"t"}`)
	viaCookie = doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", partialCookie, `{"agent":"claude-code","task":"t"}`)
	if viaPortal.Code != http.StatusForbidden || errorReasonOrBody(viaPortal) != errorReasonOrBody(viaCookie) || viaPortal.Code != viaCookie.Code {
		t.Fatalf("partial snapshot: portal %d %s, cookie %d %s — want the cookie lane's refusal", viaPortal.Code, viaPortal.Body.String(), viaCookie.Code, viaCookie.Body.String())
	}
}

func errorReasonOrBody(w *httptest.ResponseRecorder) string {
	if r := errorReason(w); r != "" {
		return r
	}
	return w.Body.String()
}

// signedAccessCookie signs an arbitrary session with accessTestHMACKey.
func signedAccessCookie(t *testing.T, s oidc.Session) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(s)
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

// TestDelegation_NoChainingNoRefreshAndExpiry is acceptance test 12: a
// delegated token cannot be exchanged again (TestDelegation_BadSubjectToken-
// MintsNothing), no refresh token is issued (the happy path), and the token
// stops at its ten-minute lifetime.
func TestDelegation_NoChainingNoRefreshAndExpiry(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	tok := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	if w := e.exchange(t, portal, cred, tok); w.Code != http.StatusBadRequest {
		t.Fatalf("re-exchange of a delegated token: %d, want 400", w.Code)
	}
	e.advance(delegatedTokenTTL - time.Second)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("delegated token inside its lifetime: %d, want 200", w.Code)
	}
	e.advance(2 * time.Second)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("delegated token past its lifetime: %d, want 401", w.Code)
	}
}
