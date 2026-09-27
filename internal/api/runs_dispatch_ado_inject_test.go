// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// adoTestStore is the store double both halves of the lane need: grant writes
// and reads, the live site config, and the run-state CAS a refusal makes.
type adoTestStore struct {
	store.Store
	mu      sync.Mutex
	site    types.SiteConfig
	siteErr error
	grants  []types.CredentialGrant
	failTo  []types.RunState
}

func (s *adoTestStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, g)
	return g, nil
}

func (s *adoTestStore) ListGrantsByRun(_ context.Context, runID uuid.UUID) ([]types.CredentialGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.CredentialGrant
	for _, g := range s.grants {
		if g.RunID == runID {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *adoTestStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, s.siteErr
}

// UpdateRunStateIf records the refusal's FAILED transition and reports it not
// applied, so the revoke cascade (which this double does not wire) is skipped.
func (s *adoTestStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, _, to types.RunState) (bool, error) {
	s.failTo = append(s.failTo, to)
	return false, nil
}

const (
	adoTestTenant = "11111111-2222-3333-4444-555555555555"
	adoTestClient = "66666666-7777-8888-9999-000000000000"
	adoTestOwner  = "alice-sub"
	adoTestRepo   = "https://dev.azure.com/contoso/proj/_git/app"
)

// adoEntraRow is the one provider row the lane serves.
func adoEntraTestRow() types.GitProvider {
	return types.GitProvider{
		ID: "ado-row-1", Kind: types.GitProviderAzureDevOps,
		BaseURLs:         []string{"https://dev.azure.com/contoso"},
		Lanes:            []types.GitLane{types.GitLaneEntra},
		CredentialSource: types.CredentialSourcePerUser,
		Entra: &types.ADOEntraConfig{
			TenantID: adoTestTenant, ClientID: adoTestClient,
			CapabilityCeiling: []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR},
			DefaultProfile:    []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite},
		},
	}
}

func adoSite(rows ...types.GitProvider) types.SiteConfig {
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: rows}}
}

// The golden host set for organisation contoso: exact, organisation-qualified,
// and no wildcard anywhere.
var adoContosoHosts = []string{
	"dev.azure.com",
	"vssps.dev.azure.com", "vsrm.dev.azure.com", "feeds.dev.azure.com", "pkgs.dev.azure.com", "almsearch.dev.azure.com",
	"contoso.visualstudio.com",
	"contoso.vssps.visualstudio.com", "contoso.vsrm.visualstudio.com", "contoso.feeds.visualstudio.com",
	"contoso.pkgs.visualstudio.com", "contoso.almsearch.visualstudio.com",
}

func TestResolveADOEntraRun_Golden(t *testing.T) {
	got, ok := resolveADOEntraRun(adoSite(adoEntraTestRow()), []string{adoTestRepo}, adoTestOwner)
	if !ok {
		t.Fatal("an entra row admitting the run's repository resolved no lane")
	}
	want := adoEntraRun{
		rowID: "ado-row-1", org: "contoso", owner: adoTestOwner,
		tenantID: adoTestTenant, clientID: adoTestClient,
		tokenMode: types.ADOTokenModeBearer, credentialSource: types.CredentialSourcePerUser,
		caps:    []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite},
		ceiling: []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved lane = %+v\nwant %+v", got, want)
	}
	legacy := adoEntraTestRow()
	legacy.BaseURLs = []string{"https://contoso.visualstudio.com"}
	if got, ok := resolveADOEntraRun(adoSite(legacy), []string{"https://contoso.visualstudio.com/proj/_git/app"}, adoTestOwner); !ok || got.org != "contoso" {
		t.Fatalf("legacy host: lane=%+v ok=%v, want organisation contoso", got, ok)
	}
}

// Every arm that must DECLINE: no owner, a shared row, a row without the
// entra lane, a disabled row, and a run spanning two organisations.
func TestResolveADOEntraRun_Declines(t *testing.T) {
	shared := adoEntraTestRow()
	shared.CredentialSource = ""
	pat := adoEntraTestRow()
	pat.Lanes = []types.GitLane{types.GitLanePAT}
	disabled := adoEntraTestRow()
	disabled.Disabled = true
	wide := adoEntraTestRow()
	wide.BaseURLs = []string{"https://dev.azure.com"}
	for name, tc := range map[string]struct {
		site  types.SiteConfig
		repos []string
		owner string
	}{
		"no owner":          {adoSite(adoEntraTestRow()), []string{adoTestRepo}, ""},
		"shared source":     {adoSite(shared), []string{adoTestRepo}, adoTestOwner},
		"pat lane only":     {adoSite(pat), []string{adoTestRepo}, adoTestOwner},
		"disabled":          {adoSite(disabled), []string{adoTestRepo}, adoTestOwner},
		"two organisations": {adoSite(wide), []string{adoTestRepo, "https://dev.azure.com/fabrikam/p/_git/r"}, adoTestOwner},
		"unconfigured":      {types.SiteConfig{}, []string{adoTestRepo}, adoTestOwner},
	} {
		if got, ok := resolveADOEntraRun(tc.site, tc.repos, tc.owner); ok {
			t.Errorf("%s: resolved %+v, want no lane", name, got)
		}
	}
}

func TestAdoOrganisationOf_RefusesNonLabels(t *testing.T) {
	for _, u := range []string{
		"https://contoso.vsrm.visualstudio.com/p/_git/r", // a service sub-host is not an organisation
		"https://dev.azure.com/",
		"https://dev.azure.com/con.toso/p/_git/r",
		"https://github.com/contoso/r",
	} {
		if org, ok := adoOrganisationOf(u); ok {
			t.Errorf("adoOrganisationOf(%q) = %q, want refused", u, org)
		}
	}
}

func newADODispatchServer(st *adoTestStore) (*Server, *memAudit) {
	audit := &memAudit{}
	return &Server{cfg: Config{Store: st, Audit: audit, Now: time.Now}}, audit
}

func adoTestRun(t *testing.T) adoEntraRun {
	t.Helper()
	run, ok := resolveADOEntraRun(adoSite(adoEntraTestRow()), []string{adoTestRepo}, adoTestOwner)
	if !ok {
		t.Fatal("fixture resolved no lane")
	}
	return run
}

// The dispatch golden: one api_key grant per exact host, each carrying the
// immutable snapshot and require_tls; egress and MITM entries port-qualified;
// the sandbox holding only an inert placeholder.
func TestAuthorADOEntraInjection_Golden(t *testing.T) {
	st := &adoTestStore{}
	s, _ := newADODispatchServer(st)
	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	env := map[string]string{}
	run := types.AgentRun{ID: uuid.New()}

	injections, mitm, ok := s.authorADOEntraInjection(context.Background(), run, adoTestRun(t), "CERT", "KEY", &policy, env, nil)
	if !ok {
		t.Fatal("authorADOEntraInjection refused a fully configured run")
	}
	var wantEntries []string
	for _, h := range adoContosoHosts {
		wantEntries = append(wantEntries, h+":443")
	}
	if !reflect.DeepEqual(mitm, wantEntries) {
		t.Errorf("MITM entries = %v\nwant %v", mitm, wantEntries)
	}
	if want := append([]string{"api.anthropic.com"}, wantEntries...); !reflect.DeepEqual(policy.AllowedDomains, want) {
		t.Errorf("AllowedDomains = %v\nwant %v", policy.AllowedDomains, want)
	}
	for _, d := range policy.AllowedDomains {
		if strings.Contains(d, "*") {
			t.Errorf("egress entry %q is a wildcard; the organisation pin needs exact hosts", d)
		}
	}
	if env[adoEntraPlaceholderEnv] != adoEntraPlaceholderValue {
		t.Errorf("sandbox %s = %q, want the inert placeholder", adoEntraPlaceholderEnv, env[adoEntraPlaceholderEnv])
	}
	// git goes through the broker: agent-run rewrites exactly these onto
	// /wardyn/git/ (the proxy's pat_broker_entra_test.go drives the same list
	// through the real agent-run-lib.sh).
	if got, want := env["WARDYN_GIT_PAT_BROKER_HOSTS"], "contoso.visualstudio.com contoso@dev.azure.com dev.azure.com"; got != want {
		t.Errorf("WARDYN_GIT_PAT_BROKER_HOSTS = %q, want %q", got, want)
	}

	if len(injections) != len(adoContosoHosts) || len(st.grants) != len(adoContosoHosts) {
		t.Fatalf("injections=%d grants=%d, want %d each", len(injections), len(st.grants), len(adoContosoHosts))
	}
	wantSnap := adoEntraScopeSnapshot{
		ProviderRowID: "ado-row-1", Organisation: "contoso", OwnerSubject: adoTestOwner,
		CredentialSource: "per_user", TenantID: adoTestTenant, ClientID: adoTestClient, TokenMode: "bearer",
		Capabilities: []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite},
	}
	for i, inj := range injections {
		r := inj.Rule
		if r.Host != adoContosoHosts[i] || !r.RequireTLS || r.Header != "Authorization" ||
			r.Format != "Bearer %s" || r.SecretName != types.ADOEntraAccessTokenSecret {
			t.Errorf("rule[%d] = %+v, want bare host %q, require_tls, Authorization/Bearer, the sentinel", i, r, adoContosoHosts[i])
		}
		g := st.grants[i]
		if g.ID != inj.GrantID || g.RunID != run.ID || g.Spec.Kind != types.GrantAPIKey || g.Spec.TTLSeconds != 3600 {
			t.Errorf("grant[%d] = %+v, want the rule's own api_key grant for this run", i, g)
		}
		var sc struct {
			Snapshot adoEntraScopeSnapshot `json:"snapshot"`
		}
		if err := json.Unmarshal(g.Spec.Scope, &sc); err != nil || !reflect.DeepEqual(sc.Snapshot, wantSnap) {
			t.Errorf("grant[%d] snapshot = %+v (err %v)\nwant %+v", i, sc.Snapshot, err, wantSnap)
		}
	}
}

// A missing per-run CA is a refusal, not a blind tunnel.
func TestAuthorADOEntraInjection_RefusesWithoutCertificateAuthority(t *testing.T) {
	for name, ca := range map[string][2]string{"no cert": {"", "KEY"}, "no key": {"CERT", ""}} {
		st := &adoTestStore{}
		s, audit := newADODispatchServer(st)
		policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
		env := map[string]string{}
		_, mitm, ok := s.authorADOEntraInjection(context.Background(), types.AgentRun{ID: uuid.New()},
			adoTestRun(t), ca[0], ca[1], &policy, env, nil)
		if ok {
			t.Fatalf("%s: dispatch went ahead with Azure DevOps hosts and no certificate authority", name)
		}
		if len(st.grants) != 0 || len(mitm) != 0 || len(policy.AllowedDomains) != 1 || len(env) != 0 {
			t.Errorf("%s: a refused run still authored grants=%d mitm=%v egress=%v env=%v", name, len(st.grants), mitm, policy.AllowedDomains, env)
		}
		if !slices.Equal(st.failTo, []types.RunState{types.RunFailed}) {
			t.Errorf("%s: run transitions = %v, want one to FAILED", name, st.failTo)
		}
		if rows := audit.find("run.create"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), "no_run_certificate_authority") {
			t.Errorf("%s: run.create audit rows = %+v, want one naming no_run_certificate_authority", name, rows)
		}
	}
}

// minted_pat is not issuable; a ceiling-escaping profile is not authorable.
func TestAuthorADOEntraInjection_RefusesUnissuableConfigurations(t *testing.T) {
	pat := adoTestRun(t)
	pat.tokenMode = types.ADOTokenModeMintedPAT
	escape := adoTestRun(t)
	escape.caps = append(escape.caps, adoscope.CapPolicyBypass)
	for name, ado := range map[string]adoEntraRun{"minted_pat": pat, "outside ceiling": escape} {
		st := &adoTestStore{}
		s, _ := newADODispatchServer(st)
		policy := types.RunPolicySpec{}
		if _, _, ok := s.authorADOEntraInjection(context.Background(), types.AgentRun{ID: uuid.New()}, ado,
			"CERT", "KEY", &policy, map[string]string{}, nil); ok || len(st.grants) != 0 {
			t.Errorf("%s: ok=%v grants=%d, want a refusal with nothing authored", name, ok, len(st.grants))
		}
	}
}

// An unconfigured deployment is byte-for-byte unchanged: the lane resolves to
// nothing, and the dispatch call with the lane off returns every input as it
// was and writes no grant.
func TestADOEntraLane_UnconfiguredDeploymentIsUnchanged(t *testing.T) {
	github := types.GitProvider{ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/acme"}}
	adoPAT := adoEntraTestRow()
	adoPAT.Lanes, adoPAT.CredentialSource, adoPAT.Entra = []types.GitLane{types.GitLanePAT}, "", nil
	for name, site := range map[string]types.SiteConfig{
		"no rows": {}, "github row": adoSite(github), "ado pat row": adoSite(adoPAT),
	} {
		ado, on := resolveADOEntraRun(site, []string{adoTestRepo, "https://github.com/acme/r"}, adoTestOwner)
		if on {
			t.Fatalf("%s: the lane switched on for a deployment with no entra row", name)
		}
		st := &adoTestStore{}
		s, audit := newADODispatchServer(st)
		policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
		env := map[string]string{"K": "V"}
		in := []runner.InjectionGrant{{GrantID: uuid.New()}}
		lane, ok := s.authorADOEntraLane(context.Background(), types.AgentRun{ID: uuid.New()}, ado, on,
			adoEntraUngraded(), dispatchLLMPlan{}, &policy, env, in)
		gotInj, mitm := lane.injections, lane.mitmHosts
		if !ok || mitm != nil || lane.gate != nil || !reflect.DeepEqual(gotInj, in) ||
			!reflect.DeepEqual(policy, types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}) ||
			!reflect.DeepEqual(env, map[string]string{"K": "V"}) || len(st.grants) != 0 || len(audit.rows) != 0 {
			t.Errorf("%s: the off lane changed dispatch output: inj=%v mitm=%v policy=%+v env=%v grants=%d audit=%d",
				name, gotInj, mitm, policy, env, len(st.grants), len(audit.rows))
		}
	}
}

// Cleartext through the plain lane is refused with no credential on the wire,
// driven through the REAL sidecar booted from this lane's own authored output:
// dispatch -> runner.BuildProxyConfig -> proxy.LoadConfigBytes -> NewServer.
// It also proves the sidecar boots with a dozen bare injection rules bound to
// port-qualified allowlist entries.
func TestADOEntraLane_CleartextThroughPlainLaneIsRefused(t *testing.T) {
	const marker = "ADO-SECRET-MARKER"
	var resolves int
	var mu sync.Mutex
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		resolves++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: "Bearer " + marker,
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Organisation: "contoso",
		})
	}))
	t.Cleanup(cp.Close)

	// The upstream leg runs through a TLS-terminating stand-in for Azure DevOps
	// that counts every request reaching it, so "nothing was forwarded" is
	// measured rather than inferred from the status code.
	fake := adofake.New()
	t.Cleanup(fake.Close)
	upstreamCA := newTestUpstreamCA(t, "dev.azure.com")
	upstream, seen := countingUpstream(t, fake.URL())
	corp := newTLSTerminatingCorpProxy(t, upstreamCA.leaf, upstream)

	certPEM, keyPEM, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatalf("generateRunCA: %v", err)
	}
	st := &adoTestStore{}
	s, _ := newADODispatchServer(st)
	policy := types.RunPolicySpec{}
	runID := uuid.New()
	lane, ok := s.authorADOEntraLane(context.Background(), types.AgentRun{ID: runID}, adoTestRun(t), true,
		adoEntraUngraded(), dispatchLLMPlan{mitmCACertPEM: string(certPEM), mitmCAKeyPEM: string(keyPEM)}, &policy, map[string]string{}, nil)
	if !ok {
		t.Fatal("authoring refused")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	raw, err := runner.BuildProxyConfig(runID, runner.ProxyConfig{
		RunToken: "run-token", ControlPlaneURL: cp.URL, Policy: policy, Injection: lane.injections,
		MITMCACertPEM: string(certPEM), MITMCAKeyPEM: string(keyPEM), MITMHosts: lane.mitmHosts,
		ADOGrant: lane.gate, UpstreamProxyURL: "http://" + corp, TrustedCAPEM: upstreamCA.caPEM,
	}, port)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	cfg.Listen = "127.0.0.1:" + strconv.Itoa(port)
	psrv, err := proxy.NewServer(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, io.Discard)
	if err != nil {
		t.Fatalf("the sidecar did not boot over this lane's own authored config: %v", err)
	}
	go func() { _ = psrv.ListenAndServe() }()
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = psrv.Shutdown(sctx)
	})
	if resolves != len(adoContosoHosts) {
		t.Fatalf("boot resolved %d injections, want %d", resolves, len(adoContosoHosts))
	}

	proxyURL, _ := url.Parse("http://" + cfg.Listen)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	var resp *http.Response
	for i := 0; i < 50; i++ { // wait for the listener
		if resp, err = client.Get("http://dev.azure.com:443/contoso/_apis/projects"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("request through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "CapabilityNotGranted") {
		t.Errorf("cleartext http:// on :443: status %d body %s, want the Azure DevOps 403", resp.StatusCode, body)
	}
	if strings.Contains(string(body), marker) {
		t.Fatal("the credential reached the refusal body")
	}

	resp, err = client.Get("http://dev.azure.com/contoso/_apis/projects") // port 80
	if err != nil {
		t.Fatalf("port-80 request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), marker) {
		t.Errorf("cleartext http:// on :80: status %d, want 403 with no credential", resp.StatusCode)
	}
	if n := seen.Load(); n != 0 {
		t.Errorf("the upstream saw %d cleartext request(s), want none", n)
	}

	// Absolute-form https:// on the same lane: the transport is not cleartext,
	// so require_tls does not refuse it, and the lane never runs the REST gate.
	assertADOPlainLaneRefused(t, cfg.Listen, seen)
}

// The wired source on a deployment with no Entra row is indistinguishable from
// NO SOURCE: same sign-in answer, and the console login is not widened.
func TestADOSignIn_UnconfiguredSourceAnswersLikeNoSource(t *testing.T) {
	answer := func(src ADOEntraSource) (int, string, bool) {
		s := &Server{cfg: Config{ADOEntra: src, Now: time.Now}}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/signin", nil)
		r = r.WithContext(withOIDCHuman(r.Context(), adoTestOwner))
		w := httptest.NewRecorder()
		s.handleADOSignIn(w, r)
		_, widened := s.adoEntraForLogin(context.Background(), "test")
		return w.Code, w.Body.String(), widened
	}
	c0, b0, l0 := answer(nil)
	c1, b1, l1 := answer(func(context.Context) (ADOEntraConfig, bool, error) { return ADOEntraConfig{}, false, nil })
	if c0 != c1 || b0 != b1 || l0 || l1 {
		t.Fatalf("nil source: %d %q widened=%v; unconfigured source: %d %q widened=%v — want identical, never widened",
			c0, b0, l0, c1, b1, l1)
	}
}

// The two lanes that write WARDYN_GIT_PAT_BROKER_HOSTS merge rather than
// overwrite: a stored-PAT host already on the list keeps its entry.
func TestAddGitBrokerHosts_Merges(t *testing.T) {
	env := map[string]string{"WARDYN_GIT_PAT_BROKER_HOSTS": "gitlab.com"}
	addGitBrokerHosts(env, adoEntraGitHosts("contoso")...)
	addGitBrokerHosts(env, "dev.azure.com")
	if got, want := env["WARDYN_GIT_PAT_BROKER_HOSTS"], "contoso.visualstudio.com contoso@dev.azure.com dev.azure.com gitlab.com"; got != want {
		t.Errorf("WARDYN_GIT_PAT_BROKER_HOSTS = %q, want %q", got, want)
	}
}

// The organisation-less app.vssps.visualstudio.com is never a host an
// organisation's credential rides to: nothing pins a request there to the
// organisation, and the classifier has no exemption for it (adoscope refuses
// it like any other organisation's host).
func TestADOEntraHostsNeverIncludeTheOrganisationlessVSSPSHost(t *testing.T) {
	for _, org := range []string{"acme", "fabrikam", "contoso-dev"} {
		for _, h := range adoEntraHosts(org) {
			if strings.HasPrefix(strings.ToLower(h), "app.vssps.") {
				t.Errorf("adoEntraHosts(%q) includes %q", org, h)
			}
		}
	}
}
