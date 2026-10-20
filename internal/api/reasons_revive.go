// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The revive-refusal reasons, split out of reasons_routes.go to keep that file
// under scripts/check-file-size.sh's 1000-line gate: the same closed set and
// the same convention as reasons.go, not a second vocabulary.
// TestReasonDocsMatchReasonsGo and TestNoAdHocAuthz read this file too.

// POST /runs/{id}/revive and the admin bulk restart (run_revive.go):
// reviveError's own status+reason+msg replaces its former bare (status, msg)
// pair, so the SAME reason recordAudit already wrote to the audit row for a
// revive refusal (#656 slice 3) now also reaches the wire. One reason per
// distinct revive-refusal cause; a few call sites answer the SAME cause two
// ways (the runner substrate cannot revive at all) and share one on purpose.
const (
	reasonPolicyPreviewRateLimited    = "policy_preview_rate_limited"
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
	reasonReviveContainmentRecovering       = "revive_containment_recovering"         // the run's runner held it contained; a recovery is queued for its next session
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
	reasonReviveConfigDoesNotLoad           = "revive_config_does_not_load"           // the stored proxy config, or the one the revive would start the new proxy with, failed to parse or validate
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
	reasonReviveCeilingDeniesAzureCLI       = "revive_ceiling_denies_azure_cli"       // the owner's current governance profile now denies Azure Resource Manager, which the run's Azure CLI door needs
	reasonReviveOwnerAuthorityUnreadable    = "revive_owner_authority_unreadable"     // the owner's launch-door or model-credential re-check, or the read of the run's git_pat grants for its brokered set, could not be completed
	reasonReviveAdminRestartCountInvalid    = "revive_admin_restart_count_invalid"    // run_ids named none, or more than the bulk maximum
	reasonReviveProxyWindowStoreUnavailable = "revive_proxy_window_store_unavailable" // this store cannot list run proxy releases
	reasonRunCapacityStoreUnavailable       = "run_capacity_store_unavailable"        // this store cannot report fleet capacity
)
