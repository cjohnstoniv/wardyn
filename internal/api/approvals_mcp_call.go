// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

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

// mcpCallScopeKeys are the keys of types.MCPCallScope. encoding/json matches a key in any letter case
// and keeps the last of a repeat, so each must appear at most once and spelled exactly so.
var mcpCallScopeKeys = []string{"tool", "mcp_server", "mcp_host", "mcp_tool", "arguments", "extra", "call_sha256", "harness", "hold_seconds"}

// mcpCallScopeKeysExact reports whether scope is an object whose top-level keys name each of
// mcpCallScopeKeys at most once and in its own spelling (strings.EqualFold covers every fold
// encoding/json applies, the Kelvin sign and the long s included).
func mcpCallScopeKeysExact(scope json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(scope))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		k, isKey := tok.(string)
		if err != nil || !isKey {
			return false
		}
		for _, want := range mcpCallScopeKeys {
			if strings.EqualFold(k, want) {
				if k != want || seen[want] {
					return false
				}
				seen[want] = true
			}
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
	}
	return true
}

// mcpShownToolName is the name Claude Code shows for a raw MCP tool name: every rune outside
// [A-Za-z0-9_-] becomes "_", and one outside the BMP "__". It mirrors claudeToolName in
// internal/egress/proxy/mcp_broker.go, which builds the display name the proxy writes; if they ever
// disagree, the raise is refused (fail closed), never accepted.
func mcpShownToolName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'):
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("__")
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// mcpCallDiscriminators are the keys only an MCP call scope carries.
var mcpCallDiscriminators = []string{"call_sha256", "mcp_server", "mcp_tool"}

// mcpCallKeyPresent reports whether scope is an object with a top-level key that folds to one of
// mcpCallDiscriminators, read from the keys alone and before any value is decoded: a later variant,
// a null or a non-string value can hide the hash from a decoder, and Postgres JSONB reorders case
// variants on the way back, so presence of the key, not its decoded value, picks the strict path.
func mcpCallKeyPresent(scope json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(scope))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	for dec.More() {
		tok, err := dec.Token()
		k, isKey := tok.(string)
		if err != nil || !isKey {
			return false
		}
		for _, d := range mcpCallDiscriminators {
			if strings.EqualFold(k, d) {
				return true
			}
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
	}
	return false
}

// mcpCallScopeShaped decodes scope as one MCP call's typed scope when its keys are exact, it names
// a server and a non-empty string hash, and its display name is the one its raw tool name maps to (mcp_tool, else the display
// name's own suffix).
func mcpCallScopeShaped(scope json.RawMessage) (types.MCPCallScope, bool) {
	var sc types.MCPCallScope
	if !mcpCallScopeKeysExact(scope) || json.Unmarshal(scope, &sc) != nil || sc.MCPServer == "" || sc.CallSHA256 == "" {
		return sc, false
	}
	prefix := "mcp__" + sc.MCPServer + "__"
	raw := sc.MCPTool
	if raw == "" {
		raw = strings.TrimPrefix(sc.Tool, prefix)
	}
	return sc, raw != "" && sc.Tool == prefix+mcpShownToolName(raw)
}

// refuseBadMCPCallScope is the raise's check of a scope with a key that folds to call_sha256,
// mcp_server or mcp_tool (mcpCallKeyPresent), whatever its value: the control plane recomputes the key from the scope's own server, tool
// and arguments and refuses a mismatch, so what a person is shown is what was hashed. It also
// refuses such a scope on another kind, one with a repeated or case-variant key, a display name that
// is not its raw tool's, and a call larger than a person can be shown, never truncated. Each refusal
// is a 400 with an auth.fail row: only a misbehaving proxy writes one. An accepted scope comes back
// re-marshalled from its typed form with canonical arguments and extra, and the row stores that,
// never the raw bytes. A scope with none of those keys comes back unchanged.
func (s *Server) refuseBadMCPCallScope(w http.ResponseWriter, r *http.Request, kind types.ApprovalKind, scope json.RawMessage) (json.RawMessage, bool) {
	if !mcpCallKeyPresent(scope) {
		return scope, false
	}
	const tooLarge = "The MCP call is larger than a person can be shown"
	reason, msg := reasonMCPCallScopeInvalid, "requested_scope is not the hash of its own MCP call"
	sc, shaped := mcpCallScopeShaped(scope)
	switch {
	case len(scope) > maxSidecarBody:
		reason, msg = reasonMCPCallTooLarge, tooLarge
	case kind == types.ApprovalToolCall && shaped:
		key, err := sc.ComputeKey()
		if errors.Is(err, types.ErrMCPCallTooLarge) {
			reason, msg = reasonMCPCallTooLarge, tooLarge
		} else if err == nil && key == sc.CallSHA256 {
			if out, ok := canonicalMCPCallScope(sc); ok {
				return out, false
			}
		}
	}
	// One constant per call, so the auth.fail enum guard can read every reason this row carries.
	if reason == reasonMCPCallTooLarge {
		s.auditAuthFailedAs(r, internalApprovalActor, reasonMCPCallTooLarge)
	} else {
		s.auditAuthFailedAs(r, internalApprovalActor, reasonMCPCallScopeInvalid)
	}
	writeErrorReason(w, http.StatusBadRequest, reason, msg)
	return nil, true
}

// canonicalMCPCallScope is sc marshalled with its arguments and extra in canonical form (extra
// dropped when it is {}), the same bytes its key hashed.
func canonicalMCPCallScope(sc types.MCPCallScope) (json.RawMessage, bool) {
	if len(bytes.TrimSpace(sc.Arguments)) == 0 {
		sc.Arguments = json.RawMessage(`{}`)
	}
	args, err := types.CanonicalJSON(sc.Arguments)
	if err != nil {
		return nil, false
	}
	sc.Arguments = args
	if len(bytes.TrimSpace(sc.Extra)) > 0 {
		ext, err := types.CanonicalJSON(sc.Extra)
		if err != nil {
			return nil, false
		}
		sc.Extra = ext
		if string(ext) == "{}" {
			sc.Extra = nil
		}
	}
	out, err := json.Marshal(sc)
	return out, err == nil
}

// projectCallText sets CallText on every MCP call row and every held kubectl write in place: the exact text of
// an MCP call's arguments, or a held kubectl write's body (empty when it has none), so a client never has to read a
// number out of the scope's JSON. Other rows stay nil.
func projectCallText(rows []types.ApprovalRequest) {
	for i := range rows {
		rows[i].CallText = nil
		if isMCPCallRow(rows[i]) {
			rows[i].CallText = types.MCPCallTextOf(rows[i].RequestedScope)
		} else if isKubeCallRow(rows[i]) {
			rows[i].CallText = types.KubeCallTextOf(rows[i].RequestedScope)
		}
	}
}
