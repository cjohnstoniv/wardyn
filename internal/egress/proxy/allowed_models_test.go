// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const modelGateHost = "llm-gateway.corp.internal"

// modelGateProxy is a run whose Anthropic and OpenAI routes both dial one internal gateway
// (served by a capturing upstream), with the allowed_models list set to allowed.
func modelGateProxy(t *testing.T, allowed []string) (*Proxy, *capturedUpstream, *bytes.Buffer) {
	t.Helper()
	up := captureUpstream(t, true, `{"ok":true}`)
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{}),
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver: fakeResolver{m: map[string][]net.IP{modelGateHost: ips("10.40.1.5")}},
		Dial:     redirectDial(upstreamAddr(up.srv)),
		Injector: staticInj(map[string]injectedHeader{modelGateHost: {name: "X-Api-Key", value: "BROKERED-KEY"}}),
		LLMUpstreams: map[string]string{
			anthropicHost: "https://" + modelGateHost + "/v1",
			openaiHost:    "https://" + modelGateHost + "/v1",
		},
		TLSClientConfig: testInsecureTLSConfig,
	})
	p.allowedModels = compileAllowedModels(allowed)
	return p, up, buf
}

func TestAllowedModels_BrokeredRoutes(t *testing.T) {
	allowed := []string{"claude-sonnet-5", "gpt-5"}
	for _, tc := range []struct {
		name, route, body string
		status            int
		cause             string // "" == forwarded
	}{
		{"anthropic allowed", llmAnthropicPrefix + "messages", `{"model":"claude-sonnet-5", "max_tokens": 4,  "messages":[]}`, 200, ""},
		{"anthropic count_tokens allowed", llmAnthropicPrefix + "messages/count_tokens", `{"messages":[],"model":"claude-sonnet-5"}`, 200, ""},
		{"openai chat allowed", llmOpenAIPrefix + "chat/completions", `{"model":"gpt-5"}`, 200, ""},
		{"openai responses allowed", llmOpenAIPrefix + "responses", `{"model":"gpt-5","input":"x"}`, 200, ""},
		{"openai completions allowed", llmOpenAIPrefix + "completions", `{"model":"gpt-5"}`, 200, ""},
		{"escaped model name allowed", llmOpenAIPrefix + "responses", `{"model":"gpt-5"}`, 200, ""},
		{"anthropic refused", llmAnthropicPrefix + "messages", `{"model":"claude-opus-9"}`, 403, `model "claude-opus-9" is not on this run's allowed models list`},
		{"count_tokens refused", llmAnthropicPrefix + "messages/count_tokens", `{"model":"claude-opus-9"}`, 403, `"claude-opus-9" is not on`},
		{"openai chat refused", llmOpenAIPrefix + "chat/completions", `{"model":"gpt-4"}`, 403, `"gpt-4" is not on`},
		{"openai responses refused", llmOpenAIPrefix + "responses", `{"model":"gpt-4"}`, 403, `"gpt-4" is not on`},
		{"case sensitive", llmAnthropicPrefix + "messages", `{"model":"Claude-Sonnet-5"}`, 403, `"Claude-Sonnet-5" is not on`},
		{"no prefix match", llmAnthropicPrefix + "messages", `{"model":"claude-sonnet-5-20260101"}`, 403, "is not on"},
		{"padded name", llmAnthropicPrefix + "messages", `{"model":" claude-sonnet-5"}`, 403, "is not on"},
		{"missing model", llmAnthropicPrefix + "messages", `{"messages":[]}`, 400, "the request's model could not be read"},
		{"non-string model", llmAnthropicPrefix + "messages", `{"model":["claude-sonnet-5"]}`, 400, "the request's model could not be read"},
		{"null model", llmOpenAIPrefix + "responses", `{"model":null}`, 400, "the request's model could not be read"},
		{"other key case", llmOpenAIPrefix + "responses", `{"Model":"gpt-5"}`, 400, "the request's model could not be read"},
		{"two model keys", llmAnthropicPrefix + "messages", `{"model":"claude-sonnet-5","model":"claude-opus-9"}`, 400, "the request's model could not be read"},
		{"bad json", llmAnthropicPrefix + "messages", `{"model":"claude-sonnet-5"`, 400, "the request's model could not be read"},
		{"not json", llmOpenAIPrefix + "chat/completions", `model=gpt-5`, 400, "the request's model could not be read"},
		{"array body", llmOpenAIPrefix + "responses", `[{"model":"gpt-5"}]`, 400, "the request's model could not be read"},
		{"trailing value", llmAnthropicPrefix + "messages", `{"model":"claude-sonnet-5"}{"model":"x"}`, 400, "the request's model could not be read"},
		{"empty body", llmAnthropicPrefix + "messages", ``, 400, "the request's model could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, up, buf := modelGateProxy(t, allowed)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, tc.route, strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.cause == "" {
				if !up.reached || up.body != tc.body {
					t.Fatalf("an allowed request must be forwarded byte for byte: reached=%v body=%q want %q", up.reached, up.body, tc.body)
				}
				return
			}
			if up.reached {
				t.Fatal("a refused request must never reach the upstream")
			}
			d := lastDecision(t, buf)
			if d.Decision != egress.Deny || d.RuleSource != ruleSourceAllowedModels || !strings.Contains(d.Cause, tc.cause) {
				t.Fatalf("decision = %+v, want a %s deny with cause containing %q", d, ruleSourceAllowedModels, tc.cause)
			}
			if !strings.Contains(rec.Body.String(), ruleSourceAllowedModels) {
				t.Fatalf("the answer must name the rule source: %q", rec.Body.String())
			}
		})
	}
}

// A refusal answers in the route's own provider shape, like the local model gate's.
func TestAllowedModels_RefusalShape(t *testing.T) {
	p, _, _ := modelGateProxy(t, []string{"gpt-5"})
	for _, tc := range []struct{ route, want string }{
		{llmAnthropicPrefix + "messages", `"type":"error"`},
		{llmOpenAIPrefix + "responses", `"code":"egress_denied"`},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, tc.route, strings.NewReader(`{"model":"x"}`)))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), tc.want) || rec.Header().Get(egressHeaderWardynReason) != reasonEgressDenied {
			t.Fatalf("%s: %d %q reason=%q, want a 403 containing %s", tc.route, rec.Code, rec.Body.String(), rec.Header().Get(egressHeaderWardynReason), tc.want)
		}
	}
}

func TestAllowedModels_OverCapBodyRefused(t *testing.T) {
	old := maxLLMScanBody
	maxLLMScanBody = 64
	t.Cleanup(func() { maxLLMScanBody = old })
	p, up, buf := modelGateProxy(t, []string{"gpt-5"})
	body := `{"model":"gpt-5","input":"` + strings.Repeat("x", 200) + `"}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"responses", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || up.reached {
		t.Fatalf("status = %d reached = %v, want a 400 and nothing forwarded (%q)", rec.Code, up.reached, rec.Body.String())
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny || !strings.Contains(d.Cause, "could not be read") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestAllowedModels_EncodedBodyRefused(t *testing.T) {
	p, up, _ := modelGateProxy(t, []string{"gpt-5"})
	req := mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"responses", strings.NewReader(`{"model":"gpt-5"}`))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || up.reached {
		t.Fatalf("status = %d reached = %v, want a 400 and nothing forwarded", rec.Code, up.reached)
	}
}

// A route that names no model is left alone with the list set: the body is not read and the
// request is forwarded as ever.
func TestAllowedModels_OtherRoutesUntouched(t *testing.T) {
	for _, tc := range []struct{ method, route, body string }{
		{http.MethodGet, llmAnthropicPrefix + "models", ""},
		{http.MethodGet, llmOpenAIPrefix + "models", ""},
		{http.MethodPost, llmOpenAIPrefix + "embeddings", `{"model":"text-embedding-3"}`},
		{http.MethodPost, llmAnthropicPrefix + "files", `not json at all`},
		{http.MethodGet, llmAnthropicPrefix + "messages", ""},
	} {
		t.Run(tc.method+" "+tc.route, func(t *testing.T) {
			p, up, _ := modelGateProxy(t, []string{"gpt-5"})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.route, strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK || !up.reached || up.body != tc.body {
				t.Fatalf("status = %d reached = %v body = %q, want it forwarded untouched", rec.Code, up.reached, up.body)
			}
		})
	}
}

// failReader fails the test the moment anything reads it.
type failReader struct{ t *testing.T }

func (f failReader) Read([]byte) (int, error) {
	f.t.Helper()
	f.t.Error("the gate read a body it must not read")
	return 0, errors.New("read")
}
func (failReader) Close() error { return nil }

// With the list unset the gate returns before it touches the request, whatever the route.
func TestAllowedModels_OffReadsNothing(t *testing.T) {
	for _, list := range [][]string{nil, {}} {
		p := newProxy(Options{})
		p.allowedModels = compileAllowedModels(list)
		for _, tc := range []struct{ host, path string }{
			{anthropicHost, "/wardyn/llm/anthropic/v1/messages"},
			{openaiHost, "/wardyn/llm/openai/v1/responses"},
			{"bedrock-runtime.us-east-1.amazonaws.com", "/model/x/invoke"},
		} {
			req := httptest.NewRequest(http.MethodPost, tc.path, failReader{t})
			req.Header.Set("Content-Encoding", "gzip")
			rec := httptest.NewRecorder()
			release, refused := p.allowedModelRefused(rec, req, tc.host, 443, strings.TrimPrefix(tc.path, "/"))
			if refused || release == nil || rec.Body.Len() != 0 || rec.Code != http.StatusOK {
				t.Fatalf("%s %s: refused = %v, wrote %d bytes, want an untouched pass", tc.host, tc.path, refused, rec.Body.Len())
			}
		}
	}
}

// With the list set, a host that is no model provider and a route that carries no model are not
// read either.
func TestAllowedModels_NonModelRequestsReadNothing(t *testing.T) {
	p := newProxy(Options{})
	p.allowedModels = compileAllowedModels([]string{"gpt-5"})
	for _, tc := range []struct{ host, method, path string }{
		{"registry.npmjs.org", http.MethodPost, "/v1/messages"},
		{"s3.us-east-1.amazonaws.com", http.MethodPost, "/model/x/invoke"},
		{anthropicHost, http.MethodGet, "/v1/messages"},
		{anthropicHost, http.MethodPost, "/v1/files"},
		{openaiHost, http.MethodPost, "/v1/embeddings"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, failReader{t})
		rec := httptest.NewRecorder()
		if release, refused := p.allowedModelRefused(rec, req, tc.host, 443, strings.TrimPrefix(tc.path, "/")); refused || release == nil {
			t.Fatalf("%s %s %s: refused = %v, want a pass", tc.host, tc.method, tc.path, refused)
		}
	}
}

// With the list unset the upstream receives the same bytes and headers as a proxy that allows the
// model: setting the list changes nothing about an allowed request.
func TestAllowedModels_OffForwardsIdentically(t *testing.T) {
	body := `{"model":"claude-sonnet-5",  "messages":[{"role":"user","content":"keep   these bytes"}]}`
	send := func(allowed []string) *capturedUpstream {
		p, up, _ := modelGateProxy(t, allowed)
		req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages?beta=true", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !up.reached {
			t.Fatalf("allowed=%v: status = %d reached = %v (%q)", allowed, rec.Code, up.reached, rec.Body.String())
		}
		return up
	}
	off, on := send(nil), send([]string{"claude-sonnet-5"})
	if off.body != body || on.body != body {
		t.Fatalf("forwarded bodies: off %q on %q, want both %q", off.body, on.body, body)
	}
	if off.path != on.path {
		t.Fatalf("paths differ: off %q on %q", off.path, on.path)
	}
	if !sameHeaders(off.header, on.header) {
		t.Fatalf("headers differ:\n off %v\n on  %v", off.header, on.header)
	}
}

func sameHeaders(a, b http.Header) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestAllowedModels_Bedrock(t *testing.T) {
	const host = "bedrock-runtime.us-east-1.amazonaws.com"
	const arn = "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-5"
	p := newProxy(Options{})
	p.allowedModels = compileAllowedModels([]string{"anthropic.claude-sonnet-5-v1:0", arn})
	for _, tc := range []struct {
		name, method, path string
		status             int // 0 == passes
		cause              string
	}{
		{"invoke allowed", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1:0/invoke", 0, ""},
		{"stream allowed", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1:0/invoke-with-response-stream", 0, ""},
		{"converse allowed", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1:0/converse", 0, ""},
		{"converse-stream allowed", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1:0/converse-stream", 0, ""},
		{"escaped colon allowed", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1%3A0/invoke", 0, ""},
		{"escaped arn allowed", http.MethodPost, "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Ainference-profile%2Fus.anthropic.claude-sonnet-5/converse", 0, ""},
		{"invoke refused", http.MethodPost, "/model/anthropic.claude-opus-9-v1:0/invoke", http.StatusForbidden, `model "anthropic.claude-opus-9-v1:0" is not on`},
		{"stream refused", http.MethodPost, "/model/amazon.titan-text/invoke-with-response-stream", http.StatusForbidden, `"amazon.titan-text" is not on`},
		{"converse refused", http.MethodPost, "/model/amazon.titan-text/converse", http.StatusForbidden, "is not on"},
		{"other arn refused", http.MethodPost, "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Ainference-profile%2Fother/converse", http.StatusForbidden, "is not on"},
		{"case sensitive", http.MethodPost, "/model/Anthropic.claude-sonnet-5-v1:0/invoke", http.StatusForbidden, "is not on"},
		{"empty id", http.MethodPost, "/model//invoke", http.StatusBadRequest, "the request's model could not be read"},
		{"other method judged too", http.MethodGet, "/model/amazon.titan-text/invoke", http.StatusForbidden, "is not on"},
		{"list models untouched", http.MethodGet, "/foundation-models", 0, ""},
		{"other op untouched", http.MethodPost, "/model/amazon.titan-text/count-tokens", 0, ""},
		{"extra segment untouched", http.MethodPost, "/model/a/b/invoke", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, failReader{t}) // a Bedrock model rides the path: no body read
			rec := httptest.NewRecorder()
			release, refused := p.allowedModelRefused(rec, req, host, 443, "")
			if tc.status == 0 {
				if refused || release == nil {
					t.Fatalf("refused = %v (%d %q), want a pass", refused, rec.Code, rec.Body.String())
				}
				return
			}
			if !refused || rec.Code != tc.status || !strings.Contains(rec.Body.String(), strings.ReplaceAll(tc.cause, `"`, `\"`)) {
				t.Fatalf("refused = %v status = %d body = %q, want %d containing %q", refused, rec.Code, rec.Body.String(), tc.status, tc.cause)
			}
			if !strings.Contains(rec.Body.String(), "__type") {
				t.Fatalf("a Bedrock refusal answers in the AWS shape: %q", rec.Body.String())
			}
		})
	}
}

// A model id that does not unescape cannot be read. (A server never hands the proxy such a
// path, so this is the helper's own fail-closed arm.)
func TestAllowedModels_BedrockPathParse(t *testing.T) {
	for _, tc := range []struct {
		path, model     string
		gated, readable bool
	}{
		{"/model/a%3Ab/invoke", "a:b", true, true},
		{"/model/a%2Fb/converse-stream", "a/b", true, true},
		{"/model/anthropic%zz/invoke", "", true, false},
		{"/model//invoke", "", true, false},
		{"/model/a/b/invoke", "", false, true},
		{"/model/a/count-tokens", "", false, true},
		{"/other/a/invoke", "", false, true},
	} {
		if m, g, ok := bedrockModelPath(tc.path); m != tc.model || g != tc.gated || ok != tc.readable {
			t.Errorf("bedrockModelPath(%q) = %q %v %v, want %q %v %v", tc.path, m, g, ok, tc.model, tc.gated, tc.readable)
		}
	}
}

// The terminated-tunnel lane (a model host the proxy MITMs) is gated before the body is forwarded.
func TestAllowedModels_TerminatedTunnelGated(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}), Resolver: publicResolver{}})
	p.allowedModels = compileAllowedModels([]string{"claude-sonnet-5"})
	req := httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(`{"model":"claude-opus-9"}`))
	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, req, anthropicHost, 443)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `\"claude-opus-9\" is not on`) {
		t.Fatalf("status = %d body = %q, want the model refused", rec.Code, rec.Body.String())
	}
}

func TestAllowedModels_ConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []string
		want string // "" == valid
	}{
		{"unset", nil, ""},
		{"set but empty", []string{}, "set but empty"},
		{"ok", []string{"a", "b:1"}, ""},
		{"empty entry", []string{"a", ""}, "must not be empty"},
		{"padded entry", []string{" a"}, "leading or trailing space"},
		{"trailing space", []string{"a\t"}, "leading or trailing space"},
		{"duplicate", []string{"a", "b", "a"}, `"a" is listed twice`},
		{"case differs is not a duplicate", []string{"a", "A"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Config{AllowedModels: tc.list}).validateAllowedModels()
			if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAllowedModels_ConfigLoadsAndOffByDefault(t *testing.T) {
	base := map[string]any{
		"run_id":               uuid.New().String(),
		"control_plane_url":    "https://wardynd:8443",
		"control_plane_ca_pem": testCPCAPEM,
		"run_token":            "tok",
	}
	load := func(extra map[string]any) (*Config, error) {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		raw, _ := json.Marshal(m)
		return LoadConfigBytes(raw)
	}
	c, err := load(map[string]any{"allowed_models": []string{"claude-sonnet-5"}})
	if err != nil || len(c.AllowedModels) != 1 || !compileAllowedModels(c.AllowedModels)["claude-sonnet-5"] {
		t.Fatalf("allowed_models did not load: %+v %v", c, err)
	}
	if _, err := load(map[string]any{"allowed_models": []string{}}); err == nil || !strings.Contains(err.Error(), "set but empty") {
		t.Fatalf("a set but empty list must fail the load, got %v", err)
	}
	if _, err := load(map[string]any{"allowed_models": []string{"a", "a"}}); err == nil {
		t.Fatal("a duplicate entry must fail the load")
	}
	if c, err = load(nil); err != nil || c.AllowedModels != nil || compileAllowedModels(c.AllowedModels) != nil {
		t.Fatalf("unset must stay off: %+v %v", c, err)
	}
	out, _ := json.Marshal(c)
	if strings.Contains(string(out), "allowed_models") {
		t.Fatalf("an unset list must not be written back (an older proxy refuses the key): %s", out)
	}
}
