// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Terminal run lifecycle: the completion watcher, the shared finalize tail
// (finalizeRunTail — the ONE terminal sequence both the live watcher and the
// boot reconciler route through, U145), the revoke cascade, failAndRevoke, and
// the kill handler. Split from runs_dispatch.go along the dispatch-vs-terminal
// seam: runs_dispatch.go gets a run STARTED; this file is everything that ends
// one.

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// startCompletionWatcher launches the detached goroutine that blocks until the
// agent process exits and then propagates its outcome to the run's durable
// state + audit trail. Invariants it upholds:
//
//   - DETACHED CONTEXT: it derives its context from s.cfg.BaseCtx (the daemon
//     rootCtx), NOT the request/dispatch ctx. The request ctx is cancelled the
//     moment the create-run handler returns, which would otherwise cancel
//     Runner.Wait and the watcher before the agent ever finishes. BaseCtx lives
//     for the daemon's lifetime (cancelled on shutdown).
//   - KILLED-RACE GUARD: the terminal transition is a conditional store update
//     from RUNNING only (UpdateRunStateIf). A user may `wardyn kill` mid-run,
//     moving the run to KILLED and tearing the sandbox down; the watcher must
//     NOT clobber that. If the conditional update does not apply (the run is no
//     longer RUNNING), the watcher does nothing further — in particular it does
//     NOT tear the sandbox down (kill already did).
//   - TEARDOWN ONLY ON WIN: StopSandbox is called only when the watcher won the
//     RUNNING->terminal transition, so resources are freed exactly once and a
//     run someone else killed is left alone.
//   - NO SILENT ABANDONMENT: this is the run's only watcher, so a Wait error that
//     is not the daemon shutting down hands off to reconcileWatch rather than
//     returning — see the Wait-error branch. agentExecID (the id Exec returned,
//     "" for exec-less substrates) is what reconcileWatch probes for AGENT, not
//     merely container, liveness.
func (s *Server) startCompletionWatcher(runID uuid.UUID, ref, agentExecID string) {
	if s.cfg.Runner == nil {
		return
	}
	base := s.cfg.BaseCtx
	if base == nil {
		base = context.Background()
	}
	go func() {
		// Contain a panic in the detached watcher (e.g. a driver Wait bug) so it
		// can't crash the daemon; record it for forensics (mirrors reconcileWatch).
		defer func() {
			if r := recover(); r != nil {
				s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
					runID.String(), "failure", mustJSON(map[string]any{"panic": fmt.Sprintf("%v", r)})))
			}
		}()
		exitCode, werr := s.cfg.Runner.Wait(base, ref)
		if werr != nil {
			// Audit the watcher's exit for forensics either way.
			s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
				runID.String(), "failure", mustJSON(map[string]any{"error": werr.Error()})))
			if base.Err() != nil {
				// Shutdown / daemon stopping: leave the run as-is for the next boot's
				// ReconcileOnBoot to observe. Forcing a state change here would
				// false-fail a healthy run the restart is about to re-adopt.
				return
			}
			// NOT shutdown. Driver.Wait gives up on the FIRST probe error, so one
			// docker API blip errors every in-flight Wait while the agents keep
			// working — and this is the run's ONLY watcher (one per dispatch, never
			// respawned). A transient probe error is NOT "the run finished": returning
			// here strands a RUNNING run with a live sandbox and un-revoked
			// credentials, with no other writer to finalize it. A kill, by contrast,
			// already set the terminal state, so the handoff below is a no-op for it.
			// Hand off to the watcher that already tolerates bounded probe errors and
			// then finalizes + revokes + tears down (reconcile.go).
			s.reconcileWatch(base, runID, ref, agentExecID)
			return
		}

		// Map exit code to terminal state: 0 => COMPLETED, non-zero => FAILED.
		terminal := types.RunCompleted
		outcome := "success"
		if exitCode != 0 {
			terminal = types.RunFailed
			outcome = "failure"
		}

		// KILLED-race guard: transition ONLY from RUNNING. If a kill/stop already
		// moved the run to a terminal state, applied is false and we leave it be.
		applied, uerr := s.cfg.Store.UpdateRunStateIf(base, runID, types.RunRunning, terminal)
		if uerr != nil {
			s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
				runID.String(), "failure", mustJSON(map[string]any{
					"exit_code": exitCode, "error": uerr.Error(),
				})))
			return
		}
		if !applied {
			// Run already terminal (e.g. KILLED by a user mid-run). Do nothing —
			// the kill path already tore the sandbox down.
			return
		}

		// We won the RUNNING->terminal transition, so this is the authoritative end
		// of the run. Run the shared terminal tail: audit run.complete, cascade the
		// kill-switch revocation (matching the docs' revoke-on-every-stop promise,
		// "including failure"), free the sandbox (auditing a failed teardown so an
		// abandoned container is never silent), and settle the workspace/record run.
		// The boot reconciler runs the IDENTICAL sequence via finalizeRunTail, so a
		// new terminal concern is added in one place, not hand-copied across paths.
		s.finalizeRunTail(base, runID, ref, "run.complete", outcome, map[string]any{
			"exit_code": exitCode, "state": terminal,
		})
	}()
}

// isTerminalRunState reports whether a run is in a terminal (already-ended)
// state. The kill-switch must not re-kill or clobber a run that already ended
// (COMPLETED/FAILED by the watcher, or KILLED/STOPPED/ARCHIVED earlier).
func isTerminalRunState(st types.RunState) bool {
	switch st {
	case types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived:
		return true
	default:
		return false
	}
}

// stopSandboxOrAudit tears a dispatch-created sandbox down and RECORDS a failed
// teardown under `action`. Silence is not an option at these sites: each one
// lands the run terminal immediately afterwards (KILLED by the racing kill, or
// FAILED via failAndRevoke), and ReconcileOnBoot skips terminal runs
// (reconcile.go), so a swallowed StopSandbox error abandons a live/routable
// container — plus its proxy sidecar, which resolved the injected credential
// VALUES into memory at startup, and RevokeRun only denies FUTURE mints —
// forever with no record. Mirrors the teardown_error audit reconcileFinalize and
// the completion watcher already emit. Reports whether the sandbox was observed
// gone, so a caller may fail closed on a teardown it cannot confirm.
func (s *Server) stopSandboxOrAudit(ctx context.Context, runID uuid.UUID, ref, action string) bool {
	if s.cfg.Runner == nil || ref == "" {
		return true
	}
	serr := s.cfg.Runner.StopSandbox(ctx, ref)
	if serr == nil {
		return true
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action,
		ref, "failure", mustJSON(map[string]any{
			"sandbox_ref": ref, "teardown_error": serr.Error(),
		})))
	return false
}

// revokeRunCascade performs the identity + broker revocation half of the
// kill-switch cascade for a run: it deny-lists the run token
// (Identity.RevokeRun) and revokes any minted broker credentials
// (Broker.RevokeRun). Both are nil-safe and best-effort; an error is audited as
// run.revoke/failure but never propagated (revocation must not gate the caller).
// It is shared by handleKillRun and the completion watcher so EVERY terminal
// transition revokes (matching the documented cascade-on-every-stop promise).
func (s *Server) revokeRunCascade(ctx context.Context, runID uuid.UUID) {
	data := map[string]any{}
	if s.cfg.Identity != nil {
		if rerr := s.cfg.Identity.RevokeRun(ctx, runID); rerr != nil {
			data["identity_error"] = rerr.Error()
		}
	}
	if s.cfg.Broker != nil {
		if berr := s.cfg.Broker.RevokeRun(ctx, runID); berr != nil {
			data["broker_error"] = berr.Error()
		}
	}
	if len(data) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.revoke",
			runID.String(), "failure", mustJSON(data)))
	}
}

// finalizeRunTail runs the terminal-transition side effects shared by the live
// completion watcher (startCompletionWatcher) and the boot reconciler
// (reconcileFinalize), AFTER the caller has won the CAS into terminal state:
//
//  1. success audit under `action`/`outcome` with the caller's `data`
//     (run.complete{exit_code,state} for the watcher, run.reconcile{to,reason}
//     for the reconciler);
//  2. the kill-switch revoke cascade (deny the run token + broker creds), so
//     EVERY terminal transition revokes, per the docs' revoke-on-every-stop;
//  3. best-effort sandbox teardown — a failed StopSandbox is audited under the
//     same `action` (the caller's `data` plus teardown_error) because the run is
//     now terminal, so no future boot reconciles the abandoned container;
//  4. workspace + record-run settlement (both idempotent).
//
// Extracting the sequence keeps the two finalize paths from silently diverging:
// a new terminal concern is added here once instead of hand-copied. The two
// callers still own their distinct CAS + error handling upstream (the watcher
// CASes from RUNNING and audits its own uerr; the reconciler reads current state
// first and slogs) — only the post-CAS tail is shared.
func (s *Server) finalizeRunTail(ctx context.Context, runID uuid.UUID, ref, action, outcome string, data map[string]any) {
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action,
		runID.String(), outcome, mustJSON(data)))
	s.revokeRunCascade(ctx, runID)
	if ref != "" && s.cfg.Runner != nil {
		if serr := s.cfg.Runner.StopSandbox(ctx, ref); serr != nil {
			td := make(map[string]any, len(data)+1)
			for k, v := range data {
				td[k] = v
			}
			td["teardown_error"] = serr.Error()
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action,
				ref, "failure", mustJSON(td)))
		}
	}
	s.reconcileWorkspaceRun(ctx, runID)
	s.reconcileRecordRun(ctx, runID)
}

// failAndRevoke transitions a run from `from` to FAILED and, if that CAS won, runs
// the credential revoke cascade — so a run that dies on ANY create/dispatch failure
// path has its minted identity + broker credentials revoked, not merely its state
// flipped. Every terminal transition must revoke (the documented cascade-on-every-
// stop promise); previously only the completion watcher, kill, and reconciler did,
// leaving the create/dispatch FAILED paths leaking a live run token + broker creds
// (C003). Revoke runs only when THIS transition won, so a concurrent kill that
// already moved the run is not double-handled.
func (s *Server) failAndRevoke(ctx context.Context, runID uuid.UUID, from types.RunState) {
	applied, err := s.cfg.Store.UpdateRunStateIf(ctx, runID, from, types.RunFailed)
	if err != nil {
		// "The compensator itself failed" is categorically different from
		// applied==false ("a concurrent kill legitimately won") and must not collapse
		// into it: nobody else will write this run's terminal state, so it strands
		// non-terminal with un-revoked credentials until the next boot reconciles it.
		slog.ErrorContext(ctx, "wardynd: failAndRevoke CAS failed, run may be stranded",
			slog.String("run_id", runID.String()), slog.String("from_state", string(from)), slog.Any("err", err))
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.fail",
			runID.String(), "failure", mustJSON(map[string]any{"from": string(from), "error": err.Error()})))
		return
	}
	if applied {
		s.revokeRunCascade(ctx, runID)
	}
}

// handleKillRun is the kill-switch: it cascades in a FIXED order — WIN the KILLED
// terminal CAS first, THEN runner teardown, identity revocation, broker credential
// revocation — then audits run.kill. The order matters: winning the CAS first means
// a kill that loses to a concurrent forward-transition 409s WITHOUT revoking, so it
// can never strip a still-live run's credentials (C002); once the transition is ours
// we tear the sandbox down (so it cannot use a credential it holds) and deny any
// future mints (identity + broker).
//
// IDEMPOTENCY / TERMINAL GUARD: a run in a NON-KILLED terminal state
// (COMPLETED/FAILED/STOPPED/ARCHIVED) is NOT re-killed — blindly writing KILLED
// would corrupt that recorded outcome — so we 409 without touching state, the
// runner, or the cascade. An already-KILLED run is the EXCEPTION (U040): its
// first kill may have failed a teardown/revoke step (the honest fail-loud path
// marks KILLED but reports the failure and advises a retry), so a re-kill must
// re-run the idempotent KillSandbox + revoke cascade to actually free the
// orphaned sandbox/credentials. Re-writing KILLED->KILLED is a value no-op.
func (s *Server) handleKillRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, id)
	if notFoundIf(w, err, "run") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}

	// TERMINAL GUARD: do not clobber a NON-KILLED already-ended run. A KILLED run
	// is exempt — re-killing re-runs the idempotent teardown/revoke cascade so a
	// first kill whose teardown failed can still free the sandbox + credentials
	// (U040). COMPLETED/FAILED/STOPPED/ARCHIVED still 409 (writing KILLED would
	// corrupt the recorded outcome).
	if isTerminalRunState(run.State) && run.State != types.RunKilled {
		writeError(w, http.StatusConflict,
			"run is already terminal (state="+string(run.State)+"); not re-killing")
		return
	}

	// Run the teardown/revocation cascade and the terminal state write on a
	// context DETACHED from the request: once a kill begins it must complete even
	// if the client disconnects, or a half-applied kill could strand a live token
	// or a running sandbox (C4). Read the principal from the request first.
	killerType, killer := actorFromRequest(r)
	cascadeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	// (1) WIN THE TERMINAL TRANSITION FIRST (C002). Revoking before this CAS meant a
	// kill that then LOST the CAS to a concurrent dispatch forward-transition
	// (PENDING->STARTING) had already revoked the run's credentials — leaving a live
	// RUNNING run with dead creds behind a silent 409. Own the KILLED transition
	// first; only then tear down + revoke what is now unambiguously ours. Conditional
	// from the (non-terminal) state we read, so a completion watcher winning
	// RUNNING->COMPLETED is not clobbered. A re-kill of an already-KILLED run still
	// CASes KILLED->KILLED (applied), re-running the idempotent teardown (U040).
	applied, serr := s.cfg.Store.UpdateRunStateIf(cascadeCtx, id, run.State, types.RunKilled)
	if serr != nil {
		writeError(w, http.StatusInternalServerError, "update run state: "+serr.Error())
		return
	}
	if !applied {
		// The run moved to another state between our read and our write (a concurrent
		// terminal transition, or a dispatch forward-transition). Report the conflict
		// WITHOUT revoking — the run legitimately advanced, and a losing kill must not
		// strip a still-live run's credentials.
		writeError(w, http.StatusConflict,
			"run state changed concurrently; not overwriting with KILLED")
		return
	}

	killData := map[string]any{}
	// (2) Runner teardown (immediate). Idempotent on a gone sandbox.
	if s.cfg.Runner != nil && run.SandboxRef != "" {
		if kerr := s.cfg.Runner.KillSandbox(cascadeCtx, run.SandboxRef); kerr != nil {
			killData["runner_error"] = kerr.Error()
		}
	}
	// (3) Identity revocation: deny every (current+future) token for the run. A
	// bounded retry guards against a transient store error leaving the run's
	// JWT-SVID valid (until its <=1h TTL) while the kill is reported as success.
	// Nil-guarded like revokeRunCascade/healthz: an embedding without an Identity
	// provider must not panic mid-cascade (after the KILLED CAS + KillSandbox but
	// before the audit) — the revoke step is simply skipped.
	if s.cfg.Identity != nil {
		if rerr := retryQuick(cascadeCtx, func() error { return s.cfg.Identity.RevokeRun(cascadeCtx, id) }); rerr != nil {
			killData["identity_error"] = rerr.Error()
		}
	}
	// (4) Broker credential revocation (best-effort; audits per minted jti).
	if s.cfg.Broker != nil {
		if berr := retryQuick(cascadeCtx, func() error { return s.cfg.Broker.RevokeRun(cascadeCtx, id) }); berr != nil {
			killData["broker_error"] = berr.Error()
		}
	}

	// HONEST OUTCOME (C2): the kill-switch is the central governance control and
	// the audit log is the system of record. If ANY teardown/revocation step
	// failed, the run is marked KILLED but a minted token may still be valid until
	// its TTL or the sandbox may still be live — so we must NOT report success.
	// Emit run.kill with the TRUE outcome plus a distinct run.revoke/failure
	// event, and return 500 so the operator/CLI retries instead of believing the
	// run is contained.
	outcome := "success"
	if len(killData) > 0 {
		outcome = "failure"
	}
	s.recordAudit(cascadeCtx, s.auditEvent(&id, killerType, killer, "run.kill",
		id.String(), outcome, mustJSON(killData)))

	// A killed workspace run still settles its workspace: verify/scan runs get
	// the no-result reconcile; a record run's kill IS the normal "Done recording"
	// for interactive mode (which has no completion watcher), so capture here.
	s.reconcileWorkspaceRun(cascadeCtx, id)
	s.reconcileRecordRun(cascadeCtx, id)

	if outcome == "failure" {
		s.recordAudit(cascadeCtx, s.auditEvent(&id, types.ActorSystem, "wardynd", "run.revoke",
			id.String(), "failure", mustJSON(killData)))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"id":     id,
			"state":  types.RunKilled,
			"errors": killData,
			"error":  "run marked KILLED but one or more teardown/revocation steps failed; the run may not be fully contained — retry the kill",
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": types.RunKilled})
}

// retryQuick runs fn up to 3 times with a short linear backoff, returning the
// last error. It stops early if ctx is done. Used by the kill-switch revocation
// steps so a transient store error does not leave a token valid while the kill is
// reported as success.
func retryQuick(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
			}
		}
		if err = fn(); err == nil {
			return nil
		}
	}
	return err
}
