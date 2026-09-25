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
	ReasonAdminSurface                   Reason = "admin_surface"
	ReasonSecurityAdminSurface           Reason = "security_admin_surface"
	ReasonNotOwner                       Reason = "not_owner"
	ReasonAttachTicketForeignRun         Reason = "attach_ticket_foreign_run"
	ReasonBYOIMember                     Reason = "byoi_member"
	ReasonCapabilityAgent                Reason = "capability_agent"
	ReasonCapabilityEgressHost           Reason = "capability_egress_host"
	ReasonCapabilityIntegration          Reason = "capability_integration"
	ReasonCapabilitySecret               Reason = "capability_secret"
	ReasonCapabilityWorkspace            Reason = "capability_workspace"
	ReasonCapabilityWorkspaceProvider    Reason = "capability_workspace_provider"
	ReasonGovernanceProfile              Reason = "governance_profile"
	ReasonGrantPairingNotEligible        Reason = "grant_pairing_not_eligible"
	ReasonGroupsSnapshotStale            Reason = "groups_snapshot_stale"
	ReasonHarnessLoginMechanismPrincipal Reason = "harness_login_mechanism_principal"
	ReasonHarnessLoginNotPerUser         Reason = "harness_login_not_per_user"
	ReasonRunNotFound                    Reason = "run_not_found"
	ReasonRunTerminal                    Reason = "run_terminal"
	ReasonSecondHumanRequired            Reason = "second_human_required"
	ReasonRunQuota                       Reason = "run_quota"
	ReasonUserTypeUnknown                Reason = "user_type_unknown"
	ReasonUserViewTypeDeleted            Reason = "user_view_type_deleted"
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
	ReasonAdminSurface:                   {Effect: EffectDeny, Audit: true, Sentence: requiresAdminRole},
	ReasonSecurityAdminSurface:           {Effect: EffectDeny, Audit: true, Sentence: requiresAdminRole},
	ReasonNotOwner:                       {Effect: EffectHidden, Audit: true},
	ReasonAttachTicketForeignRun:         {Effect: EffectHidden, Audit: true},
	ReasonBYOIMember:                     {Effect: EffectDeny, Audit: true},
	ReasonCapabilityAgent:                {Effect: EffectDeny, Audit: true},
	ReasonCapabilityEgressHost:           {Effect: EffectDeny, Audit: true},
	ReasonCapabilityIntegration:          {Effect: EffectDeny, Audit: true},
	ReasonCapabilitySecret:               {Effect: EffectDeny, Audit: true},
	ReasonCapabilityWorkspace:            {Effect: EffectDeny, Audit: true},
	ReasonCapabilityWorkspaceProvider:    {Effect: EffectDeny, Audit: true},
	ReasonGovernanceProfile:              {Effect: EffectDeny, Audit: true},
	ReasonGrantPairingNotEligible:        {Effect: EffectDeny, Audit: true},
	ReasonGroupsSnapshotStale:            {Effect: EffectDeny, Audit: true},
	ReasonHarnessLoginMechanismPrincipal: {Effect: EffectUnprocessable, Audit: true},
	ReasonHarnessLoginNotPerUser:         {Effect: EffectDeny, Audit: true},
	ReasonRunNotFound:                    {Effect: EffectDeny, Audit: true},
	ReasonRunTerminal:                    {Effect: EffectDeny, Audit: true},
	ReasonSecondHumanRequired:            {Effect: EffectDeny, Audit: true},
	ReasonRunQuota:                       {Effect: EffectUnprocessable},
	ReasonUserTypeUnknown:                {Effect: EffectDeny, Audit: true},
	ReasonUserViewTypeDeleted:            {Effect: EffectDeny, Audit: true},
}

// Lookup returns reason's registry row; false for a reason nobody registered,
// which no door may emit.
func Lookup(reason Reason) (Refusal, bool) {
	ref, ok := refusals[reason]
	return ref, ok
}

// Reasons is every registered reason, sorted.
func Reasons() []Reason { return slices.Sorted(maps.Keys(refusals)) }
