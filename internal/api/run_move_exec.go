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
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The move's executor: "stop-pause, export, relay and scan, import, commit, resume" on the same run id. The run is
// stopped under its lock only once the destination is accepted, its files go from the source runner through this
// process to the destination as one verified stream that is never written to disk here, and the run row is bound to
// the destination in one conditional transaction. Every path from admission to commit ends the move's row in a named
// state with a reason and one run.move audit row. The executor holds the run with a heartbeat until it releases it;
// a hold that goes stale is failed or released by the move sweep, which also tears down a failed move's destination,
// removes a committed move's source and retries the second revoke.

const (
	// moveSweepInterval is the move sweep's cadence.
	moveSweepInterval = time.Minute
	// moveHeartbeatEvery is how often the executor renews its hold, well inside store.RunMoveOrphanAfter.
	moveHeartbeatEvery = 30 * time.Second
	// moveSweepBatch bounds the rows one sweep pass reads.
	moveSweepBatch = 100
	// moveADORevoked is the receipt's Azure DevOps outcome a move needs: every PAT the source minted is revoked.
	moveADORevoked = "revoked"
)

// The pause reasons the move reads: the stopped pause it moves, and a Resume's claim of one.
const (
	movePauseStopped  = types.PauseReason("stopped")
	movePauseResuming = types.PauseReason("resuming")
)

// errMoveImportEnded ends the relay's pipe once the destination's import has returned.
var errMoveImportEnded = errors.New("the destination's import ended")

// moveEnd is how a move's row ends before commit: refused or failed, with its reason. A nil end is no end.
type moveEnd struct {
	state  types.MoveState
	reason string
	msg    string
}

func failMove(reason, msg string) *moveEnd {
	return &moveEnd{state: types.MoveFailed, reason: reason, msg: msg}
}

func refuseMoveEnd(reason, msg string) *moveEnd {
	return &moveEnd{state: types.MoveRefused, reason: reason, msg: msg}
}

// moveLapsed is the end of an executor whose hold lapsed, or whose write could not land: it writes nothing more and
// makes no runner call, and the move sweep fails or releases the row.
var moveLapsed = &moveEnd{}

// moveExec is one move, from its admitted row to its release.
type moveExec struct {
	s         *Server
	ms        store.RunMoveStore
	mv        types.RunMove
	run       types.AgentRun
	rec       runSandboxSpecRecord
	dest      moveDest
	caps      runner.Capabilities
	plan      moveDestPlan
	sc        types.SiteConfig
	actorType types.ActorType
	principal string
	// stopped: the run was already in a stopped pause at admission, so the locked sequence runs in the request.
	stopped  bool
	maxBytes int64

	// work is the row a new work drive's doors admitted at admission (nil: the destination needs none), workMade says
	// the move asked the store for it, and workMount is its mount once made.
	work      *keptWorkFolder
	workMade  bool
	workMount *types.DriveMount
	// destDrives are the destination spec's drives, which commit writes over the record's.
	destDrives []types.DriveMount

	jti      string
	destRef  string
	drained  bool
	manifest runner.CarriedManifest
	lapsed   atomic.Bool
}

// stamp renews the executor's hold before a mutating step. False means the hold has lapsed or is about to: the
// executor stops.
func (x *moveExec) stamp(ctx context.Context) bool {
	if x.lapsed.Load() {
		return false
	}
	ok, err := x.ms.StampRunMoveHeartbeat(ctx, x.mv.ID)
	if err != nil || !ok {
		x.lapsed.Store(true)
		return false
	}
	return true
}

// keepHeartbeat renews the hold every moveHeartbeatEvery until stopped; a renewal that changes no row cancels the
// executor's context.
func (x *moveExec) keepHeartbeat(ctx context.Context, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(moveHeartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !x.stamp(ctx) {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// moveGen is a row's pause generation: 0 for a run that is not paused.
func moveGen(ctx context.Context, run types.AgentRun) (int64, error) {
	if run.PausedAt == nil {
		return 0, nil
	}
	return movePortPauseGen.call(ctx, run)
}

// committedPauseGen is the pause generation of the run's newest committed move, -1 when it has none: the stopped
// pause such a move left at its destination is resumed before the run moves again. A failed read is an error, which
// the caller refuses on.
func (x *moveExec) committedPauseGen(ctx context.Context) (int64, error) {
	moves, err := x.ms.ListRunMoves(ctx, x.run.ID)
	if err != nil {
		return 0, err
	}
	gen := int64(-1)
	for _, m := range moves {
		if m.State == types.MoveCommitted && m.ID != x.mv.ID {
			gen = m.PauseGen
		}
	}
	return gen, nil
}

const (
	moveResumeFirstMsg = "Resume the run before moving it again."
	movePauseAgainMsg  = "Resume and pause the run again, then move it."
	moveRevokeOpenMsg  = "The pause's revoke did not finish (a token revoke, the lease end, the broker or its record), so the run stays where it is, paused; resume it and try again."
)

// stopLocked is the locked sequence (step 2): under the run's lock, the held re-read, the stop for a run that is not
// already in a stopped pause, the settle, the receipt and the cut, and the cut lineage's token stamp. Every write
// under the lock is one statement, and the lock is released before anything is created or carried.
func (x *moveExec) stopLocked(ctx context.Context) *moveEnd {
	s := x.s
	lctx, unlock, err := s.lockRunOp(ctx, x.run.ID)
	if err != nil {
		return failMove(reasonMoveStopFailed, "The run's lock could not be taken, so nothing was stopped.")
	}
	defer unlock()
	cur, err := s.getRunHeld(lctx, x.run.ID)
	if err != nil {
		return failMove(reasonMoveStopFailed, "The run could not be read under its lock, so nothing was stopped.")
	}
	if end := x.heldReread(lctx, cur); end != nil {
		return end
	}
	var stopErr error
	if !x.stopped {
		if !x.stamp(lctx) {
			return moveLapsed
		}
		_, stopErr = movePortPauseStop.call(lctx, moveStopArgs{Run: cur, ActorType: string(x.actorType), Principal: x.principal})
		after, err := s.getRunHeld(lctx, x.run.ID)
		if err != nil || after.PausedReason != movePauseStopped || after.PausedAt == nil {
			return failMove(reasonMoveStopFailed, "The run could not be stopped, so nothing was carried.")
		}
		gen, err := movePortPauseGen.call(lctx, after)
		if err != nil {
			return failMove(reasonMoveStopFailed, "The stopped run's pause could not be read.")
		}
		if ok, err := x.ms.SetRunMovePauseGen(lctx, x.mv.ID, gen); err != nil || !ok {
			return failMove(reasonMoveStopFailed, "The stopped run's pause could not be recorded on the move.")
		}
		x.mv.PauseGen = gen
	}
	if err := s.settleOwedPause(lctx, x.run.ID); err != nil {
		return refuseMoveEnd(reasonMovePauseRefused, "The run's sandbox could not be settled into its stopped pause.")
	}
	rs, rerr := movePortRevokeFor.call(lctx, moveGenArgs{RunID: x.run.ID, Gen: x.mv.PauseGen})
	cut, cerr := movePortLineageCut.call(lctx, x.run.ID)
	if rerr != nil || cerr != nil || !rs.Complete || !cut {
		if x.stopped {
			return refuseMoveEnd(reasonMovePauseRefused, movePauseAgainMsg)
		}
		return refuseMoveEnd(reasonMovePauseRefused, moveRevokeOpenMsg)
	}
	if rs.ADO != moveADORevoked {
		return refuseMoveEnd(reasonMoveRevokeRefused, "An Azure DevOps token of the run was not revoked, so the run stays where it is; resume it, pause it again, then move it.")
	}
	if stopErr != nil {
		return failMove(reasonMoveStopFailed, "The run's stop reported an error after it was recorded.")
	}
	return x.stampCutLineage(lctx)
}

// heldReread is the locked sequence's first item: the run is still RUNNING and not lost, not claimed by a Resume, at
// the generation the move admitted, and not the stopped pause a committed move left.
func (x *moveExec) heldReread(lctx context.Context, cur types.AgentRun) *moveEnd {
	if cur.State != types.RunRunning || cur.LostAt != nil || cur.PausedReason == movePauseResuming {
		return refuseMoveEnd(reasonMoveNotRunning, "The run is no longer running, so it was not moved.")
	}
	gen, err := moveGen(lctx, cur)
	if err != nil {
		return failMove(reasonMoveStopFailed, "The run's pause could not be read under its lock.")
	}
	committed, err := x.committedPauseGen(lctx)
	if err != nil {
		return failMove(reasonMoveStopFailed, "The run's earlier moves could not be read under its lock.")
	}
	if cur.PausedReason == movePauseStopped && gen == committed {
		return refuseMoveEnd(reasonMoveNotRunning, moveResumeFirstMsg)
	}
	if gen != x.mv.PauseGen {
		return refuseMoveEnd(reasonMoveNotRunning, "The run's pause changed since the move was asked for, so it was not moved.")
	}
	return nil
}

// stampCutLineage is the locked sequence's last item: the token stamp on the cut lineage, so the import's lease is
// not refused as lapsed. No row stamped means the run is no longer one the stamp may touch; one re-read chooses why.
func (x *moveExec) stampCutLineage(lctx context.Context) *moveEnd {
	ok, err := movePortStampCutLineage.call(lctx, x.run.ID)
	if err != nil {
		return failMove(reasonMoveStopFailed, "The run's token stamp could not be written, so nothing was carried.")
	}
	if ok {
		return nil
	}
	cur, err := x.s.getRunHeld(lctx, x.run.ID)
	if err != nil {
		return failMove(reasonMoveStopFailed, "The run could not be read again after its token stamp changed nothing.")
	}
	gen, gerr := moveGen(lctx, cur)
	committed, cerr := x.committedPauseGen(lctx)
	pick := func(atPause, afterStop, msg string) *moveEnd {
		if x.stopped {
			return refuseMoveEnd(atPause, msg)
		}
		return failMove(afterStop, msg)
	}
	switch {
	case cur.State != types.RunRunning || cur.LostAt != nil:
		return pick(reasonMoveNotRunning, reasonMoveRunEnded, "The run ended or was lost while it was moved.")
	case cerr != nil:
		return failMove(reasonMoveStopFailed, "The run's earlier moves could not be read after its token stamp changed nothing.")
	case gerr == nil && cur.PausedReason == movePauseStopped && gen == committed:
		return pick(reasonMoveNotRunning, reasonMoveNotRunning, moveResumeFirstMsg)
	case gerr != nil || cur.PausedReason == movePauseResuming || gen != x.mv.PauseGen:
		return pick(reasonMoveNotRunning, reasonMovePauseChanged, "A Resume took the run while it was moved.")
	}
	return pick(reasonMovePauseRefused, reasonMovePauseRefused, moveRevokeOpenMsg)
}

// execute runs the move from its admitted row, in the background: the locked sequence for a run that was running,
// then the carry, the commit and the destination's start.
func (x *moveExec) execute(base context.Context) {
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	defer x.keepHeartbeat(ctx, cancel)()
	if !x.stopped {
		if end := x.stopLocked(ctx); end != nil {
			x.end(ctx, end)
			return
		}
	}
	if end := x.carry(ctx); end != nil {
		x.end(ctx, end)
		return
	}
	if end := x.commit(ctx); end != nil {
		x.end(ctx, end)
		return
	}
	x.afterCommit(ctx)
}

// end ends the row as end says, with its run.move row, and tears down a destination the move created. An end that
// cannot land leaves the row in flight for the sweep.
func (x *moveExec) end(ctx context.Context, end *moveEnd) {
	if end == moveLapsed || x.lapsed.Load() {
		return
	}
	wctx := context.WithoutCancel(ctx)
	cleanup := types.MoveCleanupNone
	if x.destRef != "" || x.jti != "" || x.workMade {
		cleanup = types.MoveCleanupPending
	}
	ok, err := x.ms.EndRunMove(wctx, x.mv.ID, end.state, end.reason, cleanup)
	if err != nil || !ok {
		slog.WarnContext(ctx, "wardynd: a move's end could not be recorded; the move sweep fails it",
			slog.String("run_id", x.run.ID.String()), slog.String("move_id", x.mv.ID.String()), slog.Any("err", err))
		return
	}
	x.mv.State, x.mv.Failure = end.state, end.reason
	x.s.auditMoveEnd(wctx, x.mv, x.actorType, x.principal, end)
	if cleanup == types.MoveCleanupPending && x.s.moveTeardownDestination(wctx, x.run.ID, x.destRef, x.jti) &&
		x.s.dropMoveWorkDrive(wctx, x.run.ID, x.mv.ID) {
		if err := x.ms.SetRunMoveCleanup(wctx, x.mv.ID, types.MoveCleanupDone); err != nil {
			slog.WarnContext(ctx, "wardynd: a move's torn-down destination could not be recorded", slog.String("move_id", x.mv.ID.String()), slog.Any("err", err))
		}
	}
}

// auditMoveEnd writes a refused or failed move's one run.move row: denied with the reason for a refusal, failure
// with the failure value for a failed move.
func (s *Server) auditMoveEnd(ctx context.Context, mv types.RunMove, actorType types.ActorType, principal string, end *moveEnd) {
	outcome, data := "denied", map[string]any{"move_id": mv.ID.String(), "to": string(mv.ToPlacement), "reason": end.reason}
	if end.state == types.MoveFailed {
		outcome = "failure"
		data = map[string]any{"move_id": mv.ID.String(), "to": string(mv.ToPlacement), "failure": end.reason}
	}
	s.recordAudit(ctx, s.auditEvent(&mv.RunID, actorType, principal, "run.move", mv.RunID.String(), outcome, mustJSON(data)))
}

// moveTeardownDestination removes a destination sandbox the move created and revokes the destination's create-time
// token. True once both are done (a move with neither has nothing to do).
func (s *Server) moveTeardownDestination(ctx context.Context, runID uuid.UUID, ref, jti string) bool {
	if jti != "" {
		s.revokeRenewedTokens(ctx, runID, []string{jti}, "")
	}
	if ref == "" {
		return true
	}
	if err := s.cfg.Runner.KillSandbox(ctx, ref); err != nil {
		slog.WarnContext(ctx, "wardynd: a failed move's destination sandbox could not be removed; the move sweep retries",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return false
	}
	return true
}

// moveRelayResult is the relay's answer: the manifest it re-emitted, or why it stopped.
type moveRelayResult struct {
	m   movepack.Manifest
	err error
}

// carry is steps 5, 3 and 4: the destination made ready, the source's export, the relay through this process (an
// in-memory pipe, read under the hostile-archive rules and scanned for the run's secret values) and the import.
func (x *moveExec) carry(ctx context.Context) *moveEnd {
	s := x.s
	if (x.run.RunnerID != nil || x.dest.local()) && hoptls.CheckURL(s.cfg.RunnerOrgURL) != nil {
		return failMove(reasonMovePlainLink, "A runner's link to this server is not TLS, so the run's files were not carried over it.")
	}
	if !x.stamp(ctx) {
		return moveLapsed
	}
	if end := x.makeWorkDrive(ctx); end != nil {
		return end
	}
	b, end := s.buildMoveDestination(ctx, x)
	if end != nil {
		return end
	}
	x.jti, x.destDrives = b.jti, b.spec.Drives
	x.mv.ToTokenJTI, x.mv.ToExecutor = b.jti, b.executor
	if ok, err := x.ms.SetRunMoveDestination(ctx, x.mv); err != nil || !ok {
		return failMove(reasonMoveCreateFailed, "The destination could not be recorded on the move.")
	}
	if !x.stamp(ctx) {
		return moveLapsed
	}
	if ok, err := x.ms.AdvanceRunMove(ctx, x.mv.ID, types.MoveAdmitted, types.MoveExporting); err != nil || !ok {
		return failMove(reasonMoveExportFailed, "The move could not be recorded as exporting.")
	}
	cs, err := movePortCarriedSet.call(ctx, x.run)
	set := moveExcludeReattached(runner.CarriedSet{Roots: cs.Roots, Exclude: cs.Exclude}, x.plan.Drives)
	if err != nil {
		return failMove(reasonMoveExportFailed, "What the run carries could not be read.")
	}
	if !s.maskCovered(ctx, x.run.ID) {
		return failMove(reasonMoveScanUncovered, "The run's secret values are not all known here, so its files cannot be checked for them.")
	}
	ex, ok := s.cfg.Runner.(runner.StateExporter)
	if !ok {
		return failMove(reasonMoveExportFailed, "This server's runner cannot export a run's files.")
	}
	exp, err := ex.ExportState(ctx, x.run.SandboxRef, set)
	if err != nil {
		return failMove(reasonMoveExportFailed, "The source runner could not export the run's files: "+err.Error())
	}
	x.drained = exp.SpoolDrained
	if end := x.destCapsChanged(ctx); end != nil {
		_ = exp.Stream.Close()
		return end
	}
	if !x.stamp(ctx) {
		_ = exp.Stream.Close()
		return moveLapsed
	}
	if ok, err := x.ms.AdvanceRunMove(ctx, x.mv.ID, types.MoveExporting, types.MoveImporting); err != nil || !ok {
		_ = exp.Stream.Close()
		return failMove(reasonMoveImportFailed, "The move could not be recorded as importing.")
	}
	return x.relayAndImport(ctx, b, set, exp.Stream)
}

// relayAndImport runs the relay into an in-memory pipe that the destination's import reads, then compares the three
// digests: the source's manifest as read, the stream re-emitted here, and what the destination read.
func (x *moveExec) relayAndImport(ctx context.Context, b moveDestBuild, set runner.CarriedSet, src io.ReadCloser) *moveEnd {
	pr, pw := io.Pipe()
	done := make(chan moveRelayResult, 1)
	masker := x.s.cfg.MaskRegistry.JSONVariantMasker(x.run.ID)
	go func() {
		in, out := &moveStreamRead{r: src}, &moveStreamWrite{w: pw}
		m, err := moveRelay(in, out, set, x.maxBytes, masker)
		err = moveStreamCause(err, in.err, out.err)
		_ = pw.CloseWithError(err)
		done <- moveRelayResult{m: m, err: err}
	}()
	sb, destM, ierr := x.s.createMoveDestination(ctx, b, set, x.maxBytes, pr)
	_ = pr.CloseWithError(errMoveImportEnded)
	_ = src.Close()
	rr := <-done
	if sb.Ref != "" && sb.Ref != x.run.SandboxRef {
		x.destRef, x.mv.ToSandboxRef = sb.Ref, sb.Ref
		if ok, err := x.ms.SetRunMoveDestination(context.WithoutCancel(ctx), x.mv); err != nil || !ok {
			// A row the sweep has already ended no longer leads to this sandbox: remove it here.
			x.s.moveTeardownDestination(context.WithoutCancel(ctx), x.run.ID, sb.Ref, "")
			return failMove(reasonMoveImportFailed, "The destination's sandbox could not be recorded on the move.")
		}
	} else if sb.Ref != "" {
		return failMove(reasonMoveImportFailed, "The destination answered the source's own sandbox.")
	}
	if rr.err != nil && !errors.Is(rr.err, errMoveImportEnded) {
		return failMove(moveRelayReason(rr.err), "The run's files were refused on the way: "+rr.err.Error())
	}
	if ierr != nil {
		if errors.Is(ierr, runner.ErrMoveCreate) {
			return failMove(reasonMoveCreateFailed, "The destination's sandbox could not be created: "+ierr.Error())
		}
		return failMove(reasonMoveImportFailed, "The destination could not import the run's files: "+ierr.Error())
	}
	if rr.err != nil || rr.m.Digest == "" || destM.Digest != rr.m.Digest {
		return failMove(reasonMoveDigestMismatch, "The files the destination read differ from the files the source sent.")
	}
	x.manifest = rr.m
	return nil
}

// moveRelay reads the source's stream under the hostile-archive rules and the move's byte bound, scans it for the
// run's secret values, and re-emits it onto dst. Nothing is written to disk: dst is a pipe the import reads.
func moveRelay(src io.Reader, dst io.Writer, set runner.CarriedSet, maxBytes int64, m secretmask.Masker) (movepack.Manifest, error) {
	rd, err := movepack.NewReader(src, movepack.Options{Set: set, Limits: movepack.Limits{MaxBytes: maxBytes},
		Detect: func() movepack.Detector { return secretmask.NewDetector(m) }})
	if err != nil {
		return movepack.Manifest{}, err
	}
	w, err := movepack.NewWriter(dst, set)
	if err != nil {
		return movepack.Manifest{}, err
	}
	man, err := movepack.Copy(w, rd)
	if err != nil {
		rd.Close()
	}
	return man, err
}

// errMoveRelayBroken marks a relay that stopped because the source's stream or the pipe to the import broke, not
// because a stream rule refused what it read.
var errMoveRelayBroken = errors.New("the stream broke between a runner and this server")

// moveStreamRead and moveStreamWrite keep the first error the source's stream and the import's pipe answered, which
// the archive reader and writer name as a refused entry.
type moveStreamRead struct {
	r   io.Reader
	err error
}

func (m *moveStreamRead) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if err != nil && err != io.EOF && m.err == nil {
		m.err = err
	}
	return n, err
}

type moveStreamWrite struct {
	w   io.Writer
	err error
}

func (m *moveStreamWrite) Write(p []byte) (int, error) {
	n, err := m.w.Write(p)
	if err != nil && m.err == nil {
		m.err = err
	}
	return n, err
}

// moveStreamCause is the relay's error by its cause: the import ending first, a broken stream, or the rule that
// refused (err as it is).
func moveStreamCause(err, readErr, writeErr error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(writeErr, errMoveImportEnded):
		return errMoveImportEnded
	case readErr != nil || writeErr != nil:
		return fmt.Errorf("%w: %v", errMoveRelayBroken, err)
	}
	return err
}

// moveRelayReason is the failure value of a relay error: a broken stream, else each stream rule's own.
func moveRelayReason(err error) string {
	if errors.Is(err, errMoveRelayBroken) {
		return reasonMoveRelayFailed
	}
	for sentinel, reason := range map[error]string{
		movepack.ErrArchiveRefused:  reasonMoveArchiveRefused,
		movepack.ErrCarriedTooLarge: reasonMoveCarriedTooLarge,
		movepack.ErrDigestMismatch:  reasonMoveDigestMismatch,
		movepack.ErrScanUnreadable:  reasonMoveScanUnreadable,
		movepack.ErrGitOutsideRoots: reasonMoveGitOutsideRoots,
		movepack.ErrValueFound:      reasonMoveScanHit,
	} {
		if errors.Is(err, sentinel) {
			return reason
		}
	}
	return reasonMoveRelayFailed
}

// destCapsChanged fails a move whose runner destination no longer serves moves, reads an import present, or runs the
// class exec-less, as its last caps say before the import.
func (x *moveExec) destCapsChanged(ctx context.Context) *moveEnd {
	if !x.dest.local() {
		return nil
	}
	c, ok := x.s.moveRunnerCaps(*x.dest.RunnerID)
	if !ok || !c.MoveState || !c.MoveImportPresent {
		return failMove(reasonMoveImportFailed, "The destination runner's capabilities changed since the move was admitted.")
	}
	if pc, err := x.s.runnerStreamCapabilities(ctx, *x.dest.RunnerID); err != nil || isExecLess(pc.Capabilities, moveClass(x.rec)) {
		return failMove(reasonMoveImportFailed, "The destination runner's capabilities changed since the move was admitted.")
	}
	return nil
}

// commit is step 6: the destination judged again on the run as it is now, the containment read again, the pause's
// receipt and its Azure DevOps outcome, the source's spool, and then the one conditional transaction.
func (x *moveExec) commit(ctx context.Context) *moveEnd {
	s := x.s
	if !x.stamp(ctx) {
		return moveLapsed
	}
	cur, err := s.cfg.Store.GetRun(ctx, x.run.ID)
	if err != nil {
		return failMove(reasonMovePolicyChanged, "The run could not be read again before commit.")
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return failMove(reasonMovePolicyChanged, "The site config could not be read again before commit.")
	}
	d, ref, err := s.resolveMoveDestination(ctx, cur, x.dest.To, sc)
	switch {
	case err != nil:
		return failMove(reasonMovePolicyChanged, "The destination could not be resolved again before commit.")
	case ref != nil:
		return failMove(reasonMovePolicyChanged, "The destination's judgement changed: "+ref.reason)
	}
	dest := x.dest
	dest.poolRefused = d.poolRefused
	again, ref, err := s.judgeMoveDestination(ctx, cur, x.rec, dest, x.caps, sc)
	switch {
	case err != nil:
		return failMove(reasonMovePolicyChanged, "The destination could not be judged again before commit.")
	case ref != nil:
		return failMove(reasonMovePolicyChanged, "The destination's judgement changed: "+ref.layer)
	}
	if layer, same := x.plan.samePlan(again); !same {
		return failMove(reasonMovePolicyChanged, "The destination's judgement changed: "+layer)
	}
	if contained, err := movePortLocallyContained.call(ctx, x.run.ID); err != nil || contained {
		return failMove(reasonMoveLocallyContained, "The run is held contained, so it stays where it is.")
	}
	rs, err := movePortRevokeFor.call(ctx, moveGenArgs{RunID: x.run.ID, Gen: x.mv.PauseGen})
	switch {
	case err != nil || !rs.Complete:
		return failMove(reasonMovePauseRefused, moveRevokeOpenMsg)
	case rs.ADO != moveADORevoked:
		return failMove(reasonMoveRevokeRefused, "An Azure DevOps token of the run was not revoked, so the run stays where it is.")
	case !x.drained:
		return failMove(reasonMoveSourceSpoolPending, "The source runner has not delivered the run's spooled records.")
	}
	// The record says what the run's sandbox is now: the destination's drives, so a later move reads its work drive.
	rec := x.rec
	rec.Drives = slices.Clone(x.destDrives)
	sealed, err := s.sealRunSandboxSpec(x.run.ID, rec)
	if err != nil {
		return failMove(reasonMoveCreateFailed, "The run's sandbox spec could not be recorded for the destination.")
	}
	err = x.ms.CommitRunMove(ctx, store.RunMoveCommit{MoveID: x.mv.ID, RunID: x.run.ID, PlacementFilled: x.dest.Filled,
		PoolRevision: x.dest.PoolRevision, PoolSelection: x.dest.PoolSelection, Deliveries: x.plan.Deliveries,
		ManifestSHA256: x.manifest.Digest, Bytes: x.manifest.Bytes, Skipped: len(x.manifest.Skipped), SandboxSpec: sealed})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrRunMoveRunEnded):
		return failMove(reasonMoveRunEnded, "The run ended or was lost while it was moved.")
	case errors.Is(err, store.ErrRunMovePauseChanged):
		return failMove(reasonMovePauseChanged, "A Resume took the run while it was moved.")
	case errors.Is(err, store.ErrRunMoveSourceUnsettled):
		return failMove(reasonMoveSourceUnsettled, "The source runner has not settled the run.")
	}
	slog.WarnContext(ctx, "wardynd: a move's commit did not land; the move sweep ends it",
		slog.String("run_id", x.run.ID.String()), slog.String("move_id", x.mv.ID.String()), slog.Any("err", err))
	return moveLapsed
}

// afterCommit is the move's success row, the source's cleanup hint, and step 7: the second revoke of the source's
// lineage, the destination's proxy and agent started by the sealed revive, and the stopped pause cleared. A step
// that fails leaves the move committed and the run stopped at the destination, and releases the hold, so a person's
// Resume takes over.
func (x *moveExec) afterCommit(ctx context.Context) {
	s, wctx := x.s, context.WithoutCancel(ctx)
	s.recordAudit(wctx, s.auditEvent(&x.run.ID, x.actorType, x.principal, "run.move", x.run.ID.String(), "success", mustJSON(map[string]any{
		"move_id": x.mv.ID.String(), "from": string(x.mv.FromPlacement), "to": string(x.mv.ToPlacement),
		"evidence_source": string(x.mv.ToEvidenceSource), "prior_evidence_source": string(x.mv.FromEvidenceSource),
		"runner_trust_class": string(x.plan.Trust), "prior_runner_trust_class": string(x.mv.FromTrustClass),
		"prior_deliveries": x.priorDeliveries(wctx), "manifest_sha256": x.manifest.Digest, "skipped": x.manifest.Skipped,
	})))
	if err := x.ms.SetRunMoveCleanup(wctx, x.mv.ID, types.MoveCleanupPending); err != nil {
		slog.WarnContext(ctx, "wardynd: a moved run's source cleanup could not be recorded", slog.String("move_id", x.mv.ID.String()), slog.Any("err", err))
	}
	if u, ok := s.cfg.Runner.(runner.MoveUnbounder); ok && x.run.RunnerID != nil {
		if err := u.MoveUnbound(ctx, runner.MoveUnbound{RunID: x.run.ID, Ref: x.run.SandboxRef}); err != nil {
			slog.InfoContext(ctx, "wardynd: a moved run's source runner did not take its notice; it is sent again on its next ready",
				slog.String("run_id", x.run.ID.String()), slog.Any("err", err))
		}
	}
	if !x.stamp(ctx) {
		return
	}
	if !s.moveSecondRevoke(ctx, x.ms, x.mv) {
		x.release(wctx, "source_revoke")
		return
	}
	if !x.stamp(ctx) {
		return
	}
	pctx, err := movePortWithPauseResume.call(ctx, struct{}{})
	if err != nil {
		x.release(wctx, "destination_start")
		return
	}
	cur, err := s.cfg.Store.GetRun(ctx, x.run.ID)
	if err != nil {
		x.release(wctx, "destination_start")
		return
	}
	if _, rerr := s.reviveRunProxy(pctx, cur, x.actorType, x.principal, true, x.plan.Narrowed.edits()...); rerr != nil {
		x.release(wctx, "destination_start")
		return
	}
	if !x.stamp(ctx) {
		return
	}
	if _, err := movePortResumeOn.call(ctx, moveResumeArgs{Run: cur, SandboxRef: cur.SandboxRef, Gen: x.mv.PauseGen}); err != nil {
		x.release(wctx, "destination_start")
		return
	}
	if _, err := x.ms.ReleaseRunMove(wctx, x.mv.ID, false); err != nil {
		slog.WarnContext(ctx, "wardynd: a finished move's hold could not be released; it lapses", slog.String("move_id", x.mv.ID.String()), slog.Any("err", err))
	}
}

// priorDeliveries is each grant's delivery as the source recorded it, for the success row.
func (x *moveExec) priorDeliveries(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := x.s.cfg.Store.ListGrantsByRun(ctx, x.run.ID)
	if err != nil {
		return out
	}
	for _, g := range rows {
		out[g.ID.String()] = string(g.Delivery)
	}
	return out
}

// release ends the executor's hold on a committed move it gives up at stage, with one run.move failure row; a
// person's Resume then starts the run at the destination.
func (x *moveExec) release(ctx context.Context, stage string) {
	if _, err := x.ms.ReleaseRunMove(ctx, x.mv.ID, false); err != nil {
		slog.WarnContext(ctx, "wardynd: a move's hold could not be released; it lapses", slog.String("move_id", x.mv.ID.String()), slog.Any("err", err))
	}
	x.s.recordAudit(ctx, x.s.auditEvent(&x.run.ID, x.actorType, x.principal, "run.move", x.run.ID.String(), "failure",
		mustJSON(map[string]any{"move_id": x.mv.ID.String(), "stage": stage})))
}

// moveSecondRevoke is step 7.1 for mv, under the run's lock: the revoke at pause again for the source's generation,
// which catches a source mint committed after the first call listed them. True once source_revoke is done: the
// revoke completed, the run left that generation (a Resume revoked it first), or the run ended.
func (s *Server) moveSecondRevoke(ctx context.Context, ms store.RunMoveStore, mv types.RunMove) bool {
	lctx, unlock, err := s.lockRunOp(ctx, mv.RunID)
	if err != nil {
		return false
	}
	defer unlock()
	cur, err := s.getRunHeld(lctx, mv.RunID)
	done := errors.Is(err, store.ErrNotFound) || (err == nil && cur.State.IsTerminal())
	if err != nil && !done {
		return false
	}
	if !done {
		rs, rerr := movePortRevokeAtPause.call(lctx, moveGenArgs{RunID: mv.RunID, Gen: mv.PauseGen})
		gen, gerr := moveGen(lctx, cur)
		done = (rerr == nil && rs.Complete) || (gerr == nil && gen != mv.PauseGen)
	}
	if !done {
		return false
	}
	if err := ms.SetRunMoveSourceRevoke(lctx, mv.ID, types.MoveSourceRevokeDone); err != nil {
		return false
	}
	return true
}

// RunMoveSweeper runs the move sweep every moveSweepInterval until ctx ends, each pass recorded as the run_move
// sweep. cmd/wardynd starts it on the sweeper leader only, when a runner is configured.
func (s *Server) RunMoveSweeper(ctx context.Context) {
	t := time.NewTicker(moveSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_ = s.cfg.SweepHealth.Tick(ctx, sweephealth.RunMove, s.sweepRunMoves)
	}
}

// sweepRunMoves is one pass of the move sweep, keyed by run_moves rows, never by labels: an in-flight move whose
// hold went stale is failed move_orphaned, a committed move whose hold went stale is released, a failed move's
// destination is torn down, a committed move's source is removed, and a committed move's second revoke is retried
// once its executor is gone.
func (s *Server) sweepRunMoves(ctx context.Context) error {
	ms, ok := s.cfg.Store.(store.RunMoveStore)
	if !ok {
		return nil
	}
	due, err := ms.ListRunMovesDue(ctx, moveSweepBatch)
	if err != nil {
		return fmt.Errorf("list the moves due: %w", err)
	}
	for _, d := range due {
		s.sweepRunMove(ctx, ms, d)
	}
	return nil
}

// sweepRunMove is the move sweep's work on one row.
func (s *Server) sweepRunMove(ctx context.Context, ms store.RunMoveStore, d store.RunMoveDue) {
	mv := d.Move
	sys, by := types.ActorSystem, "wardynd"
	switch {
	case mv.State.InFlight() && d.Stale:
		if ok, err := ms.FailStaleRunMove(ctx, mv.ID, reasonMoveOrphaned); err == nil && ok {
			mv.State, mv.Cleanup = types.MoveFailed, types.MoveCleanupPending
			s.auditMoveEnd(ctx, mv, sys, by, failMove(reasonMoveOrphaned, ""))
			// The executor may have recorded its destination between the list and the fail: read the row again,
			// so the teardown below removes the sandbox it names.
			again, err := ms.GetRunMove(ctx, mv.ID)
			if err != nil {
				return
			}
			mv = again
		}
	case mv.State == types.MoveCommitted && mv.ReleasedAt == nil && d.Stale:
		if ok, err := ms.ReleaseRunMove(ctx, mv.ID, true); err == nil && ok {
			now := time.Now()
			mv.ReleasedAt = &now
			s.recordAudit(ctx, s.auditEvent(&mv.RunID, sys, by, "run.move", mv.RunID.String(), "failure", mustJSON(map[string]any{
				"move_id": mv.ID.String(), "failure": reasonMoveOrphaned, "stage": "destination_start"})))
		}
	}
	switch {
	case mv.State == types.MoveFailed && mv.Cleanup == types.MoveCleanupPending:
		live, err := s.moveRefLive(ctx, ms, mv, mv.ToSandboxRef)
		switch {
		case err != nil:
			return
		case live:
			// The sandbox is the run's own, or another move's destination: only its token, and the work drive the
			// move made, are this move's to remove.
			if s.moveTeardownDestination(ctx, mv.RunID, "", mv.ToTokenJTI) && s.dropMoveWorkDrive(ctx, mv.RunID, mv.ID) {
				s.moveCleanupDone(ctx, ms, mv)
			}
		case s.moveTeardownDestination(ctx, mv.RunID, mv.ToSandboxRef, mv.ToTokenJTI) && s.dropMoveWorkDrive(ctx, mv.RunID, mv.ID):
			s.moveCleanupDone(ctx, ms, mv)
		}
	case mv.State == types.MoveCommitted:
		if mv.SourceRevoke == types.MoveSourceRevokePending && (mv.ReleasedAt != nil || d.Stale) {
			s.moveSecondRevoke(ctx, ms, mv)
		}
		if mv.Cleanup == types.MoveCleanupPending {
			// The source's sandbox, unless it is the one the run now runs on or another move's destination.
			live, err := s.moveRefLive(ctx, ms, mv, mv.FromSandboxRef)
			if err != nil {
				return
			}
			if live || mv.FromSandboxRef == mv.ToSandboxRef {
				s.moveCleanupDone(ctx, ms, mv)
				return
			}
			if err := s.cfg.Runner.KillSandbox(ctx, mv.FromSandboxRef); err != nil {
				slog.InfoContext(ctx, "wardynd: a moved run's source sandbox could not be removed yet; retried next pass",
					slog.String("move_id", mv.ID.String()), slog.Any("err", err))
				return
			}
			s.moveCleanupDone(ctx, ms, mv)
		}
	}
}

// moveRefLive reports whether ref must not be removed by mv's cleanup: it is empty, the run's current sandbox, or the
// destination of another move of the run still in flight. A failed read is an error, and the cleanup waits a pass.
func (s *Server) moveRefLive(ctx context.Context, ms store.RunMoveStore, mv types.RunMove, ref string) (bool, error) {
	if ref == "" {
		return true, nil
	}
	run, err := s.cfg.Store.GetRun(ctx, mv.RunID)
	if err != nil {
		return false, err
	}
	if run.SandboxRef == ref {
		return true, nil
	}
	moves, err := ms.ListRunMoves(ctx, mv.RunID)
	if err != nil {
		return false, err
	}
	for _, m := range moves {
		if m.ID != mv.ID && m.State.InFlight() && m.ToSandboxRef == ref {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) moveCleanupDone(ctx context.Context, ms store.RunMoveStore, mv types.RunMove) {
	if err := ms.SetRunMoveCleanup(ctx, mv.ID, types.MoveCleanupDone); err != nil {
		slog.WarnContext(ctx, "wardynd: a move's cleanup could not be recorded", slog.String("move_id", mv.ID.String()), slog.Any("err", err))
	}
}

// resendMoveUnbound sends runner id, on its ready, the notice of every committed move off it whose cleanup is still
// pending: a cleanup hint only, best-effort.
func (s *Server) resendMoveUnbound(ctx context.Context, id uuid.UUID) {
	ms, ok := s.cfg.Store.(store.RunMoveStore)
	u, canTell := s.cfg.Runner.(runner.MoveUnbounder)
	if !ok || !canTell || ctx == nil {
		return
	}
	moves, err := ms.ListRunMovesFromRunner(ctx, id)
	if err != nil {
		slog.InfoContext(ctx, "wardynd: listing the moves off a ready runner failed", slog.String("runner", id.String()), slog.Any("err", err))
		return
	}
	for _, mv := range moves {
		if err := u.MoveUnbound(ctx, runner.MoveUnbound{RunID: mv.RunID, Ref: mv.FromSandboxRef}); err != nil {
			slog.InfoContext(ctx, "wardynd: a moved run's notice was not taken", slog.String("move_id", mv.ID.String()), slog.Any("err", err))
		}
	}
}

// moveReasonStatus is the HTTP status a move reason answers with in the request.
func moveReasonStatus(reason string) int {
	switch reason {
	case reasonMoveDestinationUnknown:
		return http.StatusUnprocessableEntity
	case reasonMoveToPersonalOff, reasonMoveDestinationRefused, reasonMoveResidentCredential, reasonMoveMCPToken:
		return http.StatusForbidden
	case reasonMoveContainmentUnreadable, reasonMovePolicyUnreadable, reasonMoveStopFailed:
		return http.StatusServiceUnavailable
	}
	return http.StatusConflict
}
