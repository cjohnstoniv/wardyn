// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The reasons of snapshots and forks of Linux VM sandboxes: the same closed set and convention as reasons.go, in a
// file of their own beside the snapshot code. TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonSnapshotsOff                   = "snapshots_off"                     // 409: features.machine_snapshots is off
	reasonSnapshotBusy                   = "snapshot_busy"                     // 409: another snapshot of the run is being made
	reasonSnapshotNoSpace                = "snapshot_no_space"                 // the runner has too little free disk for the copy
	reasonSnapshotSecretsPresent         = "snapshot_secrets_present"          // the run held a secret, or a fact could not be read; the reasons list says which
	reasonSnapshotMachineKindUnsupported = "snapshot_machine_kind_unsupported" // 409: only a Linux VM run is snapshotted or forked
	reasonSnapshotInUse                  = "snapshot_in_use"                   // 409: a fork uses the snapshot, so it cannot be deleted
	reasonForkContextChanged             = "fork_context_changed"              // the fork's unit or workspaces are no longer the snapshot's
	reasonForkImageNotAdmitted           = "fork_image_not_admitted"           // the snapshot's image has no digest, or the digest is no longer admitted
	reasonForkBaseChanged                = "fork_base_changed"                 // the runner no longer has the image base the snapshot backs onto
	reasonForkNotDelegable               = "fork_not_delegable"                // 403: a delegated token or a portal grant asked for a fork
	reasonForkSupportSession             = "fork_support_session"              // 403: a support helper was let into the snapshot's source run
	// Failure values: a snapshot's failure, or why a fork or a run was not launched.
	reasonSnapshotSourceEnded       = "snapshot_source_ended"       // the run ended while its snapshot was being made
	reasonSnapshotCommitUnconfirmed = "snapshot_commit_unconfirmed" // the runner never confirmed it kept the copy
	reasonSnapshotOrphaned          = "snapshot_orphaned"           // the snapshot was left being made with nothing to finish it
	reasonForkSnapshotUnavailable   = "fork_snapshot_unavailable"   // the fork's snapshot was no longer ready when its dispatch claimed it
	reasonRunSecretFactsUnrecorded  = "run_secret_facts_unrecorded" // the dispatch could not record the facts a snapshot of the run is checked against
)
