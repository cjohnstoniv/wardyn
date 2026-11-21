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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
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

func TestServiceGrant_PresetRunUnderServiceIdentity_NoCredentialPut_PG(t *testing.T) {
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

func TestServiceGrant_FirstLaunchAfterConfigureNoPriorMint_PG(t *testing.T) {
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

// pendingResolve starts a resolve whose mint the identity provider holds, runs edit, then releases it.
func (f *svcFix) pendingResolve(plan dispatchLLMPlan, edit func()) (int, map[string]any) {
	f.srv.dropServiceGrantCache(f.uid)
	release := make(chan struct{})
	f.idp.respond = func(int, svcReq) svcAnswer { return svcAnswer{Status: 200, Body: svcOK(9, 3600).Body, Block: release} }
	before := f.idp.count()
	type res struct {
		code int
		body map[string]any
	}
	out := make(chan res, 1)
	go func() { c, b := f.svcResolve(plan); out <- res{c, b} }()
	for f.idp.count() == before {
		time.Sleep(time.Millisecond)
	}
	edit()
	close(release)
	r := <-out
	return r.code, r.body
}

func TestServiceGrant_EditDuringPendingResolveReturnsNoNewBearer_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, _ := f.dispatch(t)
	code, body := f.pendingResolve(plan, func() {
		f.setRow(func(p *types.ModelProvider) {
			g := *p.OIDC.ServiceGrant
			g.Audience = "api://other"
			o := *p.OIDC
			o.ServiceGrant = &g
			p.OIDC = &o
		})
	})
	if code != http.StatusForbidden || body["reason"] != reasonScopeChanged {
		t.Fatalf("resolve across an edit = %d %v, want 403 scope_changed", code, body)
	}
	if v, _ := body["value"].(string); v != "" || strings.Contains(f.auditAll(), svcToken(9)) {
		t.Error("a bearer made under the old settings was returned or audited")
	}
}

func TestServiceGrant_ListRemovalDuringPendingResolveRefused_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, _ := f.dispatch(t)
	code, body := f.pendingResolve(plan, func() {
		f.setRow(func(p *types.ModelProvider) {
			g := *p.OIDC.ServiceGrant
			g.ServiceIdentities = []string{"service:other"}
			o := *p.OIDC
			o.ServiceGrant = &g
			p.OIDC = &o
		})
	})
	if code != http.StatusForbidden || body["reason"] != reasonServiceGrantNotListed {
		t.Fatalf("resolve across a list removal = %d %v, want 403 %s", code, body, reasonServiceGrantNotListed)
	}
	if v, _ := body["value"].(string); v != "" {
		t.Error("a bearer came back with the refusal")
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

func TestServiceGrant_SecretMaskStaleReadNeverPromotes(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	// A read that began before a rotation applies its old value after the rotation applied the new one.
	held, release := make(chan struct{}), make(chan struct{})
	f.sec.hold = func(op, _ string) {
		if op == "get" {
			f.sec.hold = nil
			close(held)
			<-release
		}
	}
	f.srv.dropServiceGrantCache(f.uid)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = f.token(serviceGrantTriggerResolve) }()
	<-held
	if w := f.serve(http.MethodPut, f.secretPath(), f.putSecretBody(svcSecretTwo)); w.Code != http.StatusNoContent {
		t.Fatalf("rotation: %d", w.Code)
	}
	close(release)
	<-done
	f.srv.cfg.MaskRegistry.SweepGlobals(f.clock.Now().Add(24 * time.Hour))
	if f.masked(svcClientSecret) || !f.masked(svcSecretTwo) {
		t.Fatalf("after the stale read: old masked %v, new masked %v, want the new value alone current", f.masked(svcClientSecret), f.masked(svcSecretTwo))
	}
}

// A run's sealed mask rows keep a copy of the token, so the output of a restarted wardynd stays masked.
func TestServiceGrant_MaskCopySealedAndMasksAfterRestart_PG(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, _ := f.dispatch(t)
	_, body := f.svcResolve(plan)
	tok := resolvedToken(t, body)
	var got [][]byte
	for _, b := range f.srv.cfg.MaskRegistry.Snapshot(f.st.run.ID) {
		got = append(got, b)
	}
	found := false
	for _, b := range got {
		found = found || string(b) == tok
	}
	if !found {
		t.Fatal("the run's mask values do not hold the token")
	}
	// A new registry with the run's values restored, as the boot reload does, masks the same output.
	restarted := secretmask.NewRegistry()
	for _, b := range got {
		if err := restarted.Add(f.st.run.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	if out := restarted.Masker(f.st.run.ID).Mask([]byte("x " + tok + " y")); strings.Contains(string(out), tok) {
		t.Fatalf("a restarted registry leaves the token in output: %s", out)
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
