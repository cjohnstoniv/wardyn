// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The machine-readable refusal reasons the credential-injection lanes (Azure
// DevOps, AWS SSO, Bedrock bearer) send on the wire alongside their human
// sentence — internal/api's half of client.APIError.Reason (#204, #656).
// docs/sdk.md documents this set. By convention a lane's refusal picks one of
// these rather than an ad-hoc string, and a lane that shares a SHAPE with
// another — a hold gone terminal, an approval store that could not be read —
// shares its reason too, instead of inventing a lane-local synonym. Nothing
// enforces the convention yet: the fail() closures take a plain string, and
// the Azure DevOps redemption passes ADOEntraFailure's own values
// (ado_entra_store.go) through unchanged. #656's guard-test acceptance item
// is where that check lands.
//
// Reason coverage is being swept lane by lane (#656); this set grows as
// each lane converts. A reason only one lane currently sends still lives
// here, not beside its lane, so the next lane converted picks from the same
// vocabulary rather than starting a second one.
const (
	// Shared by every lane's resolve arm: the dispatch-time-snapshot family
	// of refusals (I1-I3 in the Azure DevOps and AWS SSO doc comments).
	reasonMissingScopeSnapshot = "missing_scope_snapshot" // the grant names no dispatch-time snapshot
	reasonOwnerNotCaller       = "owner_not_caller"       // the snapshot's owner is not the run token's own subject
	reasonRosterUnreadable     = "roster_unreadable"      // the site configuration could not be read
	reasonScopeChanged         = "scope_changed"          // the live roster has drifted from the dispatch-time snapshot
	reasonRunUnreadable        = "run_unreadable"         // the run row itself could not be read
	reasonStoreError           = "store_error"            // the credential store read failed

	// Azure DevOps resolve only.
	reasonHostNotOrganisation = "host_not_organisation" // the requested host is outside the snapshot's organisation
	reasonTokenMode           = "token_mode"            // dispatch chose the bearer-key lane, not per-user Entra
	reasonSigninUnconfigured  = "signin_unconfigured"   // no Entra app registration for this organisation
	reasonSigninUnreadable    = "signin_unreadable"     // the Entra roster row could not be read

	// AWS SSO resolve only.
	reasonSSOHostNotPortal = "sso_host_not_portal" // the requested host is outside the credential's own SSO portal

	// Bedrock bearer resolve only.
	reasonPerUserBearerAbsent = "per_user_bearer_absent" // the roster names a per-user bearer this owner has none of
	reasonBearerAbsent        = "bearer_absent"          // no bedrock-api-key secret is in the store

	// The Azure DevOps capability-escalation chain and the sign-in/consent
	// HOLD chain — shared with AWS SSO's re-auth hold below, because both
	// are the same shape: an approval-backed hold that can go terminal, hit
	// its per-run cap, or fail to even raise.
	reasonCapabilityNotGrantable   = "capability_not_grantable"
	reasonApprovalsUnreadable      = "approvals_unreadable"
	reasonCapabilityAboveCeiling   = "capability_above_ceiling"
	reasonApprovalMismatch         = "approval_mismatch"
	reasonCapabilityDenied         = "capability_denied"
	reasonCapabilityClosed         = "capability_closed"
	reasonCapabilityAlwaysDeny     = "capability_always_deny"
	reasonCapabilityHoldsExhausted = "capability_holds_exhausted"
	reasonRaiseFailed              = "raise_failed"
	reasonCapabilityReview         = "capability_review"
	reasonOnceUnspendable          = "once_unspendable"

	// The hold-chain terminal/exhausted pair, shared by the Azure DevOps
	// sign-in hold and the AWS SSO re-auth hold — see reasonRaiseFailed and
	// reasonApprovalsUnreadable above for the other two members of this
	// shape.
	reasonSigninClosed         = "signin_closed"
	reasonSigninHoldsExhausted = "signin_holds_exhausted"

	// POST /runs with a launch preset (presets.go, #1143).
	reasonPresetUnknown       = "preset_unknown"                // no such preset, or not open to the caller's user type
	reasonPresetField         = "preset_field_not_per_launch"   // a field other than title/task set beside preset
	reasonPresetVersionMoved  = "preset_version_changed"        // the pinned preset_version is not the current one
	reasonPresetVersionNoName = "preset_version_without_preset" // preset_version with no preset

	// #656 slice 1 — GET/POST /approvals and its decide/push-content arms.
	// Shared across kinds because the shape ("the named approval row could
	// not be loaded at all") is the same regardless of which decide arm asked.
	reasonApprovalNotFound             = "approval_not_found"              // the approval row does not exist, or the caller may not see it
	reasonInvalidApprovalState         = "invalid_approval_state"          // ?state= is not one of the closed set
	reasonInvalidRunIDParam            = "invalid_run_id"                  // ?run_id= does not parse as a UUID
	reasonListingUnscopedBackend       = "listing_unscoped_backend"        // the store backend cannot scope this listing to the caller's own runs (approvals AND runs listings — same missing capability)
	reasonApprovalRunEnded             = "approval_run_ended"              // the run ended before the approval was decided; it was auto-cancelled
	reasonCredentialReauthNotDecidable = "credential_reauth_not_decidable" // a credential-reauth approval is resolved by signing in again, not Approve/Deny
	reasonDecisionScopeInvalidForKind  = "decision_scope_invalid_for_kind" // decision_scope was sent on an approval kind that does not accept one
	reasonApprovalAlreadyDecided       = "approval_already_decided"        // the approval was already approved or denied
	reasonInvalidRequestBody           = "invalid_request_body"            // the JSON body did not decode
	reasonInvalidViewParam             = "invalid_view_param"              // ?view= is not one of "", "user", "admin" (approvals AND runs listings)
	reasonEgressSecondHumanLocalMode   = "egress_second_human_local_mode"  // WARDYN_EGRESS_SECOND_HUMAN cannot be enforced with nobody authenticated (local mode)

	// The `decision_scope=always` persistence path (approvals.go): a
	// permanent approved-egress entry has its own small validation chain,
	// each arm a distinct reason so a client can tell WHICH condition failed.
	reasonInvalidDecisionScope           = "invalid_decision_scope"            // decision_scope is not one of once|run|until|always
	reasonDecisionScopeUntilNeedsExpiry  = "decision_scope_until_needs_expiry" // scope=until with no decision_expires_at
	reasonDecisionExpiryInPast           = "decision_expiry_in_past"           // decision_expires_at is not in the future
	reasonDecisionExpiryTooFar           = "decision_expiry_too_far"           // decision_expires_at is more than 30d out
	reasonDecisionExpiryWithoutUntil     = "decision_expiry_without_until"     // decision_expires_at set without scope=until
	reasonDecisionScopeAlwaysUnavailable = "decision_scope_always_unavailable" // this backend has no store to persist an always-entry to
	reasonDecisionAlwaysNoWorkspaceLink  = "decision_always_no_workspace_link" // the run this approval is on names no workspace to persist against
	reasonDecisionAlwaysInvalidHost      = "decision_always_invalid_host"      // the approval's host is not a plain lowercase host
	reasonDecisionAlwaysWorkspaceGone    = "decision_always_workspace_gone"    // the run's recorded workspace no longer exists
	reasonDecisionAlwaysHostBuiltin      = "decision_always_host_builtin"      // the host is already routed/wired in by construction; an always-entry is never consulted
	reasonDecisionAlwaysHostDenied       = "decision_always_host_denied"       // the host is on this deployment's permanent-egress reject list

	// Push-content review (approvals_push.go).
	reasonInvalidPushScope             = "invalid_push_scope"               // requested_scope failed PushContentScope.Validate or set a server-only field
	reasonInvalidPushPathList          = "invalid_push_path_list"           // path_list does not verify against the requested scope
	reasonRunStoreUnavailable          = "run_store_unavailable"            // this backend has no run store configured
	reasonPushContentUnattended        = "push_content_unattended"          // the run is unattended, so a push needing review is refused rather than held
	reasonPushPathListStoreUnavailable = "push_path_list_store_unavailable" // this backend has no push-path-list store configured
	reasonPushPathListCountUnavailable = "push_path_list_count_unavailable" // the per-run path-list count could not be read
	reasonPushPathListCapReached       = "push_path_list_cap_reached"       // the run already holds the maximum number of pending push path lists
	reasonPushNotHeld                  = "push_not_held"                    // the named approval is not a held push_content approval
	reasonPushPathListsRequirePostgres = "push_path_lists_require_postgres" // push path lists need the Postgres store backend

	// #656 slice 1 — POST /runs and its field/policy validation.
	reasonWorkspaceSeedFailed                  = "workspace_seed_failed"                    // workspace_id could not be resolved into a run spec
	reasonInvalidImageBuildRequest             = "invalid_image_build_request"              // the request's image/devcontainer build fields are inconsistent
	reasonWorkspaceSourcesInvalid              = "workspace_sources_invalid"                // the resolved workspace's sources fail structural validation
	reasonWorkspaceSourcesUnauthorized         = "workspace_sources_unauthorized"           // the caller may not launch against one of the workspace's sources
	reasonRunnerCapabilitiesUnavailable        = "runner_capabilities_unavailable"          // the runner's advertised capabilities could not be read
	reasonConfinementClassConflict             = "confinement_class_conflict"               // the requested confinement_class conflicts with the resolved policy
	reasonConfinementClassUnsupported          = "confinement_class_unsupported"            // the runner does not advertise the enforced confinement class
	reasonRunGrantsRequireSPIRE                = "run_grants_require_spire"                 // the policy names a grant (e.g. cloud_sts) the embedded identity provider cannot issue
	reasonRunFieldTooLong                      = "run_field_too_long"                       // task/agent exceeds its max length
	reasonRunFieldControlChar                  = "run_field_control_char"                   // task/agent contains a disallowed control character
	reasonAgentRequired                        = "agent_required"                           // no agent, and no image/workspace to run one in instead
	reasonAgentNotEnabled                      = "agent_not_enabled"                        // the named agent is not enabled in this deployment's site config
	reasonRunTaskReserved                      = "run_task_reserved"                        // task names a server-reserved value (e.g. a probe/verify sentinel)
	reasonConfinementClassUnknown              = "confinement_class_unknown"                // confinement_class is not CC1/CC2/CC3
	reasonTaskModeUnknown                      = "task_mode_unknown"                        // task_mode is not "harness" or "exec"
	reasonInteractiveStartUnknown              = "interactive_start_unknown"                // interactive_start is not "shell" or "agent"
	reasonToolApprovalsUnknown                 = "tool_approvals_unknown"                   // tool_approvals is not "auto" or "hold"
	reasonToolApprovalsHoldUnsupportedAgent    = "tool_approvals_hold_unsupported_agent"    // tool_approvals=hold on an agent with no external tool-approval contract (codex-cli)
	reasonToolApprovalsHoldInteractiveConflict = "tool_approvals_hold_interactive_conflict" // tool_approvals=hold on an interactive run, whose tool use is already supervised in the attach pane
	reasonIntegrationNotAIProvider             = "integration_not_ai_provider"              // integration_id does not name an AI-provider integration
	reasonRunKillAlreadyTerminal               = "run_kill_already_terminal"                // the run is already in a terminal state other than killed
	reasonRunKillStateChanged                  = "run_kill_state_changed"                   // the run moved to another state between the read and the write

	// GET /runs list filters (runs_list_filter.go) and GET /runs/policy-history
	// paging (runs_policy.go) — one reason per rejected query parameter.
	reasonInvalidOwnerParam         = "invalid_owner_param"          // ?owner= is not "", "me", or "all"
	reasonInvalidStatusParam        = "invalid_status_param"         // ?status= names a value outside the closed set
	reasonStatusNeedsExclusive      = "status_needs_exclusive"       // status=needs was combined with another status value
	reasonStatusNeedsRequiresView   = "status_needs_requires_view"   // status=needs requires view=user or view=admin
	reasonInvalidEndedWithinParam   = "invalid_ended_within_param"   // ?ended_within= is not one of the closed durations
	reasonInvalidIncludeKilledParam = "invalid_include_killed_param" // ?include_killed= is not "", "0", or "1"
	reasonRunsSearchQueryTooLong    = "runs_search_query_too_long"   // ?q= exceeds the maximum search length
	reasonInvalidLimitParam         = "invalid_limit_param"          // ?limit= does not parse as a non-negative integer
	reasonInvalidOffsetParam        = "invalid_offset_param"         // ?offset= does not parse as a non-negative integer

	// #656 slice 1 — workspaces (workspaces.go, workspace_admission.go,
	// workspace_envcode.go, workspace_providers.go).
	reasonWorkspaceRepoNotAdmitted    = "workspace_repo_not_admitted"     // the repository is not on this deployment's admitted list
	reasonWorkspaceEnvcodeNoLocalDir  = "workspace_envcode_no_local_dir"  // the workspace has no local_dir source to emit env-as-code from
	reasonWorkspaceEnvcodeNoProfile   = "workspace_envcode_no_profile"    // the workspace has no scanned profile to emit from
	reasonWorkspaceProvidersInvalid   = "workspace_providers_invalid"     // the submitted provider block fails validation
	reasonWorkspaceProvidersStale     = "workspace_providers_stale"       // If-Match does not match the current providers ETag; reload and retry
	reasonWorkspaceRequestInvalid     = "workspace_request_invalid"       // the create/update request body fails workspace-request validation
	reasonWorkspaceSSHSourcesNotReady = "workspace_ssh_sources_not_ready" // an SSH-remote source names a secret that has not been stored yet
	reasonWorkspaceSourcesNotAllowed  = "workspace_sources_not_allowed"   // the caller's own local_dir sources fail the member-safe mount gate
	reasonWorkspaceDeleteActiveRun    = "workspace_delete_active_run"     // the workspace is in use by a still-active run
)
