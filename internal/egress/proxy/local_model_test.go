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

const (
	localModelBase = "http://ollama.lan:11434"
	localModelName = "qwen2.5:3b"
)

// localModelProxy is a run whose Claude Code lane is a local_model provider
// at localModelBase allowing localModelName, dialling upstream.
func localModelProxy(t *testing.T, upstream string, inj *injector, s types.LocalModelSettings) (*Proxy, *bytes.Buffer) {
	t.Helper()
	if s.Models == nil {
		s.Models = []string{localModelName}
	}
	buf := &bytes.Buffer{}
	return newProxy(Options{
		RunID:        uuid.New(),
		Policy:       CompilePolicy(types.RunPolicySpec{}),
		Sink:         &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:     fakeResolver{m: map[string][]net.IP{"ollama.lan": ips("10.40.1.5")}},
		Dial:         redirectDial(upstream),
		Injector:     inj,
		LLMUpstreams: map[string]string{anthropicHost: localModelBase, openaiHost: localModelBase},
		LocalModels:  map[string]types.LocalModelSettings{localModelBase: s},
	}), buf
}

func TestLocalModel_AllowlistedModelPasses_PromptBytesUntouched(t *testing.T) {
	up := captureUpstream(t, false, `{"ok":true}`)
	p, buf := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{})
	body := `{"model":"qwen2.5:3b", "max_tokens": 1024,"messages":[{"role":"user","content":"keep  these   bytes"}]}`
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "wardyn-proxy-injected")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !up.reached {
		t.Fatalf("status = %d reached = %v body = %q", rec.Code, up.reached, rec.Body.String())
	}
	if up.path != "/v1/messages" || up.body != body {
		t.Fatalf("upstream got %q %q, want the path and the body byte for byte", up.path, up.body)
	}
	if got := up.header.Get("X-Api-Key"); got != "" {
		t.Fatalf("the sandbox's placeholder must be stripped, upstream saw %q", got)
	}
	if d := lastDecision(t, buf); d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want allow", d)
	}
}

func TestLocalModel_AuthConfigured_TokenInjected(t *testing.T) {
	up := captureUpstream(t, false, `{"ok":true}`)
	inj := staticInj(map[string]injectedHeader{"ollama.lan": {name: "Authorization", value: "Bearer PERSON-TOKEN"}})
	p, _ := localModelProxy(t, upstreamAddr(up.srv), inj, types.LocalModelSettings{})
	req := mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"v1/chat/completions", strings.NewReader(`{"model":"qwen2.5:3b"}`))
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	p.ServeHTTP(httptest.NewRecorder(), req)
	if got := up.header.Get("Authorization"); got != "Bearer PERSON-TOKEN" {
		t.Fatalf("upstream Authorization = %q, want the person's token", got)
	}
}

// Every refusal is answered before the server is dialled and recorded as a
// brokered:local-model deny whose cause names the rule.
func TestLocalModel_RefusedAndAudited(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		cause                    string
	}{
		{"other model", http.MethodPost, "v1/messages", `{"model":"llama3.2:3b"}`, http.StatusForbidden, `model "llama3.2:3b" is not on this run's model allowlist`},
		{"pull", http.MethodPost, "api/pull", `{"model":"qwen2.5:3b"}`, http.StatusForbidden, "POST /api/pull is not a route"},
		{"push", http.MethodPost, "api/push", `{"model":"qwen2.5:3b"}`, http.StatusForbidden, "POST /api/push is not a route"},
		{"create", http.MethodPost, "api/create", `{"model":"x"}`, http.StatusForbidden, "POST /api/create is not a route"},
		{"copy", http.MethodPost, "api/copy", `{}`, http.StatusForbidden, "POST /api/copy is not a route"},
		{"delete", http.MethodDelete, "api/delete", `{"model":"qwen2.5:3b"}`, http.StatusForbidden, "DELETE /api/delete is not a route"},
		{"blobs", http.MethodPost, "api/blobs/sha256:00", ``, http.StatusForbidden, "POST /api/blobs/sha256:00 is not a route"},
		{"me", http.MethodPost, "api/me", ``, http.StatusForbidden, "POST /api/me is not a route"},
		{"signout", http.MethodPost, "api/signout", ``, http.StatusForbidden, "POST /api/signout is not a route"},
		{"show", http.MethodPost, "api/show", `{"model":"qwen2.5:3b"}`, http.StatusForbidden, "POST /api/show is not a route"},
		{"ps", http.MethodGet, "api/ps", ``, http.StatusForbidden, "GET /api/ps is not a route"},
		{"wrong method", http.MethodGet, "api/chat", ``, http.StatusForbidden, "GET /api/chat is not a route"},
		{"cloud name", http.MethodPost, "api/chat", `{"model":"gpt-oss:120b-cloud"}`, http.StatusForbidden, "not on this run's model allowlist"},
		{"registry host", http.MethodPost, "api/generate", `{"model":"registry.example.com/ns/qwen2.5:3b"}`, http.StatusForbidden, "not on this run's model allowlist"},
		{"other model listed", http.MethodGet, "v1/models/llama3.2:3b", ``, http.StatusForbidden, "not on this run's model allowlist"},
		{"second model key", http.MethodPost, "v1/messages", `{"model":"qwen2.5:3b","Model":"llama3.2:3b"}`, http.StatusBadRequest, `names "Model" twice`},
		{"no model", http.MethodPost, "v1/messages", `{"messages":[]}`, http.StatusBadRequest, "one JSON object"},
		{"not json", http.MethodPost, "v1/messages", `model=qwen2.5:3b`, http.StatusBadRequest, "one JSON object"},
		{"trailing value", http.MethodPost, "v1/messages", `{"model":"qwen2.5:3b"}{"model":"llama3.2:3b"}`, http.StatusBadRequest, "one JSON object"},
		{"string bound", http.MethodPost, "v1/messages", `{"model":"qwen2.5:3b","max_tokens":"999999"}`, http.StatusBadRequest, "max_tokens must be a number"},
		{"second options key", http.MethodPost, "api/chat", `{"model":"qwen2.5:3b","options":{"num_ctx":8,"NUM_CTX":131072}}`, http.StatusBadRequest, `names "NUM_CTX" twice`},
		{"options not an object", http.MethodPost, "api/chat", `{"model":"qwen2.5:3b","options":[1]}`, http.StatusBadRequest, "options must be a JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := captureUpstream(t, false, `{}`)
			// The cloud name is listed too: no listing makes it reachable.
			p, buf := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{Models: []string{localModelName, "gpt-oss:120b-cloud"}})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, llmAnthropicPrefix+tc.path, strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if up.reached {
				t.Fatal("a refused request must never reach the model server")
			}
			d := lastDecision(t, buf)
			if d.Decision != egress.Deny || d.RuleSource != ruleSourceLocalModel || !strings.Contains(d.Cause, tc.cause) {
				t.Fatalf("decision = %+v, want a %s deny with cause containing %q", d, ruleSourceLocalModel, tc.cause)
			}
			if !strings.Contains(rec.Body.String(), ruleSourceLocalModel) {
				t.Fatalf("the answer must name the rule source: %q", rec.Body.String())
			}
		})
	}
}

// A bound replaces only its own value; every other byte, the prompt's
// included, reaches the server as sent.
func TestLocalModel_BoundsReplaceOnlyTheirOwnValues(t *testing.T) {
	up := captureUpstream(t, false, `{}`)
	p, _ := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{MaxContext: 8192, MaxOutput: 4096, KeepAliveSeconds: 600})
	in := `{"model":"qwen2.5:3b","messages":[{"role":"user","content":"x"}], "keep_alive" : -1,` +
		`"options":{"temperature":0,"num_ctx":131072,"NUM_PREDICT":1e6},"max_tokens":2048}`
	want := `{"model":"qwen2.5:3b","messages":[{"role":"user","content":"x"}], "keep_alive" : 600,` +
		`"options":{"temperature":0,"num_ctx":8192,"NUM_PREDICT":4096},"max_tokens":2048}`
	p.ServeHTTP(httptest.NewRecorder(), mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"api/chat", strings.NewReader(in)))
	if up.body != want {
		t.Fatalf("upstream body\n got %s\nwant %s", up.body, want)
	}

	// A negative output or context value means no limit (num_predict -1) or
	// fill the context (-2) to the server, so it is held to the cap too.
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		for _, n := range []string{"-1", "-2"} {
			got, _, err := boundLocalRequest([]byte(`{"model":"m","`+field+`":`+n+`}`), types.LocalModelSettings{MaxOutput: 4096})
			if err != nil || string(got) != `{"model":"m","`+field+`":4096}` {
				t.Fatalf("%s %s -> %s (%v), want 4096", field, n, got, err)
			}
		}
	}
	for _, opt := range []struct{ key, want string }{{"num_predict", "4096"}, {"num_ctx", "8192"}} {
		for _, n := range []string{"-1", "-2"} {
			got, _, err := boundLocalRequest([]byte(`{"model":"m","options":{"`+opt.key+`":`+n+`}}`),
				types.LocalModelSettings{MaxContext: 8192, MaxOutput: 4096})
			if err != nil || string(got) != `{"model":"m","options":{"`+opt.key+`":`+opt.want+`}}` {
				t.Fatalf("options.%s %s -> %s (%v), want %s", opt.key, n, got, err, opt.want)
			}
		}
	}

	for _, ka := range []string{`"24h"`, `"-1m"`, `86400`} {
		got, _, err := boundLocalRequest([]byte(`{"model":"m","keep_alive":`+ka+`}`), types.LocalModelSettings{KeepAliveSeconds: 600})
		if err != nil || string(got) != `{"model":"m","keep_alive":600}` {
			t.Fatalf("keep_alive %s -> %s (%v), want 600", ka, got, err)
		}
	}
}

func TestLocalModel_InventoryFilteredToAllowlist(t *testing.T) {
	for _, tc := range []struct{ path, answer, want string }{
		{"v1/models", `{"object":"list","data":[{"id":"llama3.2:3b"},{"id":"qwen2.5:3b","object":"model"},{"id":"secret:7b"}]}`,
			`{"data":[{"id":"qwen2.5:3b","object":"model"}],"object":"list"}`},
		{"api/tags", `{"models":[{"name":"llama3.2:3b","model":"llama3.2:3b"},{"name":"qwen2.5:3b","model":"qwen2.5:3b"}]}`,
			`{"models":[{"model":"qwen2.5:3b","name":"qwen2.5:3b"}]}`},
	} {
		up := captureUpstream(t, false, tc.answer)
		p, _ := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{})
		req := mustLocalReq(t, http.MethodGet, llmOpenAIPrefix+tc.path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Fatalf("%s: %d %s, want %s", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}

	// A listing the filter cannot read is refused, never relayed whole.
	up := captureUpstream(t, false, `not json, every model name`)
	p, buf := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, llmOpenAIPrefix+"v1/models", nil))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "every model name") {
		t.Fatalf("unreadable listing: %d %q, want a 502 without the listing", rec.Code, rec.Body.String())
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != ruleSourceLocalModel {
		t.Fatalf("decision = %+v, want a %s deny", d, ruleSourceLocalModel)
	}

	// One model's own entry is reachable when it is allowlisted.
	up = captureUpstream(t, false, `{"id":"qwen2.5:3b"}`)
	p, _ = localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{})
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, llmOpenAIPrefix+"v1/models/qwen2.5:3b", nil))
	if rec.Code != http.StatusOK || !up.reached {
		t.Fatalf("allowlisted model entry: %d reached=%v", rec.Code, up.reached)
	}
}

// The SSRF lift is the registered address alone: a local model server may
// answer on its own-subnet gateway (the Docker host), nothing else on the
// proxy's subnets may, and no other gateway gets the lift.
func TestLocalModel_GatewayLiftIsExactlyTheRegisteredAddress(t *testing.T) {
	_, sub, _ := net.ParseCIDR("172.18.0.0/16")
	mk := func(answer string, cp []net.IP) *Proxy {
		return newProxy(Options{
			RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
			Sink:         &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
			Resolver:     fakeResolver{m: map[string][]net.IP{"ollama.lan": ips(answer), "gw.lan": ips(answer)}},
			LLMUpstreams: map[string]string{anthropicHost: localModelBase, openaiHost: "https://gw.lan"},
			LocalModels:  map[string]types.LocalModelSettings{localModelBase: {Models: []string{localModelName}}},
			LocalSubnets: []*net.IPNet{sub}, ControlPlaneIPs: cp,
		})
	}
	if target, err := mk("172.18.0.1", nil).gatewayTarget("ollama.lan", 11434); err != nil || target != "172.18.0.1:11434" {
		t.Fatalf("the registered model server on the bridge gateway = %q %v, want admitted", target, err)
	}
	for _, tc := range []struct {
		name, host, answer string
		port               int
		cp                 []net.IP
	}{
		{"another own-subnet address", "ollama.lan", "172.18.0.5", 11434, nil},
		{"the control plane", "ollama.lan", "172.18.0.1", 11434, ips("172.18.0.1")},
		{"loopback", "ollama.lan", "127.0.0.1", 11434, nil},
		{"another port", "ollama.lan", "172.18.0.1", 22, nil},
		{"an ordinary gateway", "gw.lan", "172.18.0.1", 443, nil},
	} {
		if _, err := mk(tc.answer, tc.cp).gatewayTarget(tc.host, tc.port); !errors.Is(err, errGatewayVet) {
			t.Fatalf("%s: gatewayTarget = %v, want errGatewayVet", tc.name, err)
		}
	}
	// The ordinary lanes keep today's guard: the lift is the brokered route's alone.
	if g := mk("172.18.0.1", nil).vetHost("ollama.lan"); !g.Denied {
		t.Fatalf("vetHost(registered host) = %+v, want denied", g)
	}
}

// Every lane but the brokered route refuses the model server's host, even
// one the run's policy allowlists (an auth grant needs that entry).
func TestLocalModel_OrdinaryLanesRefuseTheHost(t *testing.T) {
	p := newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"ollama.lan", "ollama.lan.example"}}),
		Sink:         &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Resolver:     publicResolver{},
		LLMUpstreams: map[string]string{anthropicHost: localModelBase},
		LocalModels:  map[string]types.LocalModelSettings{localModelBase: {Models: []string{localModelName}}},
	})
	for _, port := range []int{11434, 443, 22} {
		d, _, log := p.evaluate(t.Context(), "ollama.lan", port, http.MethodConnect, "")
		if d != egress.Deny || log == nil || log.RuleSource != ruleSourceLocalModel {
			t.Fatalf("CONNECT ollama.lan:%d = %v %+v, want a %s deny", port, d, log, ruleSourceLocalModel)
		}
	}
	if d, _, _ := p.evaluate(t.Context(), "ollama.lan.example", 443, http.MethodConnect, ""); d == egress.Deny {
		t.Fatal("another host sharing the name's prefix must not be refused by this rule")
	}
}

// A cold model load can take 30 s and more: the route sets no deadline on the
// server's first byte.
func TestLocalModel_NoResponseHeaderDeadline(t *testing.T) {
	p, _ := localModelProxy(t, "127.0.0.1:1", nil, types.LocalModelSettings{})
	if d := p.transport.ResponseHeaderTimeout; d != 0 {
		t.Fatalf("ResponseHeaderTimeout = %v, want none", d)
	}
	if scheme, port := p.upstreamSchemeFor("ollama.lan", 11434); scheme != "http" || port != 80 {
		t.Fatalf("scheme = %s/%d, want plain http for an http:// model server", scheme, port)
	}
}

func TestLocalModel_ConfigValidation(t *testing.T) {
	for name, c := range map[string]Config{
		"not an upstream": {LocalModels: map[string]types.LocalModelSettings{localModelBase: {Models: []string{localModelName}}}},
		"cloud model": {LLMUpstreams: map[string]string{anthropicHost: localModelBase},
			LocalModels: map[string]types.LocalModelSettings{localModelBase: {Models: []string{"x:cloud"}}}},
		"no models": {LLMUpstreams: map[string]string{anthropicHost: localModelBase},
			LocalModels: map[string]types.LocalModelSettings{localModelBase: {}}},
	} {
		if err := c.validateLocalModels(); err == nil {
			t.Fatalf("%s: validateLocalModels accepted it", name)
		}
	}
	ok := Config{LLMUpstreams: map[string]string{anthropicHost: localModelBase},
		LocalModels: map[string]types.LocalModelSettings{localModelBase: {Models: []string{localModelName}}}}
	if err := ok.validateLocalModels(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Config{})
	if bytes.Contains(raw, []byte("local_models")) {
		t.Fatal("an unset local_models must write no key: an older proxy refuses one")
	}
}
