// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	p.allowedModels = modelSet(allowed)
	return p, up, buf
}

// modelSet compiles a test's list the way a loaded config does: nil is unset (off), and an empty
// non-nil list is `[]` (every model refused).
func modelSet(list []string) *modelLimit {
	if list == nil {
		return nil
	}
	return compileAllowedModels(&list, nil)
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
		{"codex compaction allowed", llmOpenAIPrefix + "responses/compact", `{"model":"gpt-5","input":"x"}`, 200, ""},
		{"codex compaction refused", llmOpenAIPrefix + "responses/compact", `{"model":"gpt-4"}`, 403, `"gpt-4" is not on`},
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

// A read is left alone with the list set: the body is not read and the request is forwarded as ever.
func TestAllowedModels_OtherRoutesUntouched(t *testing.T) {
	for _, tc := range []struct{ method, route, body string }{
		{http.MethodGet, llmAnthropicPrefix + "models", ""},
		{http.MethodGet, llmOpenAIPrefix + "models", ""},
		{http.MethodHead, llmOpenAIPrefix + "models", ""},
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
	{
		p := newProxy(Options{})
		for _, tc := range []struct{ host, path string }{
			{anthropicHost, "/wardyn/llm/anthropic/v1/messages"},
			{openaiHost, "/wardyn/llm/openai/v1/responses"},
			{openaiHost, "/v1/embeddings"},
			{openaiHost, "/v1/../v1/messages"},
			{"bedrock-runtime.us-east-1.amazonaws.com", "/model/x/invoke"},
			{"bedrock-runtime.us-east-1.amazonaws.com", "/model/../model/x/rerank/"},
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

// With the list set, a host that is no model provider and a read are not read either.
func TestAllowedModels_NonModelRequestsReadNothing(t *testing.T) {
	p := newProxy(Options{})
	p.allowedModels = modelSet([]string{"gpt-5"})
	for _, tc := range []struct{ host, method, path string }{
		{"registry.npmjs.org", http.MethodPost, "/v1/messages"},
		{"s3.us-east-1.amazonaws.com", http.MethodPost, "/model/x/invoke"},
		{anthropicHost, http.MethodGet, "/v1/messages"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, failReader{t})
		rec := httptest.NewRecorder()
		if release, refused := p.allowedModelRefused(rec, req, tc.host, 443, strings.TrimPrefix(tc.path, "/")); refused || release == nil {
			t.Fatalf("%s %s %s: refused = %v, want a pass", tc.host, tc.method, tc.path, refused)
		}
	}
	// A route the gate refuses without a model to check is refused unread too.
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", failReader{t})
	if _, refused := p.allowedModelRefused(httptest.NewRecorder(), req, openaiHost, 443, "v1/embeddings"); !refused {
		t.Fatal("embeddings must be refused")
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
	p.allowedModels = modelSet([]string{"anthropic.claude-sonnet-5-v1:0", arn})
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
		{"empty id", http.MethodPost, "/model//invoke", http.StatusBadRequest, "dot, empty or unreadable"},
		{"other method judged too", http.MethodGet, "/model/amazon.titan-text/invoke", http.StatusForbidden, "is not on"},
		{"list models untouched", http.MethodGet, "/foundation-models", 0, ""},
		{"bidirectional allowed", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1:0/invoke-with-bidirectional-stream", 0, ""},
		{"bidirectional refused", http.MethodPost, "/model/amazon.titan-text/invoke-with-bidirectional-stream", http.StatusForbidden, "is not on"},
		{"unknown op", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1:0/count-tokens", http.StatusBadRequest, modelUnreadable},
		{"model path alone", http.MethodGet, "/model", http.StatusBadRequest, modelUnreadable},
		{"trailing slash", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1:0/invoke/", http.StatusBadRequest, "dot, empty or unreadable"},
		{"doubled slash", http.MethodPost, "//model/anthropic.claude-sonnet-5-v1:0/invoke", http.StatusBadRequest, "dot, empty or unreadable"},
		{"five segments", http.MethodPost, "/model/anthropic.claude-sonnet-5-v1:0/x/y/invoke", http.StatusBadRequest, modelUnreadable},
		{"raw slash in an arn", http.MethodPost, "/model/arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-5/converse", http.StatusBadRequest, modelUnreadable},
		{"dot segment", http.MethodPost, "/model/./amazon.titan-text/invoke", http.StatusBadRequest, "dot, empty or unreadable"},
		{"dotdot to a listed id", http.MethodPost, "/model/amazon.titan-text/../anthropic.claude-sonnet-5-v1:0/invoke", http.StatusBadRequest, "dot, empty or unreadable"},
		{"dotdot out of model", http.MethodPost, "/foundation-models/../model/amazon.titan-text/invoke", http.StatusBadRequest, "dot, empty or unreadable"},
		{"escaped dotdot", http.MethodPost, "/model/amazon.titan-text/%2e%2e/anthropic.claude-sonnet-5-v1:0/invoke", http.StatusBadRequest, "dot, empty or unreadable"},
		{"parameter dotdot", http.MethodPost, "/model/..;x/invoke", http.StatusBadRequest, "dot, empty or unreadable"},
		{"rerank refused", http.MethodPost, "/rerank", http.StatusForbidden, uncheckedRouteRefusal},
		{"agent runtime refused", http.MethodPost, "/agents/A/agentAliases/B/sessions/S/text", http.StatusForbidden, uncheckedRouteRefusal},
		{"customisation job refused", http.MethodPost, "/model-customization-jobs", http.StatusForbidden, uncheckedRouteRefusal},
		{"guardrail apply refused", http.MethodPost, "/guardrail/g/version/1/apply", http.StatusForbidden, uncheckedRouteRefusal},
		{"delete refused", http.MethodDelete, "/model-invocation-job/abc", http.StatusForbidden, uncheckedRouteRefusal},
		{"head passes", http.MethodHead, "/foundation-models", 0, ""},
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

// The cleaned-path rules, directly: what a path may hold, and what a Bedrock model path reads as.
func TestAllowedModels_PathRules(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"", true}, {"/", true}, {"v1/messages", true}, {"/v1/messages", true}, {"model/a%2Fb/invoke", true},
		{"v1/./messages", false}, {"v1/../messages", false}, {"v1//messages", false}, {"v1/messages/", false},
		{"v1/%2e%2E/messages", false}, {"v1/..;x/messages", false}, {"v1/mes%zzages", false},
		{`v1\messages`, false}, {"v1/mes%00sages", false},
	} {
		if _, ok := cleanModelPath(tc.path, true); ok != tc.ok {
			t.Errorf("cleanModelPath(%q) ok = %v, want %v", tc.path, ok, tc.ok)
		}
	}
	for _, tc := range []struct {
		path  string
		check modelCheck
		model string
	}{
		{"/model/a%3Ab/invoke", checkPathModel, "a:b"},
		{"/model/a%2Fb/converse-stream", checkPathModel, "a/b"},
		{"/model/anthropic%zz/invoke", checkUnreadable, ""},
		{"/model//invoke", checkUnreadable, ""},
		{"/model/a/b/invoke", checkUnreadable, ""},
	} {
		if c, m := bedrockCheck(httptest.NewRequest(http.MethodPost, "/", nil), tc.path); c != tc.check || m != tc.model {
			t.Errorf("bedrockCheck(%q) = %v %q, want %v %q", tc.path, c, m, tc.check, tc.model)
		}
	}
}

// The terminated-tunnel lane (a model host the proxy MITMs) is gated before the body is forwarded.
func TestAllowedModels_TerminatedTunnelGated(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}), Resolver: publicResolver{}})
	p.allowedModels = modelSet([]string{"claude-sonnet-5"})
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
		{"set but empty", []string{}, ""},
		{"ok", []string{"a", "b:1"}, ""},
		{"empty entry", []string{"a", ""}, "must not be empty"},
		{"padded entry", []string{" a"}, "leading or trailing space"},
		{"trailing space", []string{"a\t"}, "leading or trailing space"},
		{"duplicate", []string{"a", "b", "a"}, `"a" is listed twice`},
		{"case differs is not a duplicate", []string{"a", "A"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			if tc.list != nil {
				c.AllowedModels = &tc.list
			}
			err := c.validateAllowedModels()
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
	// Unset is off, and is not written back (an older proxy refuses a key it does not know).
	c, err := load(nil)
	if err != nil || c.AllowedModels != nil || compileAllowedModels(c.AllowedModels, nil) != nil {
		t.Fatalf("unset must stay off: %+v %v", c, err)
	}
	if out, _ := json.Marshal(c); strings.Contains(string(out), "allowed_models") {
		t.Fatalf("an unset list must not be written back: %s", out)
	}
	// A list loads and compiles to its set.
	if c, err = load(map[string]any{"allowed_models": []string{"claude-sonnet-5"}}); err != nil || c.AllowedModels == nil ||
		!compileAllowedModels(c.AllowedModels, nil).models["claude-sonnet-5"] {
		t.Fatalf("allowed_models did not load: %+v %v", c, err)
	}
	// `[]` loads, survives the wire, and compiles to a set that is not nil: on, and empty.
	if c, err = load(map[string]any{"allowed_models": []string{}}); err != nil || c.AllowedModels == nil {
		t.Fatalf("an empty list must load as set: %+v %v", c, err)
	}
	if set := compileAllowedModels(c.AllowedModels, nil); set == nil || len(set.models) != 0 {
		t.Fatalf("an empty list must compile to a non-nil empty set, got %#v", set)
	}
	if out, _ := json.Marshal(c); !strings.Contains(string(out), `"allowed_models":[]`) {
		t.Fatalf("an empty list must survive the wire: %s", out)
	}
	if _, err := load(map[string]any{"allowed_models": []string{"a", "a"}}); err == nil {
		t.Fatal("a duplicate entry must fail the load")
	}
}

// `[]` is deny-all: the gate is on and no model is listed, so every checked request is refused and
// nothing is forwarded; a read still passes.
func TestAllowedModels_EmptyListRefusesEveryModel(t *testing.T) {
	for _, route := range []string{llmAnthropicPrefix + "messages", llmOpenAIPrefix + "responses"} {
		p, up, _ := modelGateProxy(t, []string{})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, route, strings.NewReader(`{"model":"claude-sonnet-5"}`)))
		if rec.Code != http.StatusForbidden || up.reached {
			t.Fatalf("%s: status = %d reached = %v, want every model refused", route, rec.Code, up.reached)
		}
	}
	p, up, _ := modelGateProxy(t, []string{})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, llmAnthropicPrefix+"models", nil))
	if rec.Code != http.StatusOK || !up.reached {
		t.Fatalf("a read: status = %d reached = %v, want it forwarded", rec.Code, up.reached)
	}
	bedrock := newProxy(Options{})
	bedrock.allowedModels = modelSet([]string{})
	if _, refused := bedrock.allowedModelRefused(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/model/x/invoke", nil),
		"bedrock-runtime.us-east-1.amazonaws.com", 443, ""); !refused {
		t.Fatal("an empty list must refuse a Bedrock model too")
	}
}

// With the list set, a request on a model host that carries no checked model is refused unless it is
// a read; with the list unset every one of them is forwarded as ever.
func TestAllowedModels_UncheckedRoutes(t *testing.T) {
	const body = `{"model":"gpt-5"}`
	for _, tc := range []struct {
		name, method, route string
		refused             bool
	}{
		{"embeddings", http.MethodPost, llmOpenAIPrefix + "embeddings", true},
		{"images", http.MethodPost, llmOpenAIPrefix + "images/generations", true},
		{"audio speech", http.MethodPost, llmOpenAIPrefix + "audio/speech", true},
		{"audio transcription", http.MethodPost, llmOpenAIPrefix + "audio/transcriptions", true},
		{"moderations", http.MethodPost, llmOpenAIPrefix + "moderations", true},
		{"assistants", http.MethodPost, llmOpenAIPrefix + "assistants", true},
		{"thread run", http.MethodPost, llmOpenAIPrefix + "threads/thread_1/runs", true},
		{"thread message", http.MethodPost, llmOpenAIPrefix + "threads/thread_1/messages", true},
		{"fine tuning", http.MethodPost, llmOpenAIPrefix + "fine_tuning/jobs", true},
		{"realtime sessions", http.MethodPost, llmOpenAIPrefix + "realtime/sessions", true},
		{"legacy complete", http.MethodPost, llmAnthropicPrefix + "complete", true},
		{"delete a response", http.MethodDelete, llmOpenAIPrefix + "responses/resp_1", true},
		{"put", http.MethodPut, llmAnthropicPrefix + "messages", true},
		{"get models", http.MethodGet, llmOpenAIPrefix + "models", false},
		{"get a batch", http.MethodGet, llmOpenAIPrefix + "batches/batch_1", false},
		{"head", http.MethodHead, llmOpenAIPrefix + "models", false},
		{"anthropic files", http.MethodPost, llmAnthropicPrefix + "files", true},
		{"anthropic skills", http.MethodPost, llmAnthropicPrefix + "skills", true},
		{"openai files stay batch input", http.MethodPost, llmOpenAIPrefix + "files", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, list := range [][]string{{"gpt-5"}, nil} {
				p, up, _ := modelGateProxy(t, list)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.route, strings.NewReader(body)))
				if tc.refused && list != nil {
					if rec.Code != http.StatusForbidden || up.reached {
						t.Fatalf("status = %d reached = %v body = %q, want it refused", rec.Code, up.reached, rec.Body.String())
					}
					continue
				}
				if rec.Code != http.StatusOK || !up.reached {
					t.Fatalf("list %v: status = %d reached = %v (%q), want it forwarded", list, rec.Code, up.reached, rec.Body.String())
				}
			}
		})
	}
}

// A path holding a dot or empty segment on a model host is refused outright while the list is set,
// so no suffix match is fooled by one; matching is on the cleaned path.
func TestAllowedModels_DotAndEmptySegments(t *testing.T) {
	p := newProxy(Options{})
	p.allowedModels = modelSet([]string{"gpt-5"})
	for _, tc := range []struct {
		host, method, target string // target is the request target as the sandbox sent it
	}{
		{anthropicHost, http.MethodPost, "/v1/./messages"},
		{anthropicHost, http.MethodPost, "/v1/files/../messages"},
		{openaiHost, http.MethodPost, "/v1/embeddings/../responses"},
		{openaiHost, http.MethodPost, "/v1/../v1/batches"},
		{openaiHost, http.MethodPost, "/v1//responses"},
		{openaiHost, http.MethodPost, "/v1/responses/"},
		{openaiHost, http.MethodGet, "/v1/models/../models"},
		{openaiHost, http.MethodPost, "/v1/%2e%2e/responses"},
		{openaiHost, http.MethodPost, "/v1/files%2F..%2Fresponses"},
		{anthropicHost, http.MethodPost, "/v1/..;x/messages"},
	} {
		req := httptest.NewRequest(tc.method, tc.target, failReader{t})
		rec := httptest.NewRecorder()
		if _, refused := p.allowedModelRefused(rec, req, tc.host, 443, strings.TrimPrefix(req.URL.Path, "/")); !refused ||
			rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "dot, empty or unreadable") {
			t.Errorf("%s %s: refused = %v status = %d body = %q, want a 400 refusal", tc.method, tc.target, refused, rec.Code, rec.Body.String())
		}
	}
}

// A via_org route to a model host is gated before the organisation forwards it.
func TestAllowedModels_ViaOrgGated(t *testing.T) {
	newViaOrg := func(list []string) *Proxy {
		p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
			ViaOrgRoutes: []ViaOrgRoute{{GrantID: uuid.New(), Host: anthropicHost}}})
		p.allowedModels = modelSet(list)
		return p
	}
	post := func(p *Proxy, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(body)), anthropicHost, 443)
		return rec
	}
	if rec := post(newViaOrg([]string{"claude-sonnet-5"}), `{"model":"claude-opus-9"}`); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "is not on this run's allowed models list") {
		t.Fatalf("an unlisted model: status = %d body = %q, want it refused", rec.Code, rec.Body.String())
	}
	// Listed, and list unset: the request reaches forwardViaOrg, which has no control plane here.
	for _, list := range [][]string{{"claude-sonnet-5"}, nil} {
		if rec := post(newViaOrg(list), `{"model":"claude-sonnet-5"}`); rec.Code != http.StatusBadGateway ||
			!strings.Contains(rec.Body.String(), "control plane url not configured") {
			t.Fatalf("list %v: status = %d body = %q, want it handed to the organisation", list, rec.Code, rec.Body.String())
		}
	}
}

// A method the policy refuses keeps its policy:method row: the gate does not answer first.
func TestAllowedModels_PolicyMethodKeepsItsRow(t *testing.T) {
	buf := &bytes.Buffer{}
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedMethods: []string{http.MethodGet}}),
		Sink: &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}, Resolver: publicResolver{}})
	p.allowedModels = modelSet([]string{"claude-sonnet-5"})
	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(`{"model":"claude-opus-9"}`)), anthropicHost, 443)
	if d := lastDecision(t, buf); rec.Code != http.StatusForbidden || d.RuleSource != "policy:method" {
		t.Fatalf("status = %d decision = %+v, want the policy:method refusal", rec.Code, d)
	}
}

// A batch request names its models inside the body or in an uploaded file, so it is refused with
// the list set and never forwarded; reading a batch back is not a batch request.
func TestAllowedModels_BatchRoutesRefused(t *testing.T) {
	for _, tc := range []struct {
		name, method, route string
		refused             bool
	}{
		{"anthropic batch create", http.MethodPost, llmAnthropicPrefix + "messages/batches", true},
		{"openai batch create", http.MethodPost, llmOpenAIPrefix + "batches", true},
		{"openai file upload", http.MethodPost, llmOpenAIPrefix + "files", true},
		{"openai upload create", http.MethodPost, llmOpenAIPrefix + "uploads", true},
		{"openai upload part", http.MethodPost, llmOpenAIPrefix + "uploads/upload_abc/parts", true},
		{"openai upload complete", http.MethodPost, llmOpenAIPrefix + "uploads/upload_abc/complete", true},
		{"anthropic batch read", http.MethodGet, llmAnthropicPrefix + "messages/batches/msgbatch_1", false},
		{"openai batch read", http.MethodGet, llmOpenAIPrefix + "batches/batch_1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, list := range [][]string{{"gpt-5"}, nil} {
				p, up, buf := modelGateProxy(t, list)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.route, strings.NewReader(`{"requests":[{"params":{"model":"claude-opus-9"}}]}`)))
				if tc.refused && list != nil {
					if rec.Code != http.StatusForbidden || up.reached || !strings.Contains(rec.Body.String(), batchRefusal) {
						t.Fatalf("status = %d reached = %v body = %q, want the batch refused", rec.Code, up.reached, rec.Body.String())
					}
					if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != ruleSourceAllowedModels || d.Cause != batchRefusal {
						t.Fatalf("decision = %+v", d)
					}
					continue
				}
				if rec.Code != http.StatusOK || !up.reached {
					t.Fatalf("list %v: status = %d reached = %v, want it forwarded", list, rec.Code, up.reached)
				}
			}
		})
	}
}

func TestAllowedModels_BedrockBatchPathsRefused(t *testing.T) {
	p := newProxy(Options{})
	p.allowedModels = modelSet([]string{"m"})
	for _, tc := range []struct {
		host, method, path string
		refused            bool
	}{
		{"bedrock-runtime.us-east-1.amazonaws.com", http.MethodPost, "/async-invoke", true},
		{"bedrock.us-east-1.amazonaws.com", http.MethodPost, "/model-invocation-job", true},
		{"bedrock.us-east-1.amazonaws.com", http.MethodGet, "/model-invocation-job/abc", false},
	} {
		rec := httptest.NewRecorder()
		_, refused := p.allowedModelRefused(rec, httptest.NewRequest(tc.method, tc.path, nil), tc.host, 443, "")
		if refused != tc.refused || (refused && !strings.Contains(rec.Body.String(), batchRefusal)) {
			t.Errorf("%s %s %s: refused = %v body = %q", tc.host, tc.method, tc.path, refused, rec.Body.String())
		}
	}
}

// A CONNECT to a model host the proxy would pass as an opaque tunnel is refused with the list set;
// unset it reaches the tunnel code as ever (a recorder cannot be hijacked, which is how it shows).
func TestAllowedModels_OpaqueModelTunnelRefused(t *testing.T) {
	hosts := []string{anthropicHost, openaiHost, "bedrock-runtime.us-east-1.amazonaws.com", "foundry.corp.internal", "llm-gateway.corp.internal"}
	for _, host := range hosts {
		for _, list := range [][]string{{"claude-sonnet-5"}, nil} {
			buf := &bytes.Buffer{}
			p := newProxy(Options{
				RunID:           uuid.New(),
				Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: append(hosts, "registry.npmjs.org")}),
				Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
				Resolver:        publicResolver{},
				LLMChannelHosts: map[string]string{"foundry.corp.internal": anthropicHost},
				LLMUpstreams:    map[string]string{openaiHost: "https://llm-gateway.corp.internal/v1"},
			})
			p.allowedModels = modelSet(list)
			rec := httptest.NewRecorder()
			p.handleConnect(rec, httptest.NewRequest(http.MethodConnect, host+":443", nil))
			if list != nil {
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), opaqueModelTunnelRefusal) ||
					rec.Header().Get(egressHeaderReason) != ruleSourceAllowedModels {
					t.Fatalf("%s: status = %d reason = %q body = %q, want the tunnel refused", host, rec.Code, rec.Header().Get(egressHeaderReason), rec.Body.String())
				}
				if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != ruleSourceAllowedModels || d.Request.Host != host {
					t.Fatalf("%s: decision = %+v", host, d)
				}
			} else if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "hijacking unsupported") {
				t.Fatalf("%s: unset list: status = %d body = %q, want the tunnel code reached", host, rec.Code, rec.Body.String())
			}
		}
	}
	// A host that is no model host is never refused by the gate.
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"registry.npmjs.org"}}), Resolver: publicResolver{}})
	p.allowedModels = modelSet([]string{"claude-sonnet-5"})
	rec := httptest.NewRecorder()
	p.handleConnect(rec, httptest.NewRequest(http.MethodConnect, "registry.npmjs.org:443", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a non-model tunnel: status = %d body = %q, want it left alone", rec.Code, rec.Body.String())
	}
}

// The plain-HTTP forward lane is gated like the others: an unlisted model, an unreadable body and a
// batch are refused and never forwarded; a listed model is forwarded byte for byte.
func TestAllowedModels_PlainLane(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{anthropicHost}}
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"allowed", "/v1/messages", `{"model":"claude-sonnet-5",  "messages":[]}`, 200},
		{"refused", "/v1/messages", `{"model":"claude-opus-9"}`, 403},
		{"unreadable", "/v1/messages", `{"model":`, 400},
		{"batch", "/v1/messages/batches", `{"requests":[]}`, 403},
		{"unchecked route", "/v1/embeddings", `{"model":"claude-sonnet-5"}`, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := captureUpstream(t, false, `{"ok":true}`)
			p, _ := newTestProxy(t, spec, upstreamAddr(up.srv), nil, nil)
			p.allowedModels = modelSet([]string{"claude-sonnet-5"})
			req := mustProxyReq(t, http.MethodPost, "http://"+anthropicHost+tc.path)
			req.Body = io.NopCloser(strings.NewReader(tc.body))
			req.ContentLength = int64(len(tc.body))
			rec := httptest.NewRecorder()
			p.servePlain(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == 200 && (!up.reached || up.body != tc.body) {
				t.Fatalf("reached = %v body = %q, want the body forwarded byte for byte", up.reached, up.body)
			}
			if tc.status != 200 && up.reached {
				t.Fatal("a refused request must never reach the upstream")
			}
		})
	}
}

// No route that names no model is let through while the list is set: a file or skill upload to a
// model host is refused, unread, with a deny row under the gate's rule source, until a capture of
// real harness traffic backs an exact path. A read still writes no row of its own.
func TestAllowedModels_FileAndSkillUploadsRefused(t *testing.T) {
	for _, rest := range []string{"v1/files", "v1/skills"} {
		buf := &bytes.Buffer{}
		p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
			Sink: &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}, Resolver: publicResolver{}})
		p.allowedModels = modelSet([]string{"model-a"})
		req := httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+"/"+rest, failReader{t})
		rec := httptest.NewRecorder()
		if _, refused := p.allowedModelRefused(rec, req, anthropicHost, 443, rest); !refused || rec.Code != http.StatusForbidden {
			t.Fatalf("POST %s: refused = %v status = %d, want a 403", rest, refused, rec.Code)
		}
		d := lastDecision(t, buf)
		if d.Decision != egress.Deny || d.RuleSource != ruleSourceAllowedModels {
			t.Fatalf("POST %s: decision = %+v, want a deny row under %s", rest, d, ruleSourceAllowedModels)
		}
		buf.Reset()
		get := httptest.NewRequest(http.MethodGet, "https://"+anthropicHost+"/"+rest, nil)
		if _, refused := p.allowedModelRefused(httptest.NewRecorder(), get, anthropicHost, 443, rest); refused || buf.Len() != 0 {
			t.Fatalf("GET %s was refused=%v or wrote %q", rest, refused, buf.String())
		}
	}
}

// The tunnel refusal is said in words a person can act on: no wire terms.
func TestAllowedModels_TunnelRefusalNamesNoWireTerms(t *testing.T) {
	for _, term := range []string{"mitm", "CA", "TLS", "tunnel"} {
		if strings.Contains(opaqueModelTunnelRefusal, term) {
			t.Errorf("the tunnel refusal %q says %q", opaqueModelTunnelRefusal, term)
		}
	}
}

// A plain absolute-URI request to a model host that a via_org grant serves is judged before it is
// forwarded to the organisation, as its terminated twin is: an unlisted model is refused with a deny
// row under the gate's rule source and never reaches the organisation; a listed one is forwarded.
func TestAllowedModels_PlainViaOrgRequest(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"refused", `{"model":"claude-opus-9","messages":[]}`, http.StatusForbidden},
		{"allowed", `{"model":"claude-sonnet-5","messages":[]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaptopFixture(t)
			f.p.viaOrgRoutes = append(f.p.viaOrgRoutes, ViaOrgRoute{GrantID: f.header, Host: anthropicHost})
			f.p.allowedModels = modelSet([]string{"claude-sonnet-5"})
			req, _ := http.NewRequest(http.MethodPost, "http://"+anthropicHost+"/v1/messages", strings.NewReader(tc.body))
			resp := proxyDoHandler(t, f.p, req)
			_ = readAll(t, resp)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			_ = f.p.sink.close(context.Background())
			if tc.status == http.StatusOK {
				if f.cpSeen != 1 {
					t.Fatalf("the organisation saw %d requests, want the listed model forwarded", f.cpSeen)
				}
				return
			}
			if f.cpSeen != 0 || f.lHits != 0 {
				t.Fatalf("a refused request reached the organisation (%d) or the laptop (%d)", f.cpSeen, f.lHits)
			}
			rows := f.sink.String()
			if !strings.Contains(rows, `"decision":"deny"`) || !strings.Contains(rows, ruleSourceAllowedModels) {
				t.Fatalf("no deny row under %s: %s", ruleSourceAllowedModels, rows)
			}
		})
	}
}
