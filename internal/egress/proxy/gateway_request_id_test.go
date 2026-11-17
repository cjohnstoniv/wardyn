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
func gwrSandboxHeaders(r *http.Request, extra ...string) *http.Request {
	for _, name := range gatewayRequestIDHeaders {
		r.Header.Set(name, gwrSandboxID)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		r.Header.Set(extra[i], extra[i+1])
	}
	return gwidForge(r)
}

// gwrLanes are the gateway lanes of a model call that read an answer: the brokered route, a
// MITM'd request and an absolute-form request. Each carries the sandbox's own request ids.
func gwrLanes(g *gwidRig, t *testing.T, extra ...string) map[string]func() *httptest.ResponseRecorder {
	body := func() *strings.Reader { return strings.NewReader(`{"hi":1}`) }
	return map[string]func() *httptest.ResponseRecorder{
		"brokered": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.handleLocalRoute(rec, gwrSandboxHeaders(mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", body()), extra...))
			return rec
		},
		"mitm": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.serveMITMRequest(rec, gwrSandboxHeaders(httptest.NewRequest(http.MethodPost, "https://"+gwidHost+"/v1/messages", body()), extra...), gwidHost, 443)
			return rec
		},
		"plain": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.servePlain(rec, gwrSandboxHeaders(httptest.NewRequest(http.MethodPost, "https://"+gwidHost+"/v1/messages", body()), extra...))
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
		if got := gwrRequestIDNames(g.gw.last(t).Header); len(got) != 0 {
			t.Errorf("%s: the gateway was sent the sandbox's request ids %v", lane, got)
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

// A gateway that echoes request headers records nothing for the sandbox's own request ids, on every
// lane: they are not forwarded, so there is nothing to echo. REVERT PROOF: forwarding them (see
// TestGatewayRequestID_SandboxRequestIDHeadersAreNotForwarded) and dropping the equality drop in
// stampGatewayRequestID together puts the sandbox's id on the row and fails here.
func TestGatewayRequestID_EchoedSandboxIDIsDropped(t *testing.T) {
	for name, respond := range map[string]func(http.ResponseWriter, *http.Request){
		"echoes every header": gwrEchoRespond,
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

// The helper's first rule: a match, ignoring case and punctuation, of any value of a sandbox header.
func TestGatewayRequestID_SandboxEchoMatch(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://gw.corp.test/v1", nil)
	r.Header.Add("X-Amzn-Requestid", "one")
	r.Header.Add("X-Amzn-Requestid", " two\t")
	for id, want := range map[string]bool{"one": true, "two": true, "One": true, "on": false, "three": false} {
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
		"a sandbox header copied": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Amzn-Requestid", gwrRails(r.Header.Get("X-Correlation-Id")))
			_, _ = w.Write([]byte("{}"))
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			g := newSGWHarness(t)
			g.respond = tc.respond
			forged := map[string]string{"Content-Type": "application/json", "X-Amzn-Requestid": gwrSandboxID, "X-Correlation-Id": gwrRailsInput}
			if rec := g.send(t, sgwRequest(t, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", `{}`, forged), sgwHost); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if seen := g.arrivals(); len(seen) != 1 || len(gwrRequestIDNames(seen[0].header)) != 0 {
				t.Errorf("the gateway was sent the sandbox's request id: %v", seen)
			}
			got, present := gwrAllowRow(t, name, gwcidRows(t, bytes.NewBufferString(g.log())))["gateway_request_id"]
			if tc.want == "" && present || tc.want != "" && got != tc.want {
				t.Errorf("gateway_request_id = %v (present %v), want %q", got, present, tc.want)
			}
		})
	}
}

// An MCP server that answers with a redirect names its id on the deny row, as the allow row does,
// and records none when the id it names is the sandbox's own echoed back. REVERT PROOF: dropping
// the stamp on the redirect deny in mcp_broker.go, or passing no request to it, fails here.
func TestGatewayRequestID_MCPRedirectDenyRowCarriesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		id   func(*http.Request) string
		want string
	}{
		"its own id":              {func(*http.Request) string { return "mcp-redirect-1" }, "mcp-redirect-1"},
		"a sandbox header copied": {func(r *http.Request) string { return r.Header.Get("X-Correlation-Id") }, ""},
		"upper-cased copy":        {func(r *http.Request) string { return strings.ToUpper(r.Header.Get("X-Correlation-Id")) }, ""},
		"sanitised copy":          {func(r *http.Request) string { return gwrRails(r.Header.Get("X-Correlation-Id")) }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.github.test"}}, nil, nil)
			f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "github", URL: "https://mcp.github.test/mcp"}}, []MCPBinding{mcpBindingFor("claude-code", "github")})
			backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-Id", tc.id(r))
				w.Header().Set("Location", "https://mcp.github.test/elsewhere")
				w.WriteHeader(http.StatusFound)
			}))
			t.Cleanup(backend.Close)
			f.p.dial = redirectDial(upstreamAddr(backend))
			f.p.gatewayRoutes = gwidRoutes()
			if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, "X-Request-Id", gwrSandboxID, "X-Correlation-Id", gwrRailsInput); rec.Code != http.StatusBadGateway {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			got, present := gwcidRows(t, f.buf)[0]["gateway_request_id"]
			if tc.want == "" && present || tc.want != "" && got != tc.want {
				t.Errorf("redirect deny row gateway_request_id = %v (present %v), want %q", got, present, tc.want)
			}
		})
	}
}

// A gateway answer the proxy will not relay (a content encoding it cannot scrub) still names its
// id on the deny row, and records none when that id is the sandbox's own echoed back. REVERT
// PROOF: dropping the stamp in relayGatewayRoute, or passing no request to it, fails here.
func TestGatewayRequestID_RelayRefusalRowCarriesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		id   func(*http.Request) string
		want string
	}{
		"its own id":              {func(*http.Request) string { return "relay-refused-1" }, "relay-refused-1"},
		"a sandbox header copied": {func(r *http.Request) string { return r.Header.Get("X-Correlation-Id") }, ""},
		"upper-cased copy":        {func(r *http.Request) string { return strings.ToUpper(r.Header.Get("X-Correlation-Id")) }, ""},
		"sanitised copy":          {func(r *http.Request) string { return gwrRails(r.Header.Get("X-Correlation-Id")) }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "br") // gzip would be undone by the transport, which asked for it itself
				w.Header().Set("X-Request-Id", tc.id(r))
				_, _ = w.Write([]byte("x"))
			}
			for lane, send := range gwrLanes(g, t, "X-Correlation-Id", gwrRailsInput) {
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
				if len(got) != 1 || tc.want == "" && got[0] != nil || tc.want != "" && got[0] != tc.want {
					t.Errorf("%s: relay-refusal row ids %v, want %q", lane, got, tc.want)
				}
			}
		})
	}
}

// gwrRailsInput is a value a sandbox sends in a header the proxy does not strip; gwrRails is what a
// framework's default request-id middleware makes of one: only word characters, dashes and @, cut
// to 255.
const gwrRailsInput = "req_011CZ tar!get"

func gwrRails(v string) string {
	out := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '@' {
			return r
		}
		return -1
	}, v)
	return out[:min(len(out), 255)]
}

// gwrRequestIDNames are the names of h that are, or fold into, a request-id header.
func gwrRequestIDNames(h http.Header) []string {
	var out []string
	for k := range h {
		switch strings.ToLower(strings.ReplaceAll(k, "_", "-")) {
		case "request-id", "x-request-id", "x-amzn-requestid":
			out = append(out, k)
		}
	}
	return out
}

// gwrTrace is a sandbox header the proxy forwards as it is, and the value it carries.
const (
	gwrTraceHeader = "X-Trace-Token"
	gwrTrace       = "trace-sandbox-value-0001"
)

// gwrVariants are gateways that hand a sandbox's own value back on x-request-id in a form an
// exact match misses, from a header the proxy does not strip. extra are the sandbox's own
// headers; want is what the row records (absent when empty).
var gwrVariants = map[string]struct {
	extra   []string
	respond func(http.ResponseWriter, *http.Request)
	want    string
}{
	"copied": {nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", r.Header.Get(gwrTraceHeader))
	}, ""},
	"upper-cased": {nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", strings.ToUpper(r.Header.Get(gwrTraceHeader)))
	}, ""},
	"sanitised the way a framework does": {[]string{"X-Correlation-Id", gwrRailsInput}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", gwrRails(r.Header.Get("X-Correlation-Id")))
	}, ""},
	"truncated": {nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", r.Header.Get(gwrTraceHeader)[:20])
	}, ""},
	"folded with the gateway's own": {nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", r.Header.Get(gwrTraceHeader)+", gwid-1")
	}, ""},
	"folded with the gateway's own first": {nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "gwid-1 ,"+r.Header.Get(gwrTraceHeader))
	}, ""},
	"sandbox value inside an affix": {nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "pre-"+r.Header.Get(gwrTraceHeader)+"-post")
	}, ""},
	"a sandbox request id header, which is no longer sent": {[]string{"X_Request_Id", "alias-sid-0001"}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", r.Header.Get("X_request_id"))
	}, ""},
	"gateway id sharing fewer than 16 characters": {nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "trace-sandbox-gw-9")
	}, "trace-sandbox-gw-9"},
}

// A gateway that returns the sandbox's own value in another spelling does not let the sandbox
// forge the field: the id is dropped when it matches any value of any sandbox header, ignoring
// case, when a comma element does, or when it contains a value of 16 characters or more. A
// gateway's own id that merely resembles one is kept. REVERT PROOF: scanning only the three
// request-id names, or passing no request, fails the first cases.
func TestGatewayRequestID_EchoVariantsAreDropped(t *testing.T) {
	for name, tc := range gwrVariants {
		t.Run(name, func(t *testing.T) {
			respond := func(w http.ResponseWriter, r *http.Request) { tc.respond(w, r); _, _ = w.Write([]byte("ok")) }
			g := newGwidRig(t, gwidRoutes(), false)
			g.gw.respond = respond
			for lane, send := range gwrLanes(g, t, append([]string{gwrTraceHeader, gwrTrace}, tc.extra...)...) {
				g.buf.Reset()
				if rec := send(); rec.Code != http.StatusOK {
					t.Fatalf("%s: status %d: %s", lane, rec.Code, rec.Body)
				}
				got, present := gwrAllowRow(t, lane, gwcidRows(t, g.buf))["gateway_request_id"]
				if tc.want == "" && present || tc.want != "" && got != tc.want {
					t.Errorf("%s: gateway_request_id = %v (present %v), want %q", lane, got, present, tc.want)
				}
			}
			f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.github.test"}}, nil, nil)
			f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "github", URL: "https://mcp.github.test/mcp"}}, []MCPBinding{mcpBindingFor("claude-code", "github")})
			backend := httptest.NewTLSServer(http.HandlerFunc(respond))
			t.Cleanup(backend.Close)
			f.p.dial = redirectDial(upstreamAddr(backend))
			f.p.gatewayRoutes = gwidRoutes()
			hdr := append([]string{"X-Request-Id", gwrSandboxID, gwrTraceHeader, gwrTrace}, tc.extra...)
			if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, hdr...); rec.Code != http.StatusOK {
				t.Fatalf("mcp: %d %s", rec.Code, rec.Body)
			}
			got, present := gwcidRows(t, f.buf)[0]["gateway_request_id"]
			if tc.want == "" && present || tc.want != "" && got != tc.want {
				t.Errorf("mcp: gateway_request_id = %v (present %v), want %q", got, present, tc.want)
			}
		})
	}
}

// The helper's rule, case by case: both sides normalised (lower-case, only [a-z0-9]), the id equal to
// a sandbox value or to a comma element of itself at any length, and containment either way once the
// shorter is 16 characters or more.
func TestGatewayRequestID_SandboxEchoWidened(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://gw.corp.test/v1", nil)
	r.Header.Set("X-Correlation-Id", "Corr-0123456789abcdef") // 20 normalised characters
	r.Header.Set("X-Short", "short")
	r.Header.Set("X-Rails", gwrRailsInput)         // "req011cztarget"
	r.Header.Set("X-Sixteen", "ab-cdefghijklmnop") // 16 normalised characters
	r.Header.Set("X-Fifteen", "ab-cdefghijklmno")  // 15 normalised characters
	for id, want := range map[string]bool{
		"corr-0123456789abcdef":     true, // another header, ignoring case
		"CORR0123456789ABCDEF":      true, // and punctuation
		"short":                     true, // any length, exact
		"SHORT":                     true,
		"gw-1, short":               true, // a comma element
		"short,gw-1":                true,
		"req_011CZtarget":           true,  // what a framework makes of a value with a space and a bang
		"a-CORR-0123456789ABCDEF-z": true,  // contains one of 16 or more
		"corr-0123456789abcde":      true,  // 19 characters, inside a sandbox value of 20
		"xshortx":                   false, // contains a short one: not enough
		"gw-1, gw-2":                false,
		"":                          false,
		"pre-abcdefghijklmnop-post": true,  // an affix around a 16-character value
		"pre-abcdefghijklmno-xyz":   false, // an affix around a 15-character value
		"abcdefghijklmno":           true,  // an id of 15 equal to a sandbox value is still an equal
	} {
		if got := sandboxRequestIDEcho(r, id); got != want {
			t.Errorf("sandboxRequestIDEcho(%q) = %v, want %v", id, got, want)
		}
	}
}

// The reverse floor, with the id the shorter side: inside a sandbox value, 16 characters are
// dropped and 15 are kept.
func TestGatewayRequestID_ReverseContainmentFloor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://gw.corp.test/v1", nil)
	r.Header.Set("X-Blob", "prefix-abcdefghijklmnop-suffix")
	for id, want := range map[string]bool{"abcdefghijklmnop": true, "abcdefghijklmno": false, "bcdefghijklmnop": false} {
		if got := sandboxRequestIDEcho(r, id); got != want {
			t.Errorf("sandboxRequestIDEcho(%q) = %v, want %v", id, got, want)
		}
	}
}

// A long sandbox value costs a scan and no allocation, and an id inside its first 512 normalised
// characters is still found.
func TestGatewayRequestID_LongSandboxValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://gw.corp.test/v1", nil)
	r.Header.Set("X-Long", strings.Repeat("!", 600)+"abcdefghijklmnop"+strings.Repeat("z", 600))
	if !sandboxRequestIDEcho(r, "abcdefghijklmnop") {
		t.Error("an id inside a long value was not found")
	}
	if sandboxRequestIDEcho(r, "other-gateway-id-0001") {
		t.Error("an unrelated id matched a long value")
	}
}

// On a gateway attempt the proxy does not forward the sandbox's request-id headers, in any
// case or underscore spelling, on any lane. A direct call keeps them. REVERT PROOF: not stripping
// in a lane's copyHeader site leaves the names at the gateway and fails here.
func TestGatewayRequestID_SandboxRequestIDHeadersAreNotForwarded(t *testing.T) {
	sandbox := []string{"Request-Id", "a", "X-Request-Id", "b", "X-Amzn-Requestid", "c", "X_Request_Id", "d", "x-request_id", "e", "X-Kept", "k"}
	g := newGwidRig(t, gwidRoutes(), false)
	for lane, send := range gwrLanes(g, t, sandbox...) {
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", lane, rec.Code, rec.Body)
		}
		seen := g.gw.last(t).Header
		if got := gwrRequestIDNames(seen); len(got) != 0 {
			t.Errorf("%s: the gateway was sent %v", lane, got)
		}
		if seen.Get("X-Kept") != "k" {
			t.Errorf("%s: an unrelated header did not pass: %v", lane, seen)
		}
	}
	f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.github.test"}}, nil, nil)
	f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "github", URL: "https://mcp.github.test/mcp"}}, []MCPBinding{mcpBindingFor("claude-code", "github")})
	seen := make(chan http.Header, 1)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(backend.Close)
	f.p.dial = redirectDial(upstreamAddr(backend))
	f.p.gatewayRoutes = gwidRoutes()
	if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, sandbox...); rec.Code != http.StatusOK {
		t.Fatalf("mcp: %d %s", rec.Code, rec.Body)
	}
	if h := <-seen; len(gwrRequestIDNames(h)) != 0 || h.Get("X-Kept") != "k" {
		t.Errorf("mcp: the server was sent %v, want no request-id names and X-Kept", h)
	}
	// A direct call, with no gateway route, forwards them.
	d := newGwidRig(t, nil, false)
	for lane, send := range gwrLanes(d, t, sandbox...) {
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("direct %s: status %d: %s", lane, rec.Code, rec.Body)
		}
		if got := gwrRequestIDNames(d.gw.last(t).Header); len(got) < 5 {
			t.Errorf("direct %s: the upstream was sent %v, want the sandbox's five spellings", lane, got)
		}
	}
}
