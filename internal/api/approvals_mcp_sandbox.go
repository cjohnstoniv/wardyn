// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An MCP proxy's first-use hold is an egress_domain row labelled with its server's key at the top-level
// requested_scope.mcp_sandbox, which only mcpSandboxScope writes. The label keeps it apart from the agent's
// row for the same host: each token rejoins and reads only rows of its own label.

// mcpSandboxKeyOf is the row's MCP sandbox label, "" for a row the agent raised. The key is matched exactly,
// as mcpSandboxScope writes it, never by encoding/json's case folding.
func mcpSandboxKeyOf(ap types.ApprovalRequest) string {
	var top map[string]json.RawMessage
	var key string
	if json.Unmarshal(ap.RequestedScope, &top) != nil || json.Unmarshal(top["mcp_sandbox"], &key) != nil {
		return ""
	}
	return key
}

// isMCPSandboxRow reports whether ap is an MCP proxy's egress hold.
func isMCPSandboxRow(ap types.ApprovalRequest) bool {
	return ap.Kind == types.ApprovalEgressDomain && mcpSandboxKeyOf(ap) != ""
}

// mcpRowMatchesCaller reports whether ap carries the caller's own label: the MCP proxy's server key, or none
// for the agent's internal token.
func mcpRowMatchesCaller(ctx context.Context, ap types.ApprovalRequest) bool {
	return mcpSandboxKeyOf(ap) == audit.MCPSandboxOf(ctx)
}

// mcpSandboxDecisionRule is decisionScopeForKind's rule for an MCP server's hold: it is decided for this run at
// most, so no decision_scope, "once", "until" or "run". Any other scope ("always" above all) is refused, so the
// decision never writes the host into a workspace's egress lists and never reaches a later run. Every other row
// passes. It writes the refusal and reports false.
func mcpSandboxDecisionRule(w http.ResponseWriter, ap types.ApprovalRequest, scope types.ApprovalScope) bool {
	if !isMCPSandboxRow(ap) {
		return true
	}
	switch scope {
	case "", types.ScopeOnce, types.ScopeUntil, types.ScopeRun:
		return true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonDecisionScopeInvalidForKind,
		`An MCP server's host request is decided for this run only: send no decision_scope, "once", "until" or "run"`)
	return false
}
