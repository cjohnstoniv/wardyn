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
const gatewayLocalSentence = `the MCP server "tools" sends your organisation's run identity, so it runs only on your organisation's remote-hosted runners`

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
		!strings.HasPrefix(ref.body.Error, gatewayLocalSentence) || strings.Contains(ref.body.Error, "ProxyConfig") {
		t.Fatalf("org identity MCP server on a local run = %+v, want %s %q", ref, placement.ReasonPlacementCredential, gatewayLocalSentence)
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
			!strings.HasPrefix(got.Error, gatewayLocalSentence) {
			t.Errorf("%s: %d %s, want %s %q", door, w.Code, w.Body.String(), placement.ReasonPlacementCredential, gatewayLocalSentence)
		}
	}
}

// Dispatch stays the backstop: a local spec carrying a run identity route is refused there, and the
// run's failure hint names the server, not the spec field the audit row keeps.
func TestLocalDispatchRefusesAGatewayRouteNamingTheServer(t *testing.T) {
	srv, st, audit, run := localDispatchFixture(t, types.PlacementLocal)
	spec := localOrgNetworkSpec(run.ID)
	spec.ProxyConfig.GatewayIdentity = []proxy.GatewayIdentityRoute{{Host: "mcp.corp.example", Port: 443, MCPKey: "tools", Audience: "mcp-aud"}}
	if srv.classifyLocalDispatch(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), types.SiteConfig{}, &spec, nil, llmTransport{}, false) {
		t.Fatal("a run identity route was classified for a local runner")
	}
	ev := findAudit(audit.events, run.ID, "run.dispatch", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), string(placement.ReasonPlacementCredential)) || !strings.Contains(string(ev.Data), "ProxyConfig.GatewayIdentity") {
		t.Fatalf("no canonical refusal: %s", auditDump(audit.events, run.ID))
	}
	if want := "This run was not launched on your own runner: " + gatewayLocalSentence; !strings.HasPrefix(st.failureHint, want) || strings.Contains(st.failureHint, "ProxyConfig") {
		t.Fatalf("failure hint %q, want it to start %q", st.failureHint, want)
	}
}
