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
const computerFolderOffMsg = "Your organisation doesn't allow folders from personal computers (WARDYN_COMPUTER_FOLDERS is off)"

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
// reads it as it reads a `wardyn sync` pass. The seed owns the record's next generation before it
// passes, and writes only at it: a seed that lost it (another owner moved the record on) records,
// audits and publishes nothing, and leaves the run to that owner.
func (s *Server) seedComputerFolder(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, wait time.Duration) {
	prev := s.folderStatus(ctx, run.ID)
	// Asked before the bump, which moves the record off the generation the database ages (lastPassPast).
	past := s.lastPassPast(ctx, run.ID, prev)
	rec, ok, _ := s.bumpComputerFolderGen(ctx, run.ID, prev.Gen)
	if !ok {
		return
	}
	gen := rec.Gen
	out := s.computerFolderPassWithin(ctx, wait, run, folder, false, passGuard{last: prev, past: past})
	res, errText := out.FolderPass, out.errText
	// Recorded on ctx, not the pass's own deadline: a pass that timed out still leaves its record.
	recorded := s.writeComputerFolderStatus(ctx, run.ID, gen, func(st *types.RunLocalFolderStatus) {
		st.Seed = folderPassRecord(s.cfg.Now(), res, errText, false)
		if out.settled {
			st.Settled = gen
		}
	})
	if _, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok && !recorded {
		return
	}
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
// the rest up, and so does a pass that left changes behind or could not prove them kept
// (folderBackVerdict). A pass that completes with nothing owed, fails on its first visit or does not
// start does not, nor does a sandbox that cannot be quiesced. The runner's state file holds what each pass saved, so the next pass, from
// the tail owner (folderCleanupLoop), starts after it (#3466).
func (s *Server) bringComputerFolderBack(ctx context.Context, runID uuid.UUID, ref string) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok || ref == "" {
		return false
	}
	// The reads, like the record, are on a teardownCtx: a caller already cancelled still gets the
	// record that no time was left.
	rctx, cancel := teardownCtx(ctx)
	if ok, err := s.folderBackEligible(rctx, runID); err != nil || !ok {
		cancel()
		return false
	}
	folder, _, err := st.RunLocalFolderSeed(rctx, runID)
	if err != nil || folder == nil {
		cancel()
		return false
	}
	var prev types.RunLocalFolderStatus
	unread := false
	if ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok {
		if rec, err := ss.RunLocalFolderStatus(rctx, runID); err == nil {
			// A record given up is final: no pass runs again, and the sandbox is stopped.
			if folderFinal(rec) {
				cancel()
				return false
			}
			prev = rec
		} else {
			unread = true
		}
	}
	run, err := s.cfg.Store.GetRun(rctx, runID)
	cancel()
	if err != nil {
		return false
	}
	// Killing a run doesn't bring the folder back (bringBackNotRunningMsg).
	if run.State == types.RunKilled {
		return false
	}
	run.SandboxRef = ref
	// While the last pass is open (lastPassOpen), no pass is sent (keepForOpenPass). Whether it is past its bound
	// is read once, here, before the bump below moves the record on (lastPassPast), and every check after the bump
	// reads that answer.
	octx, ocancel := teardownCtx(ctx)
	past := s.lastPassPast(octx, runID, prev)
	guard := passGuardFor(run, prev, unread, past)
	open := s.lastPassOpen(octx, run, *folder, guard)
	ocancel()
	if open {
		return s.keepForOpenPass(ctx, run, *folder, guard, ref)
	}
	// The pass back owns the next generation of the record, and writes only at it: an owner that
	// lost it (another moved the record on) writes nothing and leaves the sandbox to that owner.
	rec, ok, lost := s.bumpComputerFolderGen(ctx, runID, prev.Gen)
	if !ok {
		// Kept only for a bump lost to an owner a fresh read proves moved the record on. A store error (whose
		// bump may have landed), or a record never read, stops the sandbox, so a terminal run does not keep
		// its agent and egress until a sweep; the run then says why (recordFolderBumpMissed).
		return s.keepAfterBumpMiss(ctx, runID, *folder, prev.Gen, lost, unread)
	}
	if folderBroughtBack(rec) {
		// A pass back completed between the read above and the bump: none runs again.
		return false
	}
	gen := rec.Gen
	rctx, cancel = teardownCtx(ctx)
	defer cancel()
	if unansweredStallsPast(prev, past) {
		errText := folderBackGivenUp("the runner did not answer for its last pass back",
			fmt.Sprintf("%d passes in a row brought nothing back", prev.Resume.Stalled), prev.Resume.Unsynced)
		return s.recordFolderBackAt(rctx, runID, *folder, gen, true, nil, runnerwire.FolderPass{}, errText)
	}
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
		errText := folderBackGivenUp("no pass ran", "its agent and egress could not be stopped first: "+qerr.Error(), 0)
		if _, ok := bringBackBy(ctx); ok {
			errText = bringBackAgentRunningMsg
		}
		if errors.Is(qerr, errSandboxStopped) {
			errText = "no pass ran; the sandbox had already stopped, with its changes not brought back yet, uncounted"
		}
		// Nothing was sent, so this generation is settled.
		return s.recordFolderBackAt(rctx, runID, *folder, gen, true, nil, runnerwire.FolderPass{}, errText)
	}
	d := computerFolderBackTimeout
	if dl, ok := ctx.Deadline(); ok {
		d = min(d, time.Until(dl)-runTeardownBudget)
	}
	out := s.computerFolderPassWithin(ctx, d, run, *folder, true, guard)
	switch {
	case out.busy:
		return s.keepBusy(ctx, run, *folder, gen, qerr)
	case out.away:
		return s.keepAway(ctx, run, *folder, gen, qerr)
	case out.unconfirmed:
		// The runner the pass was to go to turned out to be one that may still run the last: kept, as
		// lastPassOpen keeps it, with nothing counted.
		return s.keepUnconfirmed(ctx, run, *folder, gen, rec, prev.GenAt, qerr)
	}
	res, passErr, cut := out.FolderPass, out.errText, out.cut
	next, errText := s.folderBackVerdict(ctx, run, &res, passErr, cut, qerr)
	if !out.settled {
		next, errText = keptUnanswered(prev.Resume, next, errText, passErr, res.Unsynced, qerr)
	}
	return s.recordFolderBackAt(rctx, runID, *folder, gen, out.settled, next, res, errText)
}

// recordFolderBackAt records a pass back's outcome at the generation it owns, settled when the runner
// answered for it or nothing was sent, and audits it. It reports whether the sandbox stays up: for next,
// a resume; and when the record did not land because another owner moved the generation on (read again,
// folderGenTaken), who then decides the sandbox. A record that did not land for any other reason stops it.
func (s *Server) recordFolderBackAt(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int, settled bool, next *types.FolderResume, res runnerwire.FolderPass, errText string) bool {
	recorded := s.writeComputerFolderStatus(ctx, id, gen, func(st *types.RunLocalFolderStatus) {
		st.Resume, st.Back = next, folderPassRecord(s.cfg.Now(), res, errText, true)
		if settled {
			st.Settled = gen
		}
	})
	if _, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok && !recorded {
		return s.folderGenTaken(ctx, id, gen)
	}
	s.auditComputerFolderPass(ctx, id, folder, true, res, errText)
	return next != nil
}

// BringComputerFolderBack is bringComputerFolderBack for cmd/wardynd's idle and max-age stop, under the
// run's lock (LockRunEnd), after its transition to STOPPED and before its StopSandbox. true keeps the
// sandbox for the tail owner's pass (folderCleanupLoop), its agent stopped and its egress cut.
func (s *Server) BringComputerFolderBack(ctx context.Context, runID uuid.UUID, ref string) bool {
	return s.bringComputerFolderBack(ctx, runID, ref)
}

// folderBackVerdict is the resume a pass back leaves (nil: stop the sandbox now) and the error to
// record for it, read against the resume before it (folderBackResume). Only a sandbox quiesced
// before the pass (qerr nil) is kept; one that was not is stopped, and the record says why.
//
// A pass owes the laptop changes when it was cut, when the runner could not prove a change kept there
// (res.NotPreserved), or when it left any behind (res.Unsynced), whatever error it ended with: it is
// kept for a resume, a stall when it moved nothing, and never recorded as done. A pass that owes nothing
// completes, or, failed, settles on a first visit; on a resume visit a failed one is a stall. Refusals
// alone (res.Refused) owe nothing.
func (s *Server) folderBackVerdict(ctx context.Context, run types.AgentRun, res *runnerwire.FolderPass, passErr string, cut bool, qerr error) (*types.FolderResume, string) {
	owed := cut || res.NotPreserved != "" || res.Unsynced > 0
	if !owed && passErr == "" {
		return nil, ""
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok && !owed {
		return nil, passErr
	}
	if !ok {
		return nil, folderBackGivenUp(passErr, "this server's store cannot keep its record", res.Unsynced)
	}
	cur, err := st.RunLocalFolderStatus(rctx, run.ID)
	if err != nil {
		return nil, folderBackGivenUp(passErr, "its record could not be read", res.Unsynced)
	}
	if !owed && cur.Resume == nil {
		return nil, passErr
	}
	switch {
	case res.NotPreserved != "":
		passErr = folderNotPreservedText(res.Unsynced, res.NotPreserved, passErr)
	case !cut && passErr == "":
		passErr = fmt.Sprintf("the pass back left %d changed files not brought back: a file changed on both sides could not be kept as a conflict copy", res.Unsynced)
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

// folderBackResume is the resume a pass back that owes changes leaves, or a failed resume, given the
// one before it (nil: give up),
// and the error to record for it. A pass saved progress when it brought a file back, or sent one,
// and saved it in the runner's state: bytes of a file it did not finish are lost with the cut.
// res.Unsynced is set to the count the record names.
func folderBackResume(prev *types.FolderResume, res *runnerwire.FolderPass, errText string) (*types.FolderResume, string) {
	next := folderResumeFor(prev, res)
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
		return next, "the sandbox is kept with its agent stopped and no egress, and a later sweep, about hourly, tries the sync again: " + boundReason(errText)
	}
	return nil, folderBackGivenUp(errText, fmt.Sprintf("%d passes in a row brought nothing back", next.Stalled), next.Unsynced)
}

// folderNotPreservedText records a pass back whose runner could not prove kept on the laptop the
// changes it owed (res.NotPreserved): never a success, never a policy refusal.
func folderNotPreservedText(unsynced int, why, passErr string) string {
	s := "the pass back could not confirm its changes kept on the laptop, so they count as not copied back"
	if unsynced > 0 {
		s = fmt.Sprintf("the pass back could not confirm %d changed files kept on the laptop, so they count as not copied back", unsynced)
	}
	s += ": " + boundReason(why)
	if passErr != "" && !strings.Contains(passErr, why) {
		s += "; " + boundReason(passErr)
	}
	return s
}

// folderBackGivenUp is the record of a cut pass back whose sandbox is stopped for why, with the
// count of the changes it leaves behind.
func folderBackGivenUp(errText, why string, unsynced int) string {
	left := "its changes not brought back yet, uncounted"
	if unsynced > 0 {
		left = fmt.Sprintf("changed files not brought back: %d", unsynced)
	}
	return fmt.Sprintf("the sandbox was stopped with %s, because %s: %s", left, why, boundReason(errText))
}

// maxFolderReason bounds the runner's or the sandbox's reason in a pass back's record, which puts its
// outcome first: a long reason is cut, and the outcome before it stays.
const maxFolderReason = 300

// boundReason is a pass back's reason as its record keeps it: one line of at most maxFolderReason runes.
func boundReason(s string) string { return localFolderErrorText(s, maxFolderReason) }

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
// through the proxy. It answers notQuiescable for a linux_vm run, whose guest the server's Docker
// cannot reach, and for a run on a runner, whose stop_proxy is a lost run's containment, queued only with its loss (a Kubernetes runner's deletes the agent pod when
// it cannot prove the stop), and for a substrate that cannot stop its proxy alone (Kubernetes in
// server mode). An exec-less sandbox, whose agent is its main process, is refused outright: its
// agent ends only with the sandbox, so no pass runs. A sandbox already stopped answers
// errSandboxStopped.
func (s *Server) quiesceKeptSandbox(ctx context.Context, run types.AgentRun, ref string) error {
	if !isTerminalRunState(run.State) {
		// A run still RUNNING (a person's bring-back, or a first pass kept at the run's end) is quiesced as
		// before the cut existed: cutAgent cuts only an ended run's agent.
		if run.RunnerID != nil {
			return notQuiescable("a sandbox on a runner is not kept")
		}
		if run.MachineKind.IsVM() {
			return notQuiescable("a virtual machine is not quiesced from the server's Docker")
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
		return s.killKeptAgent(ctx, run, ref)
	}
	if run.MachineKind.IsVM() {
		return notQuiescable("a virtual machine is not quiesced from the server's Docker")
	}
	if run.RunnerID != nil {
		if !s.keepable(run) {
			return notQuiescable("a sandbox on a runner is not kept")
		}
		if kind, err := s.cutAgent(ctx, run); kind != cutFenced || err != nil {
			return notQuiescable("a sandbox on a runner is not kept")
		}
	} else if kind, err := s.cutAgent(ctx, run); kind == cutUnsupported && err == nil || errors.Is(err, runner.ErrEndUnsupported) {
		return notQuiescable("its substrate cannot stop its proxy alone")
	} else if err != nil {
		return fmt.Errorf("its proxy did not stop: %w", err)
	}
	err := s.killKeptAgent(ctx, run, ref)
	if err != nil && run.RunnerID != nil && !errors.Is(err, errSandboxStopped) {
		// A runner run answers as one that cannot be quiesced, which takes the unquiesced first pass; a
		// failed kill after its cut keeps that answer, and the cut stays.
		return notQuiescable("a sandbox on a runner is not kept: " + err.Error())
	}
	return err
}

// killKeptAgent kills every process of a kept sandbox's agent and checks it is gone: the agent kill that
// follows the sandbox's proxy being stopped or its agent cut off.
func (s *Server) killKeptAgent(ctx context.Context, run types.AgentRun, ref string) error {
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

// computerFolderKeptRead reports whether run id's record holds a resume: its sandbox is kept for its computer
// folder's pass back to resume. A record that cannot be read is the error, no answer (a caller that treats it
// as kept says so). The sweeps ask keepsSandboxForFolder, which also keeps a pass back owed and not yet run;
// reloseEnded asks this, since its keep is proof that a stop was observed.
func (s *Server) computerFolderKeptRead(ctx context.Context, id uuid.UUID) (bool, error) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return false, nil
	}
	rec, err := st.RunLocalFolderStatus(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return rec.Resume != nil, nil
}

// keepsSandboxForFolder reports whether a destructive sweep must keep terminal run's sandbox for its computer
// folder's pass back, which the tail owner (folderCleanupLoop) runs: a record holding a resume (the keep
// computerFolderKeptRead answers), or a pass back still owed with none yet run (folderOwed: an eligible folder,
// a terminal run that was not killed, a ref, nothing given up). The second half is the crash window between a
// run's terminal commit and its tail, which leaves a seeded record with neither a Back nor a Resume. Time plays no
// part: at and past the cap the sandbox is still the tail owner's, whose give-up writes its durable record before
// anything is stopped (for a runner away too: tailGiveUpAway). An error is a record or rule that could not be read,
// which is no answer: the sweeps decide it through folderKeepVerdict, which keeps it only on a confirmed cut.
// reloseEnded does not use it: there a keep is proof that a stop was already observed, which an owed pass back
// that has not run is not.
func (s *Server) keepsSandboxForFolder(ctx context.Context, run types.AgentRun) (bool, error) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return false, nil
	}
	rec, err := st.RunLocalFolderStatus(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case rec.Resume != nil:
		return true, nil
	}
	return s.folderOwed(ctx, run, rec)
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
func (s *Server) computerFolderPassWithin(ctx context.Context, d time.Duration, run types.AgentRun, folder types.RunLocalFolder, pull bool, last passGuard) folderPassOutcome {
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
		return folderPassOutcome{errText: "no time was left for the sync: " + err.Error(), settled: true}
	}
	account, cancelAccount := context.WithDeadline(context.WithoutCancel(ctx), end)
	defer cancelAccount()
	res, cut, settled, err := s.computerFolderPass(passCtx, account, run, folder, pull, last)
	out := folderPassOutcome{FolderPass: res, cut: cut, settled: settled, busy: errors.Is(err, runner.ErrFolderPassBusy),
		unconfirmed: errors.Is(err, errFolderPassUnconfirmed)}
	var away folderRunnerAway
	out.away = errors.As(err, &away)
	switch {
	case cut:
		out.errText = "the sync was cut short: " + passCtx.Err().Error()
	case err != nil:
		out.errText = err.Error()
	default:
		out.errText = res.Error
	}
	return out
}

// folderPassOutcome is one pass as computerFolderPassWithin ended it. settled is a pass the runner
// answered for, or one that sent nothing; busy is a runner refusing it while it runs another pass for
// the run (folder_pass_busy): a pass that did not start; unconfirmed is a pass not sent, as the runner
// may still run the last one (errFolderPassUnconfirmed); away is a pass not sent because the computer's runner
// is offline or not connected here (folderRunnerAway).
type folderPassOutcome struct {
	runnerwire.FolderPass
	errText                               string
	cut, settled, busy, unconfirmed, away bool
}

// folderRunnerAway is the refusal of a pass because the computer's runner is offline or not connected here: the
// refusal's own text, as an error that says so.
type folderRunnerAway struct{ text string }

func (e folderRunnerAway) Error() string { return e.text }

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
		outcome, data["error"] = "failure", folderPassErrorText(errText, back)
	}
	s.recordAudit(ctx, s.auditEvent(&runID, actorType, actor, "run.folder.transfer", runID.String(), outcome, mustJSON(data)))
}

// folderPassErrorText is a pass's error as its record and audit row keep it: a seed's as the client's, a pass
// back's within maxFolderBackError, so the outcome its text ends with is kept.
func folderPassErrorText(errText string, back bool) string {
	if back {
		return localFolderErrorText(errText, maxFolderBackError)
	}
	return localFolderSeedError(errText)
}

func folderPassRecord(now time.Time, res runnerwire.FolderPass, errText string, back bool) *types.FolderPassRecord {
	rec := &types.FolderPassRecord{At: now, Files: res.Pushed, Bytes: res.BytesOut, Refused: []types.FolderRefusal{}}
	if back {
		rec.Files, rec.Bytes = res.Pulled, res.BytesIn
	}
	if errText != "" {
		rec.Error = folderPassErrorText(errText, back)
	}
	for _, r := range res.Refused[:min(len(res.Refused), runnerwire.MaxFolderRefusals)] {
		rec.Refused = append(rec.Refused, types.FolderRefusal{Path: localFolderSeedError(r.Path), Reason: localFolderSeedError(r.Reason)})
	}
	return rec
}

// folderStatus is run id's pass record as read (empty for none or a read that failed: a bump from its
// generation then fails on a record at another).
func (s *Server) folderStatus(ctx context.Context, id uuid.UUID) types.RunLocalFolderStatus {
	if st, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok {
		if rec, err := st.RunLocalFolderStatus(ctx, id); err == nil {
			return rec
		}
	}
	return types.RunLocalFolderStatus{}
}

// writeComputerFolderStatus is the one write of a run's pass record: set applied to the record as read,
// written only while the stored one is still at gen, the generation its pass moved the record to
// (bumpComputerFolderGen), so a write after another owner moved it on does not land.
func (s *Server) writeComputerFolderStatus(ctx context.Context, id uuid.UUID, gen int, set func(*types.RunLocalFolderStatus)) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return false
	}
	cur, err := st.RunLocalFolderStatus(ctx, id)
	if err == nil {
		ok = false
		if cur.Gen == gen {
			set(&cur)
			ok, err = st.SetRunLocalFolderStatusIf(ctx, id, store.FolderStatusGuard{Gen: gen}, cur)
		}
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
// last is what the pass is checked against: while the runner may still run the last pass (lastPassOpen,
// checked again here on the endpoint the pass is sent to), nothing is sent and nothing is settled.
func (s *Server) computerFolderPass(ctx, account context.Context, run types.AgentRun, folder types.RunLocalFolder, pull bool, last passGuard) (_ runnerwire.FolderPass, cut, settled bool, _ error) {
	sub, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID)
	if mayStillRun(last) && passMayRunBeside(sub, refusal) {
		if refusal != nil {
			return runnerwire.FolderPass{}, false, false, fmt.Errorf("%s; %w", refusal.body.Error, errFolderPassUnconfirmed)
		}
		return runnerwire.FolderPass{}, false, false, errFolderPassUnconfirmed
	}
	if refusal != nil {
		if refusal.body.Reason == reasonComputerRunnerOffline {
			return runnerwire.FolderPass{}, false, true, folderRunnerAway{refusal.body.Error}
		}
		return runnerwire.FolderPass{}, false, true, errors.New(refusal.body.Error)
	}
	sess, reason, err := s.execSFTPServer(ctx, run, "-d", folder.RemoteDir)
	if err != nil {
		return runnerwire.FolderPass{}, false, true, errors.New(reason)
	}
	defer func() { _ = sess.Close() }()
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}
	pass, err := sub.SyncFolder(ctx, runnerwire.SyncFolderArgs{
		RunID: run.ID, Path: folder.Path, RemoteDir: folder.RemoteDir, Pull: pull, Deny: s.cfg.SyncDeny,
		DeadlineUnixMs: passDeadlineMs(ctx),
	})
	if err != nil {
		// A pass that ended before its stream was taken has its own account: settled, as any pass the runner answered for.
		var ended *remote.PassEnded
		if errors.As(err, &ended) {
			return ended.Pass, false, true, nil
		}
		return runnerwire.FolderPass{}, false, runnerReplied(err), fmt.Errorf("the runner could not start the sync: %w", err)
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
			return acc, true, true, nil
		}
	}
	if err != nil {
		return runnerwire.FolderPass{}, cut, !cut && runnerReplied(err), fmt.Errorf("the runner's sync did not finish: %w", err)
	}
	return res, false, true, nil
}

// passDeadlineMs is when the runner must end the pass: the pass ctx's own deadline, unchanged
// (computerFolderPassWithin already ends that ctx one account window before the pass's end); 0 without one.
func passDeadlineMs(ctx context.Context) int64 {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return dl.UnixMilli()
}

// runnerReplied is an error the runner answered with (a typed refusal): the call was answered, not lost.
func runnerReplied(err error) bool {
	var werr *runnerwire.Error
	return errors.As(err, &werr)
}

// computerFolderRunView is GET /runs/{id}'s computer_folder: the run's folder from the person's
// computer and its passes. The path on that computer is never shown, only its name.
type computerFolderRunView struct {
	Name      string          `json:"name"`
	Runner    string          `json:"runner"`
	RemoteDir string          `json:"remote_dir"`
	Direction string          `json:"direction"`
	State     string          `json:"state"` // waiting, synced or failed: the first sync's
	Error     string          `json:"error,omitempty"`
	Seed      *folderPassView `json:"seed,omitempty"`
	Back      *folderPassView `json:"back,omitempty"`
	// Fate, SandboxCopy, LastReport and GiveUpAt are the fate of the changes not yet back (folderFateOf); each is
	// absent when the run is live or the server cannot tell.
	Fate        *string           `json:"fate,omitempty"`
	SandboxCopy *string           `json:"sandbox_copy,omitempty"`
	LastReport  *folderLastReport `json:"last_report,omitempty"`
	GiveUpAt    *time.Time        `json:"give_up_at,omitempty"`
}

// folderPassView is a pass record as the folder's views show it: types.FolderPassRecord without GivenUpAudited,
// the server's own note that the given_up audit row was written. That field cannot be tagged json:"-" on the
// record, because its tag is also the stored JSONB key that MarkGivenUpAudited writes and guards on.
type folderPassView struct {
	At               time.Time             `json:"at"`
	Files            int                   `json:"files"`
	Bytes            int64                 `json:"bytes"`
	Refused          []types.FolderRefusal `json:"refused"`
	Error            string                `json:"error,omitempty"`
	GivenUpAt        *time.Time            `json:"given_up_at,omitempty"`
	GivenUpCount     *int                  `json:"given_up_count,omitempty"`
	GivenUpPaths     []string              `json:"given_up_paths,omitempty"`
	GivenUpPathsMore int                   `json:"given_up_paths_more,omitempty"`
}

// newFolderPassView copies rec into its view; nil for nil.
func newFolderPassView(rec *types.FolderPassRecord) *folderPassView {
	if rec == nil {
		return nil
	}
	return &folderPassView{At: rec.At, Files: rec.Files, Bytes: rec.Bytes, Refused: rec.Refused, Error: rec.Error,
		GivenUpAt: rec.GivenUpAt, GivenUpCount: rec.GivenUpCount, GivenUpPaths: rec.GivenUpPaths, GivenUpPathsMore: rec.GivenUpPathsMore}
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
			v.Seed, v.Back = newFolderPassView(rec.Seed), newFolderPassView(rec.Back)
			if run, err := s.cfg.Store.GetRun(ctx, id); err == nil {
				s.setFolderFate(ctx, v, run, rec, seed.SeededAt != nil)
			}
		}
	}
	return v
}
