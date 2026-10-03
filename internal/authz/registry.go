// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"maps"
	"slices"
)

// Reason is a refusal's stable code: the `reason` of its authz.denied row and
// of its Decision. The set is the registry below and nothing else.
//
// Append-only once 0.8.0 is tagged (testdata/wire.golden): a SIEM rule is
// written against these strings, so from then on a removal or a rename is a
// major version.
type Reason string

const (
	ReasonAdminSurface                Reason = "admin_surface"
	ReasonSecurityAdminSurface        Reason = "security_admin_surface"
	ReasonNotOwner                    Reason = "not_owner"
	ReasonAttachTicketForeignRun      Reason = "attach_ticket_foreign_run"
	ReasonBYOIUser                    Reason = "byoi_user"
	ReasonCapabilityAgent             Reason = "capability_agent"
	ReasonCapabilityEgressHost        Reason = "capability_egress_host"
	ReasonCapabilityFeature           Reason = "capability_feature"
	ReasonCapabilityPolicy            Reason = "capability_policy"
	ReasonCapabilitySecret            Reason = "capability_secret"
	ReasonCapabilityWorkspace         Reason = "capability_workspace"
	ReasonCapabilityWorkspaceProvider Reason = "capability_workspace_provider"
	ReasonCapabilityModelProvider     Reason = "capability_model_provider"
	ReasonGovernanceProfile           Reason = "governance_profile"
	ReasonGrantPairingNotEligible     Reason = "grant_pairing_not_eligible"
	ReasonGroupsSnapshotStale         Reason = "groups_snapshot_stale"
	ReasonRunKept                     Reason = "run_kept"
	ReasonRunNotFound                 Reason = "run_not_found"
	ReasonRunTerminal                 Reason = "run_terminal"
	ReasonSecondHumanRequired         Reason = "second_human_required"
	ReasonRunQuota                    Reason = "run_quota"
	ReasonUserTypeUnknown             Reason = "user_type_unknown"
	ReasonUserViewTypeDeleted         Reason = "user_view_type_deleted"
	// ReasonModelProviderUnavailable: a run's model provider cannot credential
	// it — none by that name, off, not serving the agent, none chosen among
	// several, no usable credential of the caller's for it, or a policy grant
	// that would set a model credential beside it (#987).
	ReasonModelProviderUnavailable Reason = "model_provider_unavailable"
	// ReasonAdminView: an admin in the user view launched a run after the type
	// the view looks through was deleted. Not audited on its own — the cause
	// row is ReasonUserViewTypeDeleted, which the launch response answered.
	ReasonAdminView Reason = "admin_view"
	// ReasonDelegationScope: a portal's delegated token asked for a route
	// outside the delegation allow-list (#1142).
	ReasonDelegationScope Reason = "delegation_scope"
	// ReasonEventStreamCap: the caller already holds the most concurrent run
	// event streams one principal may (#1407). A limit, not a denial: not
	// audited, like ReasonRunQuota.
	ReasonEventStreamCap Reason = "event_stream_cap"
	// ReasonRunOwnerOnly: interactive entry (terminal, UI app, SSH, take-over)
	// needs the run's owner; a super admin is refused unless the run has no
	// personal owner (#1476). Not hidden: the admin can already see the run.
	ReasonRunOwnerOnly Reason = "run_owner_only"
	// ReasonMaskStateUnavailable: a door that relays or persists a run's output
	// (recording upload, live attach, SSH shell, exec relay, live output read)
	// cannot prove this run's masking corpus complete here, so it refuses with
	// 503 instead of passing bytes through unmasked (ha-l2.0).
	ReasonMaskStateUnavailable Reason = "mask_state_unavailable"
)

// Refusal is one reason's registry row.
type Refusal struct {
	Effect Effect
	// Audit: the refusal writes an authz.denied row. False only for a caller
	// who IS authorized and is at a limit — a row per quota hit would make a
	// busy user look like an attacker.
	Audit bool
	// Sentence is the reason's own body, used when the door supplies none;
	// set only where every door refusing with the reason says the same thing.
	Sentence string
}

// requiresAdminRole is byte-identical for both admin tiers on purpose: a
// refusal never maps which tier a route sits on.
const requiresAdminRole = "requires admin role"

var refusals = map[Reason]Refusal{
	ReasonAdminSurface:                {Effect: EffectDeny, Audit: true, Sentence: requiresAdminRole},
	ReasonSecurityAdminSurface:        {Effect: EffectDeny, Audit: true, Sentence: requiresAdminRole},
	ReasonNotOwner:                    {Effect: EffectHidden, Audit: true},
	ReasonAttachTicketForeignRun:      {Effect: EffectHidden, Audit: true},
	ReasonBYOIUser:                    {Effect: EffectDeny, Audit: true},
	ReasonCapabilityAgent:             {Effect: EffectDeny, Audit: true},
	ReasonCapabilityEgressHost:        {Effect: EffectDeny, Audit: true},
	ReasonCapabilityFeature:           {Effect: EffectDeny, Audit: true},
	ReasonCapabilityPolicy:            {Effect: EffectDeny, Audit: true},
	ReasonCapabilitySecret:            {Effect: EffectDeny, Audit: true},
	ReasonCapabilityWorkspace:         {Effect: EffectDeny, Audit: true},
	ReasonCapabilityWorkspaceProvider: {Effect: EffectDeny, Audit: true},
	ReasonCapabilityModelProvider:     {Effect: EffectDeny, Audit: true},
	ReasonGovernanceProfile:           {Effect: EffectDeny, Audit: true},
	ReasonGrantPairingNotEligible:     {Effect: EffectDeny, Audit: true},
	ReasonGroupsSnapshotStale:         {Effect: EffectDeny, Audit: true},
	ReasonRunKept:                     {Effect: EffectDeny, Audit: true},
	ReasonRunNotFound:                 {Effect: EffectDeny, Audit: true},
	ReasonRunOwnerOnly:                {Effect: EffectDeny, Audit: true, Sentence: "only the person who started this run can open it interactively"},
	ReasonRunTerminal:                 {Effect: EffectDeny, Audit: true},
	ReasonSecondHumanRequired:         {Effect: EffectDeny, Audit: true},
	ReasonRunQuota:                    {Effect: EffectUnprocessable},
	ReasonModelProviderUnavailable:    {Effect: EffectUnprocessable, Audit: true},
	ReasonUserTypeUnknown:             {Effect: EffectDeny, Audit: true},
	ReasonUserViewTypeDeleted:         {Effect: EffectDeny, Audit: true},
	ReasonAdminView:                   {Effect: EffectConflict},
	ReasonDelegationScope:             {Effect: EffectDeny, Audit: true},
	ReasonEventStreamCap:              {Effect: EffectUnprocessable},
	ReasonMaskStateUnavailable:        {Effect: EffectUnavailable, Audit: true},
}

// Lookup returns reason's registry row; false for a reason nobody registered,
// which no door may emit.
func Lookup(reason Reason) (Refusal, bool) {
	ref, ok := refusals[reason]
	return ref, ok
}

// Reasons is every registered reason, sorted.
func Reasons() []Reason { return slices.Sorted(maps.Keys(refusals)) }
