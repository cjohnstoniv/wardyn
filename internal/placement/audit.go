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
	ActionRunnerPostureRecord     = "runner.posture.record"
	ActionRunnerResidentErase     = "runner.resident.erase"
	ActionCredentialDeliveryWrite = "credential.delivery.write"
	ActionSSHSyncTransfer         = "ssh.sync.transfer"
	ActionRunnersEnabledSet       = "runners.enabled.set"
)

// AuditActions lists them all.
var AuditActions = []string{
	ActionRunnerTokenCreate, ActionRunnerTokenRevoke, ActionRunnerEnrol, ActionRunnerClaim, ActionRunnerRevoke,
	ActionRunnerConnect, ActionRunnerDisconnect, ActionRunnerActionRequest, ActionRunnerActionApply,
	ActionRunnerPostureRecord, ActionRunnerResidentErase, ActionCredentialDeliveryWrite,
	ActionSSHSyncTransfer, ActionRunnersEnabledSet,
}
