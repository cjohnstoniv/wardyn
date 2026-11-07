// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// isMCPCallRow reports whether ap asks a person to approve one MCP call with its exact arguments (a
// tool rule's approve_each): a tool_call known STRUCTURALLY by call_sha256, with no grant id, as an
// Azure DevOps escalation is known by its grant id. Only the proxy writes one, and the raise checked
// its key (refuseBadMCPCallScope). It restates approval.TallyKey's MCP arm, and
// TestApprovalTallyKeyAgreesWithTheLanes pins that the two agree.
//
// Who decides one: the run's owner or a security operator, as for an Azure DevOps escalation. The
// person whose agent makes the call confirms that exact call, and the decider is in the audit row.
// WARDYN_CAPABILITY_SECOND_HUMAN (default off, since it would deadlock a single-operator deployment)
// makes it four-eyes. An approval rule never decides one: the rule asks for a person.
func isMCPCallRow(ap types.ApprovalRequest) bool {
	return ap.Kind == types.ApprovalToolCall && (ap.GrantID == nil || *ap.GrantID == uuid.Nil) &&
		types.IsMCPCallScope(ap.RequestedScope)
}

// mcpCallTool is the display name of the tool an MCP call row asks about, "" when unreadable.
func mcpCallTool(ap types.ApprovalRequest) string {
	var sc types.MCPCallScope
	_ = json.Unmarshal(ap.RequestedScope, &sc)
	return sc.Tool
}

// mcpCallDecisionRule is decide's scope rule for an MCP call row: the approval is spent by one call,
// so a bodyless decision or "once" stores "once" and any wider scope is refused. isMCP reports whether
// the row is one; ok=false means the refusal is written.
func mcpCallDecisionRule(w http.ResponseWriter, ap types.ApprovalRequest, scope types.ApprovalScope) (types.ApprovalScope, bool, bool) {
	if !isMCPCallRow(ap) {
		return scope, false, true
	}
	if scope == "" || scope == types.ScopeOnce {
		return types.ScopeOnce, true, true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonDecisionScopeInvalidForKind,
		`An MCP call approval approves that one call, once: send no decision_scope, or "once"`)
	return scope, true, false
}

// refuseBadMCPCallScope is the raise's check of a scope that carries call_sha256 (in any letter case,
// as encoding/json reads it): the control plane recomputes the key from the scope's own server, tool
// and arguments and refuses a mismatch, so what a person is shown is what was hashed. It also
// refuses such a scope on another kind, a display name that is not the server's, and a call larger
// than a person can be shown, never truncated. Each refusal is a 400 with an auth.fail row: only a
// misbehaving proxy writes one. It reports whether it refused.
func (s *Server) refuseBadMCPCallScope(w http.ResponseWriter, r *http.Request, kind types.ApprovalKind, scope json.RawMessage) bool {
	if !types.IsMCPCallScope(scope) {
		return false
	}
	const tooLarge = "The MCP call is larger than a person can be shown"
	reason, msg := reasonMCPCallScopeInvalid, "requested_scope is not the hash of its own MCP call"
	var sc types.MCPCallScope
	shaped := kind == types.ApprovalToolCall && json.Unmarshal(scope, &sc) == nil &&
		sc.MCPServer != "" && strings.HasPrefix(sc.Tool, "mcp__"+sc.MCPServer+"__")
	switch {
	case len(scope) > maxSidecarBody:
		reason, msg = reasonMCPCallTooLarge, tooLarge
	case shaped:
		key, err := sc.ComputeKey()
		if errors.Is(err, types.ErrMCPCallTooLarge) {
			reason, msg = reasonMCPCallTooLarge, tooLarge
		} else if err == nil && key == sc.CallSHA256 {
			return false
		}
	}
	s.auditAuthFailedAs(r, internalApprovalActor, reason)
	writeErrorReason(w, http.StatusBadRequest, reason, msg)
	return true
}
