// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The run-context reasons (the policy tree, governance_context.go), in their own file to keep
// reasons.go under scripts/check-file-size.sh's 1000-line gate: the same closed set and the same
// convention as reasons.go, not a second vocabulary. TestReasonDocsMatchReasonsGo and
// TestNoAdHocAuthz read this file too.
const (
	reasonUnitNotAContext       = "unit_not_a_context"       // the unit a run asked to run in is not one of the caller's units (an unknown unit answers the same)
	reasonWorkspaceUnitNotYours = "workspace_unit_not_yours" // a workspace's owning unit is not on the path of any of the caller's units
	reasonComponentUnitPinned   = "component_unit_pinned"    // an organisation component's unit pin does not cover the run's context
	reasonUnitMembershipGone    = "unit_membership_gone"     // a revive: the run's owner no longer holds a membership placing them in the run's unit
	// reasonGovernanceUnitDefaultReadonly: a write to the Default unit, whose walls WARDYN_DEFAULT_POLICY sets.
	reasonGovernanceUnitDefaultReadonly = "governance_unit_default_readonly"
	// reasonGovernanceMembershipDefaultUnit: a membership added to, changed into or moved into the Default unit.
	reasonGovernanceMembershipDefaultUnit = "governance_membership_default_unit"
	// reasonGovernanceUnitNotUnder: a prune write naming ?under= whose unit is no longer at or below that unit (409).
	reasonGovernanceUnitNotUnder = "governance_unit_not_under"
	// reasonUnitKnobLocked: a unit write changes a knob the boundary or a unit above locks, or a unit or
	// boundary write locks a knob a unit below states with another value (governance_locks.go).
	reasonUnitKnobLocked = "unit_knob_locked"
	// reasonGovernanceUnitAdminInvalid: a unit-admin grant names a subject that is not a person or a
	// group, or one already an admin of the unit; reasonGovernanceUnitAdminNotFound: no such grant.
	reasonGovernanceUnitAdminInvalid  = "governance_unit_admin_invalid"
	reasonGovernanceUnitAdminNotFound = "governance_unit_admin_not_found"
	// reasonInvalidDeciders: a unit's deciders rules break the model (types.ValidateDeciderRules); the message names the rule.
	reasonInvalidDeciders = "invalid_deciders"
	// reasonDeciderRuleWiderThanParent: a unit's deciders rule names a decider, or a wider scope, second-person or duration setting, than the rules above it allow (422); the message names the rule and the field.
	reasonDeciderRuleWiderThanParent = "decider_rule_wider_than_parent"
	// reasonScheduledRunsDenied: start_at on a launch whose governance profile turns off scheduled runs
	// (run_schedule_door.go); the deployment switch stays reasonScheduledRunsOff.
	reasonScheduledRunsDenied = "scheduled_runs_denied"
	// reasonGovernanceChangeNoteProposerOnly: someone other than the proposer wrote a change's note;
	// reasonGovernanceChangeNoteInvalid: the note's reason or ticket is out of bounds or malformed.
	reasonGovernanceChangeNoteProposerOnly = "governance_change_note_proposer_only"
	reasonGovernanceChangeNoteInvalid      = "governance_change_note_invalid"
	// reasonComponentDenied and reasonModelProviderDenied: a run that attaches a component, or would use
	// a model provider, its governance profile's deny_components or deny_model_providers lists
	// (run_deny_lists.go); a hard wall, operators included.
	reasonComponentDenied     = "component_denied"
	reasonModelProviderDenied = "model_provider_denied"
	// reasonModelUsageWindowInvalid: a unit usage read whose since or until does not parse, whose window is not
	// forward, or that spans more than 92 days (governance_unit_usage.go).
	reasonModelUsageWindowInvalid = "model_usage_window_invalid"
	// The source-control capability cap (vcs_capability_cap.go), set by the boundary in hard_limits or a
	// restriction: reasonADOCapabilitiesCapped refuses a dispatch whose cap leaves the run's Azure DevOps
	// lane nothing, or a policy list past it; reasonGitHubCapabilitiesCapped refuses a launch whose cap
	// leaves the GitHub CLI nothing; reasonCapabilityCapped refuses an Azure DevOps request or resolve for a
	// capability the cap leaves out.
	reasonADOCapabilitiesCapped    = "ado_capabilities_capped"
	reasonGitHubCapabilitiesCapped = "github_capabilities_capped"
	reasonCapabilityCapped         = "capability_capped"
)

// The data_out reasons (data_out.go).
const (
	reasonDataOutBlocked           = "data_out_blocked"            // the run's data_out rule keeps content in on this path
	reasonDataOutUnreadable        = "data_out_unreadable"         // the run's data_out rule could not be read; the path is refused
	reasonDataOutReviewUnavailable = "data_out_review_unavailable" // review needs a machine that can move files through Wardyn
	reasonComputerFolderDataOut    = "computer_folder_data_out"    // a computer folder may only go in under the run's data_out rule
)

// reasonDecidersUnreadable: the unit deciders rules, or the unit tree they are walked on, could not be read, so a
// decision that one of them may govern is refused rather than read as having no rule (503).
const reasonDecidersUnreadable = "deciders_unreadable"

// reasonInvalidDecidableBy: ?decidable_by= on GET /approvals is not "me" (400).
const reasonInvalidDecidableBy = "invalid_decidable_by"

// The held-request reasons (governance_requests.go, governance_elevation.go).
const (
	// reasonGovernanceRequestNeedsPerson: a request asked by a caller who is not a signed-in person (the admin
	// token, a service, a device, local mode) (403).
	reasonGovernanceRequestNeedsPerson = "governance_request_needs_person"
	// reasonGovernanceRequestKindInvalid: a request whose kind is not one this build takes (400).
	reasonGovernanceRequestKindInvalid = "governance_request_kind_invalid"
	// reasonElevationNotOffered: the unit offers the caller no elevation: no rule, not eligible, an unusable
	// group snapshot, or a unit that is Default or unknown, all one answer (400).
	reasonElevationNotOffered = "elevation_not_offered"
	// reasonElevationDurationInvalid: the duration does not parse or is under 15m; reasonElevationTooLong: it
	// is over the unit's max_elevation (400 each).
	reasonElevationDurationInvalid = "elevation_duration_invalid"
	reasonElevationTooLong         = "elevation_too_long"
	// reasonRequestReasonRequired and reasonRequestReasonTooLong: a request's reason is empty, or over 500
	// characters (400 each).
	reasonRequestReasonRequired = "request_reason_required"
	reasonRequestReasonTooLong  = "request_reason_too_long"
	// reasonElevationAlreadyMember: a membership already places the caller in the unit (409).
	reasonElevationAlreadyMember = "elevation_already_member"
	// reasonElevationTicketRequired and reasonElevationTicketInvalid: a rule on the unit's chain names a
	// ticket_pattern, and the ticket is missing, or does not match it (400 each).
	reasonElevationTicketRequired = "elevation_ticket_required"
	reasonElevationTicketInvalid  = "elevation_ticket_invalid"
	// reasonElevationCooldown: the caller's last elevation in the unit ended less than the rule's
	// renew_after ago; retry_after says when they may ask again (409).
	reasonElevationCooldown = "elevation_cooldown"
)

// The image request reasons (governance_image_request.go).
const (
	// reasonImageRequestNotPinned: the image is not <name>@sha256:<64 hex>, or its path has an empty, "." or
	// ".." segment (400).
	reasonImageRequestNotPinned = "image_request_not_pinned"
	// reasonImageRequestNotOffered: the caller's default unit offers no image requests: no image rule on it or
	// above it, Default, or a stale or truncated group snapshot, all one answer (400).
	reasonImageRequestNotOffered = "image_request_not_offered"
	// reasonImageAlreadyGranted: the caller already holds the image exactly (409).
	reasonImageAlreadyGranted = "image_already_granted"
)
