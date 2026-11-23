// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore U1000 The move's executor, a later change, answers most of these; until it lands a move is
// refused before any of them can be reached, and the whole set is declared and documented up front.

package api

// The reasons of moving a paused run to another machine: the same closed set and convention as reasons.go, in a
// file of its own beside the move's code. TestReasonDocsMatchReasonsGo reads this file too. A value marked
// "failure" is never an HTTP answer: it is a move's recorded failure, shown on GET /runs/{id}/placements and on
// its run.move audit row.
const (
	reasonMoveUnavailable            = "move_unavailable"              // 409: moving a run is off in site config, or this release cannot turn it on
	reasonMoveNotRunning             = "move_not_running"              // 409: the run is not running, a Resume holds it, or it is still stopped where a move left it
	reasonMoveNotInteractive         = "move_not_interactive"          // 409: only an interactive run moves
	reasonMoveKeptMachine            = "move_kept_machine"             // 409: a run that keeps its machine does not move
	reasonMoveHarnessLogin           = "move_harness_login"            // 409: a run someone signed in to a harness inside does not move
	reasonMoveMachineKindUnsupported = "move_machine_kind_unsupported" // 409: a macOS, Linux VM or Windows VM run
	reasonMoveSpecUnrecorded         = "move_spec_unrecorded"          // 409: the run has no recorded sandbox spec
	reasonMovePauseRefused           = "move_pause_refused"            // 409, and a failure: the run cannot be stopped, or its pause's revoke is not complete
	reasonMoveLocallyContained       = "move_locally_contained"        // 409, and a failure: the run is held contained on its runner
	reasonMoveContainmentUnreadable  = "move_containment_unreadable"   // 503: whether the run is contained could not be read
	reasonMovePolicyUnreadable       = "move_policy_unreadable"        // 503: a read the destination's judgement needs failed; nothing was stopped
	reasonMoveBusy                   = "move_busy"                     // 409: a move holds the run, or too many moves are in flight
	reasonMoveRunnerTooOld           = "move_runner_too_old"           // 409: a runner cannot carry a move
	reasonMoveDestinationSubstrate   = "move_destination_substrate"    // 409: the destination cannot hold an imported, unstarted sandbox
	reasonMoveSitesOff               = "move_sites_off"                // 409: a site was named and sites are off
	reasonMoveDestinationUnknown     = "move_destination_unknown"      // 422: the destination names nothing the run's owner can use
	reasonMoveToPersonalOff          = "move_to_personal_off"          // 403: moving onto a person's own runner is off in site config
	reasonMoveDestinationRefused     = "move_destination_refused"      // 403: the destination's policy refuses the run; the message names the layer
	reasonMoveResidentCredential     = "move_resident_credential"      // 403: the run holds a credential where it runs
	reasonMoveMCPToken               = "move_mcp_token"                // 403: the run has a per-run MCP token
	reasonMovePolicyChanged          = "move_policy_changed"           // failure: the destination's judgement changed before commit
	reasonMoveSourceUnsettled        = "move_source_unsettled"         // failure: the source runner had not settled the run
	reasonMoveSourceSpoolPending     = "move_source_spool_pending"     // failure: the source runner had not delivered the run's spool
	reasonMoveArchiveRefused         = "move_archive_refused"          // failure: an entry of the carried files was refused
	reasonMoveCarriedTooLarge        = "move_carried_too_large"        // failure: the carried files passed a size bound
	reasonMoveScanUncovered          = "move_scan_uncovered"           // failure: the run's masking manifest does not cover it
	reasonMoveScanUnreadable         = "move_scan_unreadable"          // failure: a git file could not be decoded
	reasonMoveScanHit                = "move_scan_hit"                 // failure: a secret value was found in the carried files
	reasonMoveGitOutsideRoots        = "move_git_outside_roots"        // failure: a git directory points outside what is carried
	reasonMoveDigestMismatch         = "move_digest_mismatch"          // failure: the carried files' digests differ
	reasonMovePlainLink              = "move_plain_link"               // failure: a runner link is not TLS
	reasonMoveRunEnded               = "move_run_ended"                // failure: the run was killed, ended or lost during the move
	reasonMovePauseChanged           = "move_pause_changed"            // failure: a Resume took the run during the move
	reasonMoveOrphaned               = "move_orphaned"                 // failure: the move's executor stopped answering
	reasonMoveStopFailed             = "move_stop_failed"              // failure, and 503 for a run already paused: the run could not be stopped
	reasonMoveCreateFailed           = "move_create_failed"            // failure: the destination's sandbox could not be created
	reasonMoveExportFailed           = "move_export_failed"            // failure: the source's files could not be read
	reasonMoveRelayFailed            = "move_relay_failed"             // failure: the stream broke between a runner and the server
	reasonMoveImportFailed           = "move_import_failed"            // failure: the destination could not take the files
	reasonMoveLeaseRefused           = "move_lease_refused"            // failure: the destination runner's lease was refused
	reasonMoveIdentityRefused        = "move_identity_refused"         // failure: the destination's identity could not be minted
	reasonMoveRevokeRefused          = "move_revoke_refused"           // 409, and a failure: a source credential's revoke was refused
)
