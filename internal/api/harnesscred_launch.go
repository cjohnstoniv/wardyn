// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The login launch, split at the run id.
//
// A sign-in launch used to run the WHOLE launch inside the request:
// CreateRun, the audit stamp, then dispatchRun — which blocks on CreateSandbox
// for as long as the substrate needs. On k8s that is canaryWaitTimeout (3 min,
// canary.go) ON TOP of a cold image pull; the reporting estate measured 131 s.
// The console's own deadline is 60 s (WFETCH_TIMEOUT_MS, lib/api/core.ts), so
// the pane showed the unreachable-daemon sentence, `setRunId` never ran, and
// Cancel had nothing to kill: the sandbox was orphaned to harnessLoginIdleCap.
//
// The split point is the run id. Everything the CALLER needs — the id it polls,
// attaches and cancels with, and the launch-time scope/pin stamp the capture
// upload binds to — is written by launchHarnessLoginRun before this file
// answers. Everything that can block is finishHarnessLoginLaunch's, on a
// context.WithoutCancel so the client's own disconnect cannot strand a
// half-provisioned sandbox.
//
// Why a kill landing during the pull is safe: dispatchRun claims
// PENDING->STARTING with a CAS (runs_dispatch.go), so a POST /runs/{id}/kill
// that wins the race makes dispatch ABORT rather than resurrect the run, and
// stampRunWatcherLease runs before CreateSandbox, so no reconcile sweep adopts
// a run that is merely pulling slowly.
//
// (This file exists rather than more of harnesscred.go because that file is one
// line under the 1000-line cap: see scripts/check-file-size.sh.)

// harnessLoginDispatch is what launchHarnessLoginRun composed and the detached
// tail below still needs. Named fields, not a second dispatchParams: these four
// are the login lane's own answers, and dispatchParams has a dozen more that a
// login box deliberately never sets (no injections, no repo, no verify plan).
type harnessLoginDispatch struct {
	RunToken string
	Image    string
	Policy   types.RunPolicySpec
	ExtraEnv map[string]string
}

// DRAFT (M2 canon pending) — the run's failure_hint when the dispatch ceiling
// cannot be resolved AFTER the run row exists. Synchronously this would be a
// 500 with no run to speak of; here the caller already holds a 200 and a run
// id, so the run itself has to carry the reason or it sits PENDING forever
// with no hint and no watcher lease. Mirrors dispatchRun's own
// unresolved-ceiling sentence (runs_dispatch.go), in this lane's words.
const harnessLoginCeilingUnresolved = "this sign-in sandbox was not launched: Wardyn could not resolve the governance ceiling that bounds it — try again, and tell your admin if it keeps failing"

// DRAFT (M2 canon pending) — the run's failure_hint when the detached launch
// PANICS. It needs its own sentence: the panic window is almost entirely inside
// dispatchRun, so the ceiling sentence above would tell an operator Wardyn could
// not resolve a ceiling it in fact resolved, which is both untrue and
// un-actionable. Names the class (an internal error, not their configuration)
// and where the detail is, since a panic's own text is a stack trace nobody
// should read off a console banner.
const harnessLoginInternalError = "This sign-in sandbox hit an internal error while starting — try again; if it repeats, check the daemon log."

// finishHarnessLoginLaunch is the part of the launch that can block: resolve the
// dispatch ceiling, then dispatchRun (CreateSandbox, the STARTING->RUNNING CAS,
// run.interactive.start). It runs detached, after the caller already holds a 200 and
// a run id.
//
// The ceiling error is the missed door. resolveDispatchCeiling errors AFTER the
// run row and its audit stamp exist. Synchronously that was a 500 and the
// caller knew the launch had failed; detached, returning would leave a 200
// already answered, a run PENDING forever, no failure_hint for the pane to show
// and no watcher lease for a sweep to adopt. So it fails the run the way
// dispatchRun fails its own unresolved-ceiling arm — failAndRevoke from
// PENDING, which revokes the run identity and writes the terminal state.
//
// Unreachable through the HTTP route as things stand (effectiveCeiling memoizes
// per request and launchHarnessLoginRun already resolved it, so a second error
// cannot appear out of a successful first), and kept anyway for the same reason
// dispatchRun keeps its zero-ceiling arm: the cost of the guard is four lines
// and the cost of its absence is a silently stranded run.
func (s *Server) finishHarnessLoginLaunch(ctx context.Context, run types.AgentRun, d harnessLoginDispatch) {
	// Contain a panic in a detached goroutine so a launch bug cannot take the
	// daemon down with it (same idiom as startCompletionWatcher).
	//
	// Fail from the run's current state, re-read. `from` is a CAS precondition,
	// and by the time anything here can realistically panic dispatchRun has
	// already CASed PENDING->STARTING (runs_dispatch.go) — so a hard-coded
	// RunPending would not apply, and failAndRevoke's non-applied path is SILENT
	// (no log, no audit, no revoke). The run would sit STARTING with a live
	// identity until a reconcile sweep. One GetRun turns that into the terminal
	// state plus the identity revocation this arm exists for; if even that read
	// fails there is nothing better to do than fall back to PENDING.
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "wardynd: harness login launch panicked",
				slog.String("run_id", run.ID.String()), slog.Any("panic", rec))
			from := types.RunPending
			if s.cfg.Store != nil {
				if got, gerr := s.cfg.Store.GetRun(ctx, run.ID); gerr == nil && !isTerminalRunState(got.State) {
					from = got.State
				}
			}
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
				run.ID.String(), "failure", mustJSON(map[string]any{
					"note": "the detached harness-login launch panicked; the run is failed rather than left non-terminal",
				})))
			s.failAndRevoke(ctx, run.ID, from, harnessLoginInternalError)
		}
	}()
	// The acting principal's ceiling, for the dispatch DENY axis. It was resolved
	// rather than exempted precisely so that re-tiering this route would simply
	// start binding instead of leaving a door open — which is what happened: the
	// login lane is member-reachable under a per_user row now, and this reads the
	// member's own ceiling with no change. The memo makes it the same resolution
	// the Limits axis already made.
	dc, _, dcErr := s.resolveDispatchCeiling(ctx)
	if dcErr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": dcErr.Error()})))
		s.failAndRevoke(ctx, run.ID, types.RunPending, harnessLoginCeilingUnresolved)
		return
	}
	s.dispatchRun(ctx, run, dc, dispatchParams{
		RunToken: d.RunToken, Image: d.Image, Policy: d.Policy,
		Interactive: true, ExtraEnv: d.ExtraEnv,
	})
}
