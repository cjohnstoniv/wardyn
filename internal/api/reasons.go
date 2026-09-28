// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/authz"

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

	// #656 slice 2 — site-config, governance and the user-drive doors share this
	// one: the caller's group-membership snapshot is missing or was truncated at
	// sign-in, so a group-keyed governance profile cannot be resolved. Same cause,
	// three routes (PUT/POST governance, the drive resolver) — one reason.
	//
	// The value is authz's OWN registered reason (internal/authz/registry.go),
	// not a second copy of the string: TestNoAdHocAuthz refuses a raw literal
	// that already names a registered authz.Reason.
	reasonGroupsSnapshotStale = string(authz.ReasonGroupsSnapshotStale)

	// GET/PUT /site-config.
	reasonSiteConfigRequestInvalid          = "site_config_request_invalid"            // the PUT body did not decode
	reasonSiteConfigArtifactOverrideInvalid = "site_config_artifact_override_invalid"  // a legacy artifact-override field fails validation
	reasonSiteConfigIntegrationsViaOwnRoute = "site_config_integrations_via_own_route" // integrations were named inline instead of through their own endpoints
	reasonSiteConfigInvalid                 = "site_config_invalid"                    // the submitted config fails one of validateAgentProviders/validateSiteConfig/validateModelProviders/validateDefaultProviders/validateModelProviderImagePrereqs
	reasonSiteConfigStale                   = "site_config_stale"                      // If-Match does not match the current site-config ETag

	// POST /site-config/probe-proxy and PUT/DELETE .../egress-redirects.
	reasonSiteConfigProbeRequestInvalid = "site_config_probe_request_invalid" // the probe body did not decode
	reasonSiteConfigProbeURLInvalid     = "site_config_probe_url_invalid"     // the probe target is not a plain http(s) URL
	reasonEgressRedirectFromRequired    = "egress_redirect_from_required"     // an egress-redirect edit named no `from`
	reasonEgressRedirectNotFound        = "egress_redirect_not_found"         // no egress_redirects entry matches `from`

	// PUT/POST /governance/{profiles,assignments} and the preview routes.
	reasonGovernanceProfileRequestInvalid = "governance_profile_request_invalid" // the profile body did not decode
	reasonGovernanceCeilingInvalid        = "governance_ceiling_invalid"         // the ceiling names a grant outside the deployment's own EligibleGrants
	reasonGovernanceProfileNameConflict   = "governance_profile_name_conflict"   // a profile by that name already exists
	reasonGovernanceProfileInUse          = "governance_profile_in_use"          // the profile is still assigned; delete its assignments first
	reasonGovernanceAssignmentInvalid     = "governance_assignment_invalid"      // the assignment fails validation
	// reasonGovernancePreviewClaimsInvalid is shared by GET /governance/preview
	// and the user-drive naming preview (user_drives_preview.go): both feed the
	// same normalizeGovernancePreviewClaims validator over user_subjects/groups.
	reasonGovernancePreviewClaimsInvalid = "governance_preview_claims_invalid"

	// /api/v1/secrets.
	reasonSecretNameInvalid       = "secret_name_invalid"        // the name fails the secret-name format
	reasonSecretNameReserved      = "secret_name_reserved"       // the name is reserved for platform internals
	reasonSecretOwnerParamRefused = "secret_owner_param_refused" // ?owner= was sent on a write, which only the owning person may do
	reasonSecretBodyInvalid       = "secret_body_invalid"        // the body is not {"value":"<non-empty secret>"}
	reasonSecretValueTooShort     = "secret_value_too_short"     // the value is below the mask's minimum length
	reasonSecretCapReached        = "secret_cap_reached"         // the owner already holds the maximum number of secrets
	reasonSecretNotFound          = "secret_not_found"           // that owner holds no secret by that name

	// /api/v1/people.
	reasonPeopleStoreUnavailable  = "people_store_unavailable"  // pre-created people require the Postgres store backend
	reasonPersonPrincipalInvalid  = "person_principal_invalid"  // principal is not 1-255 printable characters
	reasonPersonPrincipalReserved = "person_principal_reserved" // principal is reserved for a non-person identity
	reasonPersonEmailInvalid      = "person_email_invalid"      // email fails validation
	reasonPersonCollision         = "person_collision"          // the principal or email collides with an existing person
	reasonPersonEmailTaken        = "person_email_taken"        // another subject is already known by this email
	// reasonPersonMintNoHuman / reasonAPITokenFromAPIToken are shared: minting a
	// token needs a signed-in human, and neither an API token nor a delegated
	// token may mint another — the SAME two shapes apitokens.go's own
	// self-service mint door refuses with byte-identical sentences.
	reasonPersonMintNoHuman    = "mint_no_human"
	reasonAPITokenFromAPIToken = "api_token_from_api_token"
	reasonPersonNotFound       = "person_not_found" // no person is recorded under this subject

	// /api/v1/tokens (self-service) and /api/v1/sessions/revoke,
	// /api/v1/delegation's authentication lookups.
	// reasonTokenLookupUnavailable is shared by the API-token and delegated-token
	// authentication middlewares: both answer the SAME shape (the revocation
	// store could not be read to authenticate the bearer) with a 503.
	reasonTokenLookupUnavailable     = "token_lookup_unavailable"
	reasonAPITokenNoHuman            = "api_token_no_human"             // an API token belongs to a signed-in human
	reasonAPITokenFromDelegatedToken = "api_token_from_delegated_token" // a delegated (portal) token cannot mint an API token
	reasonAPITokenMemberModeMint     = "api_token_member_mode_mint"     // a member-mode session cannot mint a token that would outlive the view
	reasonAPITokenNameInvalid        = "api_token_name_invalid"         // name exceeds the length cap or has a control character
	reasonAPITokenCapReached         = "api_token_cap_reached"          // the principal already holds the maximum number of live tokens
	reasonSessionsRevokeParamInvalid = "sessions_revoke_param_invalid"  // the body must set exactly one of sub/all
	reasonSSHKeyOwnerUnresolved      = "ssh_key_owner_unresolved"       // resolveSSHKeyOwner could not resolve principal to a unique directory entry

	// /api/v1/access (role-mapping admin).
	reasonSSONotConfigured             = "sso_not_configured"               // the route needs OIDC, and this deployment has none configured
	reasonAccessRoleMapValueInvalid    = "access_role_map_value_invalid"    // ?value= (or the body's value) does not canonicalize
	reasonAccessMappingTargetInvalid   = "access_mapping_target_invalid"    // the mapping key/value pair fails validation
	reasonAccessEmailMappingDisabled   = "access_email_mapping_disabled"    // email-keyed mappings are off on this install
	reasonAccessLockout                = "access_lockout"                   // the edit would lock every admin out
	reasonAccessUnknownUserType        = "access_unknown_user_type"         // the mapping names a user type this deployment does not have
	reasonAccessPreviewNoSessionClaims = "access_preview_no_session_claims" // use_session was set with no caller session to read claims from

	// /api/v1/permissions (capability grants + availability).
	reasonCapabilityGrantInvalid          = "capability_grant_invalid"           // the grant fails validation
	reasonCapabilityKindUnknown           = "capability_kind_unknown"            // the enforcement body names an unknown capability kind
	reasonCapabilityEnforcementStale      = "capability_enforcement_stale"       // If-Match does not match the current enforcement ETag
	reasonAvailabilityKindNotRestrictable = "availability_kind_not_restrictable" // this capability kind cannot be restricted to a list
	reasonAvailabilityTargetInvalid       = "availability_target_invalid"        // the wildcard path segment naming the target does not decode or names the wildcard itself
	reasonAvailabilityRestrictedRequired  = "availability_restricted_required"   // the body named no `restricted` value
	reasonAvailabilityOnlyEmpty           = "availability_only_empty"            // "Only" was chosen with an empty allow-list

	// /api/v1/delegation (portal delegate registration).
	reasonDelegationStoreUnavailable = "delegation_store_unavailable" // delegate registration requires the Postgres store backend
	reasonDelegateNameInvalid        = "delegate_name_invalid"
	reasonDelegateClientIDInvalid    = "delegate_client_id_invalid"
	reasonDelegateClientIDIsPortal   = "delegate_client_id_is_portal" // idp_client_id named this deployment's own OIDC client
	reasonDelegateGroupInvalid       = "delegate_group_invalid"

	// /api/v1/setup/integrations and /api/v1/setup/onboarding.
	reasonIntegrationInvalid              = "integration_invalid"                // the integration write fails validation
	reasonIntegrationNotFound             = "integration_not_found"              // no stored integration by that id
	reasonSetupOnboardingStoreUnavailable = "setup_onboarding_store_unavailable" // onboarding-complete needs a configured store

	// /api/v1/ssh-keys (self-service) and the admin delete-by-principal door.
	reasonSSHKeyInvalid                       = "ssh_key_invalid"              // the public-key line does not parse
	reasonSSHKeyRequiresHuman                 = "ssh_key_requires_human"       // only a signed-in human's own POST can register a working key
	reasonSSHKeyCapReached                    = "ssh_key_cap_reached"          // the principal already holds the maximum number of keys
	reasonSSHKeyRevokedSession                = "ssh_key_revoked_session"      // the session was revoked mid-request; sign in again
	reasonSSHKeyRegistrationRefused           = "ssh_key_registration_refused" // deliberately generic (THREAT-MODEL.md): never confirms whether the key is already registered, by whom
	reasonSSHKeyFingerprintInvalidEncoding    = "ssh_key_fingerprint_invalid_encoding"
	reasonSSHKeyAdminPrincipalInvalidEncoding = "ssh_key_admin_principal_invalid_encoding"
	reasonSSHKeyAdminPrincipalRequired        = "ssh_key_admin_principal_required"

	// /api/v1/drives (user drives): allocation/grant/reclaim admin.
	reasonUserDriveRequestInvalid        = "user_drive_request_invalid"         // the create/update body fails validation
	reasonUserDriveRehomeInvalid         = "user_drive_rehome_invalid"          // the re-home guard refused the requested change
	reasonUserDriveAllocatedConflict     = "user_drive_allocated_conflict"      // the drive was allocated to someone else mid-edit
	reasonUserDriveSlugConflict          = "user_drive_slug_conflict"           // another drive's name folds to the same storage-object name
	reasonUserDriveHomeNamespaceConflict = "user_drive_home_namespace_conflict" // another host_path drive on the same root uses a different home_template
	reasonUserDriveNameConflict          = "user_drive_name_conflict"           // a drive by that name already exists
	reasonUserDriveStillAllocated        = "user_drive_still_allocated"         // the drive cannot be deleted while allocations exist
	// reasonUserDriveGrantInvalid covers every shape ValidateUserDriveGrant (or
	// the group-subject/home-name checks beside it) refuses — all prefixed
	// "invalid allocation: " on the wire, one cause bucket on the wire reason.
	reasonUserDriveGrantInvalid            = "user_drive_grant_invalid"
	reasonUserDriveSizeRefused             = "user_drive_size_refused"              // the size override exceeds what the ceiling allows
	reasonUserDriveGrantConflict           = "user_drive_grant_conflict"            // that grant already exists
	reasonUserDriveReclaimInvalid          = "user_drive_reclaim_invalid"           // the reclaim request fails validation
	reasonUserDriveReclaimSubjectAmbiguous = "user_drive_reclaim_subject_ambiguous" // subject_type names more than one person's storage
	reasonUserDriveNotReclaimable          = "user_drive_not_reclaimable"           // this drive cannot be reclaimed here (wrong backend/runner target)
	reasonUserDriveReclaimFailed           = "user_drive_reclaim_failed"            // the reclaim object read/write failed
	reasonUserDriveReclaimUnsupported      = "user_drive_reclaim_unsupported"       // this deployment's runner cannot reclaim drive storage
	reasonUserDriveReclaimConflict         = "user_drive_reclaim_conflict"          // the reclaim was refused at the storage layer
	reasonUserDrivePreviewNoClaims         = "user_drive_preview_no_claims"         // the naming preview named no user_subjects
	reasonUserDriveDeniedByProfile         = "user_drive_denied_by_profile"         // the caller's governance profile shuts the drive door
)
