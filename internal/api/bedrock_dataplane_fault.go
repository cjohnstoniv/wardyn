// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Bedrock data-plane refusals after dispatch (the proxy's upstream_fault,
// internal/egress/proxy/bedrock_fault.go). An agent whose model call AWS
// refuses exits non-zero, and a FAILED-by-exit-code run carries no hint of its
// own — so the owner saw "exit 1" and nothing about the AWS policy or quota
// that caused it.
//
// The sentence is written to failure_hint AS THE DECISION ARRIVES, not when the
// run ends: the completion watcher can be on another replica, or the daemon can
// restart in between, and failure_hint is the one durable per-run line a
// reader already sees. projectFailureHint keeps it off every run that did not
// end FAILED, and a "recovered" row clears it, so a throttle the SDK's retry
// cleared never outlives that retry.
var bedrockFaultHints = map[string]string{
	"AccessDeniedException": "Amazon Bedrock refused the model call (AccessDeniedException): a policy denies it — " +
		"an AWS Organizations service control policy or an IAM policy on the role — or model access is not " +
		"enabled for this account. Ask your AWS administrator to allow bedrock:InvokeModel for this model.",
	"ThrottlingException": "Amazon Bedrock is throttling this model (ThrottlingException): the account's request " +
		"quota was still exhausted after the agent's own retries. Try again later, or ask your AWS administrator " +
		"for a higher Bedrock quota.",
}

// noteBedrockDataPlaneFault records fault (a class, or "recovered") on the
// run's failure_hint. Best-effort, like every other hint write.
func (s *Server) noteBedrockDataPlaneFault(ctx context.Context, runID uuid.UUID, fault string) {
	setter, ok := s.cfg.Store.(runFailureHintSetter)
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return
	}
	_, ours := bedrockFaultHintSet[run.FailureHint]
	hint, known := bedrockFaultHints[fault]
	switch {
	case fault == "recovered":
		if !ours {
			return // never clear a hint this file did not write
		}
		hint = ""
	case !known:
		return
	case isTerminalRunState(run.State) &&
		!(run.State == types.RunFailed && (run.FailureHint == "" || ours)):
		// A late row for a run that already ended some other way, or whose
		// failure already has its own reason: leave it. A FAILED run with no
		// hint is the watcher having won the race with this row's post.
		return
	}
	if err := setter.SetRunFailureHint(ctx, runID, hint); err != nil {
		slog.WarnContext(ctx, "wardynd: could not persist bedrock data-plane hint",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// bedrockFaultHintSet is bedrockFaultHints' values, for "is this hint ours".
var bedrockFaultHintSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(bedrockFaultHints))
	for _, h := range bedrockFaultHints {
		m[h] = struct{}{}
	}
	return m
}()

// projectFailureHint blanks failure_hint on every run that is not FAILED. Only
// the FAILED transitions wrote it before the Bedrock hint above, which is
// written while the run is still RUNNING and may belong to a run that then
// completed.
func projectFailureHint(runs []types.AgentRun) {
	for i := range runs {
		if runs[i].State != types.RunFailed {
			runs[i].FailureHint = ""
		}
	}
}
