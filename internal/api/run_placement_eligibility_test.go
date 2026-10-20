// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
	"github.com/google/uuid"
)

func TestLocalPlacementDoorsP3AndUpstreamRefuseBeforeIdentityOrStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*componentFixture)
		body    map[string]any
		reason  string
		status  int
	}{
		{name: "P3 unassigned default", body: componentBody(map[string]any{"name": "My Tool", "inline": map[string]any{"hosts": []string{"mine.example"}}}), reason: string(placement.ReasonPlacementComponentSelfDefine), status: 403},
		{name: "upstream secret", arrange: func(f *componentFixture) { f.st.siteConfig.UpstreamProxySecretRef = "corporate-parent" }, body: componentBody(), reason: string(placement.ReasonPlacementCredential), status: 422},
		{name: "own inputs still unsupported", body: componentBody(), reason: string(placement.ReasonPlacementUnavailable), status: 422},
	} {
		for _, door := range componentDoors {
			t.Run(tc.name+door, func(t *testing.T) {
				f := newComponentFixture(t)
				if tc.arrange != nil {
					tc.arrange(f)
				}
				spy := &mintCountingIdentity{Provider: f.srv.cfg.Identity}
				f.srv.cfg.Identity = spy
				body := map[string]any{}
				for k, v := range tc.body {
					body[k] = v
				}
				body["placement"] = "local"
				w := f.ask(t, door, body)
				got := decodeErrorBody(t, w)
				if w.Code != tc.status || got.Reason != tc.reason {
					t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.reason)
				}
				if spy.mints != 0 || len(f.st.runs) != 0 || len(f.st.grants) != 0 || len(f.st.snapshots) != 0 {
					t.Fatalf("refused local run wrote/minted: %d/%d/%d/%d", spy.mints, len(f.st.runs), len(f.st.grants), len(f.st.snapshots))
				}
				if tc.reason == string(placement.ReasonPlacementComponentSelfDefine) && len(f.componentAudit(0, "authz.denied")) == 0 {
					t.Fatal("P3 refusal was not audited")
				}
			})
		}
	}
}

func TestLocalPlacementRefusalCannotRenewExpiredAWSOrWritePair(t *testing.T) {
	// Proxy-inject off refuses the token cache (placement_credential); on, only the unpooled refusal is left.
	for inject, want := range map[bool]placement.Reason{true: placement.ReasonPlacementUnavailable, false: placement.ReasonPlacementCredential} {
		srv := createRenewalFixture(t)
		srv.cfg.AWSSSOProxyInject = inject
		calls := renewedOIDC(t)
		for _, door := range contractDoors {
			body := strings.TrimSuffix(createRenewalBody, "}") + `,"placement":"local"}`
			w := do(t, srv, http.MethodPost, door.path, providerAdminToken(srv, createRenewalOwner), body)
			if w.Code != 422 || errorReason(w) != string(want) {
				t.Fatalf("inject=%v %s=%d %s", inject, door.name, w.Code, w.Body.String())
			}
			if !inject && !strings.Contains(w.Body.String(), bedrockSSOSecretEnvField) {
				t.Errorf("%s refusal does not name the token cache: %s", door.name, w.Body.String())
			}
		}
		if calls.Load() != 0 || runRowCount(srv) != 0 {
			t.Fatalf("inject=%v refused path renewed=%d created=%d", inject, calls.Load(), runRowCount(srv))
		}
		if got, _ := createRenewalStored(t, srv); got.RefreshToken != "old-refresh-token-1234567890" {
			t.Fatalf("inject=%v refused local path changed the pair", inject)
		}
	}
}

func TestLocalStoredGrantPersistenceForcesOwnerOnlyAndRefusesFallback(t *testing.T) {
	scopes := []types.GrantSpec{
		{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.example","secret_name":"person-secret"}`)},
		{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"git.example","secret_name":"person-secret"}`)},
		{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"ssh.github.com","key_secret_ref":"person-secret"}`)},
		{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"MY_KEY","secret_name":"person-secret"}`)},
		{Kind: types.GrantFileSecret, Scope: json.RawMessage(`{"file":"key","secret_name":"person-secret"}`)},
	}
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	spec := types.RunPolicySpec{EligibleGrants: scopes}
	if _, ok := f.srv.persistRunGrants(context.Background(), httptest.NewRecorder(), r, uuid.New(), time.Now().UTC(), spec, types.PlacementLocal, personalJudgement(storedPolicy(t, f.srv))); !ok {
		t.Fatal("own local grants refused")
	}
	if len(f.st.grants) != len(scopes) {
		t.Fatalf("persisted=%d", len(f.st.grants))
	}
	for _, g := range f.st.grants {
		if !g.Spec.OwnerOnly {
			t.Fatalf("%s permits operator fallback", g.Spec.Kind)
		}
	}
	for _, g := range scopes {
		if g.OwnerOnly {
			t.Fatal("mutated caller policy")
		}
	}
	before := len(f.st.grants)
	spec.EligibleGrants = append(spec.EligibleGrants, types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"corp.example","secret_name":"operator-only-secret"}`)})
	if _, ok := f.srv.persistRunGrants(context.Background(), httptest.NewRecorder(), r, uuid.New(), time.Now().UTC(), spec, types.PlacementLocal, personalJudgement(storedPolicy(t, f.srv))); ok || len(f.st.grants) != before {
		t.Fatal("missing own namespace wrote a partial grant set")
	}
}

// Grant persistence judges an org-held grant by the delivery policy the run's plan was judged on, never a
// second read of the store: the store here holds no policy (every class refused), and the plan's via_org
// policy alone admits the grant, recorded via_org.
func TestLocalGrantPersistenceUsesThePlansDeliveryPolicy(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"corp.example","secret_name":"operator-only-secret"}`)}}}
	viaOrg := placement.DeliveryPolicy{Classes: map[string]placement.ClassPolicy{placement.ClassAPIKey: {Mode: placement.ModeViaOrg}}}
	gw, ok := f.srv.persistRunGrants(context.Background(), httptest.NewRecorder(), r, uuid.New(), time.Now().UTC(), spec, types.PlacementLocal, personalJudgement(viaOrg))
	if !ok || len(gw.delivery) != 1 || gw.delivery[0].Mode != placement.ModeViaOrg {
		t.Fatalf("ok=%v delivery=%+v: the plan's via_org policy did not admit the org grant", ok, gw.delivery)
	}
	if _, ok := f.srv.persistRunGrants(context.Background(), httptest.NewRecorder(), r, uuid.New(), time.Now().UTC(), spec, types.PlacementLocal, personalJudgement(storedPolicy(t, f.srv))); ok {
		t.Error("the org grant persisted under a policy that refuses its class")
	}
}

func TestLocalPlacementOrdinaryScanTaskDoesNotBecomeTrustedOutput(t *testing.T) {
	f := newComponentFixture(t)
	for _, door := range componentDoors {
		body := componentBody()
		body["task"] = "source scan"
		body["placement"] = "local"
		w := f.ask(t, door, body)
		if w.Code != 422 || errorReason(w) != string(placement.ReasonPlacementUnavailable) {
			t.Fatalf("ordinary task became trusted output: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestLocalP3ComponentNameIsResponseOnly(t *testing.T) {
	f := newComponentFixture(t)
	body := componentBody(map[string]any{"name": "Private Personal Tool", "inline": map[string]any{"hosts": []string{"mine.example"}}})
	body["placement"] = "local"
	w := f.ask(t, componentDoors[0], body)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "Private Personal Tool") {
		t.Fatalf("P3 response=%d %s", w.Code, w.Body.String())
	}
	for _, ev := range f.rec.snapshot() {
		if strings.Contains(string(ev.Data), "Private Personal Tool") {
			t.Fatalf("personal component name reached append-only %s", ev.Action)
		}
	}
}

func TestLocalP3ProfileLimitLiftsOnlyItsOwnRefusal(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	req := createRunRequest{Placement: placement.Local}
	self := runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{Name: "mine", SelfDefined: true}}}}
	org := runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{Name: "org tool"}}}}
	for _, tc := range []struct {
		name    string
		allowed bool
		comps   runComponents
		reason  placement.Reason
	}{
		{"self-defined, default deny", false, self, placement.ReasonPlacementComponentSelfDefine},
		{"self-defined, profile allows", true, self, placement.ReasonPlacementUnavailable},
		{"org component never needs the limit", false, org, placement.ReasonPlacementUnavailable},
	} {
		ceiling := governanceCeiling{Limits: types.GovernanceLimits{LocalSelfDefinedComponents: tc.allowed}}
		_, ref := f.srv.localPlacementRefusal(r, req, false, nil, types.RunPolicySpec{}, ceiling, tc.comps, nil, nil, composer.Baseline{})
		if ref == nil || ref.body.Reason != string(tc.reason) {
			t.Fatalf("%s: %+v", tc.name, ref)
		}
		if tc.reason == placement.ReasonPlacementUnavailable && ref.body.Error != localUnpooledMsg {
			t.Fatalf("%s: an unpooled local run must name its remedy, said %q", tc.name, ref.body.Error)
		}
	}
}

// LE-8: an organisation drive never goes to a person's own runner. A run a self-hosted pool places is refused
// at create for every drive backend: a managed volume or claim lives on the organisation's substrate, and a
// host_path share waits for a runner binding (placement_local_path). The same run without a drive is admitted.
func TestLocalPlacement_OrgDriveRefusedOnSelfHosted(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	req := createRunRequest{Placement: placement.Local}
	for _, tc := range []struct {
		backend types.DriveBackend
		reason  placement.Reason
	}{
		{types.DriveBackendDockerVolume, placement.ReasonPlacementCapability},
		{types.DriveBackendK8sPVC, placement.ReasonPlacementCapability},
		{types.DriveBackendK8sPVCStatic, placement.ReasonPlacementCapability},
		{types.DriveBackendHostPath, placement.ReasonPlacementLocalPath},
	} {
		drive := []types.DriveMount{{Backend: tc.backend, ObjectName: "wardyn-drive-team-home"}}
		_, ref := f.srv.localPlacementRefusal(r, req, true, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, drive, nil, composer.Baseline{})
		if ref == nil || ref.body.Reason != string(tc.reason) {
			t.Errorf("%s drive on a self-hosted run = %+v, want %s", tc.backend, ref, tc.reason)
		}
	}
	if _, ref := f.srv.localPlacementRefusal(r, req, true, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{}); ref != nil {
		t.Errorf("the same pooled run without a drive = %+v, want it admitted", ref)
	}
}

// An MCP server's credential the organisation holds never reaches a person's own runner: the broker there
// could only resolve it through the relay, which serves a grant delivered own. The doors refuse it by name;
// the same server with the person's own secret is admitted.
func TestLocalPlacement_MCPSharedCredentialRefused(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	req := createRunRequest{Placement: placement.Local}
	comps := runComponents{mcpHosts: []string{"mcp.example"}}
	grant := func(secret string, shared bool) types.RunPolicySpec {
		scope, _ := json.Marshal(map[string]any{"host": "MCP.example", "secret_name": secret, "shared": shared})
		return types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: scope, OwnerOnly: !shared}}}
	}
	_, ref := f.srv.localPlacementRefusal(r, req, true, nil, grant("operator-only-secret", true), governanceCeiling{}, comps, nil, nil, composer.Baseline{})
	if ref == nil || ref.body.Reason != string(placement.ReasonPlacementCredential) || ref.body.Error != localMCPSharedCredentialMsg {
		t.Fatalf("org-held MCP credential on a local run = %+v, want %s naming the MCP server", ref, placement.ReasonPlacementCredential)
	}
	if _, ref := f.srv.localPlacementRefusal(r, req, true, nil, grant("person-secret", false), governanceCeiling{}, comps, nil, nil, composer.Baseline{}); ref != nil {
		t.Errorf("own MCP credential on a local run = %+v, want it admitted", ref)
	}
	if _, ref := f.srv.localPlacementRefusal(r, req, true, nil, grant("operator-only-secret", true), governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{}); ref != nil && ref.body.Error == localMCPSharedCredentialMsg {
		t.Error("a shared header credential that is not an MCP server's was refused as one")
	}
}

// A key provider that routes through a configured gateway (a BaseURL, or any custom endpoint) is
// refused for the person's own runner at create, preflight and preview (localPlacementRefusal). The runner
// never gets the gateway table (LLMUpstreams is the organisation's network), so its brokered route would dial
// the vendor host while the credential is keyed on the gateway, and every model call would fail. The same key
// provider with no gateway is admitted.
func TestLocalPlacement_GatewayKeyLaneRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    types.ModelProviderKind
		base    string
		refused bool
	}{
		{"anthropic key through a gateway", types.ModelProviderAnthropicAPIKey, "https://gw.corp.example/v1", true},
		{"custom endpoint", types.ModelProviderCustomEndpoint, "https://llm.corp.example", true},
		{"anthropic key to the vendor", types.ModelProviderAnthropicAPIKey, "", false},
	} {
		f := newComponentFixture(t)
		f.srv.cfg.Store = localPlanTestStore{f.st}
		p := types.ModelProvider{ID: "gw", UID: "uid-gw", Kind: tc.kind, BaseURL: tc.base, Harnesses: []types.ProviderHarness{{Harness: "claude-code"}}}
		f.st.siteConfig = types.SiteConfig{ModelProviders: providerBlock(p)}
		ctx := utCtx("user", "", nil)
		if err := f.srv.cfg.Secrets.For(runIdentitySubject(ctx, capSub)).Put(ctx, providerSecretName(p.UID, providerKeyPart), []byte("own-key")); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
		req := createRunRequest{Placement: placement.Local, Agent: "claude-code", ModelProvider: p.ID}
		_, ref := f.srv.localPlacementRefusal(r, req, true, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{})
		switch {
		case tc.refused && (ref == nil || ref.body.Reason != string(placement.ReasonPlacementCredential) || !strings.HasPrefix(ref.body.Error, "ProxyConfig.LLMUpstreams: ")):
			t.Errorf("%s: refusal %+v, want placement_credential on ProxyConfig.LLMUpstreams", tc.name, ref)
		case !tc.refused && ref != nil:
			t.Errorf("%s: refusal %+v, want it admitted", tc.name, ref)
		}
	}
}

// A harness set judges every harness's provider at the doors, as dispatch authors every lane: a second
// harness on a gateway-routed key provider is refused at create, preflight and preview, not only after
// the run was accepted. The same set with the second harness on the vendor host is admitted.
func TestLocalPlacement_SecondHarnessGatewayRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		base    string
		refused bool
	}{
		{"second harness through a gateway", "https://gw.corp.example/v1", true},
		{"second harness to the vendor", "", false},
	} {
		f := newComponentFixture(t)
		f.srv.cfg.Store = localPlanTestStore{f.st}
		primary := types.ModelProvider{ID: "direct", UID: "uid-direct", Kind: types.ModelProviderAnthropicAPIKey, Harnesses: []types.ProviderHarness{{Harness: "claude-code"}}}
		second := types.ModelProvider{ID: "second", UID: "uid-second", Kind: types.ModelProviderOpenAIAPIKey, BaseURL: tc.base, Harnesses: []types.ProviderHarness{{Harness: codexHarness}}}
		f.st.siteConfig = types.SiteConfig{ModelProviders: providerBlock(primary, second)}
		ctx := utCtx("user", "", nil)
		for _, p := range []types.ModelProvider{primary, second} {
			if err := f.srv.cfg.Secrets.For(runIdentitySubject(ctx, capSub)).Put(ctx, providerSecretName(p.UID, providerKeyPart), []byte("own-key")); err != nil {
				t.Fatal(err)
			}
		}
		req := createRunRequest{Experience: client.ExperienceInteractive, Placement: placement.Local, Tools: []client.IncludedTool{
			{ID: "claude-code", Kind: client.IncludedToolHarness, ModelProvider: primary.ID},
			{ID: codexHarness, Kind: client.IncludedToolHarness, ModelProvider: second.ID},
		}}
		projectRunMode(&req)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
		_, ref := f.srv.localPlacementRefusal(r, req, true, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{})
		switch {
		case tc.refused && (ref == nil || ref.body.Reason != string(placement.ReasonPlacementCredential) || !strings.HasPrefix(ref.body.Error, "ProxyConfig.LLMUpstreams: ")):
			t.Errorf("%s: refusal %+v, want placement_credential on ProxyConfig.LLMUpstreams", tc.name, ref)
		case !tc.refused && ref != nil:
			t.Errorf("%s: refusal %+v, want it admitted", tc.name, ref)
		}
	}
}

// storedPolicy is the delivery policy the store holds now: what a create's local plan would be judged on.
func storedPolicy(t *testing.T, s *Server) placement.DeliveryPolicy {
	t.Helper()
	p, err := s.storedDeliveryPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// An organisation's MCP server that runs in the sandbox never reaches a person's own runner: its launch
// command and args are the organisation's, written into the harness's MCP configuration unjudged. The
// person's own such server, the organisation's on a remote-hosted runner, and the organisation's remote
// server are admitted.
func TestLocalPlacement_MCPOrgServerInSandboxRefused(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	local := createRunRequest{Placement: placement.Local}
	def := *localInline("Files").Inline
	def.Secrets = nil
	orgID := uuid.New()
	org := func(d types.ComponentDefinition) runComponents {
		return runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{ComponentID: &orgID, Name: "Org Files", Version: 1, Definition: d}}}}
	}
	self := runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{SelfDefined: true, Owner: "sub-owner", Name: "Files", Definition: def}}}}
	allowSelf := governanceCeiling{Limits: types.GovernanceLimits{LocalSelfDefinedComponents: true}}
	judge := func(req createRunRequest, ceiling governanceCeiling, comps runComponents) *runRefusal {
		_, ref := f.srv.localPlacementRefusal(r, req, true, nil, types.RunPolicySpec{}, ceiling, comps, nil, nil, composer.Baseline{})
		return ref
	}

	if ref := judge(local, governanceCeiling{}, org(def)); ref == nil || ref.status != placement.ReasonPlacementCredential.Status() ||
		ref.body.Reason != string(placement.ReasonPlacementCredential) || ref.body.Error != localMCPOrgServerMsg {
		t.Fatalf("org MCP server in the sandbox on a local run = %+v, want %s %q", ref, placement.ReasonPlacementCredential, localMCPOrgServerMsg)
	}
	if ref := judge(local, allowSelf, self); ref != nil {
		t.Errorf("the person's own MCP server in the sandbox on a local run = %+v, want it admitted", ref)
	}
	if ref := judge(createRunRequest{Placement: placement.Remote}, governanceCeiling{}, org(def)); ref != nil {
		t.Errorf("org MCP server in the sandbox on a remote-hosted run = %+v, want it admitted", ref)
	}
	remote := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.files.test/mcp"}}
	if ref := judge(local, governanceCeiling{}, org(remote)); ref != nil {
		t.Errorf("org remote MCP server on a local run = %+v, want it admitted as before", ref)
	}
}
