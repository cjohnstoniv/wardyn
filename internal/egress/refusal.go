// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// Who refused: the three values of Refusal.By. "unknown" is a real answer, not a gap:
// the connection failed with nothing Wardyn can read as a refusal.
const (
	RefusedByWardyn  = "wardyn"  // the run proxy denied the request
	RefusedByOutside = "outside" // the proxy allowed it and a peer refused or reset it
	RefusedByUnknown = "unknown" // the proxy allowed it and it failed with no readable cause
)

// Refusal says whose refusal an egress.deny row records. It is closed tokens plus the
// policy layer, never request content: no URL, header, body or error text.
type Refusal struct {
	By     string `json:"by"`
	Reason string `json:"reason"`
	// Layer and LayerName name the policy that refused: the layer kind ("profile" or
	// "deployment") and the profile's name, the same two keys authz.denied rows carry.
	// They ride a policy-decided refusal only (PolicyDecided); a guard refusal such as
	// a private address names none, since the policy owner cannot change it. The
	// contact stays on the run's own policy object.
	Layer     string `json:"layer,omitempty"`
	LayerName string `json:"layer_name,omitempty"`
}

// wardynRefusalReasons maps a deny rule_source the policy or a built-in guard decided to
// its Refusal.Reason. Closed on purpose: a rule_source not listed here is a fault or a
// brokered door with its own reason, and claiming it as Wardyn's refusal sends the person to
// a policy owner who did not cause it (policy:evaluator-error is the standing example).
var wardynRefusalReasons = map[string]string{
	"policy:denied":       "policy_denied",
	"policy:default-deny": "default_deny",
	"policy:method":       "method",
	"policy:port":         "port",
	"policy:require-tls":  "require_tls",
	"policy:tool-deny":    "tool_deny",
	"approval:denied":     "approval_denied",
	"builtin:private-ip":  "private_address",
}

// PolicyDecided reports whether r is a refusal the run's policy made, as opposed to
// a built-in guard's: only a policy-decided refusal names a layer.
func PolicyDecided(r *Refusal) bool {
	switch r.Reason {
	case "policy_denied", "default_deny", "method", "port", "require_tls", "tool_deny", "approval_denied":
		return r.By == RefusedByWardyn
	}
	return false
}

// WardynRefusal is the Refusal for a deny the policy or a guard decided, or nil when
// ruleSource is not one (see wardynRefusalReasons). A desktop VM relay's refusal
// ("vmnet:<reason>") is Wardyn's too.
func WardynRefusal(ruleSource string) *Refusal {
	if reason, ok := wardynRefusalReasons[ruleSource]; ok {
		return &Refusal{By: RefusedByWardyn, Reason: reason}
	}
	if strings.HasPrefix(ruleSource, "vmnet:") {
		return &Refusal{By: RefusedByWardyn, Reason: "vmnet"}
	}
	return nil
}

// reasonRe is the shape of every Refusal.Reason token.
var reasonRe = regexp.MustCompile(`^[a-z_]{1,32}$`)

// valid is the shape check for a Refusal that arrived from a sidecar: the audit writers
// run it so a compromised sidecar cannot put free text, or a By the console would
// misread, on a row.
func (r *Refusal) valid() bool {
	switch r.By {
	case RefusedByWardyn, RefusedByOutside, RefusedByUnknown:
	default:
		return false
	}
	if !reasonRe.MatchString(r.Reason) {
		return false
	}
	if r.By != RefusedByWardyn && (r.Layer != "" || r.LayerName != "") {
		return false
	}
	if r.Layer != "" && r.Layer != "profile" && r.Layer != "deployment" {
		return false
	}
	return len(r.LayerName) <= 128 && utf8.ValidString(r.LayerName) &&
		strings.IndexFunc(r.LayerName, policyref.UnsafeRune) < 0
}

// AuditFields adds the refusal's keys to an egress audit row's data. A nil or malformed
// Refusal adds nothing, so every unclassified row stays as it was. Both audit writers
// (the live decision door and the runner spool replay) call it, so the two cannot drift.
func (r *Refusal) AuditFields(fields map[string]any) {
	if r == nil || !r.valid() {
		return
	}
	fields["by"] = r.By
	fields["refusal_reason"] = r.Reason
	if r.Layer != "" {
		fields["layer"] = r.Layer
	}
	if r.LayerName != "" {
		fields["layer_name"] = r.LayerName
	}
}
