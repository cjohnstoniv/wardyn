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

// isHostCallRow reports whether ap asks a person to decide one host rule hold: a host, method and path a
// host rule with effect hold names (types.HostCallScope). It is a tool_call known STRUCTURALLY by
// call_sha256 and host, with no grant id. Only the proxy writes one, and the raise checked its key
// (refuseBadHostCallScope). It is never an MCP call row (types.IsMCPCallScope excludes it), and
// approval.TallyKey's host arm restates it.
//
// Who decides one: as for a kubectl write, the run's owner or a security operator, once;
// WARDYN_EGRESS_SECOND_HUMAN or WARDYN_CAPABILITY_SECOND_HUMAN makes it four-eyes (secondHumanGate), a unit's egress_domain deciders rule governs it,
// and an approval rule never decides one.
func isHostCallRow(ap types.ApprovalRequest) bool {
	return ap.Kind == types.ApprovalToolCall && (ap.GrantID == nil || *ap.GrantID == uuid.Nil) &&
		types.IsHostCallScope(ap.RequestedScope)
}

// hostCallScopeKeys are the keys of types.HostCallScope. Each must appear at most once, spelled exactly
// so, and no other key may appear.
var hostCallScopeKeys = []string{"host", "method", "path", "call_sha256", "hold_seconds"}

// hostCallKeysPresent reports whether scope is an object with a top-level key that folds to host, one
// that folds to call_sha256 and one that folds to method or path, read from the keys alone, as
// kubeCallKeyPresent reads its own: they pick the strict host call path. A host alone is an egress_domain
// scope, and passes on; a host and a hash with neither method nor path is an MCP call's scope, whose own
// check refuses the stray host (refuseBadMCPCallScope).
func hostCallKeysPresent(scope json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(scope))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	var host, hash, call bool
	for dec.More() {
		tok, err := dec.Token()
		k, isKey := tok.(string)
		if err != nil || !isKey {
			return false
		}
		host = host || strings.EqualFold(k, "host")
		hash = hash || strings.EqualFold(k, "call_sha256")
		call = call || strings.EqualFold(k, "method") || strings.EqualFold(k, "path")
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
	}
	return host && hash && call
}

// hostCallScopeKeysExact reports whether scope's top-level keys are each one of hostCallScopeKeys,
// spelled exactly so and named at most once.
func hostCallScopeKeysExact(scope json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(scope))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		k, isKey := tok.(string)
		if err != nil || !isKey || seen[k] || !slices.Contains(hostCallScopeKeys, k) {
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

// refuseBadHostCallScope accepts a host rule hold's scope only on a tool_call raise, with exactly its own
// keys, written the way a host rule names them (types.HostCallScope.Validate) and whose call_sha256 is the
// key the control plane recomputes from those fields. Anything else is a 400 with an auth.fail row: only a
// misbehaving proxy writes one. An accepted scope comes back re-marshalled from its typed form.
func (s *Server) refuseBadHostCallScope(w http.ResponseWriter, r *http.Request, kind types.ApprovalKind, scope json.RawMessage) (json.RawMessage, bool) {
	var sc types.HostCallScope
	dec := json.NewDecoder(bytes.NewReader(scope))
	dec.DisallowUnknownFields()
	if kind == types.ApprovalToolCall && len(scope) <= maxSidecarBody && hostCallScopeKeysExact(scope) && dec.Decode(&sc) == nil &&
		sc.Validate() == nil && sc.ComputeKey() == sc.CallSHA256 {
		if out, err := json.Marshal(sc); err == nil {
			return out, false
		}
	}
	s.auditAuthFailedAs(r, internalApprovalActor, reasonHostCallScopeInvalid)
	writeErrorReason(w, http.StatusBadRequest, reasonHostCallScopeInvalid, "requested_scope is not the hash of its own host, method and path")
	return nil, true
}

// hostCallDecisionRule is decide's scope rule for a host rule hold. The proxy keeps the answer for the rest
// of the run (internal/egress/proxy/host_call_hold.go), so the row says so: a bodyless decision or "run"
// stores "run", and any other scope is refused. It runs before the deciders cap (capScopeToRule), so a
// unit rule whose max_scope is below run refuses the decision (decision_scope_above_rule). isHost reports
// whether the row is one; ok=false means the refusal is written.
func hostCallDecisionRule(w http.ResponseWriter, ap types.ApprovalRequest, scope types.ApprovalScope) (types.ApprovalScope, bool, bool) {
	if !isHostCallRow(ap) {
		return scope, false, true
	}
	if scope == "" || scope == types.ScopeRun {
		return types.ScopeRun, true, true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonDecisionScopeInvalidForKind,
		`A host request approval answers that host, method and path for the rest of the run: send no decision_scope, or "run"`)
	return scope, true, false
}
