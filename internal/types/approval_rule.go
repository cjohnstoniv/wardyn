// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// ApprovalRuleMode is an approval rule's own mode. WARDYN_APPROVAL_RULES=dry_run makes every rule act as
// dry_run whatever its mode says.
type ApprovalRuleMode string

const (
	ApprovalRuleDryRun  ApprovalRuleMode = "dry_run"
	ApprovalRuleEnforce ApprovalRuleMode = "enforce"
)

// ApprovalRule is a security admin's approval rule (migration 0210): on a service identity's run it approves a
// matching tool_call or egress_domain approval through the same decide path a person's decision takes. Version
// rises by one on every write, so an audit row can name the exact text it matched.
type ApprovalRule struct {
	ID      uuid.UUID        `json:"id"`
	Name    string           `json:"name"`
	Version int              `json:"version"`
	Mode    ApprovalRuleMode `json:"mode"`
	ApprovalRuleSpec
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ApprovalRuleSpec is the part of a rule the predicates and the approve list read (approval_rules.spec).
type ApprovalRuleSpec struct {
	Kinds   []ApprovalKind      `json:"kinds"`
	Match   ApprovalRuleMatch   `json:"match"`
	Approve ApprovalRuleApprove `json:"approve"`
}

// ApprovalRuleMatch is the run predicates: AND across fields, OR within one, at least one of Repos or Presets.
type ApprovalRuleMatch struct {
	Repos              []string             `json:"repos,omitempty"`
	Presets            []ApprovalRulePreset `json:"presets,omitempty"`
	CapabilitiesWithin []string             `json:"capabilities_within,omitempty"`
}

// ApprovalRulePreset names a preset at one version: an edit of the preset stops the match.
type ApprovalRulePreset struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// ApprovalRuleApprove is what a rule may approve: exact tool names, exact hosts, and the egress decision scope.
type ApprovalRuleApprove struct {
	Tools       []string      `json:"tools,omitempty"`
	Hosts       []string      `json:"hosts,omitempty"`
	EgressScope ApprovalScope `json:"egress_scope,omitempty"`
}

// ApprovalRuleWrite is a PUT body: the name is the path's, the mode defaults to dry_run.
type ApprovalRuleWrite struct {
	Name string           `json:"name,omitempty"`
	Mode ApprovalRuleMode `json:"mode,omitempty"`
	ApprovalRuleSpec
}

// ApprovalPolicyMatch is what an approval rule matched, frozen at match: it rides the approval.decide row as
// data.policy and every approval.policy.* row.
type ApprovalPolicyMatch struct {
	RuleID      uuid.UUID         `json:"rule_id"`
	RuleName    string            `json:"rule_name"`
	RuleVersion int               `json:"rule_version"`
	RuleDigest  string            `json:"rule_digest"`
	Matched     map[string]string `json:"matched"`
}
