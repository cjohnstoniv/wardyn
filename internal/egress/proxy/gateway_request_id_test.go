// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const gwrSandboxID = "sandbox-request-id"

// gwrSandboxHeaders puts a request id of every spelling on a request the way a sandbox could.
func gwrSandboxHeaders(r *http.Request) *http.Request {
	for _, name := range gatewayRequestIDHeaders {
		r.Header.Set(name, gwrSandboxID)
	}
	return gwidForge(r)
}

// gwrLanes are the gateway lanes of a model call that read an answer: the brokered route, a
// MITM'd request and an absolute-form request. Each carries the sandbox's own request ids.
func gwrLanes(g *gwidRig, t *testing.T) map[string]func() *httptest.ResponseRecorder {
	body := func() *strings.Reader { return strings.NewReader(`{"hi":1}`) }
	return map[string]func() *httptest.ResponseRecorder{
		"brokered": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.handleLocalRoute(rec, gwrSandboxHeaders(mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", body())))
			return rec
		},
		"mitm": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.serveMITMRequest(rec, gwrSandboxHeaders(httptest.NewRequest(http.MethodPost, "https://"+gwidHost+"/v1/messages", body())), gwidHost, 443)
			return rec
		},
		"plain": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.servePlain(rec, gwrSandboxHeaders(httptest.NewRequest(http.MethodPost, "https://"+gwidHost+"/v1/messages", body())))
			return rec
		},
	}
}

// gwrAllowRow is the one allow row of an attempt that went to the gateway (not its usage row).
func gwrAllowRow(t *testing.T, lane string, rows []map[string]any) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, row := range rows {
		if row["decision"] == string(egress.Allow) && row["gateway_call_id"] != nil && row["usage"] == nil {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: want one gateway allow row, got %d in %v", lane, len(found), rows)
	}
	return found[0]
}

// The id the gateway's answer carries lands on the attempt's allow row, whichever header
// carries it and on every lane that reads the answer.
func TestGatewayRequestID_AnswerHeaderLandsOnTheAllowRow(t *testing.T) {
	for _, header := range gatewayRequestIDHeaders {
		g := newGwidRig(t, gwidRoutes(), false)
		g.gw.respond = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(header, "req-abc_123:XYZ")
			_, _ = w.Write([]byte("ok"))
		}
		for lane, send := range gwrLanes(g, t) {
			g.buf.Reset()
			if rec := send(); rec.Code != http.StatusOK {
				t.Fatalf("%s %s: status %d: %s", lane, header, rec.Code, rec.Body)
			}
			if got := gwrAllowRow(t, lane, gwcidRows(t, g.buf))["gateway_request_id"]; got != "req-abc_123:XYZ" {
				t.Errorf("%s via %s: gateway_request_id = %v, want the answer's id", lane, header, got)
			}
		}
	}
}

// An answer with no id leaves the field out, and so does a sandbox that sent request ids of its
// own: the field is read off the answer and never off the request. REVERT PROOF: reading
// r.Header in stampGatewayRequestID's place puts the sandbox's id on the row and fails here.
func TestGatewayRequestID_NoAnswerHeaderMeansNoFieldAndTheSandboxCannotForgeOne(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	g.gw.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	for lane, send := range gwrLanes(g, t) {
		g.buf.Reset()
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", lane, rec.Code, rec.Body)
		}
		if g.gw.last(t).Header.Get("X-Request-Id") != gwrSandboxID {
			t.Fatalf("%s: the sandbox's request id did not reach the gateway, so this test forges nothing", lane)
		}
		row := gwrAllowRow(t, lane, gwcidRows(t, g.buf))
		if got, present := row["gateway_request_id"]; present {
			t.Errorf("%s: row carries gateway_request_id %v with none in the answer", lane, got)
		}
		if strings.Contains(g.buf.String(), gwrSandboxID) {
			t.Errorf("%s: a row carries the sandbox's own id: %s", lane, g.buf)
		}
	}
}

// A value over 128 characters is dropped, not cut, on the lane that reads it.
func TestGatewayRequestID_OversizedAnswerValueIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		kept     bool
	}{{"128", strings.Repeat("a", 128), true}, {"129", strings.Repeat("a", 129), false}} {
		g := newGwidRig(t, gwidRoutes(), false)
		g.gw.respond = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Request-Id", tc.id)
			_, _ = w.Write([]byte("ok"))
		}
		if rec := gwrLanes(g, t)["mitm"](); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.name, rec.Code)
		}
		got, present := gwrAllowRow(t, tc.name, gwcidRows(t, g.buf))["gateway_request_id"]
		if tc.kept && got != tc.id || !tc.kept && present {
			t.Errorf("%d characters: gateway_request_id = %v (present %v), want kept %v", len(tc.id), got, present, tc.kept)
		}
	}
}

// The id is only read on a run with a gateway route: a direct call carries none, however the
// answer is headed.
func TestGatewayRequestID_AbsentWithNoRoute(t *testing.T) {
	g := newGwidRig(t, nil, false)
	g.gw.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-1")
		_, _ = w.Write([]byte("ok"))
	}
	for lane, send := range gwrLanes(g, t) {
		g.buf.Reset()
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", lane, rec.Code, rec.Body)
		}
		for _, row := range gwcidRows(t, g.buf) {
			if _, ok := row["gateway_request_id"]; ok {
				t.Errorf("%s: a row carries gateway_request_id with no route: %v", lane, row)
			}
		}
	}
}

// An MCP server of a run with a route: the allow row carries the id its answer names, and the
// sandbox's own request id is not recorded.
func TestGatewayRequestID_MCPAllowRowCarriesTheAnswersID(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.github.test"}}, nil, nil)
	f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "github", URL: "https://mcp.github.test/mcp"}}, []MCPBinding{mcpBindingFor("claude-code", "github")})
	var answer string
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if answer != "" {
			w.Header().Set("X-Request-Id", answer)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(backend.Close)
	f.p.dial = redirectDial(upstreamAddr(backend))
	f.p.gatewayRoutes = gwidRoutes()
	for _, want := range []string{"mcp-req-7", ""} {
		answer = want
		f.buf.Reset()
		if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, "X-Request-Id", gwrSandboxID); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		got, present := gwcidRows(t, f.buf)[0]["gateway_request_id"]
		if want != "" && got != want || want == "" && present {
			t.Errorf("answer id %q: row gateway_request_id = %v (present %v)", want, got, present)
		}
		if strings.Contains(f.buf.String(), gwrSandboxID) {
			t.Errorf("a row carries the sandbox's own id: %s", f.buf)
		}
	}
}

// What gatewayRequestID accepts: each provider's header, one that is absent, and the values dropped.
func TestGatewayRequestID_Value(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
		want string
	}{
		{"anthropic request-id", http.Header{"Request-Id": {"req_011CZ"}}, "req_011CZ"},
		{"openai x-request-id", http.Header{"X-Request-Id": {"req_8f2"}}, "req_8f2"},
		{"bedrock x-amzn-requestid", http.Header{"X-Amzn-Requestid": {"0b1c-4d5e"}}, "0b1c-4d5e"},
		{"generic x-request-id", http.Header{"X-Request-Id": {"7d3f"}}, "7d3f"},
		{"none", http.Header{"Content-Type": {"application/json"}}, ""},
		{"empty", http.Header{"X-Request-Id": {""}}, ""},
		{"128 characters", http.Header{"X-Request-Id": {strings.Repeat("a", 128)}}, strings.Repeat("a", 128)},
		{"129 characters", http.Header{"X-Request-Id": {strings.Repeat("a", 129)}}, ""},
		{"control character", http.Header{"X-Request-Id": {"ab\x01cd"}}, ""},
		{"newline", http.Header{"X-Request-Id": {"ab\ncd"}}, ""},
		{"delete", http.Header{"X-Request-Id": {"ab\x7fcd"}}, ""},
		{"non-ASCII", http.Header{"X-Request-Id": {"café"}}, ""},
		{"first present header decides", http.Header{"Request-Id": {"first"}, "X-Request-Id": {"second"}}, "first"},
		{"a bad first header is not replaced by a later one", http.Header{"Request-Id": {"a\x01"}, "X-Request-Id": {"second"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayRequestID(tc.h); got != tc.want {
				t.Errorf("gatewayRequestID = %q, want %q", got, tc.want)
			}
		})
	}
}

// A row with no call id (a direct call) and a nil answer are left alone.
func TestGatewayRequestID_StampNeedsAGatewayAttemptAndAnAnswer(t *testing.T) {
	resp := &http.Response{Header: http.Header{"X-Request-Id": {"req-1"}}}
	direct := egress.DecisionLog{}
	stampGatewayRequestID(&direct, resp)
	gated := egress.DecisionLog{GatewayCallID: "call-1"}
	stampGatewayRequestID(&gated, nil)
	if direct.GatewayRequestID != "" || gated.GatewayRequestID != "" {
		t.Errorf("stamped without a gateway attempt and an answer: %q, %q", direct.GatewayRequestID, gated.GatewayRequestID)
	}
	stampGatewayRequestID(&gated, resp)
	if gated.GatewayRequestID != "req-1" {
		t.Errorf("gateway attempt not stamped: %q", gated.GatewayRequestID)
	}
}
