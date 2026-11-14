// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A granted organisation MCP server is an MCP card in the catalogue, remote or
// local. A remote one shows its origin only: a key written into the URL's path
// or query, the gateway audience and a local server's launch command stay with
// the admin.
func TestOrgComponentView_MCPRowIsAnMCPCard(t *testing.T) {
	remote := orgComponentView(types.Component{ID: uuid.New(), Name: "Tracker", Definition: types.ComponentDefinition{
		Hosts: []string{"mcp.example.com:8443"},
		MCP: &types.ComponentMCP{URL: "https://mcp.example.com:8443/k/route-key-123?token=q-secret",
			Transport: types.ComponentMCPTransportHTTP, GatewayAudience: "aud-tracker"},
	}})
	if remote.Category != "mcp" || remote.MCP == nil || remote.MCP.URL != "https://mcp.example.com:8443" || remote.MCP.Transport != "http" || remote.CLI != nil {
		t.Errorf("remote view = %+v (mcp %+v), want an mcp card at its origin", remote, remote.MCP)
	}
	local := orgComponentView(types.Component{ID: uuid.New(), Name: "Files", Definition: types.ComponentDefinition{
		MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio,
			Launch: &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "files-server@1.2.3", "--key=launch-secret"}}},
	}})
	if local.Category != "mcp" || local.MCP == nil || local.MCP.URL != "" || local.MCP.Transport != "stdio" {
		t.Errorf("local view = %+v (mcp %+v), want an mcp card with no address", local, local.MCP)
	}
	for _, raw := range []string{string(mustJSON(remote)), string(mustJSON(local))} {
		for _, leak := range []string{"route-key-123", "q-secret", "aud-tracker", "files-server", "launch-secret"} {
			if strings.Contains(raw, leak) {
				t.Errorf("the member's view carries %q: %s", leak, raw)
			}
		}
	}
}
