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
	ReasonBYOIUser                       Reason = "byoi_user"
	ReasonCapabilityAgent                Reason = "capability_agent"
	ReasonCapabilityEgressHost           Reason = "capability_egress_host"
	ReasonCapabilityFeature              Reason = "capability_feature"
	ReasonCapabilityPolicy               Reason = "capability_policy"
	ReasonCapabilitySecret               Reason = "capability_secret"
	ReasonCapabilityWorkspace            Reason = "capability_workspace"
	ReasonCapabilityWorkspaceProvider    Reason = "capability_workspace_provider"
	ReasonCapabilityModelProvider        Reason = "capability_model_provider"
	ReasonCapabilityComponent            Reason = "capability_component"
	ReasonGovernanceProfile              Reason = "governance_profile"
	ReasonPlacementComponentSelfDefined  Reason = "placement_component_self_defined"
	ReasonGovernanceOverlayUnsatisfiable Reason = "governance_overlay_unsatisfiable"
	ReasonGrantPairingNotEligible        Reason = "grant_pairing_not_eligible"
	ReasonGroupsSnapshotStale            Reason = "groups_snapshot_stale"
	ReasonRunKept                        Reason = "run_kept"
	ReasonRunNotFound                    Reason = "run_not_found"
	ReasonRunTerminal                    Reason = "run_terminal"
	ReasonSecondHumanRequired            Reason = "second_human_required"
	ReasonRunQuota                       Reason = "run_quota"
	ReasonUserTypeUnknown                Reason = "user_type_unknown"
	ReasonUserViewTypeDeleted            Reason = "user_view_type_deleted"
	// ReasonModelProviderUnavailable: a run's model provider cannot credential
	// it — none by that name, off, not serving the agent, none chosen among
	// several, no usable credential of the caller's for it, or a policy grant
	// that would set a model credential beside it (#987).
	ReasonModelProviderUnavailable Reason = "model_provider_unavailable"
	// ReasonAdminView: an admin in the user view launched a run after the type
	// the view looks through was deleted. Not audited on its own — the cause
	// row is ReasonUserViewTypeDeleted, which the launch response answered.
	ReasonAdminView Reason = "admin_view"
	// ReasonUserViewPreview: with WARDYN_GOVERN_ADMIN_RUNS on, an admin whose
	// User view looks through a type other than their own sent a write; the
	// view is a read-only preview. 409, audited with the viewed and stamped types.
	ReasonUserViewPreview Reason = "user_view_preview"
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
	// ReasonRoleStampStale: an API token's role and group stamp is older than
	// WARDYN_ROLE_STAMP_TTL. Its owner signs in again to re-stamp it.
	ReasonRoleStampStale Reason = "role_stamp_stale"
	// ReasonRecordingGoverned: Record Mode is refused for an admin whose runs are
	// governed (WARDYN_GOVERN_ADMIN_RUNS) unless the deployment exempts recording.
	ReasonRecordingGoverned Reason = "recording_governed"
	// ReasonAuditExportPartitionFilter: a partition export (GET /audit/export?partition=) was asked
	// to narrow the partition with another filter, so the digest in its footer would not cover the
	// whole partition. Input shape, not a denial: not audited.
	ReasonAuditExportPartitionFilter Reason = "audit_export_partition_filter"
	// ReasonKeyDomainUnknown: a key-domain assignment named a domain the
	// deployment's key domains file does not declare.
	ReasonKeyDomainUnknown Reason = "key_domain_unknown"
	// ReasonKeyDomainAmbiguous: a group assignment would leave people whose
	// groups are assigned to different domains, with no user assignment of
	// their own, or while someone whose last sign-in lost groups has none, so
	// their next principal key would be refused.
	ReasonKeyDomainAmbiguous Reason = "key_domain_ambiguous_membership"
	// The five refusals of POST /audit/retention/drop, one per rule audit_retention_drop (migration
	// 0123) enforces. State conflicts of an authorized security operator, audited so an attempt that
	// was refused is on the record.
	ReasonAuditRetentionNotOldest      Reason = "audit_retention_not_oldest"
	ReasonAuditRetentionNotClosed      Reason = "audit_retention_not_closed"
	ReasonAuditRetentionInsideWindow   Reason = "audit_retention_inside_window"
	ReasonAuditRetentionLiveRun        Reason = "audit_retention_live_run"
	ReasonAuditRetentionDigestMismatch Reason = "audit_retention_digest_mismatch"
	// ReasonComponentAutonomy: the organisation's autonomy cap on runs that
	// carry a self-defined component (site config components.autonomy_cap)
	// alone bound the run's level, and the request asks for more than it
	// permits. A tie with a governance profile's rubric refuses as
	// ReasonGovernanceProfile instead.
	ReasonComponentAutonomy Reason = "component_autonomy"
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
	ReasonBYOIUser:                       {Effect: EffectDeny, Audit: true},
	ReasonCapabilityAgent:                {Effect: EffectDeny, Audit: true},
	ReasonCapabilityEgressHost:           {Effect: EffectDeny, Audit: true},
	ReasonCapabilityFeature:              {Effect: EffectDeny, Audit: true},
	ReasonCapabilityPolicy:               {Effect: EffectDeny, Audit: true},
	ReasonCapabilitySecret:               {Effect: EffectDeny, Audit: true},
	ReasonCapabilityWorkspace:            {Effect: EffectDeny, Audit: true},
	ReasonCapabilityWorkspaceProvider:    {Effect: EffectDeny, Audit: true},
	ReasonCapabilityModelProvider:        {Effect: EffectDeny, Audit: true},
	ReasonCapabilityComponent:            {Effect: EffectDeny, Audit: true},
	ReasonPlacementComponentSelfDefined:  {Effect: EffectDeny, Audit: true},
	ReasonGovernanceProfile:              {Effect: EffectDeny, Audit: true},
	ReasonGovernanceOverlayUnsatisfiable: {Effect: EffectDeny, Audit: true},
	ReasonGrantPairingNotEligible:        {Effect: EffectDeny, Audit: true},
	ReasonGroupsSnapshotStale:            {Effect: EffectDeny, Audit: true},
	ReasonRunKept:                        {Effect: EffectDeny, Audit: true},
	ReasonRunNotFound:                    {Effect: EffectDeny, Audit: true},
	ReasonRunOwnerOnly:                   {Effect: EffectDeny, Audit: true, Sentence: "only the person who started this run can open it interactively"},
	ReasonRecordingGoverned:              {Effect: EffectDeny, Audit: true},
	ReasonRunTerminal:                    {Effect: EffectDeny, Audit: true},
	ReasonSecondHumanRequired:            {Effect: EffectDeny, Audit: true},
	ReasonRunQuota:                       {Effect: EffectUnprocessable},
	ReasonModelProviderUnavailable:       {Effect: EffectUnprocessable, Audit: true},
	ReasonUserTypeUnknown:                {Effect: EffectDeny, Audit: true},
	ReasonUserViewTypeDeleted:            {Effect: EffectDeny, Audit: true},
	ReasonAdminView:                      {Effect: EffectConflict},
	ReasonUserViewPreview:                {Effect: EffectConflict, Audit: true},
	ReasonDelegationScope:                {Effect: EffectDeny, Audit: true},
	ReasonEventStreamCap:                 {Effect: EffectUnprocessable},
	ReasonMaskStateUnavailable:           {Effect: EffectUnavailable, Audit: true},
	ReasonRoleStampStale:                 {Effect: EffectUnauthenticated, Audit: true, Sentence: "this token's role is out of date: its owner must sign in again to refresh it"},
	ReasonAuditExportPartitionFilter:     {Effect: EffectBadRequest},
	ReasonKeyDomainUnknown:               {Effect: EffectUnprocessable, Audit: true},
	ReasonKeyDomainAmbiguous:             {Effect: EffectConflict, Audit: true},
	ReasonAuditRetentionNotOldest:        {Effect: EffectConflict, Audit: true},
	ReasonAuditRetentionNotClosed:        {Effect: EffectConflict, Audit: true},
	ReasonAuditRetentionInsideWindow:     {Effect: EffectConflict, Audit: true},
	ReasonAuditRetentionLiveRun:          {Effect: EffectConflict, Audit: true},
	ReasonAuditRetentionDigestMismatch:   {Effect: EffectConflict, Audit: true},
	ReasonComponentAutonomy:              {Effect: EffectDeny, Audit: true},
}

// Lookup returns reason's registry row; false for a reason nobody registered,
// which no door may emit.
func Lookup(reason Reason) (Refusal, bool) {
	ref, ok := refusals[reason]
	return ref, ok
}

// Reasons is every registered reason, sorted.
func Reasons() []Reason { return slices.Sorted(maps.Keys(refusals)) }
