// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func mcpDef(url string, hosts ...string) types.ComponentDefinition {
	return types.ComponentDefinition{Hosts: hosts, MCP: &types.ComponentMCP{URL: url, Transport: types.ComponentMCPTransportHTTP}}
}

func mcpHeader(host string) types.ComponentSecret {
	return types.ComponentSecret{SecretName: "linear-key", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: host}}
}

// TestComponentMCP_ValidateHttpsHostInHosts: the URL is https, carries no
// credentials, and its host is one of the component's hosts on the URL's port.
func TestComponentMCP_ValidateHttpsHostInHosts(t *testing.T) {
	ok := []types.ComponentDefinition{
		mcpDef("https://mcp.linear.app/mcp", "mcp.linear.app"),
		mcpDef("https://mcp.linear.app:443/mcp", "mcp.linear.app:443"),
		mcpDef("https://mcp.linear.app/mcp", "mcp.linear.app:443"),
		mcpDef("https://mcp.example.com:8443/mcp?profile=a", "mcp.example.com:8443"),
	}
	for _, d := range ok {
		if err := d.Validate(proxy.ValidDomainEntry, true); err != nil {
			t.Errorf("%s: %v", d.MCP.URL, err)
		}
	}
	bad := map[string]types.ComponentDefinition{
		"plain http":        mcpDef("http://mcp.linear.app/mcp", "mcp.linear.app"),
		"userinfo":          mcpDef("https://u:p@mcp.linear.app/mcp", "mcp.linear.app"),
		"host not listed":   mcpDef("https://mcp.linear.app/mcp", "api.linear.app"),
		"other port":        mcpDef("https://mcp.example.com:8443/mcp", "mcp.example.com"),
		"not a url":         mcpDef("mcp.linear.app/mcp", "mcp.linear.app"),
		"fragment":          mcpDef("https://mcp.linear.app/mcp#x", "mcp.linear.app"),
		"control byte":      mcpDef("https://mcp.linear.app/m\ncp", "mcp.linear.app"),
		"unknown transport": {Hosts: []string{"mcp.linear.app"}, MCP: &types.ComponentMCP{URL: "https://mcp.linear.app/mcp", Transport: "stdio"}},
	}
	for name, d := range bad {
		if err := d.Validate(proxy.ValidDomainEntry, true); err == nil || !strings.HasPrefix(err.Error(), "mcp.") {
			t.Errorf("%s: want an mcp.* refusal, got %v", name, err)
		}
	}

	// Its secret is a header to the server's own host, over TLS: never env,
	// file, another host or plain HTTP — for an organisation's row too.
	good := mcpDef("https://mcp.linear.app/mcp", "mcp.linear.app")
	good.Secrets = []types.ComponentSecret{mcpHeader("mcp.linear.app")}
	if err := good.Validate(proxy.ValidDomainEntry, true); err != nil {
		t.Fatalf("header secret to the server: %v", err)
	}
	secrets := map[string]types.ComponentSecret{
		"env":        {SecretName: "linear-key", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "LINEAR_KEY"}},
		"file":       {SecretName: "linear-key", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryFile, File: "linear.key"}},
		"other host": mcpHeader("api.linear.app"),
		"plain http": {SecretName: "linear-key", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "mcp.linear.app", PlainHTTP: true}},
	}
	for name, s := range secrets {
		d := mcpDef("https://mcp.linear.app/mcp", "mcp.linear.app", "api.linear.app")
		d.Secrets = []types.ComponentSecret{s}
		if err := d.Validate(proxy.ValidDomainEntry, false); err == nil || !strings.Contains(err.Error(), "MCP server's secret") {
			t.Errorf("%s: want the MCP secret refusal, got %v", name, err)
		}
	}
}

// TestComponentMCP_SSERefused: an SSE server's endpoint event names a path
// outside the broker route, so the transport is refused when written.
func TestComponentMCP_SSERefused(t *testing.T) {
	d := types.ComponentDefinition{Hosts: []string{"mcp.linear.app"}, MCP: &types.ComponentMCP{URL: "https://mcp.linear.app/sse", Transport: "sse"}}
	err := d.Validate(proxy.ValidDomainEntry, true)
	if err == nil || !strings.Contains(err.Error(), "SSE are not supported yet") {
		t.Fatalf("want the SSE refusal, got %v", err)
	}
}

// TestComponentMCP_CLIExclusive: one component is an MCP server or a CLI
// credential card, never both.
func TestComponentMCP_CLIExclusive(t *testing.T) {
	d := mcpDef("https://mcp.linear.app/mcp", "mcp.linear.app")
	d.CLI = &types.ComponentCLI{Kind: types.CLIKindAWS, AWS: &types.CLIAWS{StartURL: "https://x.awsapps.com/start",
		SSORegion: "us-east-1", AccountID: "123456789012", RoleName: "Dev", Region: "us-east-1"}}
	err := d.Validate(proxy.ValidDomainEntry, false)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want the mcp/cli refusal, got %v", err)
	}
}

// TestComponentRef_MCPHarnessesShape: harness ids, each once, never on a
// built-in.
func TestComponentRef_MCPHarnessesShape(t *testing.T) {
	id := uuid.New()
	inline := mcpDef("https://mcp.linear.app/mcp", "mcp.linear.app")
	ok := []types.ComponentRef{
		{ID: &id, MCPHarnesses: []string{"claude-code", "codex-cli"}},
		{Inline: &inline, MCPHarnesses: []string{"claude-code"}},
	}
	for _, r := range ok {
		if err := r.Validate(proxy.ValidDomainEntry); err != nil {
			t.Errorf("%v: %v", r.MCPHarnesses, err)
		}
	}
	bad := []types.ComponentRef{
		{ID: &id, MCPHarnesses: []string{"claude-code", "claude-code"}},
		{ID: &id, MCPHarnesses: []string{"Claude Code"}},
		{Builtin: types.ComponentBuiltinGitHub, Org: "o", Repos: []string{"o/r"}, MCPHarnesses: []string{"claude-code"}},
	}
	for _, r := range bad {
		if err := r.Validate(proxy.ValidDomainEntry); err == nil {
			t.Errorf("%v: want a refusal", r.MCPHarnesses)
		}
	}
}
