// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunComponents_McpOnlyRunKeepsAnEmptyAllowlist: a run whose only component
// is an MCP server (its host is the server's alone) is admitted with an empty
// allowlist, and the server is dispatched as a broker entry, so the agent
// reaches it only through the broker.
func TestRunComponents_McpOnlyRunKeepsAnEmptyAllowlist(t *testing.T) {
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, componentDoors[0], nil).WithContext(memberCtx([]string{"eng"}))
	ref := mcpInline("Linear")
	ref.Inline.Hosts = []string{"mcp.linear.test"}
	req := createRunRequest{Components: []types.ComponentRef{ref}}
	req.Tools = harnessTools("claude-code")
	spec := types.RunPolicySpec{MinConfinementClass: types.CC1, FirstUseApproval: types.FirstUseAlwaysDeny}
	comps, refusal := f.srv.applyRunComponents(r, req, &spec, governanceCeiling{Spec: govDeployment()}, nil, true)
	if refusal != nil {
		t.Fatalf("refused: %+v", refusal.body)
	}
	if len(spec.AllowedDomains) != 0 {
		t.Errorf("allowlist %v, want it empty", spec.AllowedDomains)
	}
	d := comps.dispatch()
	if !slices.Equal(d.HeaderHosts, []string{"mcp.linear.test"}) || len(d.MCP) != 1 || d.MCP[0].Key != "linear" {
		t.Errorf("dispatch %+v: want one broker entry for linear", d)
	}
}
