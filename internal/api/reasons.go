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
	reasonInvalidRunIDParam            = "invalid_run_id_param"            // ?run_id= does not parse as a UUID
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
	reasonPushContentUnattended        = "push_content_unattended"          // the run is unattended, so a push needing review is refused rather than held
	reasonPushPathListCountUnavailable = "push_path_list_count_unavailable" // the per-run path-list count could not be read
	reasonPushPathListCapReached       = "push_path_list_cap_reached"       // the run already holds the maximum number of pending push path lists
	reasonPushNotHeld                  = "push_not_held"                    // the named approval is not a held push_content approval
	// reasonPushPathListsRequirePostgres also covers approvals_push.go's earlier
	// "run store unavailable" arm (#656 L1 fold): both fire on the identical
	// s.cfg.Store == nil / not-a-*Store cause, one route apart, so one reason.
	reasonPushPathListsRequirePostgres = "push_path_lists_require_postgres" // this backend has no Postgres-backed run/push-path-list store configured

	// #656 slice 1 — POST /runs and its field/policy validation.
	//
	// seedRequestWorkspace's workspace_id resolution (#656 M1: five distinct
	// causes used to share reasonWorkspaceSeedFailed; split so a caller can
	// tell "no store" from "no base image" from "conflicts with the policy").
	reasonWorkspaceSeedStoreUnavailable    = "workspace_seed_store_unavailable"     // workspace_id was sent but this backend has no store configured
	reasonWorkspaceSeedUnreadable          = "workspace_seed_unreadable"            // the named workspace could not be read
	reasonWorkspaceSeedSourceTargetInvalid = "workspace_seed_source_target_invalid" // a stored source's target fails the authored-target deny-list
	reasonWorkspaceSeedNoBaseImage         = "workspace_seed_no_base_image"         // an exec run named a workspace with no base image and no --agent/--image
	reasonWorkspaceSeedPolicyConflict      = "workspace_seed_policy_conflict"       // the seeded sources collide with the policy's own mount/repo targets
	// validateImageBuildRequest (#656 M1: split from reasonInvalidImageBuildRequest,
	// one cause was the caller's own request shape, the other a deployment
	// capability neither request field can fix).
	reasonImageDevcontainerExclusive = "image_devcontainer_exclusive" // image and devcontainer_repo were both set
	reasonImageBuilderUnavailable    = "image_builder_unavailable"    // a custom image was requested but this control plane has no image builder wired
	// validateWorkspaceSources / authorizeSpecWorkspaceSources (#656 M1: split
	// from reasonWorkspaceSourcesInvalid/reasonWorkspaceSourcesUnauthorized —
	// except the not-onboarded arm, which STAYS one shared reason across both
	// functions on purpose, H1: a distinguishable reason there would be exactly
	// the cross-member existence oracle authorizeSpecWorkspaceSources' own doc
	// comment says the byte-identical SENTENCE already closes).
	reasonWorkspaceSourcesStoreUnavailable     = "workspace_sources_store_unavailable"      // workspace onboarding needs a store, and this backend has none
	reasonWorkspaceSourcesListUnavailable      = "workspace_sources_list_unavailable"       // the workspace list could not be read to resolve sources against
	reasonWorkspaceSourceNotOnboarded          = "workspace_source_not_onboarded"           // the source is not onboarded — SHARED, see the doc comment above
	reasonWorkspaceSourceMountNotAllowed       = "workspace_source_mount_not_allowed"       // the source IS onboarded, but the member-safe mount gate refuses this caller
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
	// This is a literal, not a reference to authz.ReasonGroupsSnapshotStale
	// (internal/authz/registry.go), so TestReasonDocsMatchReasonsGo (which
	// only reads string literals in this file) can see it — the tradeoff
	// #656 slice 2's review round chose over an unreadable-by-regex
	// reference. It is the SAME registered reason string, deliberately, not
	// a second copy invented for this package: see
	// adHocReasonLiterals["reasons.go:groups_snapshot_stale"]
	// (refusal_test.go) for TestNoAdHocAuthz's matching exception.
	reasonGroupsSnapshotStale = "groups_snapshot_stale"

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

	reasonUserDriveCeilingUnavailable     = "user_drive_ceiling_unavailable"       // the deployment's drive-size ceiling (site config) could not be read
	reasonUserDriveRehomeListUnavailable  = "user_drive_rehome_list_unavailable"   // the drive list could not be read to run the re-home guard
	reasonUserDriveReclaimNoDirectoryName = "user_drive_reclaim_no_directory_name" // the allocation resolves to no directory name, so there is no object to reclaim

	// owner_ambiguous / owner_unresolved: resolveSecretOwner's (secrets.go) and
	// resolveSSHKeyOwner's (sshkeys_admin.go) shared two-cause shape — more
	// than one known principal matches a name, or none does.
	reasonOwnerAmbiguous  = "owner_ambiguous"
	reasonOwnerUnresolved = "owner_unresolved"

	// personMintRefusal's own closed set (people.go, POST /people/{principal}/tokens).
	reasonPersonMintNoSignIn                 = "no_sign_in"                  // the derived role has no sign-in on this deployment
	reasonPersonMintDefaultRoleUnknownGroups = "default_role_unknown_groups" // an elevated role rests on the default role, which the person's still-unknown groups might narrow — they must sign in once first
	reasonPersonMintElevatedTarget           = "elevated_target"             // minting for an admin/security-admin target needs a super admin
)

// The driveRefusal* closed set (internal/api/user_drives_run.go's
// refuseDrive/driveBindFailure): one reason per launch-time drive-mount
// cause, already used for the wardyn_drive_refusals_total metric and its own
// WARN log line, now also on the wire. Declared here, not beside refuseDrive,
// so TestReasonDocsMatchReasonsGo (which only reads this file) can see every
// wire-visible reason in one place (#656 slice 2 review round).
const (
	driveRefusalNoAllocation      = "no_allocation"
	driveRefusalPaused            = "paused"
	driveRefusalRunnerCannotMount = "runner_cannot_mount"
	driveRefusalBackendElsewhere  = "backend_elsewhere"
	driveRefusalCeilingMoved      = "ceiling_moved"
	driveRefusalHomeMissing       = "home_missing"
	// driveRefusalHomeUnreadable is the #165 arm: the home directory EXISTS
	// (driveRefusalHomeMissing's own check already passed) but the sandbox's
	// own agent uid — not this daemon's root process — cannot read it. A
	// distinct reason from home_missing because the remedy differs: an admin
	// fixes permissions, not a directory that is already there.
	driveRefusalHomeUnreadable   = "home_unreadable"
	driveRefusalShareUnreachable = "share_unreachable"
	driveRefusalReadOnly         = "read_only"
	// driveRefusalDrivesDisabled is the ORG SWITCH, not a door: this install
	// offers no drives at all, so nobody was denied by a profile. Counted like
	// the rest, because an operator who turns the switch off wants to see how
	// many runs are still asking. Shared with userDriveWriteRefusal's OWN
	// org-switch check (user_drives.go): the identical cause, one route apart.
	driveRefusalDrivesDisabled = "drives_disabled"
)

// POST /runs/{id}/revive and the admin bulk restart (run_revive.go):
// reviveError's own status+reason+msg replaces its former bare (status, msg)
// pair, so the SAME reason recordAudit already wrote to the audit row for a
// revive refusal (#656 slice 3) now also reaches the wire. One reason per
// distinct revive-refusal cause; a few call sites answer the SAME cause two
// ways (the runner substrate cannot revive at all) and share one on purpose.
const (
	reasonReviveLocalModeNotOwner     = "local_mode_not_owner"          // local mode mints for the run's own owner; nobody else may revive it — the SAME literal this refusal's own audit row already carried
	reasonReviveUnsupportedDeployment = "revive_unsupported_deployment" // this deployment's store has no RunReviver, or configures no runner at all
	// reasonReviveUnsupportedRunner covers three arms that all answer the
	// identical runner.ErrReviveUnsupported fact: the runner does not
	// implement ProxyReviver, a rebooted run's runner does not also implement
	// SandboxStarter, and the run's own CanReplaceProxy check refused with
	// this same sentinel.
	reasonReviveUnsupportedRunner           = "revive_unsupported_runner"
	reasonReviveBulkCannotStartAgent        = "revive_bulk_cannot_start_agent"        // a bulk restart cannot start a stopped agent; only the run's own page can
	reasonReviveAlreadyInProgress           = "revive_already_in_progress"            // another revive of this run is already running
	reasonReviveMintIdentityFailed          = "revive_mint_identity_failed"           // minting the fresh run token failed
	reasonReviveEncodeConfigFailed          = "revive_encode_config_failed"           // the rewritten proxy config would not marshal to JSON
	reasonRevivePullImageFailed             = "revive_pull_image_failed"              // the proxy image could not be pulled
	reasonReviveClaimFailed                 = "revive_claim_failed"                   // the claim that marks the run revived failed
	reasonReviveRunChanged                  = "revive_run_changed"                    // the run ended, was lost again, or was revived elsewhere mid-request
	reasonReviveProxyKeptCurrent            = "revive_proxy_kept_current"             // the old proxy was never touched; a still-live run keeps it after a failed replace
	reasonReviveProxyReplaceFailedLost      = "revive_proxy_replace_failed_lost"      // the proxy could not be replaced, so the run has no egress and is lost again
	reasonReviveAgentStartFailedLost        = "revive_agent_start_failed_lost"        // the agent could not be started behind the new proxy, so the run is lost again
	reasonReviveSubstrateUnreadable         = "revive_substrate_unreadable"           // the run's substrate could not answer whether it can replace a proxy
	reasonReviveConfigNotStored             = "revive_config_not_stored"              // no proxy config is stored for this run (it predates this release, or none is kept)
	reasonReviveConfigUnreadable            = "revive_config_unreadable"              // the stored proxy config could not be read
	reasonReviveConfigDoesNotLoad           = "revive_config_does_not_load"           // the stored proxy config failed to parse or validate
	reasonReviveNotRunning                  = "revive_not_running"                    // the run is not in the RUNNING state, or has no sandbox
	reasonReviveEndedFilesGone              = "revive_ended_files_gone"               // the run ended and its files are no longer kept
	reasonRevivePastEnd                     = "revive_past_end"                       // the run has passed its scheduled end; extend it first
	reasonReviveEndedTaskRun                = "revive_ended_task_run"                 // a task run's agent cannot be started again once it has ended
	reasonReviveRebootAgentStopped          = "revive_reboot_agent_stopped"           // the run was lost to a reboot; only its own page can start its agent again
	reasonReviveEndedAgentStopped           = "revive_ended_agent_stopped"            // the run has ended and its agent is stopped; only its own page can start it again
	reasonReviveUnknownLostReason           = "revive_unknown_lost_reason"            // the run's lost_reason is not one revive recognizes
	reasonReviveAgentStatusUnreadable       = "revive_agent_status_unreadable"        // the run's agent status could not be probed
	reasonReviveConfigRunMismatch           = "revive_config_run_mismatch"            // the stored proxy config names a different run than the one being revived
	reasonReviveCeilingDeniesGitBroker      = "revive_ceiling_denies_git_broker"      // the owner's current governance profile now denies GitHub, which the run's git broker needs
	reasonReviveOwnerAuthorityUnreadable    = "revive_owner_authority_unreadable"     // the owner's launch-door or model-credential re-check could not be completed
	reasonReviveAdminRestartCountInvalid    = "revive_admin_restart_count_invalid"    // run_ids named none, or more than the bulk maximum
	reasonReviveProxyWindowStoreUnavailable = "revive_proxy_window_store_unavailable" // this store cannot list run proxy releases
)

// ownerRefusal's own reason values (run_owner_authority.go): a revive's and a
// run-end-extension's shared re-check of the owner's launch-door capabilities,
// governance profile and model credential. Already used for the audit row
// before #656 slice 3; declared here, not beside ownerRefusal, for the same
// reason the driveRefusal* set is declared here rather than beside refuseDrive.
const (
	reasonOwnerProfileUnreadable = "profile_unreadable" // the owner's captured governance profile could not be read back
	reasonOwnerProfileGone       = "profile_gone"       // the governance profile captured at launch no longer exists
	// reasonOwnerCapability* names the launch door persistedLaunchDoors found
	// closed: the SAME five capability kinds capabilities.go's own cap* consts
	// enumerate, so the reason names the kind rather than repeating a run's
	// specific agent/workspace/policy id (never on the wire).
	reasonOwnerCapabilityAgent             = "capability_agent"
	reasonOwnerCapabilityWorkspace         = "capability_workspace"
	reasonOwnerCapabilityModelProvider     = "capability_model_provider"
	reasonOwnerCapabilityPolicy            = "capability_policy"
	reasonOwnerCapabilityWorkspaceProvider = "capability_workspace_provider"
	// reasonOwnerCapabilityUnknown is defensive only: capabilityLostReason's
	// (run_owner_authority.go) fallback for a capability kind outside the five
	// above, which persistedLaunchDoors cannot produce today.
	reasonOwnerCapabilityUnknown     = "capability_unknown"
	reasonOwnerModelCredentialErased = "model_credential_erased" // the secret this run's proxy would inject no longer exists
	reasonOwnerModelProviderDisabled = "model_provider_disabled" // the integration supplying this run's credential was disabled
	// reasonOwnerUnverifiable is extendRefusal's own bucket (run_owner_authority.go):
	// three arms (proxy config unreadable, config does not load, capability
	// re-check itself failed) that all answer the identical client-facing fact —
	// the owner's authority to extend this run could not be confirmed right now.
	reasonOwnerUnverifiable = "owner_unverifiable"
)

// PATCH /runs/{id} (run_end_wait.go): the run's end and wait budget.
const (
	reasonRunEndWaitNeitherField     = "run_end_wait_neither_field"     // the body set neither ends_at nor wait_budget_sec
	reasonRunWaitBudgetNotANumber    = "run_wait_budget_not_a_number"   // wait_budget_sec was present but null
	reasonRunEndWaitAlreadyFinished  = "run_end_wait_already_finished"  // the run is already in a terminal state
	reasonRunEndWaitFilesGone        = "run_end_wait_files_gone"        // the run ended and its files are no longer kept
	reasonRunEndWaitStoreUnavailable = "run_end_wait_store_unavailable" // this store cannot change a run's end
	// reasonRunLimitsGateDenied is planRunEndWait's own shared cause: the
	// user_changes_limits gate refused this change, for any of the 4 fields
	// gateRefusal names — the SAME gate regardless of which field it blocked.
	reasonRunLimitsGateDenied   = "run_limits_gate_denied"
	reasonRunEndNoEndNotAllowed = "run_end_no_end_not_allowed" // the deployment's run-limits do not allow No end at all, gate or no gate
	reasonRunEndMustBeFuture    = "run_end_must_be_future"     // ends_at is not after now
	reasonRunWaitBudgetTooSmall = "run_wait_budget_too_small"  // wait_budget_sec is below 1
	reasonRunEndWaitChanged     = "run_end_wait_changed"       // the run changed while the PATCH was being decided
)

// notFoundIf's (helpers.go) own reasons: one per resource kind its ~27
// call sites name (#656 slice 3). notFoundIf itself cannot tell one kind
// from another, so each caller supplies its own; declared together here so
// TestReasonDocsMatchReasonsGo sees the whole set in one place. A kind
// already exercised for a DIFFERENT shape entirely (reasonApprovalNotFound,
// #656 slice 1) is reused rather than duplicated — same cause, same const.
// reasonRunNotFound is likewise the SAME literal auditRenewDenied
// (internal.go) already wrote for a renewal on a run that no longer exists.
const (
	reasonWorkspaceNotFound             = "workspace_not_found"
	reasonUserDriveNotFound             = "user_drive_not_found"
	reasonUserDriveAllocationNotFound   = "user_drive_allocation_not_found"
	reasonSSHKeyNotFoundEntity          = "ssh_key_not_found"
	reasonPresetNotFound                = "preset_not_found" // the admin preset-management route named no such preset — reasonPresetUnknown covers a RUN naming one instead
	reasonPolicyNotFound                = "policy_not_found"
	reasonCapabilityGrantNotFound       = "capability_grant_not_found"
	reasonRunNotFound                   = "run_not_found"
	reasonGovernanceAssignmentNotFound  = "governance_assignment_not_found"
	reasonGovernanceProfileNotFoundByID = "governance_profile_not_found" // governance.go's own by-id GET/PUT/DELETE — distinct from reviveCeiling's reasonOwnerProfileGone (a run's CAPTURED profile going missing later)
	reasonEnrolmentTokenNotFound        = "enrolment_token_not_found"
	reasonDeviceNotFound                = "device_not_found"
	reasonDelegateNotFound              = "delegate_not_found"
	reasonAPITokenNotFoundEntity        = "api_token_not_found"
	reasonRoleMappingNotFound           = "role_mapping_not_found"
)

// helpers.go's own shared foundational refusals (#656 slice 3): one path
// param parse failure, one auth-middleware claims failure, one body-decode
// failure, all reused by dozens of call sites across the package, since the
// cause really is identical regardless of which handler hit it.
const (
	reasonInvalidIDParam            = "invalid_id_param"            // a {param} path segment does not parse as a UUID
	reasonMissingRunClaims          = "missing_run_claims"          // the internal run token's claims could not be read
	reasonRunIDMismatch             = "run_id_mismatch"             // the token's run id does not match the path's {runID}
	reasonWorkspaceStoreUnavailable = "workspace_store_unavailable" // this build has no store configured at all (harness-only; wardynd always wires one)
	// scan-upload's own 3-arm refusal (authSandboxRunUpload's sole caller today,
	// scanresult.go): the same shape a future verify-result/recording caller
	// would reuse the HELPER for for, but not these three specific reasons.
	reasonScanUploadRunNotFound = "scan_upload_run_not_found"
	reasonScanUploadNotGoverned = "scan_upload_not_governed"
	reasonScanUploadWrongTask   = "scan_upload_wrong_task"
	// scopedWorkspaceWrite's 4 callers (#656 slice 3): each validator's whole
	// failure set is one cause bucket, the same grain as reasonSiteConfigInvalid
	// above — a caller already knows which route it called, so the reason need
	// only say "the body failed this route's validation", not which check.
	reasonWorkspaceRequirementsInvalid   = "workspace_requirements_invalid"
	reasonWorkspaceApprovedEgressInvalid = "workspace_approved_egress_invalid"
	// reasonWorkspaceApprovedEgressDeadHost is its own reason, not folded into
	// the bucket above: the remedy is different (use the git-broker/control-plane
	// door this host is already routed through, not fix a malformed domain).
	reasonWorkspaceApprovedEgressDeadHost = "workspace_approved_egress_dead_host"
	reasonWorkspaceDeniedEgressInvalid    = "workspace_denied_egress_invalid"
	reasonWorkspaceLLMCredInvalid         = "workspace_llm_cred_invalid"
	// readCappedBody's own two shapes (helpers.go): shared by every caller that
	// reads a request body under a cap, not only decodeStrictKeys's.
	reasonRequestBodyTooLarge   = "request_body_too_large"
	reasonRequestBodyUnreadable = "request_body_unreadable"
)

// /api/v1/policies (policies.go).
const (
	reasonPolicyRequestInvalid    = "policy_request_invalid"     // the create/update body fails decodePolicyRequest
	reasonPolicySecretRefsInvalid = "policy_secret_refs_invalid" // a secret reference in the spec fails shape validation
	reasonPolicyNameConflict      = "policy_name_conflict"       // a policy by that name already exists
)

// POST/Review /runs' model-provider door (run_model_provider.go), the 4 field
// arms outside writeProviderRefusal — which always carries its own reason,
// llmRefusalAuditReason or the generic authz.ReasonModelProviderUnavailable.
const (
	reasonModelProviderIDInvalid           = "model_provider_id_invalid"           // model_provider is not a plain provider id
	reasonModelProviderNotApplicable       = "model_provider_not_applicable"       // model_provider was set on a run that calls no model
	reasonModelProviderNoBlockConfigured   = "model_provider_no_block_configured"  // model_provider was named but this deployment has no model providers
	reasonModelProviderIntegrationConflict = "model_provider_integration_conflict" // integration_id was named alongside a model-providers block
)

// PATCH /runs/{id}/title (run_title.go).
const (
	reasonRunTitleStoreUnavailable = "run_title_store_unavailable" // this store cannot rename a run
)

// GET /runs/{id}/resources and GET /runs/{id}/files (run_resources.go,
// run_files.go): the two widgets read the identical run-state facts, so they
// share a reason per cause rather than each inventing its own synonym.
const (
	reasonRunInspectNoRunner              = "run_inspect_no_runner"               // this deployment configures no runner at all
	reasonRunInspectTerminal              = "run_inspect_terminal"                // the run has finished; its sandbox is gone
	reasonRunInspectNoSandbox             = "run_inspect_no_sandbox"              // the run never dispatched (or recorded no sandbox ref)
	reasonRunInspectPaused                = "run_inspect_paused"                  // the run is paused
	reasonRunInspectExecStreamUnsupported = "run_inspect_exec_stream_unsupported" // the runner does not support exec streaming
	reasonRunResourcesReadFailed          = "run_resources_read_failed"           // the sandbox resource usage script failed
	reasonRunFilesNoExecSession           = "run_files_no_exec_session"           // the runner returned no exec session
)

// POST /runs/{id}/resume (run_pause.go).
const (
	reasonRunResumeNotRunning = "run_resume_not_running" // the run is not in a resumable state
	reasonRunResumeFailed     = "run_resume_failed"      // thawForExec failed
)

// POST /internal/* (internal.go, internal_live_run.go): the sidecar/proxy
// surface, not the member-facing API.
const (
	reasonInternalDecisionLogInvalid = "internal_decision_log_invalid" // the egress decision log body did not decode
	reasonGroundtruthBatchInvalid    = "groundtruth_batch_invalid"     // the ground-truth batch body did not decode
	reasonGroundtruthBatchTooLarge   = "groundtruth_batch_too_large"   // the batch named more than 1000 events
	reasonGroundtruthActionNotKernel = "groundtruth_action_not_kernel" // an event's action lacks the required kernel. prefix
	reasonGroundtruthWriteFailed     = "groundtruth_write_failed"      // persisting a ground-truth event failed
	// reasonInternalApprovalRequestInvalid, reasonUnsupportedInternalApprovalKind,
	// reasonMissingRequestedScope and reasonReservedScopeKey are
	// handleInternalRequestApproval's own refusals (internal.go); the latter
	// three are already the exact strings its own s.auditAuthFailedAs call
	// wrote before #656 slice 3 gave them a reasons.go home.
	reasonInternalApprovalRequestInvalid   = "internal_approval_request_invalid"
	reasonUnsupportedInternalApprovalKind  = "unsupported_internal_approval_kind"
	reasonMissingRequestedScope            = "missing_requested_scope"
	reasonReservedScopeKey                 = "reserved_scope_key"
	reasonInternalApprovalCountUnavailable = "internal_approval_count_unavailable" // the per-run approval count could not be read
	reasonInternalApprovalCapReached       = "internal_approval_cap_reached"       // the run already holds the maximum number of approvals
	reasonBrokerNotConfigured              = "broker_not_configured"               // this deployment configures no credential broker
	reasonMintGrantIDRequired              = "mint_grant_id_required"              // the mint request named no grant_id
	// reasonBrokeredForgeSingleLane{,Unverifiable} are handleInternalMint's own
	// single-lane guard (internal.go); already the exact strings its own audit
	// row wrote before #656 slice 3 put them on the wire too.
	reasonBrokeredForgeSingleLane             = "brokered_forge_single_lane"
	reasonBrokeredForgeSingleLaneUnverifiable = "brokered_forge_single_lane_unverifiable"
	reasonGrantRunMismatch                    = "grant_run_mismatch"          // the grant belongs to a different run
	reasonGrantNotFound                       = "grant_not_found"             // no such grant
	reasonGrantRequiresSPIRE                  = "grant_requires_spire"        // the grant needs the embedded SPIRE identity provider
	reasonRunRenewStoreUnavailable            = "run_renew_store_unavailable" // no store configured to verify the run is still alive
	reasonRunRenewReadFailed                  = "run_renew_read_failed"       // reading the run for the renew gate failed
	reasonRunRenewStampFailed                 = "run_renew_stamp_failed"      // stamping the run's token-renewed marker failed
	// reasonInternalLivenessReadFailed is refuseTerminalRun's own 503
	// (internal_live_run.go) — the shared /internal/* liveness gate every
	// sidecar door runs through.
	reasonInternalLivenessReadFailed = "internal_liveness_read_failed"
)

// The credential-injection sinks' own closed set (injection.go's
// storeReadRefusal and its callers across injection_provider_key.go,
// injection_awssso.go, provider_subscription.go, internal.go): one reason per
// cause, shared across every lane that hits the identical store-level fact
// rather than each inventing a synonym — sinkStoreUnreachable's own sentence
// already flows to several of these unchanged.
const (
	reasonSinkStoreUnavailable = "store_unavailable" // the credential store did not answer (transient)
	reasonSinkSecretNotFound   = "not_found"         // the named secret is not in the store
	reasonSinkSecretRefused    = "refused"           // the secret exists but the store refused to serve it
	reasonSinkResolveFailed    = "resolve_failed"    // resolving a subscription/managed token failed for a reason other than an unreachable store
	// reasonProviderStoreRefused is providerStoreReadRefusal's own split of
	// reasonSinkSecretRefused: a person's own model-provider credential, never
	// the generic secret sentence.
	reasonProviderStoreRefused = "store_refused"
)

// http.go's own auth middlewares — local mode's loopback/CSRF gates, and the
// admin/run/sensor bearer-token chains. Every value here is already the exact
// string its own auditAuthFailed(As) call wrote before #656 slice 3 put it on
// the wire too, so an operator correlating a 401 to its audit row sees the
// same word twice.
const (
	reasonLocalModePeerNotLoopback = "local_mode_peer_not_loopback"
	reasonLocalModeHostNotLoopback = "local_mode_host_not_loopback"
	reasonAdminTokenNotConfigured  = "admin_token_not_configured"
	reasonMissingBearerToken       = "missing_bearer_token"
	reasonInvalidAdminToken        = "invalid_admin_token"
	// reasonIdentityProviderNotConfigured is shared by internalAuth and
	// internalAuthGroundtruth: the identical cause (no embedded identity
	// provider wired) refuses both the run-token and the sensor-token chains.
	reasonIdentityProviderNotConfigured = "identity_provider_not_configured"
	reasonMissingRunToken               = "missing_run_token"
	reasonInvalidRunToken               = "invalid_run_token"
	reasonMissingSensorToken            = "missing_sensor_token"
	reasonInvalidSensorToken            = "invalid_sensor_token"
)

// The UI gateway (uigateway.go, uigateway_session.go): the second,
// un-authenticated-by-session origin that serves a sandbox's own web app.
// uiDialError carries one of these from the dial/launch path back out to
// uiErrorHandler, which is the ONE place that writes the HTTP response for
// that whole path.
const (
	reasonUIGatewayNotFound          = "ui_gateway_not_found"            // no path matched the gateway's own router
	reasonUIGatewayTicketQueryOnPost = "ui_gateway_ticket_query_on_post" // a POST /__wardyn/enter carried the ticket as a query param
	reasonUIGatewayInvalidFormBody   = "ui_gateway_invalid_form_body"    // the POST form body did not parse
	reasonUIGatewayMethodNotAllowed  = "ui_gateway_method_not_allowed"   // /__wardyn/enter saw a method other than GET/POST
	reasonUIGatewayInvalidRunID      = "ui_gateway_invalid_run_id"       // the run field is missing or not a UUID
	reasonUIGatewayWrongHost         = "ui_gateway_wrong_host"           // this run's apps are pinned to a different origin (host mode)
	// reasonUIGatewayTicketInvalid is shared by "not bound to this browser"
	// and "invalid/expired/already-used" — the SAME refusal, byte for byte,
	// so a probe cannot tell binding failure from a genuinely bad ticket.
	reasonUIGatewayTicketInvalid      = "ui_gateway_ticket_invalid"
	reasonUIGatewayTicketLookupFailed = "ui_gateway_ticket_lookup_failed"
	// reasonUIGatewayTicketRunMismatch is shared by "the run could not be
	// read" and "the ticket's stamped principal is not this run's owner" —
	// the identical existence-oracle-safe shape getRunAuthorized's own foreign
	// vs missing parity uses, so a probe cannot tell a missing run from one it
	// does not own.
	reasonUIGatewayTicketRunMismatch          = "ui_gateway_ticket_run_mismatch"
	reasonUIGatewayNotRunning                 = "ui_gateway_not_running"                  // the run is not RUNNING, or has no sandbox
	reasonUIGatewayRunKept                    = "ui_gateway_run_kept"                     // the run is kept (ended/lost); its agent is stopped
	reasonUIGatewayPolicyLookupFailed         = "ui_gateway_policy_lookup_failed"         // the run's effective UI-apps policy could not be read
	reasonUIGatewayAppNotDeclared             = "ui_gateway_app_not_declared"             // the named app is not in the run's policy ui_apps
	reasonUIGatewayNoSession                  = "ui_gateway_no_session"                   // no valid UI relay session cookie for this run/app
	reasonUIGatewaySessionDestinationMismatch = "ui_gateway_session_destination_mismatch" // the dial address does not match the session's run/port
	reasonUIGatewayNoRunner                   = "ui_gateway_no_runner"                    // this deployment configures no runner
	reasonUIGatewayResumeFailed               = "ui_gateway_resume_failed"                // thawing a paused run for the relay failed
	reasonUIGatewayConnCapReached             = "ui_gateway_conn_cap_reached"             // this run already holds the maximum number of open UI connections
	reasonUIGatewayExecFailed                 = "ui_gateway_exec_failed"                  // starting the relay's own exec session failed
	reasonUIGatewayLauncherExecFailed         = "ui_gateway_launcher_exec_failed"         // starting the in-sandbox launcher probe failed
	reasonUIGatewayLauncherMissing            = "ui_gateway_launcher_missing"             // the image has no launcher script for this app
	reasonUIGatewayLauncherNotListening       = "ui_gateway_launcher_not_listening"       // the launcher ran but nothing bound the app's port in time
	reasonUIGatewayLauncherProbeFailed        = "ui_gateway_launcher_probe_failed"        // the launcher probe exited with an unrecognized code
	reasonUIGatewayConnectionClosed           = "ui_gateway_connection_closed"            // the sandbox closed the relay connection unexpectedly
	reasonUIGatewayBindMissingTicket          = "ui_gateway_bind_missing_ticket"          // POST /__wardyn/bind's form carried no ticket
	// reasonUIGatewayBind{NotSameSite,OriginNotConsole} are uiBindRefusal's own
	// closed set (the ui.authorize/denied row's stable identifiers) — moved
	// here, not left beside it, so this guard can see them (#656 slice 3).
	reasonUIGatewayBindNotSameSite      = "bind_not_same_site"
	reasonUIGatewayBindOriginNotConsole = "bind_origin_not_console"
)

// Device federation (devices_auth.go): the forwarder's own auth chain and
// audit-ingest door. Most values here are already the exact strings
// s.auditAuthFailedAs/auditEnrolFailure/auditIngestFailure wrote before #656
// slice 3 put them on the wire too.
const (
	reasonDeviceStoreUnavailable     = "device_store_unavailable" // this deployment configures no device store (deviceAuth)
	reasonMissingDeviceToken         = "missing_device_token"
	reasonInvalidDeviceToken         = "invalid_device_token"
	reasonDeviceLookupFailed         = "device_lookup_failed"         // reading the device row failed (transient)
	reasonDeviceEnrolRateLimited     = "device_enrol_rate_limited"    // too many enrolment attempts from this address
	reasonDeviceEnrolmentUnavailable = "device_enrolment_unavailable" // handleDeviceEnrol's own no-store case
	reasonDeviceEnrolTokenRequired   = "device_enrol_token_required"  // the enrolment body named no token
	reasonInvalidEnrolmentToken      = "invalid_enrolment_token"
	reasonDeviceIngestInFlight       = "device_ingest_in_flight" // a push from this device is already being processed
	reasonDeviceIngestInvalidBody    = "invalid_body"
	reasonDeviceIngestBatchTooLarge  = "batch_too_large"
	reasonDeviceIngestInvalidRow     = "invalid_row"
	reasonDeviceIngestOrgRun         = "org_run"
	reasonDeviceIngestChainMismatch  = "chain_mismatch"
	// /api/v1/admin/devices/enrolment-tokens (devices.go).
	reasonDeviceEnrolmentTokenNameInvalid = "device_enrolment_token_name_invalid"
)

// The harness.credential.refuse row's own FIXED reason vocabulary
// (refuseCapture, awssso_pin.go): a closed set on purpose, since the
// alternative is sandbox-chosen text an incident review cannot group by.
// Moved here from beside refuseCapture (#656 slice 3) so the docs guard,
// which only reads this file, can see them now that refuseCapture's reason
// also reaches the wire.
const (
	reasonCaptureBlobShape        = "blob_shape"
	reasonCaptureFieldUnsafe      = "field_unsafe"
	reasonCaptureFieldShape       = "field_shape"
	reasonCaptureRegionMismatch   = "region_mismatch"
	reasonCaptureStartURLMismatch = "start_url_mismatch"
	reasonCaptureAccountRolePin   = "account_role_pin_mismatch"
	reasonCaptureModelAccount     = "model_account_mismatch"
	reasonCaptureUnstampedScope   = "unstamped_scope"
	reasonCaptureAlreadyCaptured  = "already_captured"
	reasonCaptureStampUnreadable  = "stamp_unreadable"
	// reasonCaptureRunKilled: the login run this upload comes from has been
	// KILLED — by its own Cancel, or by the person's next sign-in superseding
	// it (harnesscred_supersede.go). See ssoTokenRunKilledRefusal.
	reasonCaptureRunKilled = "run_killed"
	// reasonCaptureProviderChanged: a sign-in through a model provider's own
	// door whose provider was removed, re-kinded or re-addressed while the
	// login sandbox was open (storeProviderSignIn).
	reasonCaptureProviderChanged = "provider_changed"
	// reasonCaptureSignInBusy: the per-person sign-in lock could not be taken
	// in time (lockLoginSupersede), so the capture was not serialized and is
	// refused rather than stored.
	reasonCaptureSignInBusy = "signin_busy"
)

// /internal/sso-token (ssotoken.go).
const (
	reasonSSOTokenNoSecretStore = "sso_token_no_secret_store" // this deployment configures no secret store
	reasonSSOTokenWrongRunKind  = "sso_token_wrong_run_kind"  // the run is not an aws-sso container-login run
)

// /api/v1/model-providers/{id}/sign-in (provider_signin.go), the console's
// own sign-in door (distinct from run-create's model-provider choice,
// run_model_provider.go).
const (
	reasonProviderSignInNoBlock            = "provider_sign_in_no_block"             // this deployment configures no model providers
	reasonModelProviderNotFoundEntity      = "model_provider_not_found"              // no such provider, or the caller may not see it
	reasonProviderSignInUntyped            = "provider_sign_in_untyped"              // no login convention is wired for this provider's kind
	reasonProviderSignInDisabled           = "provider_sign_in_disabled"             // the provider is turned off
	reasonProviderSignInConfigUnreadable   = "provider_sign_in_config_unreadable"    // the site config could not be read
	reasonProviderSignInPreviewBlocked     = "provider_sign_in_preview_blocked"      // a previewing admin cannot capture into the previewed identity
	reasonProviderSignInNoPortal           = "provider_sign_in_no_portal"            // the Bedrock provider names no SSO portal/region
	reasonProviderSignInAccountAmbiguous   = "provider_sign_in_account_ambiguous"    // more than one account could serve this caller, unpinned
	reasonProviderSignInNoImage            = "provider_sign_in_no_image"             // no sign-in image resolves for this agent
	reasonProviderSignInCaptureBodyInvalid = "provider_sign_in_capture_body_invalid" // the capture body did not decode, or run_id is not a UUID
	reasonProviderSignInAWSByHelper        = "provider_sign_in_aws_by_helper"        // this Bedrock provider captures via the CLI helper, not this door
	// reasonHarnessPasteInvalid is harnessPasteRefusal's own bucket (shared with
	// harnesscred.go's identical door): via-helper/empty/too-long all fail the
	// same paste-shape validation.
	reasonHarnessPasteInvalid          = "harness_paste_invalid"
	reasonProviderSignInNotYourRun     = "provider_sign_in_not_your_run"    // the named run is not this caller's own sign-in for this provider
	reasonProviderSignInCaptureChanged = "provider_sign_in_capture_changed" // the provider's address changed since this sign-in was launched
)

// errRecordCeilingLimit's own wire reason (workspace_run_launch.go): a launch
// refused by the acting principal's governance profile limits (interactive
// not allowed, or the concurrent-run cap) — shared by every launch door that
// maps the sentinel to a 403 (provider_signin.go, harnesscred_launch.go,
// record.go), since it is the identical cause regardless of which door hit it.
const reasonRecordCeilingLimit = "record_ceiling_limit"

// /internal/injection/{grantID} (injection.go): the proxy's own api_key
// resolve door. Most values here are already the exact strings each site's
// own secret.read audit row wrote.
const (
	reasonInjectionGrantNotAPIKey            = "injection_grant_not_api_key"
	reasonInjectionReservedSecretName        = "reserved_secret_name"
	reasonInjectionInvalidHeaderName         = "invalid_header_name"
	reasonInjectionOAuthHostNotAnthropic     = "oauth_host_not_anthropic"
	reasonInjectionSharedSubscriptionPosture = "shared_subscription_posture"
	reasonInjectionNoOAuthProvider           = "no_oauth_provider"
)

// Record Mode (record.go): per-task recording sandboxes and their promotion
// to durable requirement rows.
const (
	reasonRecordSessionNameRequired = "record_session_name_required"
	reasonRecordLabelCollision      = "record_label_collision"
	reasonRecordNoRunner            = "record_no_runner"
	reasonRecordImportStepBusy      = "record_import_step_busy"
	reasonRecordPromoteNoRecording  = "record_promote_no_recording"
	// reasonRecordPromoteRejected is promotableRecordHosts' own bucket: several
	// distinct pre-promotion checks (incomplete/borrowed/plumbing/contradicted/
	// shapeless evidence) all refuse a promotion for the same reason — the
	// entry is not durable-policy material yet.
	reasonRecordPromoteRejected          = "record_promote_rejected"
	reasonRecordPromoteHostNotPromotable = "record_promote_host_not_promotable"
	reasonRecordPromoteCapReached        = "record_promote_cap_reached"
	reasonRecordPromoteConflict          = "record_promote_conflict"
)

// /api/v1/admin/user-types (user_types.go).
const (
	reasonUserTypeRequestInvalid    = "user_type_request_invalid" // the body fails userTypeFromRequest's shape validation
	reasonUserTypeConflict          = "user_type_conflict"        // the id or name collides with an existing type
	reasonUserTypeNotFound          = "user_type_not_found"
	reasonUserTypeIDImmutable       = "user_type_id_immutable"         // the body's id does not match the path
	reasonUserTypeBuiltInNoPriority = "user_type_built_in_no_priority" // the built-in type never wins a tie, so it takes no priority
	reasonUserTypeBuiltInImmutable  = "user_type_built_in_immutable"   // the built-in type cannot be removed
	reasonUserTypeInUse             = "user_type_in_use"               // a role mapping, grant or token stamp still names this type
	reasonUserTypeDeleteConflict    = "user_type_delete_conflict"      // something started naming it between the read and the delete
)

// POST /runs/{id}/attach (attach.go): the interactive WebSocket door.
const (
	reasonAttachNoRunner         = "attach_no_runner"           // this deployment configures no runner
	reasonAttachTicketNotYourRun = "attach_ticket_not_your_run" // the ticket's stamped principal is not this run's owner
	reasonAttachNotRunning       = "attach_not_running"         // the run is not in the RUNNING state
	reasonAttachRunKept          = "attach_run_kept"            // the run is kept (ended/lost); its agent is stopped
	reasonAttachNoSandbox        = "attach_no_sandbox"          // the run has no sandbox ref
	reasonAttachResumeFailed     = "attach_resume_failed"       // thawing a paused run for the attach failed
	reasonAttachTicketMintFailed = "attach_ticket_mint_failed"  // mintAttachTicket failed
	reasonAttachTakeoverNoHolder = "attach_takeover_no_holder"  // nobody is currently attached to this run
)

// The user-drive resolver's own closed enum (user_drives_resolve.go) members
// that reach writeDriveError's wire body. driveUnavailableGroups,
// driveUnavailableUnknown and driveUnavailableGovernance stay declared beside
// their own GET /me field instead — they never reach errorBody.Reason, so
// TestReasonDocsMatchReasonsGo does not need to see them, and docs/sdk.md does
// not document them (#656 slice 2 review round S3: a documented-but-unsent
// reason is worse than an undocumented one, and `unavailable` collided with
// ADOEntraFailure's own reason of the same name).
const (
	driveUnavailableUserType    = "user_type_unknown" // the caller's stamped user type no longer exists. 403 at launch.
	driveUnavailableUnmountable = "unmountable"       // an allocation EXISTS and cannot be mounted — a home name that cannot name a directory, a share that is not there. 422 at launch, and the one state whose remedy is an admin's, not the member's.
)
