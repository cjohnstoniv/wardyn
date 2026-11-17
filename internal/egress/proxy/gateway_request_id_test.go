// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"fmt"
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
		{"boundary space is printable", http.Header{"X-Request-Id": {"a b"}}, "a b"},
		{"boundary tilde is printable", http.Header{"X-Request-Id": {"a~b"}}, "a~b"},
		{"x-request-id before x-amzn-requestid", http.Header{"X-Request-Id": {"plain"}, "X-Amzn-Requestid": {"amzn"}}, "plain"},
		{"empty value counts as absent", http.Header{"Request-Id": {""}, "X-Request-Id": {"second"}}, "second"},
		{"first value of a duplicate name", http.Header{"X-Request-Id": {"one", "two"}}, "one"},
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
	stampGatewayRequestID(&direct, nil, resp)
	gated := egress.DecisionLog{GatewayCallID: "call-1"}
	stampGatewayRequestID(&gated, nil, nil)
	if direct.GatewayRequestID != "" || gated.GatewayRequestID != "" {
		t.Errorf("stamped without a gateway attempt and an answer: %q, %q", direct.GatewayRequestID, gated.GatewayRequestID)
	}
	stampGatewayRequestID(&gated, nil, resp)
	if gated.GatewayRequestID != "req-1" {
		t.Errorf("gateway attempt not stamped: %q", gated.GatewayRequestID)
	}
}

// gwrEchoRespond is a gateway that hands the sandbox's request ids back on its answer, the way
// a gateway that echoes request headers does.
func gwrEchoRespond(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{"Request-Id", "X-Request-Id", "X-Amzn-Requestid"} {
		if v := r.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	_, _ = w.Write([]byte("ok"))
}

// A gateway that echoes the sandbox's request ids does not let the sandbox forge the field: the
// echoed value is dropped on every lane, however the echo is spread over the headers. REVERT
// PROOF: removing the equality drop in stampGatewayRequestID, or comparing against another
// request than the sandbox's, puts the sandbox's id on the row and fails here.
func TestGatewayRequestID_EchoedSandboxIDIsDropped(t *testing.T) {
	for name, respond := range map[string]func(http.ResponseWriter, *http.Request){
		"echoes every header": gwrEchoRespond,
		"echoes Request-Id and sets its own X-Request-Id": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Request-Id", r.Header.Get("Request-Id"))
			w.Header().Set("X-Request-Id", "gateway-own")
			_, _ = w.Write([]byte("ok"))
		},
		"echoes X-Request-Id only": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-Id", r.Header.Get("X-Request-Id"))
			_, _ = w.Write([]byte("ok"))
		},
		"echoes X-Amzn-Requestid only": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Amzn-Requestid", r.Header.Get("X-Amzn-Requestid"))
			_, _ = w.Write([]byte("ok"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			g.gw.respond = respond
			for lane, send := range gwrLanes(g, t) {
				g.buf.Reset()
				if rec := send(); rec.Code != http.StatusOK {
					t.Fatalf("%s: status %d: %s", lane, rec.Code, rec.Body)
				}
				if got, present := gwrAllowRow(t, lane, gwcidRows(t, g.buf))["gateway_request_id"]; present {
					t.Errorf("%s: row carries gateway_request_id %v, the sandbox's own echoed id", lane, got)
				}
			}
			f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.github.test"}}, nil, nil)
			f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "github", URL: "https://mcp.github.test/mcp"}}, []MCPBinding{mcpBindingFor("claude-code", "github")})
			backend := httptest.NewTLSServer(http.HandlerFunc(respond))
			t.Cleanup(backend.Close)
			f.p.dial = redirectDial(upstreamAddr(backend))
			f.p.gatewayRoutes = gwidRoutes()
			if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, "Request-Id", gwrSandboxID, "X-Request-Id", gwrSandboxID, "X-Amzn-Requestid", gwrSandboxID); rec.Code != http.StatusOK {
				t.Fatalf("mcp: %d %s", rec.Code, rec.Body)
			}
			if got, present := gwcidRows(t, f.buf)[0]["gateway_request_id"]; present {
				t.Errorf("mcp: row carries gateway_request_id %v, the sandbox's own echoed id", got)
			}
		})
	}
}

// A gateway that sets an id of its own, different from the one the sandbox sent, is recorded.
func TestGatewayRequestID_AnIDDifferentFromTheSandboxsIsKept(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	g.gw.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Request-Id", "gateway-own")
		_, _ = w.Write([]byte("ok"))
	}
	for lane, send := range gwrLanes(g, t) {
		g.buf.Reset()
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", lane, rec.Code)
		}
		if got := gwrAllowRow(t, lane, gwcidRows(t, g.buf))["gateway_request_id"]; got != "gateway-own" {
			t.Errorf("%s: gateway_request_id = %v, want the gateway's own", lane, got)
		}
	}
}

// The helper's own rule: an exact match of any value of any of the three names, after the trim.
func TestGatewayRequestID_SandboxEchoMatch(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://gw.corp.test/v1", nil)
	r.Header.Add("X-Amzn-Requestid", "one")
	r.Header.Add("X-Amzn-Requestid", " two\t")
	for id, want := range map[string]bool{"one": true, "two": true, "One": false, "on": false, "three": false} {
		if got := sandboxRequestIDEcho(r, id); got != want {
			t.Errorf("sandboxRequestIDEcho(%q) = %v, want %v", id, got, want)
		}
	}
	if sandboxRequestIDEcho(nil, "one") {
		t.Error("a nil request matched")
	}
}

// The re-signed Bedrock gateway route reads X-Amzn-Requestid off the answer, and drops the
// sandbox's own value when the gateway echoes it. REVERT PROOF: dropping the stamp in pat_api.go
// leaves the first case without the field.
func TestGatewayRequestID_ReSignedBedrockGatewayRoute(t *testing.T) {
	for name, tc := range map[string]struct {
		respond func(http.ResponseWriter, *http.Request)
		want    string
	}{
		"the answer's id": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Amzn-Requestid", "0b1c-4d5e")
			_, _ = w.Write([]byte("{}"))
		}, "0b1c-4d5e"},
		"the sandbox's id echoed": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Amzn-Requestid", r.Header.Get("X-Amzn-Requestid"))
			_, _ = w.Write([]byte("{}"))
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			g := newSGWHarness(t)
			g.respond = tc.respond
			forged := map[string]string{"Content-Type": "application/json", "X-Amzn-Requestid": gwrSandboxID}
			if rec := g.send(t, sgwRequest(t, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", `{}`, forged), sgwHost); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			got, present := gwrAllowRow(t, name, gwcidRows(t, bytes.NewBufferString(g.log())))["gateway_request_id"]
			if tc.want == "" && present || tc.want != "" && got != tc.want {
				t.Errorf("gateway_request_id = %v (present %v), want %q", got, present, tc.want)
			}
		})
	}
}

// An MCP server that answers with a redirect names its id on the deny row, as the allow row does.
// REVERT PROOF: dropping the stamp on the redirect deny in mcp_broker.go fails here.
func TestGatewayRequestID_MCPRedirectDenyRowCarriesIt(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.github.test"}}, nil, nil)
	f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "github", URL: "https://mcp.github.test/mcp"}}, []MCPBinding{mcpBindingFor("claude-code", "github")})
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "mcp-redirect-1")
		w.Header().Set("Location", "https://mcp.github.test/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(backend.Close)
	f.p.dial = redirectDial(upstreamAddr(backend))
	f.p.gatewayRoutes = gwidRoutes()
	if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, "X-Request-Id", gwrSandboxID); rec.Code != http.StatusBadGateway {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := gwcidRows(t, f.buf)[0]["gateway_request_id"]; got != "mcp-redirect-1" {
		t.Errorf("redirect deny row gateway_request_id = %v, want the answer's", got)
	}
}

// A gateway answer the proxy will not relay (a content encoding it cannot scrub) still names its
// id on the deny row. REVERT PROOF: dropping the stamp in relayGatewayRoute fails here.
func TestGatewayRequestID_RelayRefusalRowCarriesIt(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	g.gw.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "br") // gzip would be undone by the transport, which asked for it itself
		w.Header().Set("X-Request-Id", "relay-refused-1")
		_, _ = w.Write([]byte("x"))
	}
	for lane, send := range gwrLanes(g, t) {
		g.buf.Reset()
		if rec := send(); rec.Code != http.StatusBadGateway {
			t.Fatalf("%s: status %d", lane, rec.Code)
		}
		var got []any
		for _, row := range gwcidRows(t, g.buf) {
			if row["decision"] == string(egress.Deny) && strings.Contains(fmt.Sprint(row["cause"]), "Content-Encoding") {
				got = append(got, row["gateway_request_id"])
			}
		}
		if len(got) != 1 || got[0] != "relay-refused-1" {
			t.Errorf("%s: relay-refusal row ids %v, want the answer's", lane, got)
		}
	}
}
