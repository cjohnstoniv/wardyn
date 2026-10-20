// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Bringing a computer folder back while its run is live (#2982). One side edits at a time: the
// sandbox edits while the run is live, and the person brings the folder back to edit it on their
// computer. That ends the run. The run is stopped through the same tail as any other end, which
// revokes its credentials, brings the folder back while the sandbox still holds it, and then stops
// the sandbox. Before that pass the agent is stopped: every process in the sandbox but its idle
// main process is ended, and the pass runs only once none is left, so it never copies a file
// something is still writing. Off unless WARDYN_COMPUTER_FOLDERS is set.

// bringBackKey carries, on the ctx of the end it starts, the person who asked for a bring-back.
type bringBackKey struct{}

// bringBackActor is who asked for a bring-back.
type bringBackActor struct {
	actorType types.ActorType
	principal string
}

// bringBackBy is who asked for ctx's bring-back; false for any other end.
func bringBackBy(ctx context.Context) (bringBackActor, bool) {
	a, ok := ctx.Value(bringBackKey{}).(bringBackActor)
	return a, ok
}

// computerFolderBringBackView answers POST /runs/{id}/computer-folder/bring-back: the run's new
// state, the pass's record (files, bytes, refused paths, any error), and whether the sandbox's
// removal was confirmed. A pass can succeed while the teardown after it fails.
type computerFolderBringBackView struct {
	State          types.RunState          `json:"state"`
	Back           *types.FolderPassRecord `json:"back,omitempty"`
	SandboxRemoved bool                    `json:"sandbox_removed"`
}

// handleBringComputerFolderBack serves POST /runs/{id}/computer-folder/bring-back. Only the run's
// own person may ask, never an admin, because it writes onto that person's computer; anyone else
// gets the byte-identical 404 (ownsRun).
func (s *Server) handleBringComputerFolderBack(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRun)
	if !ok {
		return
	}
	if !s.cfg.ComputerFolders {
		writeErrorReason(w, http.StatusForbidden, reasonComputerFoldersOff, computerFolderOffMsg)
		return
	}
	actorType, principal := actorFromRequest(r)
	folder, refusal := s.bringBackRefusal(r.Context(), run)
	if refusal.write(s, w, r) {
		return
	}
	// The end runs on the daemon's ctx: a client that gives up must not leave the run half ended.
	ctx := s.cfg.BaseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, bringBackKey{}, bringBackActor{actorType, principal})
	// One winner against a kill or any other end: both move the run from RUNNING.
	applied, err := s.casRunState(ctx, id, types.RunRunning, types.RunStopped)
	if err != nil {
		writeServerError(w, r, "stop the run for its bring-back", err)
		return
	}
	if !applied {
		writeErrorReason(w, http.StatusConflict, reasonComputerFolderNotRunning, bringBackNotRunningMsg)
		return
	}
	data := map[string]any{"folder": folder.Name, "state": types.RunStopped}
	s.recordAudit(ctx, s.auditEvent(&id, actorType, principal, "run.folder.finalize", id.String(), "success", mustJSON(data)))
	// Its error is already audited by the tail; what the person is told is read back below.
	_ = s.finalizeRunEffects(ctx, id, run.SandboxRef, "run.folder.finalize", data, false, true)
	view := computerFolderBringBackView{State: types.RunStopped}
	// The tail clears the run's sandbox ref only once its StopSandbox succeeded.
	if cur, err := s.cfg.Store.GetRun(ctx, id); err == nil {
		view.SandboxRemoved = cur.SandboxRef == ""
	}
	if st, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok {
		if rec, err := st.RunLocalFolderStatus(ctx, id); err == nil {
			view.Back = rec.Back
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// The refusals board M13 words; the others are listed for the overseer in the lane's PR.
const (
	bringBackNotRunningMsg   = "This run is no longer running, so its folder can't come back. Killing a run doesn't bring the folder back."
	bringBackPausedMsg       = "This run is paused. Resume it, then bring the folder back."
	bringBackUnstoppableMsg  = "Wardyn can't stop this run's agent by itself, so its folder can't come back early. It comes back when the run ends."
	bringBackAgentRunningMsg = "the agent could not be stopped, so the folder did not come back"
)

// bringBackRefusal is run's folder from the person's computer, or why it cannot be brought back now.
func (s *Server) bringBackRefusal(ctx context.Context, run types.AgentRun) (*types.RunLocalFolder, *runRefusal) {
	if run.State != types.RunRunning {
		return nil, runError(http.StatusConflict, reasonComputerFolderNotRunning, bringBackNotRunningMsg)
	}
	none := runError(http.StatusConflict, reasonComputerFolderNone, "this run has no folder from your computer")
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return nil, none
	}
	folder, seed, err := st.RunLocalFolderSeed(ctx, run.ID)
	switch {
	case err != nil:
		return nil, runServerError("read the run's folder", err)
	case folder == nil || folder.RunnerID == nil:
		return nil, none
	case folder.Direction != types.ComputerFolderBoth:
		return nil, runError(http.StatusConflict, reasonComputerFolderCopyInOnly, "this run's folder is Copy in only, so nothing comes back")
	case seed.SeededAt == nil:
		return nil, runError(http.StatusConflict, reasonComputerFolderNotSynced, "this run's folder never finished its first sync, so there is nothing to bring back")
	case run.PausedAt != nil:
		// Resuming it here would let its agent write during the pass, and a frozen sandbox takes no exec.
		return nil, runError(http.StatusConflict, reasonComputerFolderPaused, bringBackPausedMsg)
	}
	// The runner must be able to take the pass before the run is ended for it.
	if _, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID); refusal != nil {
		if refusal.body.Reason == reasonComputerRunnerOffline {
			row, _ := s.cfg.Store.(runnerGetter).GetRunner(ctx, *folder.RunnerID)
			refusal = runError(http.StatusConflict, reasonComputerRunnerOffline,
				runnerLabel(row)+" is offline, so the folder can't come back now. Try again when it reconnects.")
		}
		return nil, refusal
	}
	if !s.agentStoppable(ctx, run) {
		return nil, runError(http.StatusConflict, reasonComputerFolderAgentUnstoppable, bringBackUnstoppableMsg)
	}
	return folder, nil
}

// bringBackExecBound bounds each exec the bring-back runs in the sandbox (the stop waits a second
// for the agent to end on SIGTERM, then retries SIGKILL for up to two more).
const bringBackExecBound = 15 * time.Second

// agentStoppable reports whether the sandbox's PID 1 is one of the idle main processes the Docker
// substrate starts (internal/runner/docker: the task idle script, or agent-run --idle), so that ending every other process ends the agent and
// leaves the sandbox up for the pass. Anywhere else (an exec-less sandbox whose main process is the
// agent, a Kubernetes agent container, an older runner's idle script) the agent cannot be
// stopped this way, and the bring-back is refused.
func (s *Server) agentStoppable(ctx context.Context, run types.AgentRun) bool {
	out, code, err := s.sandboxExecOutput(ctx, run.SandboxRef, "cat", "/proc/1/cmdline")
	if err != nil || code != 0 {
		return false
	}
	main := strings.TrimRight(out, "\x00")
	// The interactive idle is a bash script, so the kernel names bash and the script's path.
	interactive := strings.HasPrefix(main, "bash\x00") && strings.HasSuffix(main, "/agent-run\x00--idle")
	return interactive || main == "sh\x00-c\x00"+runner.AgentIdleScript
}

// agentStopScript ends every process in the sandbox but PID 1 (the idle main process) and itself:
// SIGTERM, a second's grace, then SIGKILL. It then confirms from /proc that nothing is left but
// zombies, its own children, and the sleep the idle loop restarts, retrying SIGKILL for about two
// seconds, and prints agentStoppedMark only then. A process it may not signal (another user's)
// stays, and the stop fails.
const agentStopScript = `self=$$
kill -s TERM -1 2>/dev/null
sleep 1
kill -s KILL -1 2>/dev/null
n=0
while :; do
  left=
  for d in /proc/[0-9]*; do
    p=${d#/proc/}
    if [ "$p" = 1 ] || [ "$p" = "$self" ]; then continue; fi
    st= pp=
    { while read -r k v rest; do
        case "$k" in State:) st=$v ;; PPid:) pp=$v ;; esac
      done < "$d/status"; } 2>/dev/null
    case "$st" in ''|Z|X) continue ;; esac
    [ "$pp" = "$self" ] && continue
    if [ "$pp" = 1 ] && [ "$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null)" = "sleep 3600 " ]; then continue; fi
    left="$left $p"
  done
  [ -z "$left" ] && { echo ` + agentStoppedMark + `; exit 0; }
  n=$((n+1))
  [ "$n" -ge 10 ] && { echo "left:$left"; exit 3; }
  kill -s KILL $left 2>/dev/null
  sleep 0.2
done`

// agentStoppedMark is what agentStopScript prints once nothing but PID 1 is left.
const agentStoppedMark = "wardyn-agent-stopped"

// stopAgentForPass ends the agent before a bring-back's pass and confirms it ended.
func (s *Server) stopAgentForPass(ctx context.Context, ref string) error {
	out, code, err := s.sandboxExecOutput(ctx, ref, "sh", "-c", agentStopScript)
	if err != nil {
		return err
	}
	if code != 0 || strings.TrimSpace(out) != agentStoppedMark {
		return errors.New("processes still running after SIGKILL")
	}
	return nil
}

// sandboxExecOutput runs argv in ref's sandbox, under bringBackExecBound, and returns its stdout
// (at most 8 KiB) and exit code.
func (s *Server) sandboxExecOutput(ctx context.Context, ref string, argv ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, bringBackExecBound)
	defer cancel()
	sess, err := s.cfg.Runner.ExecStream(ctx, ref, runner.ExecSpec{Argv: argv})
	if err != nil {
		return "", -1, err
	}
	if sess.Close != nil {
		defer func() { _ = sess.Close() }()
	}
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	if sess.Stdout == nil || sess.Wait == nil {
		return "", -1, errors.New("the exec has no output")
	}
	out, _ := io.ReadAll(io.LimitReader(sess.Stdout, 8<<10))
	code, err := sess.Wait()
	return string(out), code, err
}
