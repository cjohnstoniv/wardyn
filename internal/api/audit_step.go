// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
)

// auditStep returns step for an audit row's `error` field and logs err beside it. A store, driver or runner
// error can name hosts, users, databases and bytes a sandbox chose, so the row says which step failed and the
// server log keeps the cause. attrs are slog attributes that say which run or workspace it was.
func auditStep(ctx context.Context, step string, err error, attrs ...any) string {
	slog.ErrorContext(ctx, "wardynd: an audit row names this failed step; the cause is here",
		append(attrs, slog.String("step", step), slog.Any("err", err))...)
	return step
}

// The failed step a row's `error` field names, for the failures auditStep or a log already beside the row
// keeps the cause of. Each is a phrase a person reads, so each starts with a capital.
const (
	auditStepAttachOpen         = "Open attach exec"
	auditStepAWSSSORedeem       = "Redeem AWS SSO refresh token"
	auditStepBedrockInjectGrant = "Record Bedrock bearer inject grant"
	auditStepBuildRunImage      = "Build run image"
	auditStepCollectExec        = "Collect exec result"
	auditStepContainEndedRun    = "Cut ended run's agent"
	auditStepCreateSandbox      = "Create sandbox"
	auditStepDeleteExpired      = "Delete expired credential"
	auditStepDispatchCeiling    = "Resolve dispatch ceiling"
	auditStepExecOpen           = "Open exec"
	auditStepFailRun            = "Fail run"
	auditStepKubectlMint        = "Mint kubectl token"
	auditStepListRunGrants      = "List run grants"
	auditStepMarkRunComplete    = "Mark run complete"
	auditStepMITMCA             = "Provision MITM CA"
	auditStepPauseAgent         = "Pause agent"
	auditStepPauseGeneration    = "Record pause generation"
	auditStepPauseRecord        = "Record pause"
	auditStepPersistExecID      = "Persist agent exec id"
	auditStepPolicyBuild        = "Build agent policy"
	auditStepPrewrapArches      = "Record prewrapped image architectures"
	auditStepProxyConfig        = "Store proxy config"
	auditStepProxyStart         = "Record proxy start"
	auditStepReadRunFiles       = "Read run files"
	auditStepReadRunResources   = "Read run resources"
	auditStepRecordGrant        = "Record redirect grant"
	auditStepResumeAgent        = "Resume agent"
	auditStepRevokeToken        = "Revoke Azure DevOps run token"
	auditStepSelftestExec       = "Run selftest exec"
	auditStepSelftestWait       = "Wait for selftest"
	auditStepSFTPStart          = "Start sftp server"
	auditStepSSOInjectGrant     = "Record AWS SSO inject grant"
	auditStepStaleStart         = "Fail stale promoted run"
	auditStepStartAgentExec     = "Start agent exec"
	auditStepWaitAgentExec      = "Wait for agent exec"
)

// attachAuditError is a session.attach failure row's `error`: the sentence of a refusal the attach code wrote
// itself (the attach was cancelled, or the run's masking state could not be proven), else the step that failed.
func attachAuditError(ctx context.Context, runID uuid.UUID, err error) string {
	for _, sentinel := range []error{errAttachCancelled, errAttachUncovered} {
		if errors.Is(err, sentinel) {
			return sentinel.Error()
		}
	}
	return auditStep(ctx, auditStepAttachOpen, err, slog.String("run_id", runID.String()))
}

// desktopPauseFault is a desktop sign-in pause failure row's `error`: the sentence of a mark that changed nothing,
// else the step the stage names, with the cause in the log.
func desktopPauseFault(ctx context.Context, runID uuid.UUID, stage string, err error) string {
	if errors.Is(err, errDesktopSignInUnmarked) {
		return errDesktopSignInUnmarked.Error()
	}
	step := auditStepPauseAgent
	if stage == "record" {
		step = auditStepPauseRecord
	}
	return auditStep(ctx, step, err, slog.String("run_id", runID.String()))
}

// agentPolicyAuditError is a run.create failure row's `error` for an agent policy that could not be built: the
// refusal's sentence when the code wrote one (the run's own tool rules, or its runner's missing capability: the
// same sentence the run's failure hint carries), else the step, since a driver's error text can name a socket.
func agentPolicyAuditError(ctx context.Context, runID uuid.UUID, err error) string {
	var refusal agentPolicyRefusal
	if errors.As(err, &refusal) {
		return string(refusal)
	}
	return auditStep(ctx, auditStepPolicyBuild, err, slog.String("run_id", runID.String()))
}
