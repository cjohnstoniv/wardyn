// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

// Audit action names the runner lanes emit (design §12.2). The rows are added to
// docs/AUDIT-ACTIONS.md by the lane that first emits each one.
const (
	ActionRunnerTokenCreate       = "runner.token.create"
	ActionRunnerTokenRevoke       = "runner.token.revoke"
	ActionRunnerEnrol             = "runner.enrol"
	ActionRunnerClaim             = "runner.claim"
	ActionRunnerRevoke            = "runner.revoke"
	ActionRunnerConnect           = "runner.connect"
	ActionRunnerDisconnect        = "runner.disconnect"
	ActionRunnerActionRequest     = "runner.action.request"
	ActionRunnerActionApply       = "runner.action.apply"
	ActionRunnerActionRefuse      = "runner.action.refuse"
	ActionRunnerPostureRecord     = "runner.posture.record"
	ActionRunnerResidentErase     = "runner.resident.erase"
	ActionCredentialDeliveryWrite = "credential.delivery.write"
	ActionSSHSyncTransfer         = "ssh.sync.transfer"
	ActionRunnersEnabledSet       = "runners.enabled.set"
	ActionRunnerInventoryHold     = "runner.inventory.hold"
	ActionRunnerUpdate            = "runner.update"
	ActionRunnerTrustWrite        = "runner.trust.write"
	// ActionRunnerRecoveryRequest: a revive queued recover_containment for a run its runner held contained.
	ActionRunnerRecoveryRequest = "runner.recovery.request"
	// ActionRunnerOfflineSummary: a runner that connected again reported why it could not before.
	ActionRunnerOfflineSummary = "runner.offline_summary.record"
)

// AuditActions lists them all.
var AuditActions = []string{
	ActionRunnerTokenCreate, ActionRunnerTokenRevoke, ActionRunnerEnrol, ActionRunnerClaim, ActionRunnerRevoke,
	ActionRunnerConnect, ActionRunnerDisconnect, ActionRunnerActionRequest, ActionRunnerActionApply, ActionRunnerActionRefuse,
	ActionRunnerPostureRecord, ActionRunnerResidentErase, ActionCredentialDeliveryWrite,
	ActionSSHSyncTransfer, ActionRunnersEnabledSet, ActionRunnerInventoryHold, ActionRunnerUpdate,
	ActionRunnerTrustWrite, ActionRunnerRecoveryRequest, ActionRunnerOfflineSummary,
}
