// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A service identity launches a configured preset on a service-grant row through POST /runs, against a real
// Postgres: the run is created and persisted under the service, dispatched by the server's own launch, and the
// proxy's resolve on the persisted run gets the minted token. Guarded by WARDYN_TEST_PG; skipped when unset.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const svcPresetName = "svc-ci"

// svcLaunchPG is the service-identity environment with a service-grant row, its secret, a preset that names
// the row, a runner that records the sandbox it is asked for, and a minted service token.
type svcLaunchPG struct {
	svcEnv
	idp   *svcIdP
	fr    *fakeRunner
	token string
	uid   string
}

func newSvcLaunchPG(t *testing.T) *svcLaunchPG {
	t.Helper()
	e := newSvcEnv(t)
	f := &svcLaunchPG{svcEnv: e, idp: &svcIdP{}, fr: &fakeRunner{}}
	cfg := &e.h.srv.cfg
	cfg.Runner, cfg.ServiceGrantTransport, cfg.AllowTestEndpoints = f.fr, f.idp, true
	cfg.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.9")}, nil }
	if cfg.MaskRegistry == nil {
		cfg.MaskRegistry = secretmask.NewRegistry()
	}
	e.h.srv.router = e.h.srv.routes()
	ctx := context.Background()
	if _, err := e.st.PutSiteConfig(ctx, types.SiteConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx,
		`UPDATE site_config SET config = config || jsonb_build_object($1::text, to_jsonb(now()::text)) WHERE singleton`,
		store.PortalIssuersActivatedKey); err != nil {
		t.Fatal(err)
	}
	if err := e.ownerOnlyPG.sec.Put(ctx, oidcSecretRef, []byte(oidcClientSecret)); err != nil {
		t.Fatal(err)
	}
	row := svcPGRow("corp-a", true)
	admin := func(method, path string, body any) {
		t.Helper()
		if w := doSSO(t, e.h.srv, method, "/api/v1"+path, e.admin, string(mustJSON(body))); w.Code/100 != 2 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
	}
	admin(http.MethodPut, "/model-providers", types.ModelProviders{Providers: []types.ModelProvider{row}})
	admin(http.MethodPut, "/model-providers/corp-a/service-grant/secret", putServiceGrantSecretRequest{
		Value: svcClientSecret, TokenURL: row.OIDC.ServiceGrant.TokenURL, ClientID: row.OIDC.ServiceGrant.ClientID, BaseURL: row.BaseURL})
	preset := client.PresetRequest{Request: client.CreateRunRequest{
		Agent: "claude-code", ModelProvider: "corp-a",
		InlinePolicy: &types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2},
	}}
	if w := do(t, e.h.srv, http.MethodPut, "/api/v1/presets/"+svcPresetName, adminToken, string(mustJSON(preset))); w.Code/100 != 2 {
		t.Fatalf("store the preset: %d %s", w.Code, w.Body.String())
	}
	sc, err := e.st.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := modelProviderByID(sc.ModelProviders, "corp-a")
	if !ok || stored.OIDC == nil || stored.OIDC.ServiceGrant == nil {
		t.Fatalf("the stored row %+v carries no grant", stored)
	}
	f.uid = stored.UID
	e.mustCreate(t, svcName, svcSponsor)
	f.token = e.mustMint(t, e.sponsor, svcName).Token
	return f
}

// launch is the service's POST /runs naming the preset and the per-launch task; it waits for the server's own dispatch to bring
// the run to RUNNING and returns the persisted run and the sandbox spec the runner was given.
func (f *svcLaunchPG) launch(t *testing.T) (types.AgentRun, runner.SandboxSpec) {
	t.Helper()
	w := f.as(t, f.token, http.MethodPost, "/api/v1/runs", `{"preset":"`+svcPresetName+`","task":"run the checks"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("the service's preset launch = %d %s, want 201", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	f.fr.waitForSandbox(t)
	waitForRunState(t, f.h.srv, created.ID, types.RunRunning)
	run, err := f.st.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load the persisted run: %v", err)
	}
	f.fr.mu.Lock()
	spec := f.fr.lastSpec
	f.fr.mu.Unlock()
	return run, spec
}

// injection is the run's service-grant injection in the dispatched proxy config.
func (f *svcLaunchPG) injection(t *testing.T, spec runner.SandboxSpec) runner.InjectionGrant {
	t.Helper()
	for _, inj := range spec.ProxyConfig.Injection {
		if inj.Rule.SecretName == providerSecretName(f.uid, providerOIDCPart) {
			return inj
		}
	}
	t.Fatalf("the dispatched proxy config has no injection for the row: %+v", spec.ProxyConfig.Injection)
	return runner.InjectionGrant{}
}

// resolveRaw is the proxy's resolve of inj as the sidecar makes it: GET /internal/injection/{grant} on the
// authenticated route, with the run token dispatch handed the sandbox. It needs no *testing.T of its own beyond
// do's panic guard, so it may run off the test's goroutine.
func (f *svcLaunchPG) resolveRaw(t *testing.T, spec runner.SandboxSpec, inj runner.InjectionGrant) *httptest.ResponseRecorder {
	return do(t, f.h.srv, http.MethodGet, "/api/v1/internal/injection/"+inj.GrantID.String(), spec.ProxyConfig.RunToken, "")
}

func (f *svcLaunchPG) resolve(t *testing.T, run types.AgentRun, spec runner.SandboxSpec) (int, map[string]any) {
	t.Helper()
	if spec.ProxyConfig.RunToken == "" {
		t.Fatalf("run %s's sandbox spec carries no run token", run.ID)
	}
	w := f.resolveRaw(t, spec, f.injection(t, spec))
	return w.Code, svcBody(t, w)
}

// pendingResolve holds the resolve's token request open at the identity provider, makes edit while it waits,
// then lets it answer, and returns the resolve's answer. Every wait is bounded and the hold is released at
// cleanup whatever happens.
func (f *svcLaunchPG) pendingResolve(t *testing.T, run types.AgentRun, spec runner.SandboxSpec, edit func()) (int, map[string]any) {
	t.Helper()
	inj := f.injection(t, spec)
	f.h.srv.dropServiceGrantCache(f.uid)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.idp.mu.Lock()
	f.idp.respond = func(int, svcReq) svcAnswer { return svcAnswer{Status: 200, Body: svcOK(9, 3600).Body, Block: release} }
	f.idp.mu.Unlock()
	before := f.idp.count()
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() { out <- f.resolveRaw(t, spec, inj) }()
	waitFor(t, "the resolve's token request", func() bool { return f.idp.count() > before })
	edit()
	once.Do(func() { close(release) })
	select {
	case w := <-out:
		return w.Code, svcBody(t, w)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the held resolve")
	}
	return 0, nil
}

// editGrant changes corp-a's grant through PUT /model-providers as an admin.
func (f *svcLaunchPG) editGrant(t *testing.T, edit func(*types.ProviderServiceGrant)) {
	t.Helper()
	sc, err := f.st.GetSiteConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := modelProviderByID(sc.ModelProviders, "corp-a")
	if !ok || p.OIDC == nil || p.OIDC.ServiceGrant == nil {
		t.Fatalf("corp-a has no grant to edit: %+v", p)
	}
	g := *p.OIDC.ServiceGrant
	edit(&g)
	o := *p.OIDC
	o.ServiceGrant = &g
	p.OIDC = &o
	if w := doSSO(t, f.h.srv, http.MethodPut, "/api/v1/model-providers", f.admin,
		string(mustJSON(types.ModelProviders{Providers: []types.ModelProvider{p}}))); w.Code/100 != 2 {
		t.Fatalf("edit corp-a's grant: %d %s", w.Code, w.Body.String())
	}
}

// Acceptance 1: the preset launch persists the run under the service, the dispatch authors one grant on the
// row, and the resolve gets the minted token. No provider credential row exists for the service, no credential
// write is audited, and the sandbox's environment holds no token.
func TestServiceGrant_PresetRunUnderServiceIdentity_NoCredentialPut_PG(t *testing.T) {
	f := newSvcLaunchPG(t)
	run, spec := f.launch(t)
	if run.CreatedBy != svcOwner || run.ModelProviderID != "corp-a" || run.OperatorOwned {
		t.Fatalf("the persisted run: created by %q, provider %q, operator-owned %v; want the service on corp-a",
			run.CreatedBy, run.ModelProviderID, run.OperatorOwned)
	}
	code, body := f.resolve(t, run, spec)
	if code != http.StatusOK || resolvedToken(t, body) != svcToken(1) {
		t.Fatalf("resolve = %d %v, want the minted token", code, body)
	}
	ctx := context.Background()
	for owner, st := range map[string]interface {
		List(context.Context) ([]string, error)
	}{"the operator": f.ownerOnlyPG.sec, "the service": f.ownerOnlyPG.sec.For(svcOwner)} {
		names, err := st.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if strings.HasPrefix(n, "wardyn-provider-") {
				t.Errorf("%s's namespace holds a provider credential row %q", owner, n)
			}
		}
	}
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == "model_provider.credential.write" {
			t.Errorf("a credential write was audited: %s", ev.Data)
		}
	}
	if b := string(mustJSON(spec.Env)); strings.Contains(b, svcToken(1)) {
		t.Error("the sandbox environment carries the token")
	}
}

// Configured and written, then at once a launch with no refresher pass before it: the create check never
// mints, so the one token request is the dispatch's, and the run starts.
func TestServiceGrant_FirstLaunchAfterConfigureNoPriorMint_PG(t *testing.T) {
	f := newSvcLaunchPG(t)
	if f.idp.count() != 0 {
		t.Fatalf("setup: %d token requests before the launch", f.idp.count())
	}
	run, spec := f.launch(t)
	if f.idp.count() != 1 {
		t.Fatalf("token requests after the launch = %d, want the dispatch's one", f.idp.count())
	}
	var triggers []string
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == auditServiceGrantMintAction {
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			trigger, _ := d["trigger"].(string)
			triggers = append(triggers, trigger)
		}
	}
	if len(triggers) != 1 || triggers[0] != serviceGrantTriggerDispatch {
		t.Fatalf("mint rows by trigger = %v, want one dispatch mint", triggers)
	}
	if code, body := f.resolve(t, run, spec); code != http.StatusOK || resolvedToken(t, body) != svcToken(1) {
		t.Fatalf("resolve = %d %v, want the dispatch's token", code, body)
	}
}

// An edit of the grant while a resolve waits on its token request: the resolve reads the row again before it
// answers, refuses with scope_changed, and returns or audits no bearer made under the old settings.
func TestServiceGrant_EditDuringPendingResolveReturnsNoNewBearer_PG(t *testing.T) {
	f := newSvcLaunchPG(t)
	run, spec := f.launch(t)
	code, body := f.pendingResolve(t, run, spec, func() {
		f.editGrant(t, func(g *types.ProviderServiceGrant) { g.Audience = "api://other" })
	})
	if code != http.StatusForbidden || body["reason"] != reasonScopeChanged {
		t.Fatalf("resolve across an edit = %d %v, want 403 scope_changed", code, body)
	}
	if v, _ := body["value"].(string); v != "" || strings.Contains(string(mustJSON(f.h.audit.snapshot())), svcToken(9)) {
		t.Error("a bearer made under the old settings was returned or audited")
	}
}

// The service taken off the grant's list while a resolve waits: the resolve refuses not_listed with no bearer.
func TestServiceGrant_ListRemovalDuringPendingResolveRefused_PG(t *testing.T) {
	f := newSvcLaunchPG(t)
	run, spec := f.launch(t)
	code, body := f.pendingResolve(t, run, spec, func() {
		f.editGrant(t, func(g *types.ProviderServiceGrant) { g.ServiceIdentities = []string{svcOtherOwner} })
	})
	if code != http.StatusForbidden || body["reason"] != reasonServiceGrantNotListed {
		t.Fatalf("resolve across a list removal = %d %v, want 403 %s", code, body, reasonServiceGrantNotListed)
	}
	if v, _ := body["value"].(string); v != "" {
		t.Error("a bearer came back with the refusal")
	}
}
