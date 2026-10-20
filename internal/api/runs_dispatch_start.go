// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// startAgentOrIdle is dispatch's final phase, after the run is RUNNING.
//
// Interactive mode: skip the agent Exec AND the completion watcher. The
// sandbox is RUNNING and idle (the container holds open), ready for a human to
// `wardyn run attach`. There is no agent process, so there is nothing for the
// watcher to Wait on — starting it would have it observe an immediate Wait
// failure (no tracked agent exec) and could prematurely terminate the run.
// BYOI runtime preflight: a wrapped arbitrary base is guaranteed to carry the
// runner tools (the wrap COPYs them), but may still lack a shell or the harness
// CLI. Run `agent-run --selftest` and observe its exit — for a batch run, fail
// CLOSED on nonzero (honest FAILED + audit, never a hang or a silent bad run);
// for an interactive/login box, warn-only (a login box legitimately lacks repo
// wiring and the human sees the shell regardless). Keyed off the wardyn-byoi/
// image tag so convention/devcontainer runs are unaffected. Extracted verbatim
// from dispatchRun.
const mainProcessExecID = runner.MainProcessExecID

// onAgentStarted, when non-nil, runs once the agent's Exec has succeeded: on an
// exec-less runner that is the moment its container — and the managed files
// delivered into it — first exists.
func (s *Server) startAgentOrIdle(ctx context.Context, run types.AgentRun, ref, image string, interactive bool, execution runnerExecution, onAgentStarted func()) {
	byoi := strings.HasPrefix(image, "wardyn-byoi/")

	if interactive {
		if byoi {
			s.byoiSelftest(ctx, run, ref, execution, false /* warn-only */)
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.interactive.start",
			run.ID.String(), "success", mustJSON(map[string]any{
				"sandbox_ref": ref,
				"note":        "interactive run: no agent task exec'd; awaiting attach",
			})))
		return
	}

	// When a task is provided, launch the agent process inside the now-running
	// sandbox. The driver wraps the argv with wardyn-rec (recording) when
	// configured. Exec failure: audit + stop the sandbox + mark FAILED.
	if run.Task != "" {
		// A local folder holds the agent until its first sync lands (run_local_folder.go).
		if !s.holdForLocalFolder(ctx, run, ref) {
			return
		}
		// BYOI: gate the task exec on a passing selftest (fail closed) — but
		// refuse UP FRONT, before even attempting the selftest, on an
		// exec-less (krun) substrate: see byoiExecLessRefused.
		if byoi {
			if s.byoiExecLessRefused(ctx, run, ref) {
				return
			}
			if !s.byoiSelftest(ctx, run, ref, execution, true /* fail-closed */) {
				s.stopSandboxOrAudit(ctx, run.ID, ref, "run.selftest")
				s.failAndRevoke(ctx, run.ID, types.RunRunning,
					"the BYOI image failed its agent-run --selftest (missing shell/harness binary, or a nonzero selftest exit)")
				return
			}
		}
		argv := []string{"/usr/local/bin/agent-run", run.Task}
		execID, xerr := s.cfg.Runner.Exec(execution.bind(ctx), ref, argv)
		if xerr == nil {
			xerr = s.bindRunnerExecution(ctx, execution, execID)
		}
		if xerr != nil {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": xerr.Error(), "reason": "agent_start"})))
			s.stopSandboxOrAudit(ctx, run.ID, ref, "run.exec")
			// Conditional: a concurrent kill may have moved RUNNING->KILLED; don't
			// clobber it with FAILED.
			s.failAndRevoke(ctx, run.ID, types.RunRunning, "the agent process could not be started in the sandbox: "+xerr.Error())
			return
		}
		if onAgentStarted != nil {
			onAgentStarted()
		}
		// Persist the agent exec id so the boot reconciler can observe AGENT liveness
		// (ExecInspect) across a wardynd restart: an idle-container exec run whose
		// agent already exited must finalize + revoke, not strand RUNNING.
		// Best-effort like SetSandboxRef. execID=="" (no error) is the exec-less
		// substrate (container==agent) — persist the mainProcessExecID sentinel
		// instead of the bare "" reconcile.go's strand guard reserves for a run
		// that never got this far (see mainProcessExecID's doc comment).
		persistExecID := execID
		if persistExecID == "" {
			persistExecID = mainProcessExecID
		}
		// NOT best-effort, whatever the comment above once said. reconcile.go's
		// strand guard reserves "" for "SetRunAgentExecID was never called", so a
		// LOST write leaves a healthy, running agent looking exactly like a run
		// whose dispatcher died mid-dispatch — the next boot finalizes it FAILED
		// and tears the sandbox down, while this audit row said the exec
		// succeeded. One retry (the write is idempotent and the common failure is
		// a transient connection blip), then fail the run HERE, visibly, rather
		// than hand the reconciler a lie to misread hours later.
		xerr = s.cfg.Store.SetRunAgentExecID(ctx, run.ID, persistExecID)
		if xerr != nil {
			// One retry: the write is idempotent and the failure this actually
			// sees is a transient pool/connection blip, not a rejected value.
			xerr = s.cfg.Store.SetRunAgentExecID(ctx, run.ID, persistExecID)
		}
		if xerr != nil {
			slog.ErrorContext(ctx, "wardynd: could not persist the agent exec id; failing the run",
				slog.String("run_id", run.ID.String()), slog.Any("err", xerr))
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
				run.ID.String(), "failure", mustJSON(map[string]any{"argv": argv, "error": xerr.Error()})))
			// The agent IS running; stop it, the same order the Exec-failure arm
			// above uses, so a FAILED run never leaves a live agent behind.
			s.stopSandboxOrAudit(ctx, run.ID, ref, "run.exec")
			s.failAndRevoke(ctx, run.ID, types.RunRunning,
				"the agent started but its exec id could not be persisted, so the run could not be tracked")
			return
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
			run.ID.String(), "success", mustJSON(map[string]any{"argv": argv})))

		// Completion tracking: watch the agent process to exit and propagate its
		// outcome. The watcher runs on a DETACHED context (NOT ctx — that is the
		// request context, cancelled when the create-run handler returns, which
		// would kill the watcher immediately). See startCompletionWatcher. Passed
		// the RAW execID, not the sentinel: this local value only ever reaches
		// AgentStatus via reconcileWatch's Wait-error fallback (runs_lifecycle.go),
		// where "" already means "use container Status" — the sentinel only
		// matters for a value that gets persisted and re-read after a restart.
		s.startCompletionWatcher(run.ID, ref, execID)
	}
}
