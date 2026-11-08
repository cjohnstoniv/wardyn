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
)

// reasonDecidersUnreadable: the unit deciders rules, or the unit tree they are walked on, could not be read, so a
// decision that one of them may govern is refused rather than read as having no rule (503).
const reasonDecidersUnreadable = "deciders_unreadable"

// reasonInvalidDecidableBy: ?decidable_by= on GET /approvals is not "me" (400).
const reasonInvalidDecidableBy = "invalid_decidable_by"
