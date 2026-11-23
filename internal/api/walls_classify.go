// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// wallsClass is what one egress.* audit row means to the walls view. It is chosen from the row's action
// first and its rule_source second, so that a row no rule names is wallsUnclassified and never red.
type wallsClass string

const (
	// wallsControl is the run calling Wardyn's own control plane: no exit, no host.
	wallsControl wallsClass = "control"
	// wallsConfig is a start-up configuration row, not a request.
	wallsConfig wallsClass = "config"
	// wallsToolRule is a tool call answered by the run's own tool rules.
	wallsToolRule wallsClass = "tool_rule"
	// wallsAllowed is a request that was let through.
	wallsAllowed wallsClass = "allowed"
	// wallsRefused is a request Wardyn refused, or a hold that ended without a decision.
	wallsRefused wallsClass = "refused"
	// wallsHeld is a request waiting for a person.
	wallsHeld wallsClass = "held"
	// wallsFidelity is a row about the audit record itself: decisions that were not recorded.
	wallsFidelity wallsClass = "fidelity"
	// wallsFault is a failure that is not a refusal by Wardyn: the path broke or the destination refused.
	wallsFault wallsClass = "fault"
	// wallsUnclassified is a deny that no rule names.
	wallsUnclassified wallsClass = "unclassified"
)

// wallsVerdict is the class of one row, with two flags that are not classes. Guest marks a deny that a
// desktop VM's network relay made (a flow id, or a vmnet source). Late is a row recorded more than 45 s
// after it was decided: it is classified as usual and the caller keeps it out of live counters.
type wallsVerdict struct {
	Class       wallsClass
	Guest, Late bool
}

// controlPlaneHost is true when the row's host is Wardyn's own control plane (control and tool_rule rows).
func (v wallsVerdict) controlPlaneHost() bool {
	return v.Class == wallsControl || v.Class == wallsToolRule
}

// wallsClassifyControl are the sources of a run calling Wardyn's own control plane. They are control
// whatever the action.
var wallsClassifyControl = wallsClassifySet(
	"brokered:approvals", "brokered:mint", "brokered:recording", "brokered:scan-result",
	"brokered:sso-token", "brokered:mcp:list-filtered",
)

// wallsClassifyToolAllow are the allow sources whose host is the control plane.
var wallsClassifyToolAllow = wallsClassifySet("policy:tool-allow", "policy:tool-approve-each")

// wallsClassifyHoldRefused are the hold sources that end a hold with nothing to wait for.
var wallsClassifyHoldRefused = wallsClassifySet("approval:expired", "approval:refused", "approval:host-cap")

// wallsClassifyFidelityDeny are the deny sources that report lost decisions. The marker
// egress:dropped-decisions-<n> carries its count in the source, so it is matched by prefix.
var wallsClassifyFidelityDeny = wallsClassifySet("builtin:spool-lost", "builtin:spool-full")

const wallsClassifyDroppedPrefix = "egress:dropped-decisions-"

// wallsClassifyFault are the deny sources where the path broke or the evaluator could not decide, which is
// not a refusal by Wardyn.
var wallsClassifyFault = wallsClassifySet(
	"builtin:dial-failed", "builtin:tunnel-failed", "builtin:resolve-failed",
	"builtin:upstream-protocol-mismatch", "policy:evaluator-error", "credential:reauth-timeout",
)

// wallsClassifyToolDeny are the deny sources of a tool call refused by tool rules.
var wallsClassifyToolDeny = wallsClassifySet(
	"policy:tool-deny", "policy:tool-approve-each-unroutable", "policy:tool-approve-each-declined",
	"brokered:mcp:tool-deny",
)

// wallsClassifyRefusal are the deny sources that are a refusal by Wardyn, every one a guard or a rule.
var wallsClassifyRefusal = wallsClassifySet(
	// Policy.
	"policy:denied", "policy:default-deny", "policy:method", "policy:port", "policy:require-tls",
	"policy:no-network", "policy:ip-literal", "policy:host-rule", "policy:host-rule-needs-inspection",
	"policy:host-rule:approval-denied", "policy:host-rule:approval-closed",
	"policy:host-rule:approval-refused", "policy:host-rule:approval-busy",
	// Built-in guards.
	"builtin:private-ip", "builtin:tunnel-limit", "builtin:ip-spelling", "builtin:non-ascii-host",
	"builtin:signin-via-org", "builtin:gateway-vet-failed", "builtin:audit-record-too-large",
	// Approval and content scan.
	"approval:denied", "scan:blocked", "scan:reroute-keys", "scan:mitm",
	// Git.
	"brokered:git", "brokered:git:branch-ns", "brokered:git:branch-ns-encoding",
	"brokered:git:push-rules", "brokered:git:push-too-large", "brokered:git:push-uninspectable",
	"brokered:git:push-entry-underivable", "brokered:git:push-deny-all", "brokered:git:push-deny-all-forge",
	"brokered:git:push-held", "brokered:git:push-held-unattended", "brokered:git:push-capability",
	"brokered:git-pat", "brokered:git-pat:denied", "brokered:git-pat:repo", "brokered:git-pat:read-only",
	"brokered:git-pat:api:denied", "brokered:ado:denied", "brokered:ado-git:denied",
	// Doors.
	"brokered:github-api:refused", "brokered:azure-arm:refused", "brokered:kube-api:refused",
	"brokered:kube-api:approval-denied", "brokered:kube-api:approval-closed",
	"brokered:kube-api:approval-refused", "brokered:aws-sigv4:refused", "brokered:azure:denied",
	// Models and MCP.
	"brokered:local-model", "brokered:llm", "brokered:allowed-models",
	"brokered:mcp:unbound", "brokered:mcp:not-enabled", "brokered:mcp:encoding", "brokered:mcp:request",
	"brokered:mcp:credential", "brokered:mcp:redirect", "brokered:mcp:direct",
	"brokered:mcp:approval-denied", "brokered:mcp:approval-closed", "brokered:mcp:approval-refused",
	"brokered:mcp:approval-sent", "brokered:mcp:list-unreadable",
)

func wallsClassifySet(sources ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(sources))
	for _, s := range sources {
		m[s] = struct{}{}
	}
	return m
}

func wallsClassifyIn(set map[string]struct{}, source string) bool {
	_, ok := set[source]
	return ok
}

// wallsClassify classifies one decoded egress.* row. The action is tested first, then the source in the
// order the rules are listed, so an allow or a hold can never be classified as a refusal and a deny that
// matches no rule is unclassified rather than refused.
func wallsClassify(r store.DecisionRow) wallsVerdict {
	v := wallsVerdict{Late: r.Late()}
	src := r.RuleSource
	if wallsClassifyIn(wallsClassifyControl, src) {
		v.Class = wallsControl
		return v
	}
	switch r.Action {
	case "egress.allow":
		v.Class = wallsClassifyAllow(src)
	case "egress.hold", "egress.pending":
		v.Class = wallsClassifyHold(src)
	case "egress.deny":
		v.Guest = r.Flow != "" || strings.HasPrefix(src, "vmnet:")
		v.Class = wallsClassifyDeny(r, v.Guest)
	default:
		v.Class = wallsUnclassified
	}
	return v
}

func wallsClassifyAllow(src string) wallsClass {
	switch {
	case src == "builtin:upstream-proxy":
		return wallsConfig
	case wallsClassifyIn(wallsClassifyToolAllow, src):
		return wallsToolRule
	}
	return wallsAllowed
}

func wallsClassifyHold(src string) wallsClass {
	if wallsClassifyIn(wallsClassifyHoldRefused, src) {
		return wallsRefused
	}
	return wallsHeld
}

func wallsClassifyDeny(r store.DecisionRow, guest bool) wallsClass {
	src := r.RuleSource
	switch {
	case strings.HasPrefix(src, wallsClassifyDroppedPrefix) || wallsClassifyIn(wallsClassifyFidelityDeny, src):
		return wallsFidelity
	case wallsClassifyIn(wallsClassifyFault, src) || r.By == "outside" || r.By == "unknown":
		return wallsFault
	case wallsClassifyIn(wallsClassifyToolDeny, src):
		return wallsToolRule
	case guest, wallsClassifyIn(wallsClassifyRefusal, src):
		return wallsRefused
	}
	return wallsUnclassified
}
