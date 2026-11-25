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

// The person's view carries an organisation component's image requirement, and
// no "image" key when the admin set none.
func TestOrgComponentView_CarriesTheImageRequirement(t *testing.T) {
	const image = "ghcr.io/acme/tools:1.2"
	with := orgComponentView(types.Component{ID: uuid.New(), Name: "Tools", Definition: types.ComponentDefinition{
		Hosts: []string{"tools.example"}, Image: image,
	}})
	if with.Image != image {
		t.Errorf("Image = %q, want %q", with.Image, image)
	}
	if raw := string(mustJSON(with)); !strings.Contains(raw, `"image":"`+image+`"`) {
		t.Errorf("the view's JSON has no image: %s", raw)
	}
	without := orgComponentView(types.Component{ID: uuid.New(), Name: "Plain", Definition: types.ComponentDefinition{Hosts: []string{"plain.example"}}})
	if without.Image != "" {
		t.Errorf("Image = %q, want none", without.Image)
	}
	if raw := string(mustJSON(without)); strings.Contains(raw, `"image"`) {
		t.Errorf("the view's JSON has an image key without a requirement: %s", raw)
	}
}
