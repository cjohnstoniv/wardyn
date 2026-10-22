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
		"unknown transport": {Hosts: []string{"mcp.linear.app"}, MCP: &types.ComponentMCP{URL: "https://mcp.linear.app/mcp", Transport: "websocket"}},
		"launch on http": {Hosts: []string{"mcp.linear.app"}, MCP: &types.ComponentMCP{URL: "https://mcp.linear.app/mcp", Transport: types.ComponentMCPTransportHTTP,
			Launch: &types.ComponentMCPLaunch{Command: "npx", Args: []string{"@example/mcp@1.0.0"}}}},
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

func localMCP(command string, args ...string) types.ComponentDefinition {
	return types.ComponentDefinition{Hosts: []string{"api.example.com"},
		MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio, Launch: &types.ComponentMCPLaunch{Command: command, Args: args}}}
}

// TestComponentMCP_StdioLaunch: a server that runs in the sandbox has a launch
// command and no url; the command is npx or an absolute path, and a package
// npx fetches names one exact version. Its secrets follow every component's
// rules: env and file enter the sandbox, a header goes to one of its hosts.
func TestComponentMCP_StdioLaunch(t *testing.T) {
	ok := map[string]types.ComponentDefinition{
		"npx -y scoped":    localMCP("npx", "-y", "@example/mcp-server@1.4.2", "--root", "/work"),
		"npx bare":         localMCP("npx", "mcp-server-x@0.1.0-rc.1"),
		"absolute program": localMCP("/usr/local/bin/mcp-files", "--read-only"),
		"absolute no args": localMCP("/opt/mcp/server"),
	}
	for name, d := range ok {
		for _, person := range []bool{false, true} {
			if err := d.Validate(proxy.ValidDomainEntry, person); err != nil {
				t.Errorf("%s (person %v): %v", name, person, err)
			}
		}
		if !d.LocalMCP() || d.RemoteMCP() {
			t.Errorf("%s: LocalMCP %v RemoteMCP %v", name, d.LocalMCP(), d.RemoteMCP())
		}
	}
	withURL := localMCP("npx", "@example/mcp@1.0.0")
	withURL.MCP.URL = "https://api.example.com/mcp"
	bad := map[string]types.ComponentDefinition{
		"url":            withURL,
		"no launch":      {Hosts: []string{"api.example.com"}, MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio}},
		"relative":       localMCP("bin/server"),
		"bare name":      localMCP("node", "server.js"),
		"unclean path":   localMCP("/opt/../bin/sh"),
		"npx unpinned":   localMCP("npx", "-y", "@example/mcp-server"),
		"npx latest":     localMCP("npx", "@example/mcp-server@latest"),
		"npx range":      localMCP("npx", "@example/mcp-server@^1.4.2"),
		"npx no package": localMCP("npx", "-y"),
		"npx --package":  localMCP("npx", "--package=@example/mcp-server@1.4.2", "mcp-server"),
		"abs npx":        localMCP("/usr/bin/npx", "@example/mcp-server@1"),
		"control byte":   localMCP("/opt/mcp/server", "a\nb"),
		"too many args":  localMCP("/opt/mcp/server", make([]string, types.MaxComponentMCPArgs+1)...),
	}
	for name, d := range bad {
		if err := d.Validate(proxy.ValidDomainEntry, true); err == nil || !strings.HasPrefix(err.Error(), "mcp.") {
			t.Errorf("%s: want an mcp.* refusal, got %v", name, err)
		}
	}
	d := localMCP("npx", "-y", "@example/mcp-server@1.4.2")
	d.Secrets = []types.ComponentSecret{
		{SecretName: "gh-token", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "GITHUB_TOKEN"}},
		{SecretName: "api-key", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "api.example.com"}},
	}
	if err := d.Validate(proxy.ValidDomainEntry, true); err != nil {
		t.Fatalf("env and header secrets on a local server: %v", err)
	}
	if remote := mcpDef("https://mcp.linear.app/mcp", "mcp.linear.app"); !remote.RemoteMCP() || remote.LocalMCP() {
		t.Error("an http server is remote")
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
