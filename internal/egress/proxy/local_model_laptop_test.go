// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
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

// laptopModelBase is a laptop local model (#2957). On a runner, host.docker.internal and the
// control-plane name both resolve to the runner's relay address (172.17.0.1 here); the proxy's own
// subnet is wardyn-internal's.
const laptopModelBase = "http://host.docker.internal:11434"

// laptopProxy is a laptop runner's proxy for a run granted the laptop model (granted), told by the
// runner's driver that it relays it (relayed), dialling upstream.
func laptopProxy(t *testing.T, upstream string, granted, relayed, exclusionUnknown bool) (*Proxy, *bytes.Buffer) {
	t.Helper()
	_, sub, _ := net.ParseCIDR("172.18.0.0/16")
	o := Options{
		RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
		Resolver:     fakeResolver{m: map[string][]net.IP{"host.docker.internal": ips("172.17.0.1"), "wardynd.internal": ips("172.17.0.1")}},
		LocalSubnets: []*net.IPNet{sub}, ControlPlaneIPs: ips("172.17.0.1"), ExclusionUnknown: exclusionUnknown,
		Dial: redirectDial(upstream),
	}
	if granted {
		o.LLMUpstreams = map[string]string{anthropicHost: laptopModelBase, openaiHost: laptopModelBase}
		o.LocalModels = map[string]types.LocalModelSettings{laptopModelBase: {Models: []string{localModelName}, AllowOnLaptopRunners: true}}
	}
	if relayed {
		o.RunnerLocalModels = []string{laptopModelBase}
	}
	buf := &bytes.Buffer{}
	o.Sink = &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	return newProxy(o), buf
}

// The runner's relay address is admitted for the relayed server's host and port alone. Without
// runner_local_models the vet is today's (the address is the control plane's, so refused); a run not
// granted the server reaches it on no lane.
func TestLocalModel_LaptopRelayAdmitsOnlyItsHostAndPort(t *testing.T) {
	p, _ := laptopProxy(t, "127.0.0.1:1", true, true, false)
	if target, err := p.gatewayTarget("host.docker.internal", 11434); err != nil || target != "172.17.0.1:11434" {
		t.Fatalf("the relayed laptop model = %q %v, want 172.17.0.1:11434", target, err)
	}
	for name, vet := range map[string]func() error{
		"not relayed (flag off)": func() error {
			p, _ := laptopProxy(t, "127.0.0.1:1", true, false, false)
			_, err := p.gatewayTarget("host.docker.internal", 11434)
			return err
		},
		"another port": func() error { _, err := p.gatewayTarget("host.docker.internal", 443); return err },
		"the control-plane name": func() error {
			_, err := p.gatewayTarget("wardynd.internal", 11434)
			return err
		},
		"exclusion unknown": func() error {
			p, _ := laptopProxy(t, "127.0.0.1:1", true, true, true)
			_, err := p.gatewayTarget("host.docker.internal", 11434)
			return err
		},
		"a run not granted the server": func() error {
			p, _ := laptopProxy(t, "127.0.0.1:1", false, false, false)
			_, err := p.gatewayTarget("host.docker.internal", 11434)
			return err
		},
	} {
		if err := vet(); !errors.Is(err, errGatewayVet) {
			t.Errorf("%s: gatewayTarget = %v, want errGatewayVet", name, err)
		}
	}
	// A run not granted the server: its agent's own CONNECT is refused too.
	q, _ := laptopProxy(t, "127.0.0.1:1", false, false, false)
	if d, _, _ := q.evaluate(t.Context(), "host.docker.internal", 11434, http.MethodConnect, ""); d != egress.Deny {
		t.Fatalf("CONNECT host.docker.internal:11434 from a run not granted the server = %v, want deny", d)
	}
}

// On the laptop path the shipped route gate holds: an allowlisted model passes, another model and
// the management routes are refused and audited, never reaching the server.
func TestLocalModel_LaptopPathKeepsTheRouteGate(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
		cause            string
	}{
		{"allowlisted", "v1/messages", `{"model":"qwen2.5:3b"}`, http.StatusOK, ""},
		{"other model", "v1/messages", `{"model":"llama3.2:3b"}`, http.StatusForbidden, `model "llama3.2:3b" is not on this run's model allowlist`},
		{"pull", "api/pull", `{"model":"qwen2.5:3b"}`, http.StatusForbidden, "POST /api/pull is not a route"},
		{"push", "api/push", `{"model":"qwen2.5:3b"}`, http.StatusForbidden, "POST /api/push is not a route"},
		{"create", "api/create", `{"model":"x"}`, http.StatusForbidden, "POST /api/create is not a route"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := captureUpstream(t, false, `{}`)
			p, buf := laptopProxy(t, upstreamAddr(up.srv), true, true, false)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+tc.path, strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.cause == "" {
				if !up.reached {
					t.Fatal("the allowlisted request never reached the model server")
				}
				return
			}
			if up.reached {
				t.Fatal("a refused request reached the model server")
			}
			if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != ruleSourceLocalModel || !strings.Contains(d.Cause, tc.cause) {
				t.Fatalf("decision = %+v, want a %s deny with cause %q", d, ruleSourceLocalModel, tc.cause)
			}
		})
	}
}

// runner_local_models names only a laptop local model the config carries.
func TestLocalModel_RunnerLocalModelsValidation(t *testing.T) {
	up := map[string]string{anthropicHost: laptopModelBase}
	for name, s := range map[string]map[string]types.LocalModelSettings{
		"absent":    nil,
		"unflagged": {laptopModelBase: {Models: []string{localModelName}}},
	} {
		c := Config{LLMUpstreams: up, LocalModels: s, RunnerLocalModels: []string{laptopModelBase}}
		if err := c.validateLocalModels(); err == nil || !strings.Contains(err.Error(), "runner_local_models") {
			t.Errorf("%s: validateLocalModels = %v", name, err)
		}
	}
	ok := Config{LLMUpstreams: up, RunnerLocalModels: []string{laptopModelBase},
		LocalModels: map[string]types.LocalModelSettings{laptopModelBase: {Models: []string{localModelName}, AllowOnLaptopRunners: true}}}
	if err := ok.validateLocalModels(); err != nil {
		t.Fatal(err)
	}
}
