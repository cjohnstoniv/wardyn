// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The service grant at run time: dispatch under a service identity, the proxy's resolves, what stays a person's
// own sign-in, and what is masked or kept out of the sandbox.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resolved is a 200 resolve's bearer token and its credential expiry.
func resolvedToken(t *testing.T, body map[string]any) string {
	t.Helper()
	v, _ := body["value"].(string)
	if !strings.HasPrefix(v, "Bearer ") {
		t.Fatalf("resolve body %v carries no bearer", body)
	}
	return strings.TrimPrefix(v, "Bearer ")
}

// The direct-helper form of the preset launch test, kept for diagnosis: the in-memory run, dispatched and
// resolved by the helpers alone.
func TestServiceGrant_RunUnderServiceIdentityDirect_NoCredentialPut(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, env, _, ok := f.dispatch(t)
	if !ok || len(plan.injections) != 1 {
		t.Fatalf("dispatch ok=%v injections=%d, want one", ok, len(plan.injections))
	}
	code, body := f.svcResolve(plan)
	if code != http.StatusOK || resolvedToken(t, body) != svcToken(1) {
		t.Fatalf("resolve = %d %v, want the minted token", code, body)
	}
	names, _ := f.sec.List(context.Background())
	for _, n := range names {
		if strings.HasPrefix(n, "wardyn-provider-") {
			t.Errorf("a provider credential row %q exists for the service", n)
		}
	}
	if rows := f.audit.find("model_provider.credential.write"); len(rows) != 0 {
		t.Errorf("%d credential.write rows", len(rows))
	}
	if b, _ := json.Marshal(env); strings.Contains(string(b), svcToken(1)) {
		t.Error("the sandbox environment carries the token")
	}
}

// The direct-helper form of the first launch test, kept for diagnosis.
func TestServiceGrant_FirstLaunchAfterConfigureNoPriorMintDirect(t *testing.T) {
	f := newSvcFixWithSecret(t)
	if f.idp.count() != 0 || f.cached() {
		t.Fatal("setup: a mint ran before the launch")
	}
	// Create's check never mints and passes; dispatch mints on the cold cache.
	if _, d, err := f.srv.providerLiveness(context.Background(), f.provider(), "claude-code", svcOwner, false); err != nil || d.msg != "" || f.idp.count() != 0 {
		t.Fatalf("create check: %+v %v with %d requests, want a pass and no mint", d, err, f.idp.count())
	}
	if _, _, _, ok := f.dispatch(t); !ok || f.idp.count() != 1 {
		t.Fatalf("dispatch ok=%v requests=%d, want a pass and one mint", ok, f.idp.count())
	}
}

// phases is the proxy's resolves, in minutes from the first mint, across one token's life.
func TestServiceGrant_RunSpansTokenExpiry(t *testing.T) {
	f := newSvcFixWithSecret(t)
	issued := map[string]time.Time{}
	var mu sync.Mutex
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		mu.Lock()
		issued[svcToken(n)] = f.clock.Now()
		mu.Unlock()
		return svcAnswer{}
	}
	plan, _, _, ok := f.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused")
	}
	start := f.clock.Now()
	seen := map[string]bool{}
	for _, min := range []float64{0, 2.5, 4.9, 30, 45, 49, 50.1, 55, 59} {
		target := start.Add(time.Duration(min * float64(time.Minute)))
		if d := target.Sub(f.clock.Now()); d > 0 {
			f.clock.Advance(d)
		}
		code, body := f.svcResolve(plan)
		if code != http.StatusOK {
			t.Fatalf("resolve at minute %.1f: %d %v", min, code, body)
		}
		tok := resolvedToken(t, body)
		seen[tok] = true
		mu.Lock()
		left := issued[tok].Add(time.Hour).Sub(f.clock.Now())
		mu.Unlock()
		if left <= 10*time.Minute {
			t.Fatalf("minute %.1f: handed out a token with %v left, want more than 10 minutes", min, left)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("distinct tokens = %d, want the first and its replacement", len(seen))
	}
	if rows := f.mintRows(); len(rows) != f.idp.count() {
		t.Fatalf("mint rows = %d for %d requests sent", len(rows), f.idp.count())
	}
	// The refresher's passes at 0, 30 and 59 seconds into a minute change nothing but the one renewal.
	before := f.idp.count()
	for _, sec := range []time.Duration{0, 30 * time.Second, 29 * time.Second} {
		f.clock.Advance(sec)
		f.srv.serviceGrantPass(context.Background())
	}
	if f.idp.count() != before {
		t.Fatalf("the refresher minted %d more times inside a fresh token's life", f.idp.count()-before)
	}
}

func TestServiceGrant_TwoRunsAnHourApart(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, ok := f.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused")
	}
	_, first := f.svcResolve(plan)
	f.clock.Advance(61 * time.Minute)
	plan2, _, _, ok := f.dispatch(t)
	if !ok {
		t.Fatal("the second dispatch refused")
	}
	code, second := f.svcResolve(plan2)
	if code != http.StatusOK || resolvedToken(t, second) == resolvedToken(t, first) {
		t.Fatalf("second run's resolve = %d %v, want a new token", code, second)
	}
}

// The proxy renews a header five minutes before its credential expires; here the renewal is its resolve at that
// moment, which must return a token that outlives the old one.
func TestServiceGrant_ProxyRenewsAcrossExpiry(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, _ := f.dispatch(t)
	code, first := f.svcResolve(plan)
	if code != http.StatusOK {
		t.Fatalf("first resolve: %d", code)
	}
	ms, _ := first["credential_expires_at"].(float64)
	exp := time.UnixMilli(int64(ms))
	f.clock.Advance(exp.Add(-5 * time.Minute).Sub(f.clock.Now())) // the proxy's renewal moment
	code, renewed := f.svcResolve(plan)
	if code != http.StatusOK || resolvedToken(t, renewed) == resolvedToken(t, first) {
		t.Fatalf("renewal = %d %v, want a replacement token", code, renewed)
	}
	ms2, _ := renewed["credential_expires_at"].(float64)
	exp2 := time.UnixMilli(int64(ms2))
	if !exp2.After(exp) {
		t.Errorf("the renewed credential expires %v, not after the old %v", exp2, exp)
	}
}

func TestServiceGrant_PersonOwnerUsesOwnSignIn(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.st.run.CreatedBy = oidcPersonSub
	f.signIn(t)
	plan, _, _, ok := f.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused for a person on a row with a grant")
	}
	rec := f.request(plan)
	w, _ := f.resolveWith(rec, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Bearer ") {
		t.Fatalf("person resolve = %d %s", w.Code, w.Body.String())
	}
	if f.idp.count() != 0 {
		t.Fatalf("a person's run asked the service grant's identity provider %d times", f.idp.count())
	}
	if strings.Contains(w.Body.String(), svcToken(1)) {
		t.Error("a person was handed the service token")
	}
}

func TestServiceGrant_SnapshotDigestDriftRefused(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, _ := f.dispatch(t)
	before := f.idp.count()
	f.setRow(func(p *types.ModelProvider) {
		g := *p.OIDC.ServiceGrant
		g.Scope = "api.write"
		o := *p.OIDC
		o.ServiceGrant = &g
		p.OIDC = &o
	})
	code, body := f.svcResolve(plan)
	if code != http.StatusForbidden || body["reason"] != reasonScopeChanged || f.idp.count() != before {
		t.Fatalf("resolve after an edit = %d %v with %d new requests, want 403 scope_changed and none", code, body, f.idp.count()-before)
	}
	if _, ok := body["value"]; ok {
		t.Error("a bearer came back with the refusal")
	}
}

func TestServiceGrant_AvailableToGatesService(t *testing.T) {
	f := newSvcFix(t)
	sc, _ := f.st.GetSiteConfig(context.Background())
	id := f.provider().ID
	denied, err := chooseModelProvider(sc, "claude-code", id, "", func(string) (bool, error) { return false, nil })
	if err != nil || denied.chosen || !denied.notGranted {
		t.Fatalf("a service identity outside Available to: %+v %v, want refused as not granted", denied, err)
	}
	ok, err := chooseModelProvider(sc, "claude-code", id, "", func(string) (bool, error) { return true, nil })
	if err != nil || !ok.chosen || ok.provider.ID != id {
		t.Fatalf("a service identity inside Available to: %+v %v, want the row chosen", ok, err)
	}
}

func TestServiceGrant_NotListedRefused(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.st.run.CreatedBy = "service:not-listed"
	if _, _, _, ok := f.dispatch(t); ok {
		t.Fatal("an unlisted service identity dispatched")
	}
	_, d, err := f.srv.providerLiveness(context.Background(), f.provider(), "claude-code", "service:not-listed", false)
	if err != nil || !d.notListed || !d.serviceGrant && d.msg == "" {
		t.Fatalf("create check = %+v %v, want not listed", d, err)
	}
	// One removed from the list loses its token at its running run's next resolve.
	g := newSvcFixWithSecret(t)
	plan, _, _, _ := g.dispatch(t)
	if code, _ := g.svcResolve(plan); code != http.StatusOK {
		t.Fatalf("listed resolve: %d", code)
	}
	g.setRow(func(p *types.ModelProvider) {
		gr := *p.OIDC.ServiceGrant
		gr.ServiceIdentities = []string{"service:other"}
		o := *p.OIDC
		o.ServiceGrant = &gr
		p.OIDC = &o
	})
	code, body := g.svcResolve(plan)
	if code != http.StatusForbidden || body["reason"] != reasonServiceGrantNotListed {
		t.Fatalf("resolve after removal = %d %v, want 403 %s", code, body, reasonServiceGrantNotListed)
	}
}

func TestServiceGrant_EditEndsNoSignIn(t *testing.T) {
	f := newSvcFix(t)
	before := f.provider()
	after := before
	o := *before.OIDC
	g := *o.ServiceGrant
	g.Scope, g.Audience, g.ClientID, g.TokenURL = "api.other", "api://x", "another", "https://idp2.example.test/token"
	g.ServiceIdentities = []string{"service:someone"}
	o.ServiceGrant = &g
	after.OIDC = &o
	if oidcAddress(before) != oidcAddress(after) || providerAddressDigest(before) != providerAddressDigest(after) {
		t.Fatal("a service grant change moved the address a person's sign-in is bound to")
	}
	if providerAddressChanged(before, after) {
		t.Fatal("a service grant change ends people's sign-ins")
	}
}

func TestServiceGrant_TokenMaskedInRunOutput(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, _ := f.dispatch(t)
	code, body := f.svcResolve(plan)
	if code != http.StatusOK {
		t.Fatalf("resolve: %d", code)
	}
	tok := resolvedToken(t, body)
	out := f.srv.cfg.MaskRegistry.Masker(f.st.run.ID).Mask([]byte("curl said Authorization: Bearer " + tok + " ok"))
	if strings.Contains(string(out), tok) {
		t.Fatalf("the run's output kept the token: %s", out)
	}
}

// failingRunBackend refuses every per-run value, as a committed corpus that cannot answer does.
type failingRunBackend struct{ secretmask.Backend }

func (failingRunBackend) PutRun(uuid.UUID, []byte) error { return errSvcDown }
func (failingRunBackend) GlobalGeneration(context.Context) (int64, error) {
	return 0, nil
}

func TestServiceGrant_UnmaskableTokenNotHandedOut(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, _ := f.dispatch(t)
	f.srv.cfg.MaskRegistry.SetBackend(failingRunBackend{})
	code, body := f.svcResolve(plan)
	if code != http.StatusServiceUnavailable || body["reason"] != "mask_state_unavailable" {
		t.Fatalf("resolve with the registry failing = %d %v, want 503 mask_state_unavailable", code, body)
	}
	if v, _ := body["value"].(string); v != "" {
		t.Error("a token the registry could not record was handed out")
	}
}

func TestServiceGrant_SandboxHoldsPlaceholderOnly(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, env, policy, ok := f.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused")
	}
	code, body := f.svcResolve(plan)
	tok := resolvedToken(t, body)
	if code != http.StatusOK {
		t.Fatalf("resolve: %d", code)
	}
	sandbox, _ := json.Marshal(env)
	planned, _ := json.Marshal(policy)
	grants, _ := json.Marshal(f.st.grants)
	for name, b := range map[string]string{"sandbox env": string(sandbox), "policy": string(planned), "grants": string(grants), "audit": f.auditAll()} {
		if strings.Contains(b, tok) || strings.Contains(b, svcClientSecret) {
			t.Errorf("%s carries the token or the client secret", name)
		}
	}
}

// On one replica a token read and a rotation of the same record are serialised by the name's mutex: the read
// holds it while its Get is paused, the route's write waits behind it, and the write's value is the one current
// once both are done, whatever order the goroutines reach the mutex in.
func TestServiceGrant_SecretMaskStaleReadNeverPromotes(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	f.srv.dropServiceGrantCache(f.uid)
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var armed, holding, putWhileHeld atomic.Bool
	armed.Store(true)
	f.sec.hold = func(op, name string) {
		switch {
		case op == "get" && name == f.secretName() && armed.CompareAndSwap(true, false):
			holding.Store(true)
			close(held)
			if !svcWaited(release) {
				t.Error("the paused read was never released")
			}
			holding.Store(false)
		case op == "put" && holding.Load():
			putWhileHeld.Store(true)
		}
	}
	readDone := make(chan struct{})
	go func() { defer close(readDone); _, _ = f.token(serviceGrantTriggerResolve) }()
	svcAwait(t, held, "the token read to pause in Get")
	// The writer starts while the read holds the mutex; its row read is the barrier that it is under way.
	var once sync.Once
	writing := make(chan struct{})
	f.st.gmu.Lock()
	f.st.getErr = func() error { once.Do(func() { close(writing) }); return nil }
	f.st.gmu.Unlock()
	wrote := make(chan int, 1)
	go func() { wrote <- f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretTwo)).Code }()
	svcAwait(t, writing, "the concurrent writer to read the row")
	releaseOnce.Do(func() { close(release) })
	svcAwait(t, readDone, "the paused read to finish")
	select {
	case code := <-wrote:
		if code != http.StatusNoContent {
			t.Fatalf("rotation: %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the concurrent writer")
	}
	if putWhileHeld.Load() {
		t.Fatal("the route stored the record while a read of the same name held its mutex")
	}
	f.srv.cfg.MaskRegistry.SweepGlobals(f.clock.Now().Add(24 * time.Hour))
	if f.masked(svcClientSecret) || !f.masked(svcSecretTwo) {
		t.Fatalf("after the stale read: old masked %v, new masked %v, want the new value alone current", f.masked(svcClientSecret), f.masked(svcSecretTwo))
	}
}

// svcPauseAfterGet is a replica's view of the shared secret store whose Get returns only after pause: the read
// has captured the record and has not yet applied it.
type svcPauseAfterGet struct {
	*svcSecrets
	pause func(name string)
}

func (s *svcPauseAfterGet) Get(ctx context.Context, name string) ([]byte, error) {
	v, err := s.svcSecrets.Get(ctx, name)
	s.pause(name)
	return v, err
}

func (s *svcPauseAfterGet) For(owner string) secretstore.Store {
	if owner == "" {
		return s
	}
	return s.svcSecrets.For(owner)
}

// Across replicas the masks are each process's own, and the record in the shared store is the truth: B captures
// the old record and pauses, A's route writes the new one, and B then applies what it captured. A keeps the new
// value current after the grace; B's mint sees the revision move, reads again, and holds the new value alone
// after the grace too. Neither replica serves the old value as its current mask.
func TestServiceGrant_SecretMaskStaleReadOnAnotherReplicaNeverPromotes(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	captured, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var armed atomic.Bool
	armed.Store(true)
	view := &svcPauseAfterGet{svcSecrets: f.sec, pause: func(name string) {
		if name == f.secretName() && armed.CompareAndSwap(true, false) {
			close(captured)
			if !svcWaited(release) {
				t.Error("B's paused read was never released")
			}
		}
	}}
	cfg := f.srv.cfg
	cfg.MaskRegistry, cfg.Secrets = secretmask.NewRegistry(), view
	b := &Server{cfg: cfg}
	minted := make(chan error, 1)
	go func() {
		_, err := b.serviceGrantToken(context.Background(), f.provider(), "", serviceGrantTriggerRefresh)
		minted <- err
	}()
	svcAwait(t, captured, "B to capture the old record")
	if w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretTwo)); w.Code != http.StatusNoContent {
		t.Fatalf("rotation on A: %d %s", w.Code, w.Body.String())
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-minted:
		if err != nil {
			t.Fatalf("B's mint after the rotation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for B's mint")
	}
	after := f.clock.Now().Add(24 * time.Hour)
	f.srv.cfg.MaskRegistry.SweepGlobals(after)
	b.cfg.MaskRegistry.SweepGlobals(after)
	for name, s := range map[string]*Server{"A": f.srv, "B": b} {
		if svcMasked(s, svcClientSecret) || !svcMasked(s, svcSecretTwo) {
			t.Errorf("%s after the grace: old masked %v, new masked %v, want the new value alone current",
				name, svcMasked(s, svcClientSecret), svcMasked(s, svcSecretTwo))
		}
	}
	if got := f.idp.requests(); len(got) == 0 {
		t.Fatal("B sent no token request")
	} else if _, sec := svcBasic(t, got[len(got)-1].Header.Get("Authorization")); sec != svcSecretTwo {
		t.Errorf("B's last token request carried %q, want the rotated secret", sec)
	}
}

func TestServiceGrant_GetShowsReferenceOnly(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	if w := f.writeSecret("corp-a", svcClientSecret); w.Code != http.StatusNoContent {
		t.Fatalf("secret write: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.mint("corp-a"); err != nil {
		t.Fatal(err)
	}
	uid := f.uid("corp-a")
	for _, path := range []string{"/model-providers", "/site-config"} {
		w := f.do(http.MethodGet, path, f.admin, nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"corp-a"`) {
			t.Fatalf("GET %s = %d %.200s, want 200 with the seeded provider", path, w.Code, w.Body.String())
		}
		for _, leak := range []string{svcClientSecret, "written_at", `"client_secret":`, `"v":1`} {
			if strings.Contains(w.Body.String(), leak) {
				t.Errorf("GET %s shows %q", path, leak)
			}
		}
	}
	// The model-provider read names the derived reference and nothing else about the secret.
	var read struct {
		Providers []struct {
			ID string `json:"id"`
		} `json:"providers"`
		ServiceGrants map[string]types.ServiceGrantStatus `json:"service_grants"`
	}
	w := f.do(http.MethodGet, "/model-providers", f.admin, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := read.ServiceGrants["corp-a"]; got.ClientSecretRef != serviceGrantSecretName(uid) || got.ClientID != svcClientID {
		t.Fatalf("service_grants[corp-a] = %+v, want client %s and the derived reference %s", got, svcClientID, serviceGrantSecretName(uid))
	}
}

// The credential door refuses a service identity on a row whose grant supplies its token.
func TestPutProviderCredential_ServiceOnServiceGrantRow409(t *testing.T) {
	row := oidcRowForSite()
	row.OIDC.ServiceGrant = &types.ProviderServiceGrant{GrantType: types.ServiceGrantTypeClientCreds, TokenURL: svcTokenURL,
		ClientID: svcClientID, ServiceIdentities: []string{svcOwner}}
	site := credentialSite(row)
	srv := modelProvidersStatusSrv(t, site, &capStore{})
	svc := ssoSession(t, svcOwner, "svc@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/"+row.ID+"/credential", svc, `{"value":"a-pasted-token-0123456789"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonModelProviderCredentialSignsIn) || !strings.Contains(w.Body.String(), mpcServiceGrant) {
		t.Fatalf("PUT = %d %s, want 409 with the service grant sentence", w.Code, w.Body.String())
	}
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	w = doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/"+row.ID+"/credential", member, `{"value":"a-pasted-token-0123456789"}`)
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), mpcServiceGrant) {
		t.Fatalf("a person's PUT = %d %s, want the existing sign-in sentence", w.Code, w.Body.String())
	}
}

func TestSetupStatus_ServiceIdentityServiceGrantState(t *testing.T) {
	f := newSvcFixWithSecret(t)
	ctx := context.Background()
	grade := func() SetupProviderAccess { return f.srv.providerAccessFor(ctx, f.provider(), svcOwner) }
	if got := grade(); got.State != modelAccessLive {
		t.Fatalf("before any attempt: %+v, want live", got)
	}
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	f.token(serviceGrantTriggerRefresh) //nolint:errcheck // the failure is the point
	if got := grade(); got.State != modelAccessNotConfigured || got.Cause != reasonServiceGrantFailing {
		t.Fatalf("after a definitive failure: %+v, want not_configured %s", got, reasonServiceGrantFailing)
	}
	f.idp.respond = nil
	f.clock.Advance(2 * time.Minute)
	f.mustToken(serviceGrantTriggerRefresh)
	if got := grade(); got.State != modelAccessLive {
		t.Fatalf("after a success: %+v, want live", got)
	}
}
