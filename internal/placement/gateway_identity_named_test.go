// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A run that carries identity routes is refused by the gateway identity gate, which names the server or
// provider, even when an earlier gate's field is set too; without routes the earlier gate answers.
func TestLocalEligibility_GatewayIdentityNamedOverEarlierGates(t *testing.T) {
	upstreams := map[string]string{"api.openai.com": "https://gw.corp.example/v1"}
	headers := map[string]map[string]string{"gw.corp.example": {"X-Team": "platform"}}
	provider := []proxy.GatewayIdentityRoute{{Host: "gw.corp.example", Port: 443, ProviderID: "corp-gw", Audience: "a"}}
	mcp := []proxy.GatewayIdentityRoute{{Host: "corp-mcp.corp.example", Port: 443, MCPKey: "corp-mcp", Audience: "corp-mcp"}}
	for _, tc := range []struct {
		name      string
		cfg       runner.ProxyConfig
		wantField string
		wantName  string
	}{
		{"upstreams with a provider route", runner.ProxyConfig{LLMUpstreams: upstreams, GatewayIdentity: provider}, "ProxyConfig.GatewayIdentity", "corp-gw"},
		{"upstreams and headers with a provider route", runner.ProxyConfig{LLMUpstreams: upstreams, LLMHeaders: headers, GatewayIdentity: provider}, "ProxyConfig.GatewayIdentity", "corp-gw"},
		{"upstreams with an MCP-only route", runner.ProxyConfig{LLMUpstreams: upstreams, GatewayIdentity: mcp}, "ProxyConfig.GatewayIdentity", "corp-mcp"},
		{"upstreams alone", runner.ProxyConfig{LLMUpstreams: upstreams}, "ProxyConfig.LLMUpstreams", ""},
		{"headers alone", runner.ProxyConfig{LLMHeaders: headers}, "ProxyConfig.LLMHeaders", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, r := LocalEligibility(LocalPlan{Spec: runner.SandboxSpec{ProxyConfig: tc.cfg}})
			if r == nil || r.Field != tc.wantField {
				t.Fatalf("refusal %v, want a refusal on %s", r, tc.wantField)
			}
			if tc.wantName != "" && !strings.Contains(r.Sentence(), tc.wantName) {
				t.Fatalf("sentence %q does not name %q", r.Sentence(), tc.wantName)
			}
		})
	}
}
