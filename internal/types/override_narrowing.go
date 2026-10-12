// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"slices"
	"strings"
)

// OverrideKind names one thing a person may change on one component for one run
// (OD-1: New Run card edits are per-component overrides on the request).
type OverrideKind string

const (
	OverrideAgentHost         OverrideKind = "agent_host"          // a destination on the agent component
	OverrideAgentHostWildcard OverrideKind = "agent_host_wildcard" // the same, spelled with a wildcard
	OverrideAgentSecret       OverrideKind = "agent_secret"        // a stored secret injected as a header on the agent component
	OverrideToolRuleRestrict  OverrideKind = "tool_rule_restrict"  // a tool rule whose effect is hold or deny
	OverrideToolRuleAllow     OverrideKind = "tool_rule_allow"     // a tool rule whose effect is allow
	OverrideADOCapability     OverrideKind = "ado_capability"      // the run's Azure DevOps capability set
	OverrideGitPATScope       OverrideKind = "git_pat_scope"       // repos, access or api of a git_pat grant
	OverridePushDeny          OverrideKind = "push_rule_deny"      // a push deny path of one provider and organisation
	OverridePushReview        OverrideKind = "push_rule_review"    // a push require-review path of one provider and organisation
	OverrideRunHostAllow      OverrideKind = "run_host_allow"      // a host the person adds to the run's allowed list, on top of the base policy
	OverrideRunHostWildcard   OverrideKind = "run_host_wildcard"   // the same, spelled with a wildcard
	OverrideRunHostDeny       OverrideKind = "run_host_deny"       // a host the person adds to the run's denied list, on top of the base policy
	OverrideFirstUseApproval  OverrideKind = "first_use_approval"  // how an unlisted destination is handled, chosen on top of the base policy
	OverrideAllowAllEgress    OverrideKind = "allow_all_egress"    // request all other egress, on top of the base policy
)

// OverrideOp is what the person does to the entry.
type OverrideOp string

const (
	OverrideAdd    OverrideOp = "add"
	OverrideRemove OverrideOp = "remove"
	// OverrideNarrow reduces a value the source resolved: a smaller capability
	// set, a subset of repositories, write to read.
	OverrideNarrow OverrideOp = "narrow"
	// OverrideSet chooses a run-wide scalar the source also holds.
	OverrideSet OverrideOp = "set"
)

// OverrideDirection says whether the operation gives the run less reach or more.
type OverrideDirection string

const (
	OverrideTighten OverrideDirection = "tighten"
	OverrideWiden   OverrideDirection = "widen"
)

// OverrideRule is what a person may do with the operation.
type OverrideRule string

const (
	// OverrideAllowed: always, because it only takes reach away.
	OverrideAllowed OverrideRule = "allowed"
	// OverrideClamped: allowed to ask, and the ceiling and the member grant
	// pipeline may still drop it; the preview shows what they dropped.
	OverrideClamped OverrideRule = "ceiling_clamped"
	// OverrideRefused: never, from a run request.
	OverrideRefused OverrideRule = "refused"
	// OverrideWithinPolicy: allowed when no looser than the policy for that
	// entry, refused otherwise. The check needs the resolved policy, so the
	// server answers it after the policy fold, not from this row alone.
	OverrideWithinPolicy OverrideRule = "within_policy"
)

// OverrideNarrowing is one row of the narrowing table.
type OverrideNarrowing struct {
	Kind      OverrideKind      `json:"kind"`
	Op        OverrideOp        `json:"op"`
	Direction OverrideDirection `json:"direction"`
	Rule      OverrideRule      `json:"rule"`
}

// overrideNarrowing is the ONE place that decides, per override kind and
// operation, whether a person may add, narrow or remove it. The server reads it
// to refuse (internal/api runOverridesRefusal) and the console reads the same
// rows to disable a control (ui/src/app/lib/override-narrowing.json, written
// from this table by TestOverrideNarrowingGolden; a parity test pins both).
//
// Tightening is always allowed. Widening is either clamped (a host or a secret
// the person adds is still bounded by their ceiling), within the policy (a tool
// allow stands only where the policy already allows that tool) or refused (anything the
// source did not grant, and anything a rule removal would give back). Push and
// branch rules are monotone: a person may add a deny or a review path, never
// remove one (OD-1).
var overrideNarrowing = []OverrideNarrowing{
	{OverrideAgentHost, OverrideAdd, OverrideWiden, OverrideClamped},
	{OverrideAgentHost, OverrideRemove, OverrideTighten, OverrideAllowed},
	{OverrideAgentHostWildcard, OverrideAdd, OverrideWiden, OverrideRefused},
	{OverrideAgentSecret, OverrideAdd, OverrideWiden, OverrideClamped},
	{OverrideAgentSecret, OverrideRemove, OverrideTighten, OverrideAllowed},
	{OverrideToolRuleRestrict, OverrideAdd, OverrideTighten, OverrideAllowed},
	{OverrideToolRuleAllow, OverrideAdd, OverrideWiden, OverrideWithinPolicy},
	{OverrideADOCapability, OverrideNarrow, OverrideTighten, OverrideAllowed},
	{OverrideADOCapability, OverrideAdd, OverrideWiden, OverrideRefused},
	{OverrideGitPATScope, OverrideNarrow, OverrideTighten, OverrideAllowed},
	{OverrideGitPATScope, OverrideAdd, OverrideWiden, OverrideRefused},
	{OverridePushDeny, OverrideAdd, OverrideTighten, OverrideAllowed},
	{OverridePushDeny, OverrideRemove, OverrideWiden, OverrideRefused},
	{OverridePushReview, OverrideAdd, OverrideTighten, OverrideAllowed},
	{OverridePushReview, OverrideRemove, OverrideWiden, OverrideRefused},
	// Run-wide hosts on top of a base policy. The base's own hosts are read-only:
	// a person adds to either list and never edits or removes what the base says,
	// which is why there is no remove row (a removal is refused by name, not asked
	// of this table).
	{OverrideRunHostAllow, OverrideAdd, OverrideWiden, OverrideClamped},
	{OverrideRunHostWildcard, OverrideAdd, OverrideWiden, OverrideRefused},
	{OverrideRunHostDeny, OverrideAdd, OverrideTighten, OverrideAllowed},
	// Run-wide scalars. Both can reach further than the base, so both go through
	// the ceiling: a value past it is clamped back, not refused. A stricter value
	// is just as acceptable and the clamp leaves it alone.
	{OverrideFirstUseApproval, OverrideSet, OverrideWiden, OverrideClamped},
	{OverrideAllowAllEgress, OverrideSet, OverrideWiden, OverrideClamped},
}

// OverrideNarrowingTable returns the table, in its fixed order.
func OverrideNarrowingTable() []OverrideNarrowing { return slices.Clone(overrideNarrowing) }

// OverrideRuleFor looks one operation up; false for a pair the table has no row for.
func OverrideRuleFor(kind OverrideKind, op OverrideOp) (OverrideNarrowing, bool) {
	i := slices.IndexFunc(overrideNarrowing, func(n OverrideNarrowing) bool { return n.Kind == kind && n.Op == op })
	if i < 0 {
		return OverrideNarrowing{}, false
	}
	return overrideNarrowing[i], true
}

// IsWildcardHost reports whether an override host is spelled with a wildcard.
func IsWildcardHost(host string) bool { return strings.Contains(host, "*") }
