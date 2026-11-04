// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// inspMarker is what the overlapping detector (a classified marker) finds; a
	// gateway that inspects prompts runs checks like it.
	inspMarker = "INTERNAL ONLY"
	// inspSecret is one of the run's own secret values: only Wardyn holds it.
	inspSecret = "run-own-secret-value-0123456789abcdef"
)

func inspSpec(detectSecrets bool) types.LLMInspectionSpec {
	return types.LLMInspectionSpec{Mode: "block", DetectSecrets: detectSecrets, ClassifiedMarkers: []string{inspMarker}}
}

// deferredRoutes is gwidRoutes with the route's gateway inspecting prompts.
func deferredRoutes() []GatewayIdentityRoute {
	rts := gwidRoutes()
	rts[0].InspectionDeferred = true
	return rts
}

// newInspectRig is the gateway rig with content inspection on, built as the server builds it.
func newInspectRig(t *testing.T, routes []GatewayIdentityRoute, spec types.LLMInspectionSpec) *gwidRig {
	t.Helper()
	g := newGwidRig(t, routes, false)
	corpus := [][]byte{[]byte(inspSecret)}
	eng, err := contentscan.NewEngine(spec, corpus)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine = %v, %v", eng, err)
	}
	g.p.scanner = eng
	if g.p.deferredScan, err = deferredScanFor(eng, spec, corpus, routes); err != nil {
		t.Fatal(err)
	}
	return g
}

func inspBody(text string) string {
	b, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 8, "messages": []map[string]string{{"role": "user", "content": text}}})
	return string(b)
}

// inspSend sends a prompt on one lane: "brokered" (the route's own /v1/messages) or
// "mitm" to path on the gateway host.
func inspSend(t *testing.T, g *gwidRig, lane, path, text string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	switch lane {
	case "brokered":
		g.p.handleLocalRoute(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader(inspBody(text))))
	case "mitm":
		g.p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+gwidHost+path, strings.NewReader(inspBody(text))), gwidHost, 443)
	default:
		t.Fatalf("lane %q", lane)
	}
	return rec
}

// inspScans is every scan summary the proxy recorded, in order.
func inspScans(t *testing.T, g *gwidRig) []egress.ScanSummary {
	t.Helper()
	var out []egress.ScanSummary
	sc := bufio.NewScanner(strings.NewReader(g.buf.String()))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var d egress.DecisionLog
		if json.Unmarshal(sc.Bytes(), &d) == nil && d.Scan != nil {
			out = append(out, *d.Scan)
		}
	}
	return out
}

// On the route, the overlapping check is the gateway's: a prompt it would block is sent,
// and the row says whose check it was. The same prompt is refused with the switch off.
func TestGatewayInspection_RouteLeavesOverlappingChecksToItsGateway(t *testing.T) {
	for _, lane := range []string{"brokered", "mitm"} {
		t.Run(lane, func(t *testing.T) {
			off := newInspectRig(t, gwidRoutes(), inspSpec(true))
			if rec := inspSend(t, off, lane, "/v1/messages", "this is "+inspMarker); rec.Code != http.StatusForbidden {
				t.Fatalf("switch off: %d %s; the marker must be refused, or this test proves nothing", rec.Code, rec.Body)
			}

			g := newInspectRig(t, deferredRoutes(), inspSpec(true))
			rec := inspSend(t, g, lane, "/v1/messages", "this is "+inspMarker)
			if rec.Code != http.StatusOK || g.gw.count() != 1 {
				t.Fatalf("switch on: %d %s, gateway saw %d; the marker is the gateway's to judge", rec.Code, rec.Body, g.gw.count())
			}
			scans := inspScans(t, g)
			if len(scans) != 1 {
				t.Fatalf("scan rows = %+v; each prompt left to the gateway is one row", scans)
			}
			if s := scans[0]; s.DeferredTo != "prov-1" || s.SkipReason != skipDeferredToGateway || !s.Skipped || !s.Scanned || s.Action != "skip" {
				t.Fatalf("row = %+v; want the provider named, scanned for secrets, the rest skipped", s)
			}
		})
	}
}

// The run's own secret values are checked on the route whatever the switch says, and
// enforced as the policy says: block refuses it before the gateway sees a byte.
func TestGatewayInspection_RunsOwnSecretsStayChecked(t *testing.T) {
	for _, lane := range []string{"brokered", "mitm"} {
		t.Run(lane, func(t *testing.T) {
			g := newInspectRig(t, deferredRoutes(), inspSpec(true))
			rec := inspSend(t, g, lane, "/v1/messages", "the key is "+inspSecret)
			if rec.Code != http.StatusForbidden || g.gw.count() != 0 {
				t.Fatalf("got %d %s, gateway saw %d; the run's own secret must be refused on the route", rec.Code, rec.Body, g.gw.count())
			}
			scans := inspScans(t, g)
			if len(scans) != 1 || scans[0].Action != "block" || scans[0].DeferredTo != "prov-1" {
				t.Fatalf("rows = %+v; want one block row naming the provider", scans)
			}
			if strings.Contains(g.buf.String(), inspSecret) {
				t.Fatal("a decision row holds the secret")
			}
		})
	}
}

// Requests that do not go to the route keep the whole policy: the same host off the
// route's path, a path with a dot segment, and the plain lane, which attaches no token.
func TestGatewayInspection_TrafficOffTheRouteKeepsFullInspection(t *testing.T) {
	for _, path := range []string{"/v2/messages", "/v1/../v2/messages", "/v1/./messages", "/v1x/messages"} {
		t.Run(path, func(t *testing.T) {
			g := newInspectRig(t, deferredRoutes(), inspSpec(true))
			if rec := inspSend(t, g, "mitm", path, "this is "+inspMarker); rec.Code != http.StatusForbidden {
				t.Fatalf("%s: %d %s; off the route the marker must be refused", path, rec.Code, rec.Body)
			}
			for _, s := range inspScans(t, g) {
				if s.DeferredTo != "" {
					t.Fatalf("%s: row %+v left a check to the gateway off its route", path, s)
				}
			}
		})
	}
	t.Run("plain lane", func(t *testing.T) {
		g := newInspectRig(t, deferredRoutes(), inspSpec(true))
		body, _, _, blocked := g.p.inspectLLM(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://"+gwidHost+":443/v1/messages",
			strings.NewReader(inspBody("this is "+inspMarker))), gwidHost, 443, "v1/messages", contentscan.ChannelAnthropicMessages)
		if !blocked || body != nil {
			t.Fatal("a lane that attaches no token must not leave the prompt to the gateway")
		}
	})
}

// A state the proxy cannot vouch for is inspected in full: a deferred route on a proxy
// that never built the cut engine, and a route with the switch off. An MCP route
// cannot carry the switch at all.
func TestGatewayInspection_UnknownStateFailsClosedToWardyn(t *testing.T) {
	g := newInspectRig(t, deferredRoutes(), inspSpec(true))
	g.p.deferredScan = nil
	if rec := inspSend(t, g, "mitm", "/v1/messages", "this is "+inspMarker); rec.Code != http.StatusForbidden {
		t.Fatalf("no cut engine: %d %s; want the whole policy", rec.Code, rec.Body)
	}

	g = newInspectRig(t, gwidRoutes(), inspSpec(true))
	if g.p.deferredScan != nil {
		t.Fatal("a run with no deferred route built the cut engine")
	}
	g.p.deferredScan = &deferredScan{} // built, yet the route itself does not defer
	if rec := inspSend(t, g, "brokered", "", "this is "+inspMarker); rec.Code != http.StatusForbidden {
		t.Fatalf("switch off: %d %s; want the whole policy", rec.Code, rec.Body)
	}

	if d, err := deferredScanFor(nil, inspSpec(true), [][]byte{[]byte(inspSecret)}, deferredRoutes()); d != nil || err != nil {
		t.Fatalf("a run that does not inspect built a cut engine: %v, %v", d, err)
	}

	mcp := GatewayIdentityRoute{Host: "mcp.corp.test", Port: 443, MCPKey: "tools", Audience: "a", InspectionDeferred: true}
	if err := mcp.validate(nil); err == nil || !strings.Contains(err.Error(), "inspection_deferred") {
		t.Fatalf("an MCP route with the switch validated: %v", err)
	}
	raw, _ := json.Marshal(Config{RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:8080", RunToken: "tok", GatewayIdentity: []GatewayIdentityRoute{mcp}})
	if _, err := LoadConfigBytes(raw); err == nil || !strings.Contains(err.Error(), "inspection_deferred") {
		t.Fatalf("a config with an MCP route carrying the switch loaded: %v", err)
	}
}

// With no secret-value check on the policy, the route has nothing of Wardyn's left to
// run: the prompt goes to the gateway unscanned, and the row says so.
func TestGatewayInspection_PolicyWithoutSecretCheckLeavesAll(t *testing.T) {
	g := newInspectRig(t, deferredRoutes(), inspSpec(false))
	if g.p.deferredScan == nil || g.p.deferredScan.eng != nil {
		t.Fatalf("deferredScan = %+v; want built with no engine", g.p.deferredScan)
	}
	rec := inspSend(t, g, "brokered", "", "this is "+inspMarker+" and "+inspSecret)
	if rec.Code != http.StatusOK || g.gw.count() != 1 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	scans := inspScans(t, g)
	if len(scans) != 1 || scans[0].Scanned || scans[0].SkipReason != skipDeferredToGateway || scans[0].DeferredTo != "prov-1" {
		t.Fatalf("rows = %+v; want one unscanned row naming the provider", scans)
	}
	_, _, data := egress.ScanAudit(&scans[0], gwidHost)
	if !strings.Contains(string(data), `"deferred_to":"prov-1"`) {
		t.Fatalf("audit data %s does not name the provider", data)
	}
}

// A Bedrock gateway's calls ride the generic channel, where inspect_forward_egress
// scans the body with the run's whole engine. A route that says it defers changes
// nothing there: the marker is still refused.
func TestGatewayInspection_BedrockLaneKeepsTheFullScanner(t *testing.T) {
	routes := []GatewayIdentityRoute{{Host: gwidOther, Port: 443, ProviderID: "bedrock-gw", Audience: "bedrock-aud", InspectionDeferred: true}}
	spec := inspSpec(true)
	spec.InspectForwardEgress = true
	g := newInspectRig(t, routes, spec)
	if g.p.channelForHost(gwidOther) != contentscan.ChannelGeneric || g.p.deferredScan == nil {
		t.Fatal("the rig is not a deferred route on the generic channel")
	}
	rec := httptest.NewRecorder()
	g.p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+gwidOther+"/model/m/invoke",
		strings.NewReader(`{"prompt":"this is `+inspMarker+`"}`)), gwidOther, 443)
	if rec.Code != http.StatusForbidden || g.other.count() != 0 {
		t.Fatalf("got %d %s, upstream saw %d; the full scanner must refuse the marker", rec.Code, rec.Body, g.other.count())
	}
	for _, s := range inspScans(t, g) {
		if s.DeferredTo != "" {
			t.Fatalf("row %+v claims a check was left to the gateway", s)
		}
	}
}
