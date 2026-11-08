// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// isKubeCallRow reports whether ap asks a person to approve one kubectl write with its exact request
// (a kubectl row's approve_each): a tool_call known STRUCTURALLY by call_sha256 and cluster, with no
// grant id. Only the proxy writes one, and the raise checked its key (refuseBadKubeCallScope). It is
// never an MCP call row (types.IsMCPCallScope excludes it), and approval.TallyKey's kube arm restates
// it.
//
// Who decides one: as for one MCP call, the run's owner or a security operator, once;
// WARDYN_CAPABILITY_SECOND_HUMAN makes it four-eyes, and an approval rule never decides one.
func isKubeCallRow(ap types.ApprovalRequest) bool {
	return ap.Kind == types.ApprovalToolCall && (ap.GrantID == nil || *ap.GrantID == uuid.Nil) &&
		types.IsKubeCallScope(ap.RequestedScope)
}

// isExactCallRow reports whether ap approves one exact call the proxy holds: an MCP call or a kubectl
// write. The decide gates treat the two alike.
func isExactCallRow(ap types.ApprovalRequest) bool { return isMCPCallRow(ap) || isKubeCallRow(ap) }

// kubeCallScopeKeys are the keys of types.KubeCallScope. Each must appear at most once, spelled
// exactly so, and no other key may appear.
var kubeCallScopeKeys = []string{"cluster", "namespace", "method", "path", "query", "media_type", "body", "call_sha256", "hold_seconds"}

// kubeCallKeyPresent reports whether scope is an object with a top-level key that folds to cluster,
// read from the keys alone, as mcpCallKeyPresent reads its own: that key picks the strict kubectl path.
func kubeCallKeyPresent(scope json.RawMessage) bool {
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
		if strings.EqualFold(k, "cluster") {
			return true
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
	}
	return false
}

// kubeCallScopeKeysExact reports whether scope's top-level keys are each one of kubeCallScopeKeys,
// spelled exactly so and named at most once.
func kubeCallScopeKeysExact(scope json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(scope))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		k, isKey := tok.(string)
		if err != nil || !isKey || seen[k] || !slices.Contains(kubeCallScopeKeys, k) {
			return false
		}
		seen[k] = true
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
	}
	return true
}

// refuseBadCallScope is the raise's check of a held call's scope: a kubectl write's when a key folds to
// cluster, else an MCP call's (refuseBadMCPCallScope, which passes any other scope through).
func (s *Server) refuseBadCallScope(w http.ResponseWriter, r *http.Request, kind types.ApprovalKind, scope json.RawMessage) (json.RawMessage, bool) {
	if kubeCallKeyPresent(scope) {
		return s.refuseBadKubeCallScope(w, r, kind, scope)
	}
	return s.refuseBadMCPCallScope(w, r, kind, scope)
}

// refuseBadKubeCallScope accepts a kubectl write's scope only on a tool_call raise, with exactly its own
// keys, that a person can read exactly (types.KubeCallScope.Validate: a namespaced write, canonical
// query and body, a JSON media type, a body of at most 32 KiB) and whose call_sha256 is the key the
// control plane recomputes from those fields. Anything else is a 400 with an auth.fail row: only a
// misbehaving proxy writes one. An accepted scope comes back re-marshalled from its typed form.
func (s *Server) refuseBadKubeCallScope(w http.ResponseWriter, r *http.Request, kind types.ApprovalKind, scope json.RawMessage) (json.RawMessage, bool) {
	var sc types.KubeCallScope
	dec := json.NewDecoder(bytes.NewReader(scope))
	dec.DisallowUnknownFields()
	if kind == types.ApprovalToolCall && len(scope) <= maxSidecarBody && kubeCallScopeKeysExact(scope) && dec.Decode(&sc) == nil &&
		sc.Validate() == nil {
		if key, err := sc.ComputeKey(); err == nil && key == sc.CallSHA256 {
			if out, err := json.Marshal(sc); err == nil {
				return out, false
			}
		}
	}
	s.auditAuthFailedAs(r, internalApprovalActor, reasonKubeCallScopeInvalid)
	writeErrorReason(w, http.StatusBadRequest, reasonKubeCallScopeInvalid, "requested_scope is not the hash of its own kubectl request")
	return nil, true
}

// kubeCallDecisionRule is decide's scope rule for a kubectl write row: the approval is spent by one
// request, so a bodyless decision or "once" stores "once" and any wider scope is refused. isKube reports
// whether the row is one; ok=false means the refusal is written.
func kubeCallDecisionRule(w http.ResponseWriter, ap types.ApprovalRequest, scope types.ApprovalScope) (types.ApprovalScope, bool, bool) {
	if !isKubeCallRow(ap) {
		return scope, false, true
	}
	if scope == "" || scope == types.ScopeOnce {
		return types.ScopeOnce, true, true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonDecisionScopeInvalidForKind,
		`A kubectl change approval approves that one request, once: send no decision_scope, or "once"`)
	return scope, true, false
}
