// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// statusDetailWriteTimeout bounds ONE startup-detail UPDATE. The callback that
// makes it is SYNCHRONOUS on CreateSandbox's own goroutine, dispatchRun runs
// under context.WithoutCancel, and store.execRun hands its context straight to
// the pool — so a locked row or an exhausted pool would park the driver's 200ms
// poll on a diagnostic write while the canary's three-minute budget burned.
// Startup progress must never depend on startup DIAGNOSTICS being persistable:
// an overdue write is dropped, and it is dropped IN PLACE rather than handed to
// a goroutine, which would only move an unbounded pile-up somewhere less
// visible.
const statusDetailWriteTimeout = 500 * time.Millisecond

// runStatusDetailSetter is the OPTIONAL store capability the startup-detail
// writer needs. Kept off the core store.Store interface for the reason
// runFailureHintSetter states: the test doubles that embed store.Store do not
// implement it, and a core method would nil-panic in every one of them. The real
// PG store implements it; a store that does not simply never reports.
type runStatusDetailSetter interface {
	SetRunStatusDetail(ctx context.Context, id uuid.UUID, detail string) error
}

// runStatusDetailWriter builds this run's runner.SandboxSpec.OnWaiting, or
// returns nil when the store cannot record one — in which case every driver sees
// exactly the spec it saw before 0.7.6.
//
// One scoped UPDATE per CHANGE of reason. The driver already dedupes per pod,
// and this dedupes across the two pods CreateSandbox waits on in turn, so the
// same reason arriving from the proxy's wait and then the agent's costs one
// write rather than two. No mutex: the contract on OnWaiting is that it is
// called synchronously on CreateSandbox's goroutine, one call at a time.
func (s *Server) runStatusDetailWriter(ctx context.Context, runID uuid.UUID) func(string) {
	setter, ok := s.cfg.Store.(runStatusDetailSetter)
	if !ok {
		return nil
	}
	var last string
	return func(detail string) {
		if detail == "" || detail == last {
			return
		}
		last = detail
		wctx, cancel := context.WithTimeout(ctx, statusDetailWriteTimeout)
		defer cancel()
		if err := setter.SetRunStatusDetail(wctx, runID, detail); err != nil {
			// Debug, and discarded: a lost status line is a lost sentence on a
			// screen, never a reason to fail a dispatch.
			slog.DebugContext(ctx, "wardynd: could not persist run status detail",
				slog.String("run_id", runID.String()), slog.Any("err", err))
		}
	}
}

// projectStatusDetail decides what a run's startup detail SAYS to a reader, on
// every route that serves a run. It is a read-side projection, beside
// projectRecordingMeta, because status_detail is never cleared by a write: the
// column keeps the last reason for a postmortem, and honesty on the wire is this
// function's job alone.
//
//   - STARTING: the reason as stored, plus the derived token.
//   - FAILED on a TERMINAL reason: kept. waitContainerRunning errors the instant
//     the kubelet says ImagePullBackOff, and dispatch marks the run FAILED
//     between two of the browser's 3–4 second polls, so a reader who never
//     caught a STARTING poll would otherwise lose the one reason that names the
//     fix. The reason is taken from the detail, or — when the detail never
//     landed — from the canary's own "container stuck waiting (<Reason>)"
//     sentence in failure_hint.
//   - anything else, including a FAILED run whose last wait was ordinary:
//     blanked. The run did not fail BECAUSE it was creating a container, and
//     presenting a stale wait as a cause is the misdiagnosis this whole field
//     exists to end.
func projectStatusDetail(runs []types.AgentRun) {
	for i := range runs {
		r := &runs[i]
		reason := statusDetailReason(r.StatusDetail)
		switch {
		case r.State == types.RunStarting:
			r.StatusReason = reason
		case r.State == types.RunFailed:
			if reason == "" {
				reason = stuckReasonFromFailureHint(r.FailureHint)
			}
			if !runner.IsTerminalWaitingReason(reason) {
				r.StatusDetail = ""
				continue
			}
			r.StatusReason = reason
		default:
			r.StatusDetail = ""
		}
	}
}

// statusDetailReason pulls the bare reason token out of a
// "<component>: <Reason>[: <message>]" detail. The first two colons delimit it:
// a component name and a kubelet reason never contain one, and everything after
// the second is the platform's message, which routinely does ("rpc error: code =
// Unknown desc = …"). Empty for a detail that is not in that shape at all,
// which is how a pre-0.7.6 row or an unexpected substrate string degrades — the
// console still renders the raw string, it just gets no token.
func statusDetailReason(detail string) string {
	_, rest, ok := strings.Cut(detail, ": ")
	if !ok {
		return ""
	}
	reason, _, _ := strings.Cut(rest, ": ")
	return strings.TrimSpace(reason)
}

// stuckReasonFromFailureHint recovers the reason from the sentence
// waitContainerRunning fails with ("agent container stuck waiting
// (ImagePullBackOff): …"), which failAndRevoke stamps onto failure_hint. It is
// the fallback for the ordering race where the run is marked FAILED before the
// last status write landed: the same fact, reached through the field that did
// survive.
func stuckReasonFromFailureHint(hint string) string {
	_, rest, ok := strings.Cut(hint, "container stuck waiting (")
	if !ok {
		return ""
	}
	reason, _, ok := strings.Cut(rest, ")")
	if !ok {
		return ""
	}
	return reason
}
