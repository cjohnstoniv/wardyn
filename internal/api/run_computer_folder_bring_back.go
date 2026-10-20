// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Bringing a computer folder back while its run is live (#2982). One side edits at a time: the
// sandbox edits while the run is live, and the person brings the folder back to edit it on their
// computer. That ends the run. The run is stopped through the same tail as any other end, which
// revokes its credentials, brings the folder back while the sandbox still holds it, and then stops
// the sandbox. The pass is refused while the agent is active, so it never copies a file the agent
// is still writing. Off unless WARDYN_COMPUTER_FOLDERS is set.

// bringBackQuietFor is how long a run must have been quiet (its presence clock: a person's input,
// the agent's egress decisions and proxied bytes) before its folder may be brought back. It is
// longer than presenceStampEvery, which coalesces the clock's writes. A var so tests can shorten it.
var bringBackQuietFor = 2 * time.Minute

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
// state and the pass's record (files, bytes, refused paths, any error).
type computerFolderBringBackView struct {
	State types.RunState          `json:"state"`
	Back  *types.FolderPassRecord `json:"back,omitempty"`
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
	if run.PausedAt != nil {
		// The daemon refuses an exec into a frozen sandbox, and the pass is one.
		if err := s.thawForExec(ctx, run, actorType, principal, "presence"); err != nil {
			writeErrorReason(w, http.StatusConflict, reasonRunResumeFailed, loggedMsg(r.Context(), "the paused run could not be resumed for the pass", err))
			return
		}
	}
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
	_ = s.finalizeRunEffects(ctx, id, run.SandboxRef, "run.folder.finalize", data, false, true)
	view := computerFolderBringBackView{State: types.RunStopped}
	if st, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok {
		if rec, err := st.RunLocalFolderStatus(ctx, id); err == nil {
			view.Back = rec.Back
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// bringBackNotRunningMsg is the refusal when a kill or another end got there first (board M13).
const bringBackNotRunningMsg = "This run is no longer running, so its folder can't come back. Killing a run doesn't bring the folder back."

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
	if !s.agentQuiet(ctx, run) {
		return nil, runError(http.StatusConflict, reasonComputerFolderAgentActive,
			fmt.Sprintf("the agent is still working: bring the folder back once the run has been quiet for %d minutes", int(bringBackQuietFor.Minutes())))
	}
	return folder, nil
}

// agentQuiet reports whether run has been quiet for bringBackQuietFor and its agent's CPU reads
// below the idle threshold the pause uses. A run with no CPU reading is not quiet.
func (s *Server) agentQuiet(ctx context.Context, run types.AgentRun) bool {
	if s.cfg.Now().Sub(activeSince(run)) < bringBackQuietFor {
		return false
	}
	pct, ok := s.sampleRunCPU(ctx, []types.AgentRun{run})[run.ID]
	return ok && pct < idleQuietCorePercent
}
