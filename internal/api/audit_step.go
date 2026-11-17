// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// auditStep returns step for an audit row's `error` field and logs err beside it. A store, driver or runner
// error can name hosts, users, databases and bytes a sandbox chose, so the row says which step failed and the
// server log keeps the cause. attrs are slog attributes that say which run or workspace it was.
func auditStep(ctx context.Context, step string, err error, attrs ...any) string {
	slog.ErrorContext(ctx, "wardynd: an audit row names this failed step; the cause is here",
		append(attrs, slog.String("step", step), slog.Any("err", err))...)
	return step
}

// selftestUnsupported reports whether err is a runner's refusal of a selftest because it is too old to run one.
// Only that sentence is matched: other refusals can format a daemon's, a keystore's or a member's path's text.
func selftestUnsupported(err error) bool {
	var we *runnerwire.Error
	return errors.As(err, &we) && we.Code == runnerwire.CodeRefused && we.Message == remote.SelftestUnsupportedMsg
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
	auditStepPersistAWSSSO      = "Persist renewed AWS SSO credential"
	auditStepPersistExecID      = "Persist agent exec id"
	auditStepPolicyBuild        = "Build agent policy"
	auditStepPrewrapArches      = "Record prewrapped image architectures"
	auditStepProbeSecretWrite   = "Probe secret store write"
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
	auditStepSSHExec            = "Open ssh exec"
	auditStepSSHForward         = "Open ssh forward"
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
		// A refusal wrapping a cause (run_inner_walls.go) keeps the cause in the log, not the row.
		if err.Error() != string(refusal) {
			slog.ErrorContext(ctx, "wardynd: an agent policy refusal had a cause",
				slog.String("run_id", runID.String()), slog.Any("err", err))
		}
		return string(refusal)
	}
	return auditStep(ctx, auditStepPolicyBuild, err, slog.String("run_id", runID.String()))
}

// The failed step an assignment-form row names (data["error"] = step), beside the consts above.
const (
	auditStepClaimDispatch   = "Claim run for dispatch"
	auditStepContainStop     = "Stop kept run's agent"
	auditStepDeleteSSHKeys   = "Delete SSH keys"
	auditStepGrantScope      = "Read api_key grant scope"
	auditStepMarkRunning     = "Mark run running"
	auditStepReplaceThenRest = "Replace run's proxy, then restore its lost mark"
	auditStepReplaceThenUndo = "Replace run's proxy, then undo its policy generation"
	auditStepReviveReplace   = "Replace run's proxy"
	auditStepReviveStart     = "Start revived agent"
	auditStepRevokeSessions  = "Revoke sessions"
	auditStepRunnerCaps      = "Read runner capabilities"
	auditStepSaveRecording   = "Save recording"
	auditStepSignOut         = "Sign out session"
	auditStepStopSandbox     = "Stop sandbox"
	auditStepStopAgain       = "Stop agent again"
	auditStepThawAgent       = "Thaw agent and confirm it runs"
	auditStepTrackBuild      = "Track run build"
	auditStepPolicyGenRecord = "Record policy generation"
	auditStepPolicyGenUndo   = "Undo policy generation"
)

// The failed step a per-step failure key names (a run.revoke, run.kill, run.ended, run.lost, run.pause or
// run.reconcile row's `*_error`, and run.build's `cache_warn`), beside the consts above.
const (
	auditStepCancelApprovals  = "Cancel run approvals"
	auditStepCheckRunTokens   = "Check unrevoked run tokens"
	auditStepDropProxyConfig  = "Drop run proxy config"
	auditStepEndSandbox       = "End sandbox"
	auditStepKillSandbox      = "Kill sandbox"
	auditStepReadFolderRecord = "Read computer folder record"
	auditStepReadLostEnd      = "Read lost run's end"
	auditStepRecordImage      = "Record workspace image"
	auditStepRevokeBroker     = "Revoke broker credentials"
	auditStepRevokeIdentity   = "Revoke run identity"
	auditStepSetSandboxRef    = "Record sandbox ref"
	auditStepStopFailedProxy  = "Stop failed proxy start"
	auditStepStopLost         = "Stop lost sandbox"
	auditStepCutAgent         = "Cut run's agent off" // a fence or a proxy stop, the sandbox left as it is
)

// auditEndError is an end or lost row's `end_error` or `lost_error`: the sentence of a substrate that cannot keep
// a sandbox (runner.ErrEndUnsupported, which the code wrote), else the step, with the cause in the log.
func auditEndError(ctx context.Context, runID uuid.UUID, step string, err error) string {
	if errors.Is(err, runner.ErrEndUnsupported) {
		if err.Error() != runner.ErrEndUnsupported.Error() { // joined with another error: the log keeps it
			slog.ErrorContext(ctx, "wardynd: an end the substrate cannot keep had a cause", slog.String("run_id", runID.String()), slog.String("step", step), slog.Any("err", err))
		}
		return runner.ErrEndUnsupported.Error()
	}
	return auditStep(ctx, step, err, slog.String("run_id", runID.String()))
}
