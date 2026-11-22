// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gatewayLocalSentence is the refusal of the organisation's "tools" MCP server, which sends run identity.
const gatewayLocalSentence = `The MCP server "tools" sends your organisation's run identity, so it runs only on your organisation's remote-hosted runners. ` +
	"Remove it from this run, or run this on a remote-hosted runner."

// An organisation's remote MCP server that sends run identity takes a token from a mint no runner relays,
// so a run on a person's own runner that carries it is refused before it is made, naming the server and
// never the spec field. The same run on a remote-hosted runner, and the person's own server with the same
// definition (which sends no identity), are admitted.
func TestLocalPlacement_GatewayMCPServerRefusedBeforeTheRun(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	def := testGatewayMCPComponent().Definition
	id := uuid.New()
	org := runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{ComponentID: &id, Name: "Tools", Version: 3, Definition: def}}}}
	self := runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{SelfDefined: true, Owner: "sub-owner", Name: "Tools", Definition: def}}}}
	judge := func(p types.Placement, ceiling governanceCeiling, comps runComponents) *runRefusal {
		_, ref := f.srv.localPlacementRefusal(r, createRunRequest{Placement: p}, true, nil, nil, types.RunPolicySpec{}, ceiling, comps, nil, nil, composer.Baseline{})
		return ref
	}
	ref := judge(placement.Local, governanceCeiling{}, org)
	if ref == nil || ref.status != placement.ReasonPlacementCredential.Status() || ref.body.Reason != string(placement.ReasonPlacementCredential) ||
		ref.body.Error != gatewayLocalSentence {
		t.Fatalf("org identity MCP server on a local run = %+v, want %s %q", ref, placement.ReasonPlacementCredential, gatewayLocalSentence)
	}
	// A pool that also offers a remote-hosted runner adds how to get one, as its own sentence.
	ref.addRemoteInstead("Mixed")
	if want := gatewayLocalSentence + " " + remoteInsteadMsg("Mixed"); ref.body.Error != want {
		t.Errorf("with the remote-hosted offer = %q, want %q", ref.body.Error, want)
	}
	if ref := judge(placement.Remote, governanceCeiling{}, org); ref != nil {
		t.Errorf("org identity MCP server on a remote-hosted run = %+v, want it admitted", ref)
	}
	if ref := judge(placement.Local, governanceCeiling{Limits: types.GovernanceLimits{LocalSelfDefinedComponents: true}}, self); ref != nil {
		t.Errorf("the person's own server, which sends no identity, on a local run = %+v, want it admitted", ref)
	}
}

// Create, Review (preflight) and the policy preview each refuse the local run with the same named sentence.
func TestLocalPlacement_GatewayMCPServerRefusedAtEveryDoor(t *testing.T) {
	for _, door := range componentDoors {
		f := newComponentFixture(t)
		c := testGatewayMCPComponent()
		f.st.components = append(f.st.components, c)
		body := componentBody(map[string]any{"id": c.ID.String()})
		body["placement"] = "local"
		w := f.ask(t, door, body)
		got := decodeErrorBody(t, w)
		if w.Code != placement.ReasonPlacementCredential.Status() || got.Reason != string(placement.ReasonPlacementCredential) ||
			got.Error != gatewayLocalSentence {
			t.Errorf("%s: %d %s, want %s %q", door, w.Code, w.Body.String(), placement.ReasonPlacementCredential, gatewayLocalSentence)
		}
	}
}

// localPersonRunnerStore binds a local dispatch fixture's run to a person's runner, the one runner whose
// refusal says "your own runner" (localRefusalPrefix).
type localPersonRunnerStore struct {
	localDispatchStore
	row types.Runner
}

func (s localPersonRunnerStore) GetRunner(context.Context, uuid.UUID) (types.Runner, error) {
	return s.row, nil
}

// Dispatch stays the backstop: a local spec carrying a run identity route is refused there, and the
// run's failure hint names the server, not the spec field the audit row keeps.
func TestLocalDispatchRefusesAGatewayRouteNamingTheServer(t *testing.T) {
	srv, st, audit, run := localDispatchFixture(t, types.PlacementLocal)
	id := uuid.New()
	run.RunnerID = &id
	st.run = run
	srv.cfg.Store = localPersonRunnerStore{localDispatchStore: st, row: types.Runner{ID: id, OwnerKind: types.RunnerOwnerPerson, TrustClass: types.RunnerTrustPersonal}}
	spec := localOrgNetworkSpec(run.ID)
	spec.ProxyConfig.GatewayIdentity = []proxy.GatewayIdentityRoute{{Host: "mcp.corp.example", Port: 443, MCPKey: "tools", Audience: "mcp-aud"}}
	if srv.classifyLocalDispatch(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), types.SiteConfig{}, &spec, nil, llmTransport{}, false) {
		t.Fatal("a run identity route was classified for a local runner")
	}
	ev := findAudit(audit.snapshot(), run.ID, "run.dispatch", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), string(placement.ReasonPlacementCredential)) || !strings.Contains(string(ev.Data), "ProxyConfig.GatewayIdentity") {
		t.Fatalf("no canonical refusal: %s", auditDump(audit.snapshot(), run.ID))
	}
	if want := "This run was not launched on your own runner: " + gatewayLocalSentence; st.failureHint != want {
		t.Fatalf("failure hint %q, want %q", st.failureHint, want)
	}
}

// A Bedrock provider that sends run identity sets no LLMUpstreams entry, so nothing else refuses it on a
// person's own runner: create judges its route on the same gate and refuses the run, naming the provider.
func TestLocalPlacement_GatewayBedrockProviderRefusedAtCreate(t *testing.T) {
	f := newComponentFixture(t)
	f.srv.cfg.Store = localPlanTestStore{f.st}
	p := testGatewayBedrockBearer()
	f.st.siteConfig = types.SiteConfig{ModelProviders: providerBlock(p)}
	ctx := utCtx("user", "", nil)
	if err := f.srv.cfg.Secrets.For(runIdentitySubject(ctx, capSub)).Put(ctx, providerSecretName(p.UID, providerKeyPart), []byte("own-key")); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	req := createRunRequest{Placement: placement.Local, Agent: "claude-code", ModelProvider: p.ID}
	_, ref := f.srv.localPlacementRefusal(r, req, true, nil, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{})
	want := `The model provider "corp-gw" sends your organisation's run identity, so it runs only on your organisation's remote-hosted runners. ` +
		"Remove it from this run, or run this on a remote-hosted runner."
	if ref == nil || ref.body.Reason != string(placement.ReasonPlacementCredential) || ref.body.Error != want {
		t.Fatalf("a Bedrock identity provider on a local run = %+v, want %s %q", ref, placement.ReasonPlacementCredential, want)
	}
}

// A key or custom_endpoint provider that sends run identity and has a base URL sets both an LLMUpstreams entry and a
// run identity route. Create refuses the local run on the identity gate, which names the provider, not on the
// LLMUpstreams gate, whose sentence names nothing (placement/gateway_identity_named_test.go is the unit twin).
func TestLocalPlacement_GatewayKeyProviderRefusedAtCreate(t *testing.T) {
	endpoint := testGatewayProvider()
	key := endpoint
	key.Kind = types.ModelProviderAnthropicAPIKey
	for _, p := range []types.ModelProvider{endpoint, key} {
		t.Run(string(p.Kind), func(t *testing.T) {
			f := newComponentFixture(t)
			f.srv.cfg.Store = localPlanTestStore{f.st}
			f.st.siteConfig = types.SiteConfig{ModelProviders: providerBlock(p)}
			ctx := utCtx("user", "", nil)
			if err := f.srv.cfg.Secrets.For(runIdentitySubject(ctx, capSub)).Put(ctx, providerSecretName(p.UID, providerKeyPart), []byte("own-key")); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
			req := createRunRequest{Placement: placement.Local, Agent: "claude-code", ModelProvider: p.ID}
			_, ref := f.srv.localPlacementRefusal(r, req, true, nil, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{})
			want := `The model provider "corp-gw" sends your organisation's run identity, so it runs only on your organisation's remote-hosted runners. ` +
				"Remove it from this run, or run this on a remote-hosted runner."
			if ref == nil || ref.status != placement.ReasonPlacementCredential.Status() || ref.body.Reason != string(placement.ReasonPlacementCredential) || ref.body.Error != want {
				t.Fatalf("a %s identity provider with a base URL on a local run = %+v, want %s %q", p.Kind, ref, placement.ReasonPlacementCredential, want)
			}
			if strings.Contains(ref.body.Error, "ProxyConfig.LLMUpstreams") {
				t.Errorf("the refusal %q is the unnamed LLMUpstreams one", ref.body.Error)
			}
		})
	}
}

// A run that waits on its pool's remote-hosted runners records that it can never take one of the person's
// own when it carries an identity server, so the queue never offers it one; without the identity it can.
func TestStepLocalEligibility_WaitingRunWithAnIdentityServerRecordsNoLocal(t *testing.T) {
	def := testGatewayMCPComponent().Definition
	plain := def
	mcp := *def.MCP
	mcp.GatewayAudience = ""
	plain.MCP = &mcp
	id := uuid.New()
	for _, tc := range []struct {
		name    string
		def     types.ComponentDefinition
		noLocal bool
	}{{"identity server", def, true}, {"same server, no identity", plain, false}} {
		f := newComponentFixture(t)
		req := createRunRequest{Agent: "claude-code", Task: "t"}
		w := httptest.NewRecorder()
		fold := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil)), req: &req, queued: true,
			place: runPlacement{pooled: true, placement: types.PlacementRemote},
			comps: runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{ComponentID: &id, Name: "Tools", Version: 3, Definition: tc.def}}}}}
		if ok := f.srv.stepLocalEligibility(fold, &foldRecorder{}); !ok || fold.place.noLocal != tc.noLocal || w.Body.Len() != 0 {
			t.Errorf("%s: the step = %v, no_local %v, answered %q; want it passed, no_local %v, nothing written", tc.name, ok, fold.place.noLocal, w.Body, tc.noLocal)
		}
	}
}
