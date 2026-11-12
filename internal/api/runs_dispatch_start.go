// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// startAgentOrIdle is dispatch's final phase, after the run is RUNNING.
//
// Interactive mode: skip the agent Exec AND the completion watcher. The
// sandbox is RUNNING and idle (the container holds open), ready for a human to
// `wardyn attach`. There is no agent process, so there is nothing for the
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
	s.startAgentsOrIdle(ctx, run, nil, ref, image, interactive, execution, onAgentStarted, nil)
}

// startAgentsOrIdle is startAgentOrIdle for a run whose harnesses may run tasks of their
// own (several agents in one run): the task exec then starts every agent (agentTaskArgv).
// onPrepared, when non-nil, runs once an interactive macOS VM run's prep has recorded done, and never when the
// prep failed: the run's attended window can open from then on.
func (s *Server) startAgentsOrIdle(ctx context.Context, run types.AgentRun, harnesses []types.RunHarness, ref, image string, interactive bool, execution runnerExecution, onAgentStarted, onPrepared func()) {
	byoi := strings.HasPrefix(image, "wardyn-byoi/")

	if interactive {
		if byoi {
			s.byoiSelftest(ctx, run, ref, execution, false /* warn-only */)
		}
		data := map[string]any{
			"sandbox_ref": ref,
			"note":        "interactive run: no agent task exec'd; awaiting attach",
		}
		if run.MachineKind.IsMacOSVM() {
			if !s.prepareMacOSInteractive(ctx, run, ref) {
				return
			}
			data["prepared"] = true
			if onPrepared != nil {
				onPrepared()
			}
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.interactive.start",
			run.ID.String(), "success", mustJSON(data)))
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
		argv := agentTaskArgv(run, harnesses)
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

// statusDetailMacPrepDone is the run's status_detail once a macOS VM's interactive prep has succeeded: the
// control plane's own stage line, as statusDetailBuilding is, in the "<stage>: <word>" shape. The projection
// that serves a run blanks it for a RUNNING run, so it is read off the stored row (macPrepDone) and never
// reaches a client.
const statusDetailMacPrepDone = "prep: done"

// statusDetailMacPrepRunning is written before the prep starts, so a restart of the control plane during it
// leaves a mark: sweepRunWatchers fails a run adopted with it (failInterruptedMacPrep) instead of leaving it
// RUNNING with its window refused for good. macPrepDone stays an equality check on statusDetailMacPrepDone.
const statusDetailMacPrepRunning = "prep: running"

// macPrepInterruptedMsg is the sentence of a run whose prep a restart of the control plane cut short.
const macPrepInterruptedMsg = "This run's setup was interrupted when the server restarted. Start a new run."

// macPrepDone says an interactive macOS VM run's prep (repo clone and harness setup) has finished. It is
// written only after `agent-run --prepare` exits 0, and reads the stored row: a run read back through the
// serving projection is never done. The attended window must not open before it is true.
func macPrepDone(run types.AgentRun) bool { return run.StatusDetail == statusDetailMacPrepDone }

// macOSPrepRunning is GET /runs/{id}'s macos_preparing: a RUNNING interactive macOS VM run, not kept, whose prep
// has not recorded done. It is its own field because mac_window answers its earlier rows (the site switch, the
// profile) before preparing, so a run with windows off would never say its setup is still going. stored is the
// stored row (macPrepDone).
func macOSPrepRunning(stored types.AgentRun) bool {
	return stored.State == types.RunRunning && !runIsKept(stored) && macRunPreparing(stored)
}

// macPrepareTimeout bounds `agent-run --prepare`: a shallow clone of each repo and the harness setup.
const macPrepareTimeout = 20 * time.Minute

// macPrepareArgv is the guest launcher's prep-only mode (macos_agentrun.go, prepareOnly).
var macPrepareArgv = []string{"/usr/local/bin/agent-run", "--prepare"}

// launcherSentenceMax bounds the tail of the launcher output kept to read its sentence from.
const launcherSentenceMax = 64 << 10

// prepareMacOSInteractive gives an interactive macOS VM run the prep a task run gets before the person
// reaches it: the repo clone and the harness setup, from the same guest env Init gave every exec (the grants'
// brokers, never a token). It runs `agent-run --prepare` over ExecStream, never Exec: Exec registers the guest's
// agent process, which the completion watcher waits on, so a prep that exits would end the run
// (startCompletionWatcher, runs_lifecycle.go). On success it records macPrepDone and reports true. On a
// failure it audits run.exec as a failed task start does, stops the sandbox, fails the run with the launcher's
// sentence and reports false.
func (s *Server) prepareMacOSInteractive(ctx context.Context, run types.AgentRun, ref string) bool {
	pctx, cancel := context.WithTimeout(ctx, macPrepareTimeout)
	defer cancel()
	exit := 0
	sentence := s.recordMacPrepStage(ctx, run.ID, statusDetailMacPrepRunning)
	if sentence == "" {
		exit, sentence = s.runMacPrepare(pctx, ref)
	}
	if sentence == "" {
		sentence = s.recordMacPrepStage(ctx, run.ID, statusDetailMacPrepDone)
	}
	if sentence != "" {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"argv": macPrepareArgv, "error": sentence, "exit_code": exit, "reason": "interactive_prepare"})))
		s.stopSandboxOrAudit(ctx, run.ID, ref, "run.exec")
		s.failAndRevoke(ctx, run.ID, types.RunRunning, sentence)
		s.macWindowForgetPending(ctx, run) // a window asked for at start is asked only after a prep that succeeded
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
		run.ID.String(), "success", mustJSON(map[string]any{"argv": macPrepareArgv, "reason": "interactive_prepare"})))
	return true
}

// runMacPrepare runs the prep launcher to its exit and returns the sentence the start fails with: empty when
// it exited 0, the launcher's own sentence when it printed one, else a plain account of how it ended.
func (s *Server) runMacPrepare(ctx context.Context, ref string) (exit int, sentence string) {
	sess, err := s.cfg.Runner.ExecStream(ctx, ref, runner.ExecSpec{Argv: macPrepareArgv})
	if err != nil {
		return -1, "the machine could not be prepared: " + err.Error()
	}
	defer func() { _ = sess.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	var out, errOut tailBuffer
	var wg sync.WaitGroup
	for _, d := range []struct {
		r   io.Reader
		dst *tailBuffer
	}{{sess.Stdout, &out}, {sess.Stderr, &errOut}} {
		if d.r == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(d.dst, d.r) // drained to the end: an undrained stream blocks the exec's Wait
		}()
	}
	wg.Wait()
	exit, werr := sess.Wait()
	switch {
	case ctx.Err() != nil:
		return -1, "the machine's preparation did not finish in time, so the run was not started."
	case werr != nil:
		return -1, "the machine could not be prepared: " + werr.Error()
	case exit == 0:
		return 0, ""
	}
	if line := launcherSentence(string(errOut.b) + "\n" + string(out.b)); line != "" {
		return exit, line
	}
	return exit, "the machine's preparation failed (agent-run --prepare exited " + strconv.Itoa(exit) + "), so the run was not started."
}

// tailBuffer keeps the last launcherSentenceMax bytes written to it: the launcher's sentence is its last line.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > launcherSentenceMax {
		t.b = append(t.b[:0], t.b[len(t.b)-launcherSentenceMax:]...)
	}
	return len(p), nil
}

// launcherSentence is the last "agent-run: <sentence>" line of the launcher's output, without its prefix.
func launcherSentence(output string) string {
	sentence := ""
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "agent-run: "); ok && rest != "" {
			sentence = rest
		}
	}
	return sentence
}

// recordMacPrepStage writes detail (a prep stage) to the run's status_detail, once more on a failed write, and
// returns the sentence the start fails with when it cannot: a prep that is not on record would keep the window
// closed for good (done) or leave a restart nothing to find (running).
func (s *Server) recordMacPrepStage(ctx context.Context, runID uuid.UUID, detail string) string {
	setter, ok := s.cfg.Store.(runStatusDetailSetter)
	if !ok {
		return "the machine's preparation can't be recorded in this store, so the run was not started."
	}
	err := setter.SetRunStatusDetail(ctx, runID, detail)
	if err != nil {
		err = setter.SetRunStatusDetail(ctx, runID, detail)
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not record a macOS VM's prep stage; failing the run",
			slog.String("run_id", runID.String()), slog.String("stage", detail), slog.Any("err", err))
		return "the machine's preparation could not be recorded, so the run was not started."
	}
	return ""
}

// macPrepInterrupted says a run adopted after a restart was an interactive macOS VM run whose prep was
// under way when the control plane stopped.
func macPrepInterrupted(run types.AgentRun) bool {
	return run.Interactive && run.MachineKind.IsMacOSVM() && run.State == types.RunRunning &&
		run.StatusDetail == statusDetailMacPrepRunning
}

// failInterruptedMacPreps ends each run of runs that macPrepInterrupted names and returns the rest. The sweep
// that adopts runs calls it first: an interrupted run has no setup to wait for, and left adopted it would
// stay RUNNING with its window refused as "preparing" for good.
func (s *Server) failInterruptedMacPreps(ctx context.Context, runs []types.AgentRun) []types.AgentRun {
	rest := make([]types.AgentRun, 0, len(runs))
	for _, run := range runs {
		if macPrepInterrupted(run) {
			s.failInterruptedMacPrep(ctx, run)
			continue
		}
		rest = append(rest, run)
	}
	return rest
}

// failInterruptedMacPrep ends a run macPrepInterrupted names, as reconcileFinalize ends a stranded one: the
// RUNNING -> FAILED transition, the sentence as its failure hint, then the shared terminal tail (run.reconcile
// audit, revoke cascade, sandbox teardown). reconcileFinalize itself leaves a run its runner holds to the
// runner, and an interactive macOS VM run has no execution row to receive an exit from, so this ends it here.
func (s *Server) failInterruptedMacPrep(ctx context.Context, run types.AgentRun) {
	ctx, unlock, locked := s.lockRunEnd(ctx, run.ID, endHoldSweep) // through the tail's computer folder pass
	if !locked {
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, reconcileFinalizeTimeout)
	defer cancel()
	applied, err := s.casRunState(ctx, run.ID, types.RunRunning, types.RunFailed)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: reconcile finalize failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if !applied {
		return // someone else won the transition
	}
	if setter, ok := s.cfg.Store.(runFailureHintSetter); ok {
		if herr := setter.SetRunFailureHint(ctx, run.ID, macPrepInterruptedMsg); herr != nil {
			slog.WarnContext(ctx, "wardynd: could not persist reconciled run failure hint",
				slog.String("run_id", run.ID.String()), slog.Any("err", herr))
		}
	}
	s.finalizeRunTail(ctx, run.ID, run.SandboxRef, "run.reconcile", "success",
		map[string]any{"to": string(types.RunFailed), "reason": macPrepInterruptedMsg})
}
