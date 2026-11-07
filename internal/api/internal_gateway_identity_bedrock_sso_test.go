// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Run identity on a bedrock_sso provider: the proxy signs each Bedrock call as the person for the
// regional Bedrock host and sends it to the admin's gateway with the run's identity token.

const (
	ssoGatewayHost    = "ai-gw.corp.example"
	ssoGatewayBaseURL = "https://ai-gw.corp.example/bedrock"
)

// testGatewayBedrockSSO is a sign-in row with identity on and a gateway in bedrock.base_url.
func testGatewayBedrockSSO() types.ModelProvider {
	p := testGatewayBedrockBearer()
	p.Kind = types.ModelProviderBedrockSSO
	return p
}

// ssoGatewayProvider is awsSSOTestProvider (the provider the stored sign-in was captured for) with
// identity on and its calls going to the gateway.
func ssoGatewayProvider() types.ModelProvider {
	p := awsSSOTestProvider()
	p.Bedrock.BaseURL = ssoGatewayBaseURL
	p.GatewayIdentity = &types.ModelProviderGatewayIdentity{Audience: "gw-aud"}
	return p
}

// A sign-in row may send identity once its bedrock.base_url names a gateway of the admin's own and
// its region names the Bedrock address each call is signed for. An AWS address, however spelled, a
// missing region and inspection left to the gateway are refused.
func TestGatewayIdentityBedrockSSOValidation(t *testing.T) {
	if err := validateProviderGatewayIdentity(testGatewayBedrockSSO()); err != nil {
		t.Fatalf("a sign-in row with a gateway address and a region refused: %v", err)
	}
	for _, tc := range []struct {
		name, field, want string
		edit              func(*types.ModelProvider)
	}{
		{"no base_url", "bedrock.base_url", "gateway_identity needs a bedrock.base_url, or the token would go to AWS",
			func(p *types.ModelProvider) { p.Bedrock.BaseURL = "" }},
		{"a dot segment", "bedrock.base_url", "no dot segment", func(p *types.ModelProvider) { p.Bedrock.BaseURL = "https://gw.corp.example/a/../b" }},
		{"no region", "bedrock.region", "needs bedrock.region", func(p *types.ModelProvider) { p.Bedrock.Region = "" }},
		{"inspection left to the gateway", "gateway_identity.inspection", "",
			func(p *types.ModelProvider) { p.GatewayIdentity.Inspection = types.GatewayInspectionGateway }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testGatewayBedrockSSO()
			tc.edit(&p)
			err := validateProviderGatewayIdentity(p)
			if err == nil || refusalField(err) != tc.field || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v (field %q), want field %q containing %q", err, refusalField(err), tc.field, tc.want)
			}
		})
	}
	// The token never goes to AWS: every AWS-owned name is refused, the ones the signer cannot sign
	// for too, and so is a gateway on a name AWS generated for it.
	for _, host := range []string{
		"vpce-0a1b2c3d4e5f-abcdefgh.bedrock-runtime.us-east-1.vpce.amazonaws.com",
		"vpce-0a1b2c3d4e5f-abcdefgh.bedrock-runtime.us-gov-west-1.vpce.amazonaws.com",
		"bedrock-runtime.us-east-1.amazonaws.com",
		"bedrock-runtime.cn-north-1.amazonaws.com.cn",
		"bedrock-runtime.us-east-1.api.aws",
		"abc123.execute-api.us-east-1.amazonaws.com",
		"my-lb-1234.us-east-1.elb.amazonaws.com",
		"BEDROCK-RUNTIME.US-EAST-1.AMAZONAWS.COM.",
	} {
		p := testGatewayBedrockSSO()
		p.Bedrock.BaseURL = "https://" + host + "/bedrock"
		err := validateProviderGatewayIdentity(p)
		if err == nil || refusalField(err) != "bedrock.base_url" || !strings.Contains(err.Error(), "not an AWS address") {
			t.Errorf("%s: err %v (field %q), want the AWS-address refusal", host, err, refusalField(err))
		}
	}
	// The key row keeps its rules: an AWS-owned base_url is not newly refused there.
	bearer := testGatewayBedrockBearer()
	bearer.Bedrock.BaseURL = "https://vpce-0a1b2c3d4e5f-abcdefgh.bedrock-runtime.us-east-1.vpce.amazonaws.com"
	if err := validateProviderGatewayIdentity(bearer); err != nil {
		t.Errorf("a key row's rules changed: %v", err)
	}
}

// ssoGatewaySaveServer is a save door whose install keeps run configs and can make the signing key,
// with re-signing at the proxy on or off.
func ssoGatewaySaveServer(t *testing.T, resign bool) (*Server, *gatewayPutStore) {
	t.Helper()
	st := testGatewayPutStore(types.SiteConfig{})
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.RunConfigKey = make([]byte, kek.DEKSize)
	raw, err := gatewayidentity.Rotate(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gatewayidentity.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.GatewayIdentity = func(context.Context, bool) (*gatewayidentity.Signer, error) { return signer, nil }
	cfg.AWSSSOProxyInject, cfg.AWSSigV4AtProxy = true, resign
	return New(cfg), st
}

const ssoGatewayPut = `{"providers":[{"id":"corp-sso","kind":"bedrock_sso","bedrock":{"region":"us-east-1",` +
	`"base_url":"https://ai-gw.corp.example/bedrock","sso_start_url":"https://example.awsapps.com/start"},` +
	`"harnesses":[{"harness":"claude-code","model":"us.anthropic.claude-sonnet-4-5-20250929-v1:0"}],"gateway_identity":{"audience":"gw-aud"}}]}`

// Saving identity on a sign-in row needs re-signing at the proxy: off, the save is refused with the
// switch named; on, it saves.
func TestGatewayIdentityBedrockSSOSaveNeedsResigning(t *testing.T) {
	srv, st := ssoGatewaySaveServer(t, false)
	w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, ssoGatewayPut)
	want := `model_providers: "corp-sso": gateway_identity on a bedrock_sso provider needs WARDYN_AWS_SIGV4_AT_PROXY on, so Wardyn signs each AWS request at its proxy`
	if w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonSiteConfigInvalid || !strings.Contains(w.Body.String(), strings.ReplaceAll(want, `"`, `\"`)) {
		t.Fatalf("re-signing off: PUT = %d %s, want 400 %q", w.Code, w.Body, want)
	}
	if st.putSeen != nil {
		t.Error("re-signing off: stored")
	}
	srv, st = ssoGatewaySaveServer(t, true)
	if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, ssoGatewayPut); w.Code != http.StatusOK {
		t.Fatalf("re-signing on: PUT = %d %s, want 200", w.Code, w.Body)
	}
	if st.putSeen == nil || st.putSeen.ModelProviders.Providers[0].GatewayIdentity == nil {
		t.Errorf("re-signing on: stored %+v", st.putSeen)
	}
}

// The model-providers answer says whether a sign-in row can take identity on this install: re-signing
// on, with the proxy-side session it builds on and no test sign-in endpoint.
func TestModelProvidersGetGatewayIdentityBedrockSSO(t *testing.T) {
	for _, tc := range []struct {
		name           string
		resign, inject bool
		override       string
		want           bool
	}{
		{"on", true, true, "", true},
		{"re-signing off", false, true, "", false},
		{"proxy-side session off", true, false, "", false},
		{"test sign-in endpoint", true, true, "http://127.0.0.1:8090", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{})
			srv.cfg.AWSSigV4AtProxy, srv.cfg.AWSSSOProxyInject, srv.cfg.AWSSSOEndpointOverride = tc.resign, tc.inject, tc.override
			w := do(t, srv, http.MethodGet, "/api/v1/model-providers", adminToken, "")
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
				t.Fatalf("GET = %d %s (%v)", w.Code, w.Body, err)
			}
			if v, ok := got["gateway_identity_bedrock_sso"].(bool); !ok || v != tc.want {
				t.Errorf("gateway_identity_bedrock_sso = %v (present %v), want %v", got["gateway_identity_bedrock_sso"], ok, tc.want)
			}
		})
	}
}

// dispatchSSOGateway launches a run on ssoGatewayProvider with re-signing on or off, under b, and
// returns the server and the run's id.
func dispatchSSOGateway(t *testing.T, resign bool, b types.Boundary) (*Server, uuid.UUID) {
	t.Helper()
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(ssoGatewayProvider())}, &capStore{}, nil)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	srv.cfg.AWSSSOProxyInject, srv.cfg.AWSSigV4AtProxy = true, resign
	blob := createRenewalBlob()
	blob.ExpiresAt = time.Now().Add(2 * time.Hour)
	storeSSOBlobFor(t, srv, createRenewalOwner, blob)
	tok := providerAdminToken(srv, createRenewalOwner)
	srv.cfg.Store = boundaryIntegStore{integStore: srv.cfg.Store.(*integStore), b: b}
	w := do(t, srv, http.MethodPost, "/api/v1/runs", tok, createRenewalBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == uuid.Nil {
		t.Fatalf("create body %s: %v", w.Body, err)
	}
	return srv, created.ID
}

// ssoGatewayWant is the gateway entry dispatch writes for ssoGatewayProvider.
var ssoGatewayWant = proxy.AWSSigV4Gateway{Host: ssoGatewayHost, Port: 443, PathPrefix: "/bedrock", SigningHost: sigv4RuntimeHost}

// Dispatch writes the route and the gateway entry, stages the placeholder profile with no sign-in
// cache and no route to the sign-in hosts, keeps the gateway out of the injection and MITM tables,
// and the run's Bedrock row records the signing and the gateway. The proxy accepts the config.
func TestGatewayDispatchBedrockSSORoute(t *testing.T) {
	srv, runID := dispatchSSOGateway(t, true, denyBoundary("pypi.org"))
	spec := lastDispatched(t, srv.cfg.Runner.(*fakeRunner))
	pc := spec.ProxyConfig
	wantRoute := proxy.GatewayIdentityRoute{Host: ssoGatewayHost, Port: 443, PathPrefix: "/bedrock", ProviderID: "bedrock-sso",
		ProviderUID: awsSSOTestProviderUID, Audience: "gw-aud"}
	if len(pc.GatewayIdentity) != 1 || pc.GatewayIdentity[0] != wantRoute {
		t.Fatalf("gateway_identity = %+v, want [%+v]", pc.GatewayIdentity, wantRoute)
	}
	if pc.AWSSigV4 == nil || !slices.Equal(pc.AWSSigV4.Hosts, sigv4LaneHosts) ||
		len(pc.AWSSigV4.Gateways) != 1 || pc.AWSSigV4.Gateways[0] != ssoGatewayWant {
		t.Fatalf("aws_sigv4 = %+v, want hosts %v and gateway %+v", pc.AWSSigV4, sigv4LaneHosts, ssoGatewayWant)
	}
	files := decodeSSOFiles(t, cmp.Or(spec.Env[awsSSOConfigEnvVar], spec.SecretEnv[awsSSOConfigEnvVar]))
	if _, ok := files[".aws/"+awsSigV4CredentialFile]; !ok {
		t.Errorf("no placeholder credential file: %v", slices.Collect(maps.Keys(files)))
	}
	for name := range files {
		if strings.Contains(name, "sso/cache") {
			t.Errorf("a sign-in cache was staged: %s", name)
		}
	}
	for _, d := range pc.Policy.AllowedDomains {
		if strings.Contains(d, "portal.sso.") || strings.HasPrefix(d, "oidc.") {
			t.Errorf("a sign-in host is allowed: %s", d)
		}
	}
	if !slices.Contains(pc.Policy.AllowedDomains, ssoGatewayHost) {
		t.Errorf("the gateway is not allowed: %v", pc.Policy.AllowedDomains)
	}
	for _, m := range pc.MITMHosts {
		if strings.Contains(m, ssoGatewayHost) {
			t.Errorf("mitm_hosts carries the gateway: %v", pc.MITMHosts)
		}
	}
	for _, in := range pc.Injection {
		if strings.EqualFold(in.Rule.Host, ssoGatewayHost) {
			t.Errorf("an injection targets the gateway: %+v", in.Rule)
		}
	}
	ev := waitForRecAudit(t, srv.cfg.Audit.(*recRecorder), runID, "run.bedrock.configure", "success")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["aws_signing"] != "proxy" || data["aws_signing_gateway"] != ssoGatewayHost {
		t.Errorf("run.bedrock.configure = %v, want aws_signing=proxy and aws_signing_gateway=%s", data, ssoGatewayHost)
	}
	// The runner fills in how its proxy reaches wardynd; the rest is what dispatch wrote.
	pc.RunToken, pc.ControlPlaneURL = "run-token", "http://127.0.0.1:8080"
	raw, err := runner.BuildProxyConfig(spec.RunID, pc, 3128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.LoadConfigBytes(raw); err != nil {
		t.Errorf("the proxy refuses the dispatched config: %v", err)
	}
}

// With re-signing turned off after the save, the run is refused with the reason and nothing is
// started: no sandbox, so no resident sign-in files.
func TestGatewayDispatchBedrockSSORefusedWithoutResign(t *testing.T) {
	srv, runID := dispatchSSOGateway(t, false, denyBoundary("pypi.org"))
	ev := waitForRecAudit(t, srv.cfg.Audit.(*recRecorder), runID, "run.create", "failure")
	want := `Model provider "bedrock-sso" sends run identity to a gateway, which needs Wardyn to sign its AWS requests at the proxy; ` +
		`this install doesn't (WARDYN_AWS_SIGV4_AT_PROXY is off). Ask your operator.`
	if got := auditData(t, *ev)["error"]; got != want {
		t.Errorf("run.create failure error = %v, want %q (%s)", got, want, ev.Data)
	}
	fr := srv.cfg.Runner.(*fakeRunner)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.lastSpec.RunID != uuid.Nil {
		t.Errorf("a sandbox was started: env %v", fr.lastSpec.Env)
	}
}

// A boundary that withholds the lane takes the gateway route and entry with the re-signer.
func TestGatewayDispatchBedrockSSOWithheld(t *testing.T) {
	srv, runID := dispatchSSOGateway(t, true, denyBoundary(ssoGatewayHost))
	pc := lastDispatched(t, srv.cfg.Runner.(*fakeRunner)).ProxyConfig
	if pc.AWSSigV4 != nil || len(pc.GatewayIdentity) != 0 {
		t.Fatalf("withheld lane left aws_sigv4 %+v and routes %+v", pc.AWSSigV4, pc.GatewayIdentity)
	}
	ev := waitForRecAudit(t, srv.cfg.Audit.(*recRecorder), runID, "run.ceiling.reassert", "success")
	if !strings.Contains(string(ev.Data), `"`+bedrockCeilingLane+`"`) {
		t.Errorf("run.ceiling.reassert = %s, want %s in dropped_broker_lanes", ev.Data, bedrockCeilingLane)
	}
}

// The session grant of a gateway lane names the gateway among the hosts it signs for, so a boundary
// that withholds by host sees where the signed credential goes; the re-signer carries the gateway.
func TestGatewayBedrockSSOGrantNamesTheGateway(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	captured := &captureGrantStore{}
	s.cfg.Store = captured
	auth := ssoInjectAuth()
	auth.sigv4Hosts = sigv4LaneHosts
	gw := ssoGatewayWant
	auth.sigv4Gateway = &gw
	_, mitm, sig, ok := s.authorBedrockSSOInjection(context.Background(), types.AgentRun{ID: uuid.New()}, llmTransport{bedrock: auth},
		awsSSOScope{perUser: true, owner: "member@corp.example"}, nil)
	if !ok || sig == nil || len(captured.grants) != 1 || mitm != nil {
		t.Fatalf("authored ok=%v sigv4=%+v grants=%d mitm=%v", ok, sig, len(captured.grants), mitm)
	}
	if !slices.Equal(sig.Hosts, sigv4LaneHosts) || len(sig.Gateways) != 1 || sig.Gateways[0] != ssoGatewayWant {
		t.Errorf("aws_sigv4 = %+v", *sig)
	}
	hosts := grantHosts(captured.grants[0].Spec)
	if !slices.Contains(hosts, ssoGatewayHost) || !slices.Contains(hosts, sigv4RuntimeHost) {
		t.Errorf("grant hosts %v, want the gateway and %s", hosts, sigv4RuntimeHost)
	}
}

// A mint for a sign-in row passes from the route its dispatch sealed, and refuses once its gateway moves.
func TestGatewayMintForABedrockSSORow(t *testing.T) {
	f := newGatewayFixture(t)
	p := testGatewayBedrockSSO()
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

// A revive keeps a sign-in row's route while its binding holds. A boundary that now withholds the
// gateway drops the re-signer with its entry and the route, named in dropped_broker_lanes.
func TestGatewayReviveBedrockSSO(t *testing.T) {
	f := newGatewayFixture(t)
	p := testGatewayBedrockSSO()
	f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(p) })
	rt, _ := gatewayRouteOf(p)
	if ref, err := f.srv.gatewayIdentityRefusal(context.Background(), &proxy.Config{RunID: f.run.ID, GatewayIdentity: []proxy.GatewayIdentityRoute{rt}}); ref != nil || err != nil {
		t.Fatalf("binding held: %v %v", ref, err)
	}
	gw := proxy.AWSSigV4Gateway{Host: rt.Host, Port: rt.Port, PathPrefix: rt.PathPrefix, SigningHost: sigv4RuntimeHost}
	other := testGatewayProvider()
	otherRT, _ := gatewayRouteOf(other)
	for _, tc := range []struct {
		deny string
		kept bool
	}{{"pypi.org", true}, {rt.Host, false}, {sigv4RuntimeHost, false}} {
		deny := []string{tc.deny}
		layers := []sealLayer{{rec: boundaryProvenance, egress: boundarySealEgress(types.BoundarySpec{DeniedDomains: &deny})}}
		cfg := &proxy.Config{RunID: f.run.ID, GatewayIdentity: []proxy.GatewayIdentityRoute{rt, otherRT},
			AWSSigV4: &proxy.AWSSigV4Config{GrantID: uuid.New(), PortalHost: "portal.sso.us-east-1.amazonaws.com", Hosts: sigv4LaneHosts,
				Gateways: []proxy.AWSSigV4Gateway{gw}}}
		re, rerr := reassertProxyCeiling(f.run, cfg, ownerCeiling{}, layers)
		if rerr != nil {
			t.Fatalf("deny %s: refused: %+v", tc.deny, rerr)
		}
		kept := cfg.AWSSigV4 != nil
		hasRoute := slices.Contains(cfg.GatewayIdentity, rt)
		if kept != tc.kept || hasRoute != tc.kept || slices.Contains(re.droppedLane, awsSigV4Lane) == tc.kept {
			t.Errorf("deny %s: aws_sigv4 kept %v, route kept %v (dropped %v), want kept %v", tc.deny, kept, hasRoute, re.droppedLane, tc.kept)
		}
		if !slices.Contains(cfg.GatewayIdentity, otherRT) {
			t.Errorf("deny %s: another provider's route went with it: %+v", tc.deny, cfg.GatewayIdentity)
		}
	}
}

// A person's own runner cannot reach the mint, so placement refuses a gateway lane there.
func TestGatewayBedrockSSORefusedOnAPersonsRunner(t *testing.T) {
	f, _, run, spec, llm := resignedLocalRun(t)
	rt, _ := gatewayRouteOf(ssoGatewayProvider())
	spec.ProxyConfig.GatewayIdentity = []proxy.GatewayIdentityRoute{rt}
	spec.ProxyConfig.AWSSigV4.Gateways = []proxy.AWSSigV4Gateway{ssoGatewayWant}
	p, err := f.srv.localResolvedPlan(context.Background(), run, false, f.st.siteConfig, spec, nil, llm)
	if err != nil {
		t.Fatal(err)
	}
	if _, ref := placement.LocalEligibility(p); ref == nil {
		t.Fatal("a gateway lane on a person's runner was placed there")
	}
}

// An ordinary run on a person's own runner, with no Bedrock lane and no run identity, is classified
// and admitted: every field the gateway lane added to the dispatch metadata is classified.
func TestGatewayIdentityOffLocalRunAdmitted(t *testing.T) {
	if got := localDispatchUnclassified(); len(got) != 0 {
		t.Fatalf("unclassified dispatch metadata: %v", got)
	}
	f := newComponentFixture(t)
	f.srv.cfg.Store = localPlanTestStore{f.st}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal}
	p, err := f.srv.localResolvedPlan(context.Background(), run, false, types.SiteConfig{}, runner.SandboxSpec{RunID: run.ID}, nil, llmTransport{})
	if err != nil {
		t.Fatalf("an ordinary run on a person's runner: %v", err)
	}
	if _, ref := placement.LocalEligibility(p); ref != nil {
		t.Fatalf("an ordinary run on a person's runner was refused: %v", ref)
	}
}
