// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Continues reasons.go's closed set of machine-readable wire reasons (see its
// own header comment for the convention this whole set follows). Split into a
// second file only because #656 slice 3 finishing the route-surface sweep
// pushed reasons.go past scripts/check-file-size.sh's 1000-line gate — not a
// second vocabulary or a different rule. TestReasonDocsMatchReasonsGo and
// TestNoAdHocAuthz both read this file too, so a value declared here is exactly
// as visible to the guards as one declared in reasons.go itself.

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
	reasonReviveLiveTooSoon                 = "revive_live_too_soon"                  // a live run's proxy was replaced less than reviveLiveEvery ago
	reasonReviveMintIdentityFailed          = "revive_mint_identity_failed"           // minting the fresh run token failed
	reasonReviveEncodeConfigFailed          = "revive_encode_config_failed"           // the rewritten proxy config would not marshal to JSON
	reasonRevivePullImageFailed             = "revive_pull_image_failed"              // the proxy image could not be pulled
	reasonReviveClaimFailed                 = "revive_claim_failed"                   // the claim that marks the run revived failed
	reasonReviveRunChanged                  = "revive_run_changed"                    // the run ended, was lost again, or was revived elsewhere mid-request
	reasonReviveProxyKeptCurrent            = "revive_proxy_kept_current"             // the old proxy was never touched; a still-live run keeps it after a failed replace
	reasonReviveProxyReplaceFailedLost      = "revive_proxy_replace_failed_lost"      // the proxy could not be replaced, so the run has no egress and is lost again
	reasonReviveAgentStartFailedLost        = "revive_agent_start_failed_lost"        // the agent could not be started behind the new proxy, so the run is lost again
	reasonReviveRecoveryUnresolved          = "revive_recovery_unresolved"            // the revive failed and the run could not be recorded as lost; its proxy is stopped and a sweep recovers it
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
	reasonReviveOwnerAuthorityUnreadable    = "revive_owner_authority_unreadable"     // the owner's launch-door or model-credential re-check, or the read of the run's git_pat grants for its brokered set, could not be completed
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
	reasonOwnerModelProviderDisabled = "model_provider_disabled" // the integration supplying this run's credential, or the run's model provider, was turned off
	reasonOwnerModelProviderGone     = "model_provider_gone"     // the model provider that authored this run's credential was deleted (or re-created under a new UID)
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
	// Shared by the policy CRUD door, POST /runs' inline_policy and a launch
	// preset's inline_policy: the spec's
	// azure_devops_capabilities names something the catalogue cannot grant.
	reasonADOCapabilityUnknown = "ado_capability_unknown"
	// Review's mirror of the dispatch refusal: a member's azure_devops_capabilities
	// that leaves nothing standing (POST /runs/preflight).
	reasonADOCapabilitiesNonePermitted = "ado_capabilities_none_permitted"
)

// POST/Review /runs' model-provider door (run_model_provider.go), the 3 field
// arms outside writeProviderRefusal — which always carries its own reason,
// llmRefusalAuditReason or the generic authz.ReasonModelProviderUnavailable.
const (
	reasonModelProviderIDInvalid         = "model_provider_id_invalid"          // model_provider is not a plain provider id
	reasonModelProviderNotApplicable     = "model_provider_not_applicable"      // model_provider was set on a run that calls no model
	reasonModelProviderNoBlockConfigured = "model_provider_no_block_configured" // model_provider was named but this deployment has no model providers
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

// GET /runs/{id}/output (run_output.go).
const (
	reasonRunOutputTailInvalid = "run_output_tail_invalid" // ?tail= is not a positive number of bytes
	reasonRunOutputInteractive = "run_output_interactive"  // the run is interactive; only a task_mode=exec run keeps its output
	reasonRunOutputOff         = "run_output_off"          // WARDYN_EXEC_OUTPUT_TAIL=off
	reasonRunOutputNotKept     = "run_output_not_kept"     // no tail is held for the run (not exec, or started before a restart)
	reasonRunOutputExpired     = "run_output_expired"      // the tail outlived WARDYN_EXEC_OUTPUT_TAIL_TTL
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
	reasonInjectionGrantNotAPIKey     = "injection_grant_not_api_key"
	reasonInjectionReservedSecretName = "reserved_secret_name"
	reasonInjectionInvalidHeaderName  = "invalid_header_name"
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

// /api/v1/admin/presets (presets.go): validatePresetRequest's whole check is
// one cause bucket, the same grain as reasonSiteConfigInvalid. Deliberately
// NOT split per arm (#656 final review round L2 considered it: the "user type
// does not exist" and "unknown confinement_class" arms echo POST /runs' own
// per-field reasons) — this is an operator-only authoring route, the field
// the caller sent is already in the 400's own message, and slice 1's
// reasonWorkspaceRequestInvalid bucket is the same policy for the same kind
// of route.
const reasonPresetRequestInvalid = "preset_request_invalid"

// /api/v1/model-providers/{id}/credential (model_provider_credentials.go):
// the console's own key/token storage door, distinct from the sign-in door
// (provider_signin.go) and run-create's model-provider choice
// (run_model_provider.go).
const (
	reasonModelProviderCredentialNoStore          = "model_provider_credential_no_store"          // this deployment configures no secret store
	reasonModelProviderCredentialNoPerson         = "model_provider_credential_no_person"         // the caller resolves to no storable identity (e.g. the admin token)
	reasonModelProviderCredentialIsSignIn         = "model_provider_credential_is_sign_in"        // this provider's kind is a sign-in, not a pasted key/token
	reasonModelProviderCredentialBodyInvalid      = "model_provider_credential_body_invalid"      // the body did not decode, or value is empty
	reasonModelProviderCredentialTooShort         = "model_provider_credential_too_short"         // the value is below the mask's minimum length
	reasonModelProviderCredentialStoreUnavailable = "model_provider_credential_store_unavailable" // the secret store did not answer (transient)
)

// /api/v1/admin/audit (audit.go): the security tier's audit-log query,
// export and chain-verification doors.
const (
	reasonAuditInvalidRunID                = "audit_invalid_run_id"
	reasonAuditExportStoreUnavailable      = "audit_export_store_unavailable"
	reasonAuditExportReadFailed            = "audit_export_read_failed"
	reasonAuditScopeUnavailable            = "audit_scope_unavailable"
	reasonAuditChainVerifyStoreUnavailable = "audit_chain_verify_store_unavailable"
	reasonAuditChainVerifyBusy             = "audit_chain_verify_busy"
	reasonAuditChainSweepFailed            = "audit_chain_sweep_failed"
	reasonAuditInvalidTimestampParam       = "audit_invalid_timestamp_param"
	reasonAuditInvalidActorType            = "audit_invalid_actor_type"
	reasonAuditInvalidOrigin               = "audit_invalid_origin"
)

// POST /api/v1/sources/{id}/scan and the admin bulk scan (source_scan.go).
const (
	reasonSourceNotFound            = "source_not_found"
	reasonSourceScanAlreadyRunning  = "source_scan_already_running"
	reasonSourceScanUnsupportedKind = "source_scan_unsupported_kind" // this source kind has nothing to scan
	// reasonSourceScanFailed is scanLocalDirSource's own bucket: whatever
	// `detail` names, the scan itself did not complete.
	reasonSourceScanFailed   = "source_scan_failed"
	reasonSourceScanNoRunner = "source_scan_no_runner"
)

// POST /runs and POST /runs/preflight's policy resolution (inline_policy.go):
// resolveRunPolicy and boundUserSpec, shared by create and the dry-run
// preview so the two can never disagree.
const (
	reasonInlinePolicyXOR  = "inline_policy_xor" // policy_id and inline_policy were both set
	reasonPolicyIDNotFound = "policy_id_not_found"
	// reasonInlinePolicyInvalid is the whole resolution chain's bucket
	// (grant filtering, domain-count cap, validatePolicySpec,
	// validateInlineSecretRefs — inline or stored): one cause, "this
	// policy/spec fails validation", the same grain as reasonSiteConfigInvalid.
	reasonInlinePolicyInvalid = "inline_policy_invalid"
)

// GET/PUT /api/v1/ui-layout (ui_layout.go): the console's own saved-layout
// door.
const (
	reasonUILayoutInvalidPreset          = "ui_layout_invalid_preset"
	reasonUILayoutTooManyWidgets         = "ui_layout_too_many_widgets"
	reasonUILayoutUnknownWidget          = "ui_layout_unknown_widget"
	reasonUILayoutInvalidGeometry        = "ui_layout_invalid_geometry"
	reasonUILayoutPersistenceUnavailable = "ui_layout_persistence_unavailable"
)

// /scm/azure-devops/signin and its callback (ado_entra.go): the console's own
// Azure DevOps per-person sign-in doors, distinct from the ADOEntraFailure
// enum a REDEMPTION classifies as (ado_entra_store.go, its own documented
// guard exception). DELETE /scm/azure-devops/connection (ado_pat_console.go)
// reuses the no-session and unconfigured values.
const (
	reasonADOSignInUnconfigured = "ado_sign_in_unconfigured"
	reasonADOSignInForeignApp   = "ado_sign_in_foreign_app"
	reasonADOSignInNoSession    = "ado_sign_in_no_session"
	reasonADOSignInScopeInvalid = "ado_sign_in_scope_invalid"
	// reasonADOSignInPromptInvalid: ?prompt= (adoRequestedPrompt) is set to
	// anything other than "" or "select_account" — landed on main (#659 Q2)
	// after this branch was cut, caught by the merge's own guard re-run
	// (#656 final review round).
	reasonADOSignInPromptInvalid = "ado_sign_in_prompt_invalid"
	// ReasonADOPATNeedsConsoleApp is S1 (ErrADOMintNeedsSecret): a minted_pat
	// row the console cannot redeem with its own secret. Exported for the boot
	// log in cmd/wardynd.
	ReasonADOPATNeedsConsoleApp = "ado_pat_needs_console_app"
)

// GET /model-providers-entra/signin and the callback it shares with the Azure
// DevOps sign-in (azure_foundry_entra.go): the per-row door of the Azure
// Foundry capture. The callback answers a refusal the person can act on as a
// redirect with a fixed code (the vocabulary of the Azure DevOps callback plus
// row_changed) and an attack-shaped one in band.
const (
	reasonAzureSignInUnconfigured     = "azure_sign_in_unconfigured"     // no console Entra sign-in is configured
	reasonAzureSignInUnknownRow       = "azure_sign_in_unknown_row"      // the uid is not an azure_foundry provider
	reasonAzureSignInNoSession        = "azure_sign_in_no_session"       // no session subject to bind the capture to
	reasonAzureCallbackCookiesInvalid = "azure_callback_cookies_invalid" // the one-time nonce or verifier cookie is missing, or the stamped row is malformed
	reasonAzureCallbackMissingCode    = "azure_callback_missing_code"    // the authority redirected back with no code
)

// POST /workspace-providers/git/{id}/org-check (ado_pat_orgcheck.go).
const (
	reasonADOOrgCheckUnknownRow   = "ado_org_check_unknown_row"  // no such row, or not the row that creates tokens (D-6)
	reasonADOOrgCheckOrganisation = "ado_org_check_organisation" // the row names no organisation and the request named none it serves
)

// PUT/DELETE /me/scm/azure-devops/token (ado_own_pat.go): a person adding or
// removing their own Azure DevOps token. A caller with no session subject is
// answered with reasonADOSignInNoSession, the same cause at the sign-in door.
const (
	reasonADOOwnPATUnknownRow       = "ado_own_pat_unknown_row"       // no own-token row the caller may use has this address
	reasonADOOwnPATTokenInvalid     = "ado_own_pat_token_invalid"     // the pasted value is empty, too long, or has spaces
	reasonADOOwnPATExpiryInvalid    = "ado_own_pat_expiry_invalid"    // expires_on is not a date after today
	reasonADOOwnPATExpiryTooLong    = "ado_own_pat_expiry_too_long"   // expires_on is past the row's pat_max_days
	reasonADOOwnPATRejected         = "ado_own_pat_rejected"          // Azure DevOps did not accept the token for the organisation
	reasonADOOwnPATIdentityMismatch = "ado_own_pat_identity_mismatch" // the token belongs to another account (never named)
	reasonADOOwnPATCheckUnavailable = "ado_own_pat_check_unavailable" // Azure DevOps could not be asked; nothing is known
	reasonADOOwnPATRequestRefused   = "ado_own_pat_request_refused"   // Azure DevOps answered 400: it refused the request, not the token
)

// The own-token arm of the Azure DevOps injection resolve
// (runs_dispatch_ado_own_pat.go), beside reasons.go's Azure DevOps resolve set.
const (
	reasonADOOwnPATNotAdded = "ado_own_pat_not_added" // the run's owner has no token of their own stored for the row
	reasonADOOwnPATExpired  = "ado_own_pat_expired"   // the owner's token has reached the expiry they entered
	reasonADOOwnPATOtherOrg = "ado_own_pat_other_org" // the owner's token is for a different organisation than the run's
)

// The callback half of the same door (consumeADOCookies, handleADOCallback):
// browser-reachable (the identity provider's own redirect lands here), and
// previously answered with a bare http.Error — no JSON body, no reason at
// all. reasonADOCallbackCookiesInvalid covers
// all three single-use state/nonce/pkce cookie causes as one bucket: the
// remedy is identical for all three (start the sign-in again from Settings),
// so there is nothing a caller could do differently by telling them apart.
// reasonADOCallbackIdentityBinding and reasonADOCallbackUnusableGrant reuse
// the EXACT strings this callback's own auditADOCapture rows already carried
// for these two causes, unchanged by this fix; its store-write failure reuses
// reasonStoreError directly rather than a third name for "a store errored".
const (
	reasonADOCallbackCookiesInvalid  = "ado_callback_cookies_invalid"
	reasonADOCallbackMissingCode     = "ado_callback_missing_code"
	reasonADOCallbackIdentityBinding = "identity_binding"
	reasonADOCallbackUnusableGrant   = "unusable_grant"
)

// POST /api/v1/me/view (user_view.go): the admin/security-admin user-view
// toggle.
const (
	reasonUserViewNoHuman      = "user_view_no_human"
	reasonUserViewInvalidField = "user_view_invalid_field"
	reasonUserViewTypeInvalid  = "user_view_type_invalid"
	reasonUserViewNoSession    = "user_view_no_session"
)

// /api/v1/sources (sources.go): the shared source library.
const (
	reasonSourceWriteInvalid   = "source_write_invalid" // validateSourceWrite's own bucket
	reasonSourceDeleteConflict = "source_delete_conflict"
	reasonSourceInUse          = "source_in_use"
)

// /api/v1/admin/branding (branding.go).
const (
	reasonBrandingNotBranded       = "branding_not_branded"
	reasonBrandingStoreUnavailable = "branding_store_unavailable"
	reasonBrandingBodyUnreadable   = "branding_body_unreadable"
)

// writeServerError's own classified/unclassified split (writeservererror.go):
// the ONE 5xx chokepoint ~149 sites in this package were written against
// before it existed. reasonInternalError is deliberately the single generic
// fallback for everything writeServerError does not otherwise classify —
// never the driver text err carries (that stays in the log line, not the
// wire), just enough for a caller to tell "server-side, not yours" from a
// specific classified cause.
const (
	reasonOrgRevoked    = "org_revoked"
	reasonInternalError = "internal_error"
)

// GET /api/v1/directory/search (directory_search.go).
const (
	reasonDirectorySearchQueryTooShort = "directory_search_query_too_short"
	reasonDirectorySearchUnknownType   = "directory_search_unknown_type"
	reasonDirectorySearchRateLimited   = "directory_search_rate_limited"
	reasonDirectorySearchFailed        = "directory_search_failed"
)

// /api/v1/base-images (base_images.go).
const (
	reasonBaseImageWriteInvalid = "base_image_write_invalid" // validateBaseImageWrite's own bucket
	reasonBaseImageInUse        = "base_image_in_use"
	reasonBaseImageNotFound     = "base_image_not_found"
)

// DELETE /people/{principal}/credentials (credential_erase.go).
const (
	reasonCredentialErasePrincipalRequired = "credential_erase_principal_required"
	reasonCredentialEraseOperatorNamespace = "credential_erase_operator_namespace"
	// The Azure DevOps sign-in's configuration could not be read, so the erase
	// could not take the sign-in's lock and refused (#1478).
	reasonCredentialEraseSignInConfigUnreadable = "credential_erase_signin_config_unreadable"
)

// GET /permissions/explain (capabilities_explain.go).
const reasonExplainPrincipalInvalid = "explain_principal_invalid"

// GET /admin/credentials/inventory (credential_inventory.go).
const reasonCredentialInventoryNoMeta = "credential_inventory_no_meta"

// PUT /internal/recordings/{runID} (recording.go).
const (
	reasonRecordingStoreUnavailable = "recording_store_unavailable"
	reasonRecordingTooLarge         = "recording_too_large"
	reasonRecordingInvalidPart      = "recording_invalid_part" // {part} is not canonical decimal >= 2 (handleUploadRecordingPart)
	// reasonRecordingPartLimit is recording.upload's ONLY name for a part
	// above types.RecordingMaxParts — the wire reason AND the nested
	// audit-detail field this refusal's own recording.upload row carries
	// (#656 final review round L3: recording.go used to declare a second,
	// separately-drifting const for the identical fact; deleted in favor of
	// this one).
	reasonRecordingPartLimit = "part_limit"
)

// The Azure DevOps escalation's decision rule (injection_ado_capability.go).
const (
	reasonADODecisionScopeInvalid = "ado_decision_scope_invalid"
	reasonADOAccessAboveCeiling   = "ado_access_above_ceiling"
)

// reasonReservedPrincipal is the SAME value as authFailedReservedPrincipal
// (oidc.DenialReservedPrincipal) — a literal here so the docs guard, which
// only reads this file, can see it (#656 slice 3).
const reasonReservedPrincipal = "reserved_principal"

// /internal/scan-results/{runID} (scanresult.go).
const (
	reasonScanFactsInvalid     = "scan_facts_invalid"
	reasonScanUploadSuperseded = "scan_upload_superseded"
)

// POST /policies/grade (policy_grade.go) — a dry-run grading preview,
// distinct from the real policy CRUD door (reasonPolicyRequestInvalid) even
// though both run validatePolicySpec.
const reasonPolicyGradeSpecInvalid = "policy_grade_spec_invalid"

// The AI Run Composer's profile synthesis (profile.go).
const reasonSynthesizedProfileInvalid = "synthesized_profile_invalid"

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

// Moved here from beside their own call sites (#656 final review round):
// each was already a named, wire-reaching const, just declared in a file
// TestReasonDocsMatchReasonsGo never reads. No call site changes — a Go
// const's visibility is package-wide regardless of which file declares it.

// PUT /api/v1/branding (branding.go): the named reasons a refused write
// carries, one per validation rule.
const (
	brandReasonOrgName    = "invalid_org_name"
	brandReasonNameFormat = "invalid_name_format"
	brandReasonColour     = "invalid_colour"
	brandReasonContrast   = "low_contrast"
	brandReasonLink       = "link_not_https"
	brandReasonLinkShape  = "invalid_link"
	brandReasonLogoSize   = "logo_too_large"
	brandReasonLogo       = "invalid_logo"
	// brandReasonLogoFromFile: remove_logo on a logo the site config delivers (#1215).
	brandReasonLogoFromFile = "logo_from_site_config"
)

// The CSRF guard's own refusal (csrf.go, http.go's local-mode arm, attach.go):
// the SAME reason its auth.fail audit row already carried.
const csrfAuditReason = "cross_origin_refused"

// llmRefusalAuditReason is POST /runs' model-credential refusal CLASS
// (runs_dispatch_llm_mechanism.go) — a wire value the console grades an
// ending by (lib/api/audit.ts's CREDENTIAL_REASON), not ordinary copy.
const llmRefusalAuditReason = "model_credential"

// gitCredentialRefusalReason is the 422 the New Run rail recognises
// (scmaccess.go, the 0.7.7 relaunch path).
const gitCredentialRefusalReason = "git_credential"

// The UI-sandbox relay session's own re-check refusals (uigateway_session.go)
// that are not already covered by an existing value. The fourth member of
// this closed set, "run row unreadable", reuses reasonRunUnreadable (above)
// directly rather than a second name for the same cause.
const (
	uiDeniedReasonNotAuthorized         = "not_authorized"
	uiDeniedReasonRevoked               = "revoked"
	uiDeniedReasonRevocationUnavailable = "revocation_unavailable"
	// A session opened through a portal ends with that portal's grant (#1475):
	// the portal was revoked or the grant expired, or the store could not say.
	uiDeniedReasonDelegationEnded       = "delegation_ended"
	uiDeniedReasonDelegationUnavailable = "delegation_unavailable"
)
