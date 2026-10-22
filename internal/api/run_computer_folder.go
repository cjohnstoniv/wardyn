// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A folder on the person's computer for a run on another machine (#2982), held by the person's
// own folder-only runner (`wardyn-runnerd serve --substrate folders`). The person browses it
// through that runner (GET /me/runners/{id}/folders), and Wardyn seeds it into the sandbox itself
// before the agent starts, with no `wardyn sync`. That pass is the `wardyn sync` engine, run by
// the runner as the sftp client over its own outbound session. wardynd joins that stream to the
// sandbox's sftp-server, which it already execs for the gateway's wardyn-sync. With
// direction "both", one more pass brings the sandbox's changes back when the run ends, under the
// `wardyn sync --pull` rules: one side edits at a time.
//
// Off unless WARDYN_COMPUTER_FOLDERS is set. The runner checks every path itself (internal/runner/folders).

const (
	// computerFolderListTimeout bounds one browse call, the runner's counting included.
	computerFolderListTimeout = 10 * time.Second
	// computerFolderBackTimeout bounds the pass that brings changes back, which holds the teardown.
	// A caller with a nearer deadline gets less (bringComputerFolderBack).
	computerFolderBackTimeout = 2 * time.Minute
)

// computerFolderOffMsg is the "blocked by your org" refusal.
const computerFolderOffMsg = "your organisation doesn't allow folders from personal computers (WARDYN_COMPUTER_FOLDERS is off)"

// folderRunner is the caller's own claimed runner id that shares folders and is online on this
// replica, with its endpoint. It writes the refusal and returns false otherwise.
func (s *Server) folderRunner(ctx context.Context, owner string, id uuid.UUID) (*remote.Substrate, *runRefusal) {
	if !s.cfg.ComputerFolders {
		return nil, runError(http.StatusForbidden, reasonComputerFoldersOff, computerFolderOffMsg)
	}
	rs, ok := s.cfg.Store.(runnerGetter)
	if !ok {
		return nil, runError(http.StatusNotImplemented, reasonComputerRunnerNoFolders, "this server's store holds no runners")
	}
	row, err := rs.GetRunner(ctx, id)
	if err != nil || owner == "" || row.Owner != owner || row.OwnerKind == types.RunnerOwnerOrg || row.State != types.RunnerClaimed {
		return nil, runError(http.StatusNotFound, reasonRunnerNotFound, "no runner of yours has that id")
	}
	s.runnerStreams.subsMu.Lock()
	ep, live := s.runnerStreams.subs[id]
	s.runnerStreams.subsMu.Unlock()
	if !live || !s.runnerOnline(id) {
		return nil, runError(http.StatusConflict, reasonComputerRunnerOffline,
			fmt.Sprintf("%s is offline: start the runner on that computer, then try again", runnerLabel(row)))
	}
	if caps, ok := ep.sub.Caps(); !ok || !caps.Folders {
		return nil, runError(http.StatusConflict, reasonComputerRunnerNoFolders,
			fmt.Sprintf("%s does not share folders: run 'wardyn-runnerd serve --substrate folders' (0.10 or later) on that computer", runnerLabel(row)))
	}
	return ep.sub, nil
}

// runnerGetter is the one runner read a computer folder needs.
type runnerGetter interface {
	GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
}

// runnerLabel is how a person knows the runner: its computer's name, else the runner's.
func runnerLabel(row types.Runner) string {
	if row.MachineName != "" {
		return row.MachineName
	}
	return row.Name
}

// computerFolderView is GET /me/runners/{id}/folders: the folders in path and, with survey=1, what
// a run would get from it.
type computerFolderView struct {
	runnerwire.FolderList
	Runner string `json:"runner"`
}

// handleListMyRunnerFolders is GET /api/v1/me/runners/{id}/folders?path=&survey=1. Only folder
// names, file counts and sizes reach Wardyn; no file is sent until a run starts.
func (s *Server) handleListMyRunnerFolders(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerOwnerForRead(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	sub, refusal := s.folderRunner(r.Context(), owner, id)
	if refusal.write(s, w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), computerFolderListTimeout)
	defer cancel()
	list, err := sub.ListFolders(ctx, runnerwire.ListFoldersArgs{
		Path: r.URL.Query().Get("path"), Survey: r.URL.Query().Get("survey") == "1", Deny: s.cfg.SyncDeny,
	})
	var werr *runnerwire.Error
	switch {
	case errors.As(err, &werr) && werr.Code == runnerwire.CodeRefused:
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonComputerFolderRefused, werr.Message)
		return
	case err != nil:
		writeErrorReason(w, http.StatusBadGateway, reasonComputerRunnerOffline, loggedMsg(r.Context(), "the runner did not answer", err))
		return
	}
	row, _ := s.cfg.Store.(runnerGetter).GetRunner(r.Context(), id)
	writeJSON(w, http.StatusOK, computerFolderView{FolderList: list, Runner: runnerLabel(row)})
}

// computerFolderRefusal checks a local_folder that names a runner, for the caller in ctx, and
// normalises it. It is localFolderRefusal's branch for a folder Wardyn seeds itself, so it needs no
// SSH gateway, and a background-only run may have one.
func (s *Server) computerFolderRefusal(ctx context.Context, req *createRunRequest, name, dir string) *runRefusal {
	f := req.LocalFolder
	bad := func(msg string) *runRefusal {
		return runError(http.StatusBadRequest, reasonLocalFolderInvalid, "local_folder."+msg)
	}
	if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path || strings.ContainsFunc(f.Path, isControlRune) {
		return bad("path must be a clean absolute path on that computer")
	}
	direction := f.Direction
	switch direction {
	case "":
		direction = types.ComputerFolderIn
	case types.ComputerFolderIn, types.ComputerFolderBoth:
	default:
		return bad(`direction must be "both" or "in"`)
	}
	if name == "" {
		name = filepath.Base(f.Path)
	}
	switch {
	case req.Interactive || req.Experience == types.ExperienceInteractive:
		return runError(http.StatusUnprocessableEntity, reasonLocalFolderUnsupported,
			"local_folder: an interactive run has no agent start to hold for the folder's first sync")
	case req.Task == "":
		return runError(http.StatusUnprocessableEntity, reasonLocalFolderUnsupported, "local_folder: a run with no task has no agent start to hold")
	}
	if _, ok := s.cfg.Store.(store.RunLocalFolderStatuses); !ok {
		return runError(http.StatusUnprocessableEntity, reasonLocalFolderUnsupported, "local_folder: this server's store cannot record a folder's sync")
	}
	if _, refusal := s.folderRunner(ctx, poolOwner(ctx), *f.RunnerID); refusal != nil {
		return refusal
	}
	id := *f.RunnerID
	req.LocalFolder = &types.RunLocalFolder{Name: name, RemoteDir: dir, RunnerID: &id, Path: f.Path, Direction: direction}
	return nil
}

func isControlRune(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }

// seedComputerFolder runs the first pass of run's computer folder, before the agent starts, and
// records its outcome as the folder's first sync (localFolderPassed), so the hold that follows
// reads it as it reads a `wardyn sync` pass.
func (s *Server) seedComputerFolder(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, wait time.Duration) {
	res, errText, _ := s.computerFolderPassWithin(ctx, wait, run, folder, false)
	// Recorded on ctx, not the pass's own deadline: a pass that timed out still leaves its record.
	s.recordComputerFolderPass(ctx, run.ID, func(st *types.RunLocalFolderStatus) { st.Seed = folderPassRecord(s.cfg.Now(), res, errText, false) })
	s.auditComputerFolderPass(ctx, run.ID, folder, false, res, errText)
	s.localFolderPassed(ctx, run.ID, run.CreatedBy, folder.RemoteDir, errText == "", errText)
}

// computerFolderStalls is how many cut passes back in a row that saved nothing give up the
// sandbox's unsynced changes.
const computerFolderStalls = 2

// bringComputerFolderBack is the pass that brings the sandbox's changes back to the person's
// computer when a "both" run ends: after the revoke cascade, before the sandbox is stopped. A run
// whose folder never seeded, or that has no such folder, (or whose changes a pass already brought back: a later end of the same run, such as its runner's exit) is left alone. The pass ends
// runTeardownBudget before ctx's deadline, so the teardown after it keeps its time. Its reads
// before the pass and its record after it are each on a teardownCtx, which outlives ctx's
// deadline and cancel.
//
// It reports whether the sandbox stays up for a resume, with its agent stopped and no egress
// (quiesceKeptSandbox): a pass that was cut short after it saved files keeps it, and so does the
// first of computerFolderStalls such passes in a row that saved nothing, the last of which gives
// the rest up. A pass that completes, fails or does not start does not, nor does a sandbox that
// cannot be quiesced. The runner's state file holds what each pass saved, so the next pass, from
// resumeComputerFolderBacks, starts after it (#3466).
func (s *Server) bringComputerFolderBack(ctx context.Context, runID uuid.UUID, ref string) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok || ref == "" {
		return false
	}
	// The reads, like the record, are on a teardownCtx: a caller already cancelled still gets the
	// record that no time was left.
	rctx, cancel := teardownCtx(ctx)
	folder, seed, err := st.RunLocalFolderSeed(rctx, runID)
	if err != nil || folder == nil || folder.RunnerID == nil || folder.Direction != types.ComputerFolderBoth || seed.SeededAt == nil {
		cancel()
		return false
	}
	if ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok {
		if rec, err := ss.RunLocalFolderStatus(rctx, runID); err == nil && rec.Back != nil && rec.Back.Error == "" {
			cancel()
			return false
		}
	}
	run, err := s.cfg.Store.GetRun(rctx, runID)
	cancel()
	if err != nil {
		return false
	}
	run.SandboxRef = ref
	// Every pass back, the first included, runs with the agent stopped and no egress where the
	// substrate can do that and keep the folder; where that fails, no pass runs. On a sandbox it
	// cannot quiesce (on a runner, or on Kubernetes) the first pass runs as it is and is never kept.
	qctx, qcancel := teardownCtx(ctx)
	qerr := s.quiesceKeptSandbox(qctx, run, ref)
	qcancel()
	var cannot notQuiescable
	if errors.As(qerr, &cannot) {
		if _, ok := bringBackBy(ctx); ok {
			// The person ended the run to bring the folder back: where the sandbox cannot be quiesced
			// (a runner's), nothing may write during the pass, so the agent is stopped first.
			if err := s.stopAgentForPass(ctx, ref); err != nil {
				slog.WarnContext(ctx, "wardynd: stop the agent before a bring-back", slog.String("run_id", runID.String()), slog.Any("err", err))
				qerr = fmt.Errorf("the agent could not be stopped: %w", err)
			}
		}
	}
	if qerr != nil && !errors.As(qerr, &cannot) {
		rctx, cancel = teardownCtx(ctx)
		defer cancel()
		errText := folderBackGivenUp("no pass ran", "its agent and egress could not be stopped first: "+qerr.Error(), 0)
		if _, ok := bringBackBy(ctx); ok {
			errText = bringBackAgentRunningMsg
		}
		if errors.Is(qerr, errSandboxStopped) {
			errText = "no pass ran; the sandbox had already stopped, with its changes not brought back yet, uncounted"
		}
		s.recordComputerFolderPass(rctx, runID, func(st *types.RunLocalFolderStatus) {
			st.Resume, st.Back = nil, folderPassRecord(s.cfg.Now(), runnerwire.FolderPass{}, errText, true)
		})
		s.auditComputerFolderPass(rctx, runID, *folder, true, runnerwire.FolderPass{}, errText)
		return false
	}
	d := computerFolderBackTimeout
	if dl, ok := ctx.Deadline(); ok {
		d = min(d, time.Until(dl)-runTeardownBudget)
	}
	res, passErr, cut := s.computerFolderPassWithin(ctx, d, run, *folder, true)
	next, errText := s.folderBackVerdict(ctx, run, &res, passErr, cut, qerr)
	rctx, cancel = teardownCtx(ctx)
	defer cancel()
	recorded := s.recordComputerFolderPass(rctx, runID, func(st *types.RunLocalFolderStatus) {
		st.Resume, st.Back = next, folderPassRecord(s.cfg.Now(), res, errText, true)
	})
	keep := next != nil
	if keep && !recorded {
		// A sandbox kept with no record of it is never resumed, and the orphan sweep would take it.
		errText, keep = passErr+"; the pass could not be recorded, so the sandbox was stopped", false
	}
	s.auditComputerFolderPass(rctx, runID, *folder, true, res, errText)
	return keep
}

// folderBackVerdict is the resume a pass back leaves (nil: stop the sandbox now) and the error to
// record for it, read against the resume before it (folderBackResume). Only a sandbox quiesced
// before the pass (qerr nil) is kept; one that was not is stopped, and the record says why.
func (s *Server) folderBackVerdict(ctx context.Context, run types.AgentRun, res *runnerwire.FolderPass, passErr string, cut bool, qerr error) (*types.FolderResume, string) {
	if !cut {
		return nil, passErr
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return nil, folderBackGivenUp(passErr, "this server's store cannot keep its record", res.Unsynced)
	}
	cur, err := st.RunLocalFolderStatus(rctx, run.ID)
	if err != nil {
		return nil, folderBackGivenUp(passErr, "its record could not be read", res.Unsynced)
	}
	next, errText := folderBackResume(cur.Resume, res, passErr)
	if next == nil {
		return nil, errText
	}
	if qerr != nil {
		return nil, folderBackGivenUp(passErr, "it cannot be kept with its agent stopped and no egress: "+qerr.Error(), next.Unsynced)
	}
	return next, errText
}

// folderBackResume is the resume a cut pass back leaves, given the one before it (nil: give up),
// and the error to record for it. A pass saved progress when it brought a file back, or sent one,
// and saved it in the runner's state: bytes of a file it did not finish are lost with the cut.
// res.Unsynced is set to the count the record names.
func folderBackResume(prev *types.FolderResume, res *runnerwire.FolderPass, errText string) (*types.FolderResume, string) {
	next := &types.FolderResume{Unsynced: res.Unsynced}
	if prev != nil && next.Unsynced == 0 {
		next.Unsynced = prev.Unsynced
	}
	if res.Pulled+res.Pushed == 0 {
		next.Stalled = 1
		if prev != nil {
			next.Stalled = prev.Stalled + 1
		}
	}
	res.Unsynced = next.Unsynced
	if next.Stalled < computerFolderStalls {
		return next, errText + "; the sandbox is kept with its agent stopped and no egress, and a later sweep resumes the sync"
	}
	return nil, folderBackGivenUp(errText, fmt.Sprintf("%d passes in a row brought nothing back", next.Stalled), next.Unsynced)
}

// folderBackGivenUp is the record of a cut pass back whose sandbox is stopped for why, with the
// count of the changes it leaves behind.
func folderBackGivenUp(errText, why string, unsynced int) string {
	left := "its changes not brought back yet, uncounted"
	if unsynced > 0 {
		left = fmt.Sprintf("changed files not brought back: %d", unsynced)
	}
	return fmt.Sprintf("%s; %s, so the sandbox was stopped with %s", errText, why, left)
}

// notQuiescable is quiesceKeptSandbox's answer for a sandbox whose substrate cannot stop its
// agent and its egress and keep the folder, as opposed to one where that failed.
type notQuiescable string

func (e notQuiescable) Error() string { return string(e) }

// errSandboxStopped is quiesceKeptSandbox's answer for a sandbox that no longer runs, as at a lease
// end whose EndSandbox stopped it: no pass can run in it, whatever stopping its agent would do.
var errSandboxStopped = errors.New("the sandbox had already stopped")

// unlessStopped is err, or errSandboxStopped when ref's sandbox reads as no longer running.
func (s *Server) unlessStopped(ctx context.Context, ref string, err error) error {
	if st, serr := s.cfg.Runner.Status(ctx, ref); serr == nil && (st.State == types.RunStopped || st.State == types.RunFailed) {
		return errSandboxStopped
	}
	return err
}

// keptAgentKill kills every process of a kept sandbox that runs as the exec's user, the agent's
// own, except the sandbox's idle main process: kill -1 never signals PID 1, whose loop
// (runner.AgentIdleScript) restarts its sleep. That is the agent and everything it started.
var keptAgentKill = []string{"/bin/sh", "-c", "kill -KILL -1 2>/dev/null; exit 0"}

// keptAgentStopWait bounds the wait for a killed agent's exec to read as ended.
const keptAgentStopWait = 2 * time.Second

// quiesceKeptSandbox leaves a sandbox past its run's end running the pass back alone, before each
// pass. Its proxy is stopped and removed (runner.ProxyStopper, as for a run lost to an outage), so
// nothing in it has egress, and its agent's processes are killed and seen ended (AgentStatus). The
// pass back is untouched: it is an exec through the substrate (execSFTPServer), not traffic
// through the proxy. It answers notQuiescable for a run on a runner, whose stop_proxy is a lost
// run's containment, queued only with its loss (a Kubernetes runner's deletes the agent pod when
// it cannot prove the stop), and for a substrate that cannot stop its proxy alone (Kubernetes in
// server mode). An exec-less sandbox, whose agent is its main process, is refused outright: its
// agent ends only with the sandbox, so no pass runs. A sandbox already stopped answers
// errSandboxStopped.
func (s *Server) quiesceKeptSandbox(ctx context.Context, run types.AgentRun, ref string) error {
	if run.RunnerID != nil {
		return notQuiescable("a sandbox on a runner is not kept")
	}
	stopper, ok := s.cfg.Runner.(runner.ProxyStopper)
	if !ok {
		return notQuiescable("its substrate cannot stop its proxy alone")
	}
	if err := stopper.StopProxy(ctx, ref); errors.Is(err, runner.ErrEndUnsupported) {
		return notQuiescable("its substrate cannot stop its proxy alone")
	} else if err != nil {
		// Not unlessStopped: Docker's StopProxy fails closed by stopping the agent's container, so
		// a stopped sandbox here is this failure's own doing, and the record names the proxy.
		return fmt.Errorf("its proxy did not stop: %w", err)
	}
	if run.AgentExecID == runner.MainProcessExecID {
		// An exec-less sandbox (the container is the agent): its agent ends only with the sandbox.
		return s.unlessStopped(ctx, ref, errors.New("its agent is the sandbox's main process, which cannot be stopped without stopping the sandbox"))
	}
	if run.AgentExecID == "" {
		return errors.New("its agent's process is not known")
	}
	sess, err := s.cfg.Runner.ExecStream(ctx, ref, runner.ExecSpec{Argv: keptAgentKill})
	if err != nil {
		return s.unlessStopped(ctx, ref, fmt.Errorf("its agent could not be stopped: %w", err))
	}
	for _, r := range []io.Reader{sess.Stdout, sess.Stderr} {
		if r != nil {
			go func() { _, _ = io.Copy(io.Discard, r) }()
		}
	}
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	if sess.Wait != nil {
		_, _ = sess.Wait()
	}
	if sess.Close != nil {
		_ = sess.Close()
	}
	deadline := time.Now().Add(keptAgentStopWait)
	for {
		st, err := s.cfg.Runner.AgentStatus(ctx, ref, run.AgentExecID)
		if err == nil && st.State != types.RunRunning {
			// The agent's own exec has ended; a process of another user, which keptAgentKill could not
			// signal, would survive unseen, so only the sandbox's init may be left.
			if err := s.verifyAgentGone(ctx, ref); err != nil {
				return s.unlessStopped(ctx, ref, fmt.Errorf("its agent could not be stopped: %w", err))
			}
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("its agent's status could not be read: %w", err)
			}
			return errors.New("its agent still reads as running")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("its agent still reads as running: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// computerFolderKept reports whether run id's sandbox is kept for its computer folder's pass back
// to resume. A record that cannot be read counts as kept: a sweep never tears down what it cannot
// check, and the next sweep reads it again.
func (s *Server) computerFolderKept(ctx context.Context, id uuid.UUID) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return false
	}
	rec, err := st.RunLocalFolderStatus(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	return err != nil || rec.Resume != nil
}

// resumeComputerFolderBacks resumes, on the orphan sweep's cadence and before it, each terminal
// run's pass back that kept its sandbox, through finalizeRunEffects: the path that kept it, which
// stops the sandbox once the pass completes or gives up. A run whose lock is held is left to a
// later sweep.
func (s *Server) resumeComputerFolderBacks(ctx context.Context) error {
	if _, ok := s.cfg.Store.(store.RunLocalFolderStatuses); !ok {
		return nil
	}
	runs, err := s.terminalRunsWithSandbox(ctx)
	if err != nil {
		return fmt.Errorf("resume computer folders: list runs: %w", err)
	}
	for _, run := range runs {
		if s.computerFolderKept(ctx, run.ID) {
			s.resumeComputerFolderBack(ctx, run.ID)
		}
	}
	return nil
}

func (s *Server) resumeComputerFolderBack(ctx context.Context, id uuid.UUID) {
	lctx, unlock, ok := s.tryLockRunOp(ctx, id)
	if !ok {
		return
	}
	defer unlock()
	run, err := s.cfg.Store.GetRun(lctx, id)
	if err != nil || !isTerminalRunState(run.State) || run.SandboxRef == "" || !s.computerFolderKept(lctx, id) {
		return
	}
	fctx, cancel := context.WithTimeout(lctx, reconcileFinalizeTimeout)
	defer cancel()
	_ = s.finalizeRunEffects(fctx, id, run.SandboxRef, "sandbox.orphan.sweep", map[string]any{"sandbox_ref": run.SandboxRef}, false, false)
}

// computerFolderAccountTime is the end of a pass back kept for the runner's account of a pass
// that was cut short: its stream is closed at the cut, and the runner answers with what it saved.
const computerFolderAccountTime = 5 * time.Second

// computerFolderPassWithin is computerFolderPass under a deadline of d derived from ctx: the pass
// ends by d, or by ctx's own deadline or cancel when that comes first. A pass with no time left
// does not start. cut is a pass that started and was stopped by that end; a pass back stops
// computerFolderAccountTime before it, for the runner's account. The caller records the outcome:
// the seed on ctx (seedComputerFolder), the pass back on teardownCtx, since a finalizing caller's
// ctx can end before d does (#3466).
func (s *Server) computerFolderPassWithin(ctx context.Context, d time.Duration, run types.AgentRun, folder types.RunLocalFolder, pull bool) (_ runnerwire.FolderPass, errText string, cut bool) {
	end := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Before(end) {
		end = dl
	}
	stop := end
	if pull {
		stop = end.Add(-computerFolderAccountTime)
	}
	passCtx, cancel := context.WithDeadline(ctx, stop)
	defer cancel()
	if err := passCtx.Err(); err != nil {
		return runnerwire.FolderPass{}, "no time was left for the sync: " + err.Error(), false
	}
	account, cancelAccount := context.WithDeadline(context.WithoutCancel(ctx), end)
	defer cancelAccount()
	res, cut, err := s.computerFolderPass(passCtx, account, run, folder, pull)
	if cut {
		return res, "the sync was cut short: " + passCtx.Err().Error(), true
	}
	if err != nil {
		return res, err.Error(), false
	}
	return res, res.Error, false
}

// auditComputerFolderPass records one pass as run.folder.transfer: direction seed (the computer to
// the sandbox, before the agent starts), back (the sandbox to the computer, when the run ends) or
// bring_back (the same pass, when the person ended the run to bring the folder back; they are its actor).
func (s *Server) auditComputerFolderPass(ctx context.Context, runID uuid.UUID, folder types.RunLocalFolder, back bool, res runnerwire.FolderPass, errText string) {
	data := map[string]any{"folder": folder.Name, "remote_dir": folder.RemoteDir, "direction": "seed", "files": res.Pushed, "bytes": res.BytesOut}
	actorType, actor := types.ActorSystem, "wardynd"
	if back {
		data["direction"], data["files"], data["bytes"] = "back", res.Pulled, res.BytesIn
		if by, ok := bringBackBy(ctx); ok {
			data["direction"], actorType, actor = "bring_back", by.actorType, by.principal
		}
	}
	outcome := "success"
	if errText != "" {
		outcome, data["error"] = "failure", localFolderSeedError(errText)
	}
	s.recordAudit(ctx, s.auditEvent(&runID, actorType, actor, "run.folder.transfer", runID.String(), outcome, mustJSON(data)))
}

func folderPassRecord(now time.Time, res runnerwire.FolderPass, errText string, back bool) *types.FolderPassRecord {
	rec := &types.FolderPassRecord{At: now, Files: res.Pushed, Bytes: res.BytesOut, Refused: []types.FolderRefusal{}}
	if back {
		rec.Files, rec.Bytes = res.Pulled, res.BytesIn
	}
	if errText != "" {
		rec.Error = localFolderSeedError(errText)
	}
	for _, r := range res.Refused[:min(len(res.Refused), runnerwire.MaxFolderRefusals)] {
		rec.Refused = append(rec.Refused, types.FolderRefusal{Path: localFolderSeedError(r.Path), Reason: localFolderSeedError(r.Reason)})
	}
	return rec
}

// recordComputerFolderPass applies set to run id's pass record, and reports whether it was written.
func (s *Server) recordComputerFolderPass(ctx context.Context, id uuid.UUID, set func(*types.RunLocalFolderStatus)) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return false
	}
	cur, err := st.RunLocalFolderStatus(ctx, id)
	if err == nil {
		set(&cur)
		ok, err = st.SetRunLocalFolderStatus(ctx, id, cur)
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: record a computer folder's pass", slog.String("run_id", id.String()), slog.Any("err", err))
	}
	return err == nil && ok
}

// computerFolderPass is one pass between run's computer folder and its sandbox: the runner's sftp
// client on one side, the sandbox's own sftp-server started in folder.RemoteDir on the other. The
// runner is checked again here: it must still be the run creator's own, online, sharing folders.
// A pass that ctx ends before it finishes is cut: its stream is closed, and the runner's account
// of what it saved is awaited on account, for at most computerFolderAccountTime.
func (s *Server) computerFolderPass(ctx, account context.Context, run types.AgentRun, folder types.RunLocalFolder, pull bool) (_ runnerwire.FolderPass, cut bool, _ error) {
	sub, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID)
	if refusal != nil {
		return runnerwire.FolderPass{}, false, errors.New(refusal.body.Error)
	}
	sess, reason, err := s.execSFTPServer(ctx, run, "-d", folder.RemoteDir)
	if err != nil {
		return runnerwire.FolderPass{}, false, errors.New(reason)
	}
	defer func() { _ = sess.Close() }()
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	pass, err := sub.SyncFolder(ctx, runnerwire.SyncFolderArgs{
		RunID: run.ID, Path: folder.Path, RemoteDir: folder.RemoteDir, Pull: pull, Deny: s.cfg.SyncDeny,
	})
	if err != nil {
		return runnerwire.FolderPass{}, false, fmt.Errorf("the runner could not start the sync: %w", err)
	}
	defer func() { _ = pass.Stream.Close() }()
	var joined sync.WaitGroup
	joined.Go(func() {
		_, _ = io.Copy(sess.Stdin, pass.Stream)
		_ = sess.Stdin.Close()
	})
	joined.Go(func() {
		_, _ = io.Copy(pass.Stream, sess.Stdout)
		_ = pass.Stream.CloseWrite()
	})
	res, err := pass.Wait(ctx)
	_ = pass.Stream.Close()
	_ = sess.Close()
	joined.Wait()
	if cut = err != nil && ctx.Err() != nil; cut {
		actx, cancel := context.WithTimeout(account, computerFolderAccountTime)
		defer cancel()
		if acc, aerr := pass.Wait(actx); aerr == nil {
			return acc, true, nil
		}
	}
	if err != nil {
		return runnerwire.FolderPass{}, cut, fmt.Errorf("the runner's sync did not finish: %w", err)
	}
	return res, false, nil
}

// computerFolderRunView is GET /runs/{id}'s computer_folder: the run's folder from the person's
// computer and its passes. The path on that computer is never shown, only its name.
type computerFolderRunView struct {
	Name      string                  `json:"name"`
	Runner    string                  `json:"runner"`
	RemoteDir string                  `json:"remote_dir"`
	Direction string                  `json:"direction"`
	State     string                  `json:"state"` // waiting, synced or failed: the first sync's
	Error     string                  `json:"error,omitempty"`
	Seed      *types.FolderPassRecord `json:"seed,omitempty"`
	Back      *types.FolderPassRecord `json:"back,omitempty"`
}

// computerFolderRunState values.
const (
	computerFolderWaiting = "waiting"
	computerFolderSynced  = "synced"
	computerFolderFailed  = "failed"
)

// computerFolderRunView reads run id's computer folder, nil when it has none.
func (s *Server) computerFolderRunView(ctx context.Context, id uuid.UUID) *computerFolderRunView {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return nil
	}
	folder, seed, err := st.RunLocalFolderSeed(ctx, id)
	if err != nil || folder == nil || folder.RunnerID == nil {
		return nil
	}
	v := &computerFolderRunView{Name: folder.Name, RemoteDir: folder.RemoteDir, Direction: folder.Direction, State: computerFolderWaiting}
	if rs, ok := s.cfg.Store.(runnerGetter); ok {
		if row, err := rs.GetRunner(ctx, *folder.RunnerID); err == nil {
			v.Runner = runnerLabel(row)
		}
	}
	switch {
	case seed.SeededAt != nil:
		v.State = computerFolderSynced
	case seed.Error != "":
		v.State, v.Error = computerFolderFailed, seed.Error
	}
	if ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok {
		if rec, err := ss.RunLocalFolderStatus(ctx, id); err == nil {
			v.Seed, v.Back = rec.Seed, rec.Back
		}
	}
	return v
}
