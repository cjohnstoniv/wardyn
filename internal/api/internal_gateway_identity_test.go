// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// testGatewayIdentityOn answers "is any provider on" yes for the coming window, so
// a shared matrix server's walk reaches the mint's own door.
func testGatewayIdentityOn(srv *Server) {
	c := &srv.gatewayIdentity
	c.onMu.Lock()
	defer c.onMu.Unlock()
	c.onAt, c.on, c.onRead, c.onErr = srv.cfg.Now(), true, true, nil
}

func testGatewayProvider() types.ModelProvider {
	return types.ModelProvider{
		ID: "corp-gw", UID: "uid-1", Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://gw.corp.example/llm",
		GatewayIdentity: &types.ModelProviderGatewayIdentity{Audience: "gw-aud"},
		Harnesses:       []types.ProviderHarness{{Harness: "claude-code", Path: "/anthropic"}},
	}
}

func testGatewaySite(p ...types.ModelProvider) types.SiteConfig {
	return types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: p}}
}

// gatewayStore is a store with a run row, the person's status and the sealed
// config table. Everything the mint reads can be set or broken here.
type gatewayStore struct {
	store.Store
	store.RunProxyConfigs
	store.ServiceIdentityStore

	mu      sync.Mutex
	site    types.SiteConfig
	siteErr error
	run     types.AgentRun
	sealed  map[uuid.UUID][]byte
	blocked bool
	blkErr  error
	service types.ServiceIdentity
	svcErr  error
	// blockedWho, when set, is the one principal IdentityBlocked answers blocked for.
	blockedWho string

	// The portals a delegated run was launched through, and a read of one that fails.
	*fakeDelegateStore
	delErr error

	// comps and runComps are the stored components and the run's snapshot of them (the MCP arm's reads).
	comps    map[uuid.UUID]types.Component
	runComps []types.RunComponent
	compErr  error
}

func (s *gatewayStore) GetComponent(_ context.Context, id uuid.UUID, owner string) (types.Component, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.compErr != nil {
		return types.Component{}, s.compErr
	}
	c, ok := s.comps[id]
	if !ok || c.Owner != owner {
		return types.Component{}, store.ErrNotFound
	}
	return c, nil
}

func (s *gatewayStore) ListRunComponents(_ context.Context, runID uuid.UUID) ([]types.RunComponent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.compErr != nil {
		return nil, s.compErr
	}
	return s.runComps, nil
}

func (s *gatewayStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.run.ID {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.run, nil
}

func (s *gatewayStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.site, s.siteErr
}

func (s *gatewayStore) set(f func(*gatewayStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *gatewayStore) IdentityBlocked(_ context.Context, who string, _ int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked || (s.blockedWho != "" && who == s.blockedWho), s.blkErr
}

func (s *gatewayStore) GetServiceIdentity(context.Context, string) (types.ServiceIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.service, s.svcErr
}

func (s *gatewayStore) PutRunProxyConfig(_ context.Context, id uuid.UUID, sealed []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed[id] = sealed
	return nil
}

func (s *gatewayStore) GetRunProxyConfig(_ context.Context, id uuid.UUID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.sealed[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return raw, nil
}

// revokedSessions answers every question "cut off", as a suspension of a person
// no identity row binds leaves it.
type revokedSessions struct{ fakeAuthzSessionRevocations }

func (revokedSessions) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return true, nil
}

func testGatewayKeySet(t *testing.T, s *gatewayidentity.Signer) jose.JSONWebKeySet {
	t.Helper()
	raw, err := s.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	return set
}

type gatewayFixture struct {
	h       *harness
	srv     *Server
	st      *gatewayStore
	signer  *gatewayidentity.Signer
	run     types.AgentRun
	token   string
	created []bool // every create argument the signer closure saw
	signErr error
	noKey   bool
	now     time.Time
}

func newGatewayFixture(t *testing.T) *gatewayFixture {
	t.Helper()
	raw, err := gatewayidentity.Rotate(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gatewayidentity.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	f := &gatewayFixture{h: newHarness(t), signer: signer, now: time.Now()}
	f.run = types.AgentRun{
		ID: uuid.New(), CreatedBy: "alice@example.com", State: types.RunRunning, Agent: "claude-code",
		ModelProviderID: "corp-gw", RunnerPoolExecutor: "runner:pool-exec", RunnerTarget: "docker",
		Placement: types.PlacementRemote, CreatedAt: time.Now().Add(-time.Minute),
	}
	f.st = &gatewayStore{site: testGatewaySite(testGatewayProvider()), run: f.run, sealed: map[uuid.UUID][]byte{},
		fakeDelegateStore: newFakeDelegateStore()}
	cfg := baseTestConfig(f.h, f.st)
	cfg.RunConfigKey = make([]byte, kek.DEKSize)
	cfg.Now = func() time.Time { return f.now }
	cfg.GatewayIdentity = func(_ context.Context, create bool) (*gatewayidentity.Signer, error) {
		f.created = append(f.created, create)
		switch {
		case f.signErr != nil:
			return nil, f.signErr
		case f.noKey:
			return nil, nil
		}
		return f.signer, nil
	}
	f.srv = New(cfg)
	f.token = f.h.mintRunToken(t, f.run.ID)
	f.seal(t, f.run.ID, testGatewayProvider())
	return f
}

// seal stores the route dispatch would have written for p.
func (f *gatewayFixture) seal(t *testing.T, runID uuid.UUID, p types.ModelProvider) {
	t.Helper()
	rt, ok := gatewayRouteOf(p)
	if !ok {
		t.Fatalf("no route for %+v", p)
	}
	raw, err := json.Marshal(proxy.Config{GatewayIdentity: []proxy.GatewayIdentityRoute{rt}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.storeRunProxyConfig(context.Background(), runID, raw); err != nil {
		t.Fatal(err)
	}
}

func (f *gatewayFixture) mint(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodPost, gatewayMintPath, f.token, `{"provider_id":"corp-gw"}`)
}

func (f *gatewayFixture) mintEvents() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == "gateway_identity.mint" {
			out = append(out, ev)
		}
	}
	return out
}

// A mint answers {"token","expires_at"} with an RFC3339 string, the only shape the
// proxy reads, and the token verifies at the served key set and says what the run
// and the provider say: nothing came from the request but the provider's id.
func TestGatewayMintNamesThePersonRunAndRunner(t *testing.T) {
	f := newGatewayFixture(t)
	w := f.mint(t)
	if w.Code != http.StatusOK {
		t.Fatalf("mint = %d %s", w.Code, w.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 {
		t.Errorf("answer keys = %v, want token and expires_at alone", body)
	}
	expires, _ := body["expires_at"].(string)
	expiresAt, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", expires, err)
	}
	var set jose.JSONWebKeySet
	jw := testGatewayGet(t, f.srv, http.MethodGet, testGatewayJWKSPath)
	if err := json.Unmarshal(jw.Body.Bytes(), &set); err != nil {
		t.Fatalf("key set %d %q: %v", jw.Code, jw.Body, err)
	}
	token, _ := body["token"].(string)
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var c gatewayidentity.Claims
	if err := parsed.Claims(set, &c); err != nil {
		t.Fatalf("the served set does not verify the minted token: %v", err)
	}
	if c.Issuer != "spiffe://wardyn.local" || c.Audience != "gw-aud" || c.Subject != "alice@example.com" ||
		c.Run != f.run.ID.String() || c.Runner != "runner:pool-exec" || c.Provider != "corp-gw" || c.Sponsor != "" {
		t.Errorf("claims = %+v", c)
	}
	if c.ExpiresAt != expiresAt.Unix() {
		t.Errorf("exp %d != expires_at %s", c.ExpiresAt, expires)
	}
	for _, create := range f.created {
		if create {
			t.Error("a mint asked for a key to be made")
		}
	}
}

// wardyn_runner is the run's executor exactly, or the target of a run with none. A
// run on a person's runner reaches neither: placement refuses its route, and the
// same run's relayed request is refused here before any claim is read.
func TestGatewayMintRunnerClaim(t *testing.T) {
	for _, tc := range []struct{ name, executor, want string }{
		{"remote executor", "remote.gpu-fleet", "remote.gpu-fleet"},
		{"no executor", "", "docker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatewayFixture(t)
			f.st.set(func(s *gatewayStore) { s.run.RunnerPoolExecutor = tc.executor })
			w := f.mint(t)
			if w.Code != http.StatusOK {
				t.Fatalf("mint = %d %s", w.Code, w.Body)
			}
			var body struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			parsed, err := jwt.ParseSigned(body.Token, []jose.SignatureAlgorithm{jose.ES256})
			if err != nil {
				t.Fatal(err)
			}
			var c gatewayidentity.Claims
			if err := parsed.Claims(testGatewayKeySet(t, f.signer), &c); err != nil {
				t.Fatal(err)
			}
			if c.Runner != tc.want {
				t.Errorf("wardyn_runner = %q, want %q", c.Runner, tc.want)
			}
			req := httptest.NewRequest(http.MethodPost, gatewayMintPath, strings.NewReader(`{"provider_id":"corp-gw"}`))
			req.Header.Set("Authorization", "Bearer "+f.token)
			req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: uuid.New()}))
			relayed := httptest.NewRecorder()
			f.srv.relayRoutes(f.srv.router).ServeHTTP(relayed, req)
			if relayed.Code != http.StatusForbidden || reasonOf(t, relayed) != string(placement.ReasonRelayRouteRefused) {
				t.Fatalf("relayed mint = %d %s", relayed.Code, relayed.Body)
			}
		})
	}
}

// One audit row for a run's first token on this replica, never holding the token,
// and every refusal has one.
func TestGatewayMintAudit(t *testing.T) {
	f := newGatewayFixture(t)
	var token string
	for range 3 {
		w := f.mint(t)
		if w.Code != http.StatusOK {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		token = body.Token
	}
	evs := f.mintEvents()
	if len(evs) != 1 || evs[0].Outcome != "success" || evs[0].Target != "corp-gw" {
		t.Fatalf("rows = %+v, want one success on the provider", evs)
	}
	if strings.Contains(string(evs[0].Data), token) {
		t.Error("the audit row holds the token")
	}
	for _, key := range []string{`"jti"`, `"expires_at"`, `"first":true`, `"provider":"corp-gw"`} {
		if !strings.Contains(string(evs[0].Data), key) {
			t.Errorf("row %s misses %s", evs[0].Data, key)
		}
	}
	f.st.set(func(s *gatewayStore) { s.blocked = true })
	if w := f.mint(t); w.Code != http.StatusForbidden {
		t.Fatalf("mint = %d", w.Code)
	}
	evs = f.mintEvents()
	if last := evs[len(evs)-1]; len(evs) != 2 || last.Outcome != "denied" || !strings.Contains(string(last.Data), reasonIdentityDeactivated) {
		t.Fatalf("rows = %+v, want the refusal recorded", evs)
	}
}

// A suspended person's run gets no token whether or not the kill their suspension
// runs has happened: here the run is still RUNNING, as after a failed kill.
func TestGatewayMintRefusesASuspendedPersonWithTheRunStillAlive(t *testing.T) {
	t.Run("identity row", func(t *testing.T) {
		f := newGatewayFixture(t)
		f.st.set(func(s *gatewayStore) { s.blocked = true })
		if w := f.mint(t); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonIdentityDeactivated {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
	})
	t.Run("cutoff of a principal no row binds", func(t *testing.T) {
		f := newGatewayFixture(t)
		f.srv.cfg.SessionRevocations = revokedSessions{}
		if w := f.mint(t); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonIdentityDeactivated {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
	})
	t.Run("status unreadable", func(t *testing.T) {
		f := newGatewayFixture(t)
		f.st.set(func(s *gatewayStore) { s.blkErr = errors.New("db down") })
		if w := f.mint(t); w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonGatewayIdentityUnavailable {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
	})
}

func TestGatewayMintServiceRun(t *testing.T) {
	retired := time.Now()
	for _, tc := range []struct {
		name    string
		service types.ServiceIdentity
		err     error
		blocked bool
		code    int
		reason  string
	}{
		{name: "live", service: types.ServiceIdentity{Name: "ci-bot", Sponsor: "sponsor@example.com"}, code: http.StatusOK},
		{name: "retired", service: types.ServiceIdentity{Name: "ci-bot", RetiredAt: &retired}, code: http.StatusForbidden, reason: reasonServiceIdentityInactive},
		{name: "suspended", service: types.ServiceIdentity{Name: "ci-bot", SuspendedAt: &retired}, code: http.StatusForbidden, reason: reasonServiceIdentityInactive},
		{name: "gone", err: store.ErrNotFound, code: http.StatusForbidden, reason: reasonServiceIdentityInactive},
		{name: "unreadable", err: errors.New("db down"), code: http.StatusServiceUnavailable, reason: reasonGatewayIdentityUnavailable},
		{name: "sponsor suspended", service: types.ServiceIdentity{Name: "ci-bot", Sponsor: "sponsor@example.com"}, blocked: true, code: http.StatusForbidden, reason: reasonIdentityDeactivated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatewayFixture(t)
			f.token = mintRunTokenAs(t, f.h, f.run.ID, types.ServicePrincipalPrefix+"ci-bot")
			f.st.set(func(s *gatewayStore) { s.service, s.svcErr, s.blocked = tc.service, tc.err, tc.blocked })
			w := f.mint(t)
			if w.Code != tc.code {
				t.Fatalf("mint = %d %s", w.Code, w.Body)
			}
			if tc.reason != "" {
				if got := reasonOf(t, w); got != tc.reason {
					t.Fatalf("reason %q, want %q", got, tc.reason)
				}
				return
			}
			var body struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			parsed, err := jwt.ParseSigned(body.Token, []jose.SignatureAlgorithm{jose.ES256})
			if err != nil {
				t.Fatal(err)
			}
			var c gatewayidentity.Claims
			if err := parsed.Claims(testGatewayKeySet(t, f.signer), &c); err != nil {
				t.Fatal(err)
			}
			if c.Subject != types.ServicePrincipalPrefix+"ci-bot" || c.Sponsor != "sponsor@example.com" {
				t.Errorf("claims = %+v", c)
			}
		})
	}
}

// Every way the provider can have moved from the sealed route refuses, naming the
// new-run step; editing it back passes. A credential cached by the run is no way
// round this, since the mint reads the live record each time.
func TestGatewayMintRefusesAProviderThatChanged(t *testing.T) {
	edits := map[string]func(*types.ModelProvider){
		"base_url":     func(p *types.ModelProvider) { p.BaseURL = "https://other.corp.example/llm" },
		"port":         func(p *types.ModelProvider) { p.BaseURL = "https://gw.corp.example:8443/llm" },
		"path":         func(p *types.ModelProvider) { p.BaseURL = "https://gw.corp.example/other" },
		"audience":     func(p *types.ModelProvider) { p.GatewayIdentity.Audience = "new-aud" },
		"identity off": func(p *types.ModelProvider) { p.GatewayIdentity = nil },
		"disabled":     func(p *types.ModelProvider) { p.Disabled = true },
		"recreated":    func(p *types.ModelProvider) { p.UID = "uid-2" },
		"harness moved": func(p *types.ModelProvider) {
			p.Harnesses = []types.ProviderHarness{{Harness: "codex-cli", Path: "/v1"}}
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			f := newGatewayFixture(t)
			testGatewayIdentityOn(f.srv) // the edit may turn the last provider off; the window has not passed
			p := testGatewayProvider()
			edit(&p)
			f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(p) })
			w := f.mint(t)
			if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayIdentityChanged {
				t.Fatalf("mint = %d %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), "Create a new run") {
				t.Errorf("no new-run step: %s", w.Body)
			}
			f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(testGatewayProvider()) })
			if w := f.mint(t); w.Code != http.StatusOK {
				t.Fatalf("edited back: mint = %d %s", w.Code, w.Body)
			}
		})
	}
	t.Run("removed", func(t *testing.T) {
		f := newGatewayFixture(t)
		testGatewayIdentityOn(f.srv)
		f.st.set(func(s *gatewayStore) { s.site = testGatewaySite() })
		if w := f.mint(t); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayIdentityChanged {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
	})
}

// A lifetime saved after the run was dispatched is not part of the sealed route: the next
// mint still passes, and the token lives as long as the live row now says.
func TestGatewayMintTakesTheLifetimeFromTheLiveProvider(t *testing.T) {
	f := newGatewayFixture(t)
	life := func() int64 {
		w := f.mint(t)
		if w.Code != http.StatusOK {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
		var body struct{ Token string }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		parsed, err := jwt.ParseSigned(body.Token, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatal(err)
		}
		var c gatewayidentity.Claims
		if err := parsed.UnsafeClaimsWithoutVerification(&c); err != nil {
			t.Fatal(err)
		}
		return c.ExpiresAt - c.IssuedAt
	}
	if got := life(); got != 600 {
		t.Errorf("unset life = %d s, want 600", got)
	}
	p := testGatewayProvider()
	p.GatewayIdentity.TokenLifetimeSeconds = 90
	f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(p) })
	if got := life(); got != 90 {
		t.Errorf("life after the edit = %d s, want 90", got)
	}
}

// The body names a provider; the run's sealed routes decide whether it is this
// run's. One with identity on that the run never started with is refused.
func TestGatewayMintRefusesAProviderNotOnTheRun(t *testing.T) {
	f := newGatewayFixture(t)
	other := testGatewayProvider()
	other.ID, other.UID = "other-gw", "uid-9"
	f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(testGatewayProvider(), other) })
	w := do(t, f.srv, http.MethodPost, gatewayMintPath, f.token, `{"provider_id":"other-gw"}`)
	if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayIdentityChanged {
		t.Fatalf("mint = %d %s", w.Code, w.Body)
	}
	f.st.set(func(s *gatewayStore) { delete(s.sealed, f.run.ID) })
	if w := f.mint(t); w.Code != http.StatusForbidden {
		t.Fatalf("no sealed config: mint = %d %s", w.Code, w.Body)
	}
	if w := do(t, f.srv, http.MethodPost, gatewayMintPath, f.token, `{"provider_id":"corp-gw","run_id":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown body field: mint = %d %s", w.Code, w.Body)
	}
}

// The signing key is read and never made here, and a key that cannot be read fails
// the mint closed, not as an unsigned or empty answer.
func TestGatewayMintFailsClosedWithoutTheKey(t *testing.T) {
	for name, set := range map[string]func(*gatewayFixture){
		"unreadable": func(f *gatewayFixture) { f.signErr = errors.New("decrypt failed") },
		"no row":     func(f *gatewayFixture) { f.noKey = true },
		"not wired":  func(f *gatewayFixture) { f.srv.cfg.GatewayIdentity = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newGatewayFixture(t)
			set(f)
			testGatewayIdentityOn(f.srv)
			w := f.mint(t)
			if w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonGatewayIdentityUnavailable {
				t.Fatalf("mint = %d %s", w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), "decrypt") {
				t.Errorf("the answer names the cause: %s", w.Body)
			}
		})
	}
}

// The mint keeps internalAuth's refusals: it is not in the mountInternalRoutes
// group, so nothing else would have pinned them.
func TestGatewayMintBehindInternalAuth(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		f := newGatewayFixture(t)
		f.st.set(func(s *gatewayStore) { s.run.State, s.run.UpdatedAt = types.RunCompleted, time.Now().Add(-time.Hour) })
		if w := f.mint(t); w.Code != http.StatusForbidden || reasonOf(t, w) != "run_terminal" {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
		if evs := f.mintEvents(); len(evs) != 0 {
			t.Errorf("the handler ran: %+v", evs)
		}
	})
	t.Run("not started", func(t *testing.T) {
		f := newGatewayFixture(t)
		f.st.set(func(s *gatewayStore) { s.run.State = types.RunQueued })
		if w := f.mint(t); w.Code != http.StatusForbidden {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
	})
	t.Run("no token", func(t *testing.T) {
		f := newGatewayFixture(t)
		f.token = ""
		if w := f.mint(t); w.Code != http.StatusUnauthorized {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
	})
	t.Run("relayed", func(t *testing.T) {
		f := newGatewayFixture(t)
		req := httptest.NewRequest(http.MethodPost, gatewayMintPath, strings.NewReader(`{"provider_id":"corp-gw"}`))
		req.Header.Set("Authorization", "Bearer "+f.token)
		req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: uuid.New()}))
		w := httptest.NewRecorder()
		f.srv.relayRoutes(f.srv.router).ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || reasonOf(t, w) != string(placement.ReasonRelayRouteRefused) {
			t.Fatalf("relayed mint = %d %s", w.Code, w.Body)
		}
	})
	t.Run("a local run off the relay", func(t *testing.T) {
		f := newGatewayFixture(t)
		runner := uuid.New()
		f.st.set(func(s *gatewayStore) { s.run.Placement, s.run.RunnerID = types.PlacementLocal, &runner })
		if w := f.mint(t); w.Code != http.StatusForbidden || reasonOf(t, w) != string(placement.ReasonRelayRequired) {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
	})
}

// Off, the mint answers as a path no route holds: same status, headers and body at
// every method, with and without a console and a base path, whatever the bearer.
// The sibling is the comparison, so the two cannot drift apart.
func TestGatewayMintOffEqualsAnUnregisteredPath(t *testing.T) {
	const sibling = "/api/v1/internal/not-a-route"
	uiDir := testGatewayUIDir(t)
	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}
	// Under the /api/v1 mount no method reaches a console route, so the answer is 404 in all four.
	for _, conf := range []struct {
		ui, base string
		status   int
	}{
		{"", "", http.StatusNotFound},
		{"", "/wardyn", http.StatusNotFound},
		{uiDir, "", http.StatusNotFound},
		{uiDir, "/wardyn", http.StatusNotFound},
	} {
		ui, base := conf.ui, conf.base
		for _, bearer := range []string{"", "not-a-token"} {
			srv := testGatewayServer(t, ui, base, nil)
			for _, method := range methods {
				ask := func(path string) *httptest.ResponseRecorder {
					r := httptest.NewRequest(method, base+path, strings.NewReader(`{"provider_id":"p"}`))
					if bearer != "" {
						r.Header.Set("Authorization", "Bearer "+bearer)
					}
					w := httptest.NewRecorder()
					panicFails(t, srv.Handler()).ServeHTTP(w, r)
					return w
				}
				got, want := ask(gatewayMintPath), ask(sibling)
				if got.Code != want.Code || got.Body.String() != want.Body.String() || !reflect.DeepEqual(got.Header(), want.Header()) {
					t.Errorf("ui=%v base=%q %s: route %d %v %q\nsibling %d %v %q", ui != "", base, method, got.Code, got.Header(), got.Body, want.Code, want.Header(), want.Body)
				}
				if got.Code != conf.status {
					t.Errorf("ui=%v base=%q %s: off answered %d, want %d", ui != "", base, method, got.Code, conf.status)
				}
			}
		}
	}
}

// The key set and the mint both follow the provider setting, past the signer a
// replica keeps for life, within the window: on; the last provider off with its key
// row left; disabled and enabled again; and a failed read.
func TestGatewayOnGate(t *testing.T) {
	jwks := func(t *testing.T, f *gatewayFixture) int {
		return testGatewayGet(t, f.srv, http.MethodGet, testGatewayJWKSPath).Code
	}
	step := func(f *gatewayFixture, edit func(*gatewayStore)) {
		f.st.set(edit)
		f.now = f.now.Add(gatewayOnWindow)
	}
	offProvider := testGatewayProvider()
	offProvider.GatewayIdentity = nil
	disabled := testGatewayProvider()
	disabled.Disabled = true

	t.Run("last provider off with the key kept", func(t *testing.T) {
		f := newGatewayFixture(t)
		if got := jwks(t, f); got != http.StatusOK {
			t.Fatalf("on: %d", got)
		}
		f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(offProvider) })
		if got := jwks(t, f); got != http.StatusOK {
			t.Errorf("inside the window: %d, want the kept answer", got)
		}
		step(f, func(*gatewayStore) {})
		if got := jwks(t, f); got != http.StatusNotFound {
			t.Errorf("off: key set %d, want 404", got)
		}
		if w := f.mint(t); w.Code != http.StatusNotFound {
			t.Errorf("off: mint %d %s, want the unheld path's 404", w.Code, w.Body)
		}
	})
	t.Run("disable and enable again", func(t *testing.T) {
		f := newGatewayFixture(t)
		step(f, func(s *gatewayStore) { s.site = testGatewaySite(disabled) })
		if got := jwks(t, f); got != http.StatusNotFound {
			t.Errorf("disabled: %d", got)
		}
		step(f, func(s *gatewayStore) { s.site = testGatewaySite(testGatewayProvider()) })
		if got := jwks(t, f); got != http.StatusOK {
			t.Errorf("enabled again: %d", got)
		}
		if w := f.mint(t); w.Code != http.StatusOK {
			t.Errorf("enabled again: mint %d %s", w.Code, w.Body)
		}
	})
	t.Run("a failed read never publishes", func(t *testing.T) {
		f := newGatewayFixture(t)
		boom := errors.New("db down")
		f.st.set(func(s *gatewayStore) { s.siteErr = boom })
		if got := jwks(t, f); got != http.StatusServiceUnavailable {
			t.Errorf("first read failed: %d, want 503", got)
		}
		if w := f.mint(t); w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonGatewayIdentityUnavailable {
			t.Errorf("first read failed: mint %d %s", w.Code, w.Body)
		}
		step(f, func(s *gatewayStore) { s.siteErr = nil })
		if got := jwks(t, f); got != http.StatusOK {
			t.Errorf("recovered: %d", got)
		}
		step(f, func(s *gatewayStore) { s.siteErr = boom })
		if got := jwks(t, f); got != http.StatusServiceUnavailable {
			t.Errorf("failed after on: %d, want 503", got)
		}
	})
	t.Run("an install read as off stays off through an outage", func(t *testing.T) {
		f := newGatewayFixture(t)
		step(f, func(s *gatewayStore) { s.site = testGatewaySite() })
		if got := jwks(t, f); got != http.StatusNotFound {
			t.Fatalf("off: %d", got)
		}
		step(f, func(s *gatewayStore) { s.siteErr = errors.New("db down") })
		if got := jwks(t, f); got != http.StatusNotFound {
			t.Errorf("outage: %d, want the same 404", got)
		}
	})
	t.Run("the read is one per window", func(t *testing.T) {
		f := newGatewayFixture(t)
		f.st.set(func(s *gatewayStore) { s.siteErr = errors.New("db down") })
		jwks(t, f)
		f.st.set(func(s *gatewayStore) { s.siteErr = nil })
		if got := jwks(t, f); got != http.StatusServiceUnavailable {
			t.Errorf("inside the window after a failure: %d, want the kept 503", got)
		}
	})
}

func testGatewayPutStore(cfg types.SiteConfig) *gatewayPutStore {
	return &gatewayPutStore{fakeSiteConfigStore: &fakeSiteConfigStore{cfg: cfg}}
}

// gatewayPutStore is the save doors' store with the sealed config table present.
type gatewayPutStore struct {
	*fakeSiteConfigStore
	store.RunProxyConfigs
}

const testGatewayPutOne = `{"providers":[{"id":"corp-gw","kind":"custom_endpoint","base_url":"https://gw.corp.example/llm",` +
	`"harnesses":[{"harness":"claude-code","path":"/anthropic"}],"gateway_identity":{"audience":"gw-aud"}}]}`

// The first save that turns identity on makes the key, so the key set can serve it
// before any run. A failed create saves nothing. A later save does not ask, so a
// damaged key row cannot wedge the document that would turn identity off.
func TestGatewayIdentityFirstEnableMakesTheKey(t *testing.T) {
	for _, door := range []struct{ name, path, body string }{
		{"model providers", "/api/v1/model-providers", testGatewayPutOne},
		{"site config", "/api/v1/site-config", `{"model_providers":` + testGatewayPutOne + `}`},
	} {
		t.Run(door.name, func(t *testing.T) {
			setup := func(t *testing.T, stored types.SiteConfig, err error) (*Server, *gatewayPutStore, *[]bool) {
				t.Helper()
				st := testGatewayPutStore(stored)
				h := newHarness(t)
				cfg := baseTestConfig(h, st)
				cfg.RunConfigKey = make([]byte, kek.DEKSize)
				raw, rerr := gatewayidentity.Rotate(nil, false, time.Now())
				if rerr != nil {
					t.Fatal(rerr)
				}
				signer, rerr := gatewayidentity.New(raw)
				if rerr != nil {
					t.Fatal(rerr)
				}
				var asked []bool
				cfg.GatewayIdentity = func(_ context.Context, create bool) (*gatewayidentity.Signer, error) {
					asked = append(asked, create)
					return signer, err
				}
				return New(cfg), st, &asked
			}
			t.Run("first", func(t *testing.T) {
				srv, st, asked := setup(t, types.SiteConfig{}, nil)
				if w := do(t, srv, http.MethodPut, door.path, adminToken, door.body); w.Code != http.StatusOK {
					t.Fatalf("PUT = %d %s", w.Code, w.Body)
				}
				if len(*asked) != 1 || !(*asked)[0] {
					t.Errorf("create asked %v, want once with true", *asked)
				}
				if st.putSeen == nil || st.putSeen.ModelProviders.Providers[0].GatewayIdentity.Audience != "gw-aud" {
					t.Errorf("stored %+v", st.putSeen)
				}
			})
			t.Run("key failure saves nothing", func(t *testing.T) {
				srv, st, _ := setup(t, types.SiteConfig{}, errors.New("no kek"))
				w := do(t, srv, http.MethodPut, door.path, adminToken, door.body)
				if w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonGatewayIdentityUnavailable {
					t.Fatalf("PUT = %d %s", w.Code, w.Body)
				}
				if !strings.Contains(w.Body.String(), "wasn't saved") {
					t.Errorf("body %s", w.Body)
				}
				if st.putSeen != nil {
					t.Error("a failed create stored the provider")
				}
			})
			t.Run("later saves do not ask", func(t *testing.T) {
				srv, _, asked := setup(t, testGatewaySite(testGatewayProvider()), errors.New("damaged row"))
				if w := do(t, srv, http.MethodPut, door.path, adminToken, door.body); w.Code != http.StatusOK {
					t.Fatalf("PUT = %d %s", w.Code, w.Body)
				}
				if len(*asked) != 0 {
					t.Errorf("create asked %v, want never", *asked)
				}
			})
		})
	}
	t.Run("an install keeping no run config cannot turn it on", func(t *testing.T) {
		fake := &fakeSiteConfigStore{}
		srv, _ := newSiteConfigHarness(t, fake)
		srv.cfg.GatewayIdentity = func(context.Context, bool) (*gatewayidentity.Signer, error) {
			t.Error("the key was asked for")
			return nil, nil
		}
		if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, testGatewayPutOne); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("PUT = %d %s", w.Code, w.Body)
		}
		if fake.putSeen != nil {
			t.Error("stored")
		}
	})
}

// Save's refusals, in the words the console maps to a field.
func TestGatewayIdentitySaveRefusals(t *testing.T) {
	row := func(id, kind, baseURL, aud string) string {
		base := ""
		if baseURL != "" {
			base = `"base_url":"` + baseURL + `",`
		}
		harness := `{"harness":"claude-code"}`
		if kind == "custom_endpoint" {
			harness = `{"harness":"claude-code","path":"/anthropic"}`
		}
		return `{"id":"` + id + `","kind":"` + kind + `",` + base + `"harnesses":[` + harness + `],"gateway_identity":{"audience":"` + aud + `"}}`
	}
	put := func(rows ...string) string { return `{"providers":[` + strings.Join(rows, ",") + `]}` }
	for _, tc := range []struct {
		name, body, want, field string
	}{
		{"subscription", put(row("p", "anthropic_subscription", "", "a")), "gateway_identity is not available for anthropic_subscription providers", ""},
		{"key row with no base_url", put(row("p", "anthropic_api_key", "", "a")), "gateway_identity needs a base_url, or the token would go to the vendor", "base_url"},
		{"no audience", put(row("p", "custom_endpoint", "https://gw.corp.example", "  ")), "gateway_identity: audience is required", "gateway_identity.audience"},
		{"dot segment", put(row("p", "custom_endpoint", "https://gw.corp.example/a/../b", "a")), "no dot segment", "base_url"},
		{"one audience on two addresses", put(row("p", "custom_endpoint", "https://a.corp.example", "aud"), row("q", "custom_endpoint", "https://b.corp.example", "aud")),
			`gateway_identity.audience "aud" is already used by provider "p"`, "gateway_identity.audience"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSiteConfigStore{}
			srv, _ := newSiteConfigHarness(t, fake)
			w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, tc.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), strings.ReplaceAll(tc.want, `"`, `\"`)) {
				t.Fatalf("PUT = %d %s, want 400 %q", w.Code, w.Body, tc.want)
			}
			if tc.field != "" && !strings.Contains(w.Body.String(), `"field":"`+tc.field+`"`) {
				t.Errorf("no field %q: %s", tc.field, w.Body)
			}
			if fake.putSeen != nil {
				t.Error("stored")
			}
		})
	}
	// One destination in two spellings, a real shared gateway, keeps one audience.
	block := &types.ModelProviders{Providers: []types.ModelProvider{testGatewayProvider(), testGatewayProvider()}}
	block.Providers[1].ID, block.Providers[1].BaseURL = "corp-gw-2", "https://GW.corp.example.:443/llm/"
	if err := validateGatewayAudiences(block, nil); err != nil {
		t.Errorf("one destination refused: %v", err)
	}
}

// The token lifetime is unset or from 60 to 600 seconds; anything else is refused with the
// provider named and the field set.
func TestGatewayIdentityTokenLifetimeBounds(t *testing.T) {
	for _, tc := range []struct {
		secs int
		ok   bool
	}{{0, true}, {60, true}, {120, true}, {600, true}, {59, false}, {601, false}, {-1, false}, {86400, false}} {
		p := testGatewayProvider()
		p.GatewayIdentity.TokenLifetimeSeconds = tc.secs
		err := validateProviderGatewayIdentity(p)
		if tc.ok {
			if err != nil {
				t.Errorf("%d seconds refused: %v", tc.secs, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "gateway_identity: token_lifetime_seconds must be between 60 and 600") {
			t.Errorf("%d seconds: err = %v", tc.secs, err)
		}
	}
}

// A base_url change that keeps the audience is refused, and a new audience passes,
// as does restoring the original pair from any other pair.
func TestGatewayIdentityEditMustReenterTheAudience(t *testing.T) {
	stored := &types.ModelProviders{Providers: []types.ModelProvider{testGatewayProvider()}}
	moved := testGatewayProvider()
	moved.BaseURL = "https://other.corp.example/llm"
	err := validateGatewayIdentityEdit(&types.ModelProviders{Providers: []types.ModelProvider{moved}}, stored)
	if err == nil || !strings.Contains(err.Error(), "moved to a new address, so gateway_identity.audience can't stay the same: use a different one, or save with identity off first") {
		t.Fatalf("got %v", err)
	}
	if refusalField(err) != "gateway_identity.audience" {
		t.Errorf("field %q", refusalField(err))
	}
	moved.GatewayIdentity = &types.ModelProviderGatewayIdentity{Audience: "other-aud"}
	block := &types.ModelProviders{Providers: []types.ModelProvider{moved}}
	if err := validateGatewayIdentityEdit(block, stored); err != nil {
		t.Errorf("new audience refused: %v", err)
	}
	// A kind change does not excuse it: the audience still names the old address.
	rekinded := testGatewayProvider()
	rekinded.Kind, rekinded.BaseURL = types.ModelProviderAnthropicAPIKey, "https://other.corp.example/llm"
	if err := validateGatewayIdentityEdit(&types.ModelProviders{Providers: []types.ModelProvider{rekinded}}, stored); err == nil {
		t.Error("a kind and base_url change that keeps the audience passed")
	}
	rekinded.BaseURL = testGatewayProvider().BaseURL
	if err := validateGatewayIdentityEdit(&types.ModelProviders{Providers: []types.ModelProvider{rekinded}}, stored); err != nil {
		t.Errorf("a kind change at the same address refused: %v", err)
	}
	back := &types.ModelProviders{Providers: []types.ModelProvider{testGatewayProvider()}}
	if err := validateGatewayIdentityEdit(back, block); err != nil {
		t.Errorf("editing back refused: %v", err)
	}
	same := testGatewayProvider()
	same.BaseURL = "https://GW.corp.example:443/llm/"
	if err := validateGatewayIdentityEdit(&types.ModelProviders{Providers: []types.ModelProvider{same}}, stored); err != nil {
		t.Errorf("a respelling refused: %v", err)
	}
}

// A revive after an edit refuses with the reason and the new-run step, not a generic
// failure; editing back passes; a run with no route reads nothing.
func TestGatewayReviveAfterAnEdit(t *testing.T) {
	f := newGatewayFixture(t)
	rt, _ := gatewayRouteOf(testGatewayProvider())
	cfg := &proxy.Config{RunID: f.run.ID, GatewayIdentity: []proxy.GatewayIdentityRoute{rt}}
	moved := testGatewayProvider()
	moved.GatewayIdentity = &types.ModelProviderGatewayIdentity{Audience: "new-aud"}
	f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(moved) })
	ref, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg)
	if err != nil || ref == nil {
		t.Fatalf("got %v %v, want a refusal", ref, err)
	}
	if ref.status != http.StatusConflict || ref.reason != reasonGatewayIdentityChanged ||
		!strings.Contains(ref.msg, "Create a new run") || !strings.Contains(ref.msg, "corp-gw") {
		t.Errorf("refusal %+v", ref)
	}
	if rev := f.srv.reviveOwnerRecheck(context.Background(), f.run, cfg, types.ActorSystem, "wardynd"); rev == nil || rev.reason != reasonGatewayIdentityChanged {
		t.Errorf("revive recheck = %+v", rev)
	}
	f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(testGatewayProvider()) })
	if ref, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg); ref != nil || err != nil {
		t.Errorf("edited back: %v %v", ref, err)
	}
	f.st.set(func(s *gatewayStore) { s.siteErr = errors.New("db down") })
	if _, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg); err == nil {
		t.Error("a failed read passed")
	}
	if ref, err := f.srv.gatewayIdentityRefusal(context.Background(), &proxy.Config{RunID: f.run.ID}); ref != nil || err != nil {
		t.Errorf("no route read the store: %v %v", ref, err)
	}
}

// Off writes nothing a run sends: no route, so the config's key is absent and the
// bytes are today's. On, the route is normalized, and two lanes share one.
func TestGatewayDispatchRoute(t *testing.T) {
	var d providerDispatch
	off := testGatewayProvider()
	off.GatewayIdentity = nil
	if !d.addGatewayRoute(off) || d.gatewayRoutes != nil {
		t.Fatalf("off: %+v", d.gatewayRoutes)
	}
	withRoute, err := json.Marshal(proxy.Config{GatewayIdentity: d.gatewayRoutes})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withRoute), "gateway_identity") {
		t.Errorf("off wrote the key: %s", withRoute)
	}
	on := testGatewayProvider()
	on.BaseURL = "https://GW.corp.example.:8443/llm/"
	for range 2 {
		if !d.addGatewayRoute(on) {
			t.Fatal("on: refused")
		}
	}
	want := proxy.GatewayIdentityRoute{Host: "gw.corp.example", Port: 8443, PathPrefix: "/llm", ProviderID: "corp-gw", ProviderUID: "uid-1", Audience: "gw-aud"}
	if len(d.gatewayRoutes) != 1 || d.gatewayRoutes[0] != want {
		t.Errorf("routes %+v", d.gatewayRoutes)
	}
	// A second provider on the same gateway path clashes; one on a path apart does not.
	same := testGatewayProvider()
	same.ID, same.UID, same.BaseURL = "corp-gw-2", "uid-2", "https://gw.corp.example:8443/llm/v1"
	if got := d.gatewayRouteClash(same); got != "corp-gw" {
		t.Errorf("clash = %q, want corp-gw", got)
	}
	same.BaseURL = "https://gw.corp.example:8443/other"
	if got := d.gatewayRouteClash(same); got != "" {
		t.Errorf("a path apart clashed with %q", got)
	}
	if got := d.gatewayRouteClash(on); got != "" {
		t.Errorf("a provider clashed with itself: %q", got)
	}
	bad := testGatewayProvider()
	bad.BaseURL = "https://gw.corp.example/a/../b"
	if d.addGatewayRoute(bad) {
		t.Error("an address no route can hold was dispatched")
	}
}

// The issuer the console shows is the token's iss, not the bare trust domain
// /healthz reports: a gateway told the bare name would refuse every token.
func TestGatewayIdentityIssuerIsTheTokensIss(t *testing.T) {
	f := newGatewayFixture(t)
	if got := f.srv.gatewayIdentityIssuer(); got != "spiffe://wardyn.local" {
		t.Errorf("issuer %q", got)
	}
}

func testGatewayBedrockBearer() types.ModelProvider {
	return types.ModelProvider{
		ID: "corp-gw", UID: "uid-1", Kind: types.ModelProviderBedrockBearer,
		Bedrock:         &types.BedrockSettings{Region: "us-east-1", BaseURL: "https://GW.corp.example:8443/bedrock/"},
		GatewayIdentity: &types.ModelProviderGatewayIdentity{Audience: "gw-aud"},
		Harnesses:       []types.ProviderHarness{{Harness: "claude-code", Model: "profile"}},
	}
}

// A Bedrock key row may send identity once bedrock.base_url names a gateway, and is refused
// without one, with the field the admin must fill; a Bedrock sign-in row with a gateway of its
// own passes too (its own rules: TestGatewayIdentityBedrockSSOValidation).
func TestGatewayIdentityBedrockKinds(t *testing.T) {
	if err := validateProviderGatewayIdentity(testGatewayBedrockBearer()); err != nil {
		t.Fatalf("a Bedrock key row with a gateway address refused: %v", err)
	}
	noAddress := testGatewayBedrockBearer()
	noAddress.Bedrock.BaseURL = ""
	noAddress.BaseURL = "https://gw.corp.example" // the top-level address is never a Bedrock row's
	err := validateProviderGatewayIdentity(noAddress)
	if err == nil || !strings.Contains(err.Error(), "gateway_identity needs a bedrock.base_url, or the token would go to AWS") || refusalField(err) != "bedrock.base_url" {
		t.Errorf("no bedrock.base_url: %v (field %q)", err, refusalField(err))
	}
	badAddress := testGatewayBedrockBearer()
	badAddress.Bedrock.BaseURL = "https://gw.corp.example/a/../b"
	if err := validateProviderGatewayIdentity(badAddress); err == nil || refusalField(err) != "bedrock.base_url" {
		t.Errorf("a dot segment: %v (field %q)", err, refusalField(err))
	}
	sso := testGatewayBedrockBearer()
	sso.Kind = types.ModelProviderBedrockSSO
	if err := validateProviderGatewayIdentity(sso); err != nil {
		t.Errorf("sign-in row with a gateway address refused: %v", err)
	}
}

// A Bedrock key row's route is its bedrock.base_url normalized, holds for a mint while that address and
// audience stand, and breaks when either moves. Its audience may not name a second address.
func TestGatewayBedrockBearerRoute(t *testing.T) {
	p := testGatewayBedrockBearer()
	rt, ok := gatewayRouteOf(p)
	want := proxy.GatewayIdentityRoute{Host: "gw.corp.example", Port: 8443, PathPrefix: "/bedrock", ProviderID: "corp-gw", ProviderUID: "uid-1", Audience: "gw-aud"}
	if !ok || rt != want {
		t.Fatalf("route %+v ok=%v, want %+v", rt, ok, want)
	}
	if !gatewayBindingHolds(rt, p) {
		t.Error("the sealed route does not hold for the row it was sealed from")
	}
	moved := testGatewayBedrockBearer()
	moved.Bedrock.BaseURL = "https://other.corp.example/bedrock"
	if gatewayBindingHolds(rt, moved) {
		t.Error("a token for one gateway could go to another")
	}
	// The region does not decide the route, and the top-level base_url is not read.
	same := testGatewayBedrockBearer()
	same.Bedrock.Region, same.BaseURL = "eu-west-1", "https://other.corp.example"
	if !gatewayBindingHolds(rt, same) {
		t.Error("an edit that leaves the address alone broke the binding")
	}
	// One audience, two addresses: refused across a Bedrock row and an endpoint row.
	other := testGatewayProvider()
	other.ID, other.UID = "other-gw", "uid-2"
	other.GatewayIdentity.Audience = "gw-aud"
	if err := validateGatewayAudiences(&types.ModelProviders{Providers: []types.ModelProvider{p, other}}, nil); err == nil {
		t.Error("one audience on a Bedrock gateway and a different endpoint passed")
	}
	other.BaseURL = "https://gw.corp.example:8443/bedrock/"
	if err := validateGatewayAudiences(&types.ModelProviders{Providers: []types.ModelProvider{p, other}}, nil); err != nil {
		t.Errorf("one destination refused: %v", err)
	}
	// A new bedrock.base_url that keeps the audience is refused.
	stored := &types.ModelProviders{Providers: []types.ModelProvider{testGatewayBedrockBearer()}}
	err := validateGatewayIdentityEdit(&types.ModelProviders{Providers: []types.ModelProvider{moved}}, stored)
	if err == nil || !strings.Contains(err.Error(), "moved to a new address") {
		t.Errorf("a moved Bedrock gateway kept its audience: %v", err)
	}
}

// Dispatch records a Bedrock key row's route once, and writes none for a row with identity off or a kind that cannot carry it.
func TestGatewayDispatchBedrockBearerRoute(t *testing.T) {
	var d providerDispatch
	srv := &Server{}
	off := testGatewayBedrockBearer()
	off.GatewayIdentity = nil
	if !srv.takeProviderGatewayRoute(context.Background(), types.AgentRun{}, &d, off, llmTransport{}) || d.gatewayRoutes != nil {
		t.Fatalf("off: %+v", d.gatewayRoutes)
	}
	on := testGatewayBedrockBearer()
	for range 2 {
		if !srv.takeProviderGatewayRoute(context.Background(), types.AgentRun{}, &d, on, llmTransport{}) {
			t.Fatal("on: refused")
		}
	}
	want := proxy.GatewayIdentityRoute{Host: "gw.corp.example", Port: 8443, PathPrefix: "/bedrock", ProviderID: "corp-gw", ProviderUID: "uid-1", Audience: "gw-aud"}
	if len(d.gatewayRoutes) != 1 || d.gatewayRoutes[0] != want {
		t.Errorf("routes %+v", d.gatewayRoutes)
	}
	// A kind that cannot carry identity writes no route, even with the field stored.
	sub := testGatewayBedrockBearer()
	sub.Kind = types.ModelProviderAnthropicSubscription
	var none providerDispatch
	if !srv.takeProviderGatewayRoute(context.Background(), types.AgentRun{}, &none, sub, llmTransport{}) || none.gatewayRoutes != nil {
		t.Errorf("a subscription row wrote a route: %+v", none.gatewayRoutes)
	}
}

// A mint for a Bedrock key row passes from the route its dispatch sealed, and refuses once its gateway moves.
func TestGatewayMintForABedrockKeyRow(t *testing.T) {
	f := newGatewayFixture(t)
	p := testGatewayBedrockBearer()
	f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(p) })
	f.seal(t, f.run.ID, p)
	if w := f.mint(t); w.Code != http.StatusOK {
		t.Fatalf("mint = %d %s", w.Code, w.Body)
	}
	p.Bedrock.BaseURL = "https://other.corp.example/bedrock"
	f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(p) })
	if w := f.mint(t); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayIdentityChanged {
		t.Fatalf("moved: mint = %d %s", w.Code, w.Body)
	}
}
