// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The twelve move ports' fakes: each stands in for the stopped pause or the revoke at pause the move calls through
// its port, so the move's own work is tested on this tree. The stop, receipt, cut, stamp and revoke fakes check that
// they run under the run's lock, held on the context they were handed, as the real calls do.

// moveRowWriter is the test-only statements the fakes write: the row marked stopped, and the token stamp.
type moveRowWriter interface {
	moveMarkStopped(ctx context.Context, runID uuid.UUID, at time.Time) error
	moveStampToken(ctx context.Context, runID uuid.UUID) error
}

// moveFakes holds what the fakes answer and records what they were asked.
type moveFakes struct {
	srv *Server
	rw  moveRowWriter

	mu          sync.Mutex
	stopRefusal string
	// stopRefusalErrAt is the stop-refusal call (1 is step 1's, 2 step 2's) that answers an error.
	stopRefusalErrAt int
	stopErr          error
	receipt          moveRevokeState
	receiptErr       error
	cut              bool
	stamp            bool
	stampErr         error
	contained        bool
	containedErr     error
	mintErr          error
	mintPlan         moveMintPlan
	carried          moveCarriedSet
	revokeAtPause    moveRevokeState
	resumeErr        error
	// receiptAt, when set, answers the receipt at commit (the second call) instead of receipt.
	receiptAt func(call int) moveRevokeState
	// onStop runs inside the stop fake, under the run's lock.
	onStop func()

	calls    map[string]int
	unlocked []string
	resumed  []moveResumeArgs
}

// moveFakeCtxKey marks the context withPauseResume's fake answers.
type moveFakeCtxKey struct{}

func (f *moveFakes) count(port string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[port]
}

func (f *moveFakes) note(port string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[port]++
	return f.calls[port]
}

// locked records a port called without the run's lock held on ctx, or while another caller could take it.
func (f *moveFakes) locked(ctx context.Context, port string, runID uuid.UUID) {
	ok := db.LockHeld(ctx) == nil
	if ok && f.srv != nil {
		_, unlock, got, held := f.srv.tryLockRunOpHeld(context.Background(), runID)
		if got {
			unlock()
		}
		ok = !got && held
	}
	if !ok {
		f.mu.Lock()
		f.unlocked = append(f.unlocked, port)
		f.mu.Unlock()
	}
}

func (f *moveFakes) unlockedPorts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unlocked...)
}

// withMovePortFakes sets all twelve ports to their fakes for one test and restores each to nil when it ends. The
// fakes answer a move that goes through: nothing refused, the receipt complete with every Azure DevOps token revoked,
// every grant kept, /home/agent/work carried.
func withMovePortFakes(t *testing.T) *moveFakes {
	t.Helper()
	f := &moveFakes{
		receipt: moveRevokeState{Complete: true, ADO: moveADORevoked}, cut: true, stamp: true,
		carried:       moveCarriedSet{Roots: []string{"/home/agent/work"}},
		revokeAtPause: moveRevokeState{Complete: true, ADO: moveADORevoked},
		calls:         map[string]int{},
	}
	movePortStopRefusal = fakeMoveStopRefusal(f)
	movePortPauseGen = fakeMovePauseGen(f)
	movePortPauseStop = fakeMoveStop(f)
	movePortRevokeFor = fakeMoveReceipt(f)
	movePortLineageCut = fakeMoveCut(f)
	movePortStampCutLineage = fakeMoveStamp(f)
	movePortLocallyContained = fakeMoveContained(f)
	movePortMintAtDestination = fakeMoveMint(f)
	movePortCarriedSet = fakeMoveCarried(f)
	movePortRevokeAtPause = fakeMoveRevokeAtPause(f)
	movePortWithPauseResume = fakeMoveWithPauseResume(f)
	movePortResumeOn = fakeMoveResume(f)
	t.Cleanup(func() {
		movePortStopRefusal, movePortPauseGen, movePortPauseStop, movePortRevokeFor = nil, nil, nil, nil
		movePortLineageCut, movePortStampCutLineage, movePortLocallyContained, movePortMintAtDestination = nil, nil, nil, nil
		movePortCarriedSet, movePortRevokeAtPause, movePortWithPauseResume, movePortResumeOn = nil, nil, nil, nil
	})
	return f
}

func fakeMoveStopRefusal(f *moveFakes) movePort[moveKeepArgs, string] {
	return func(context.Context, moveKeepArgs) (string, error) {
		n := f.note("stop_refusal")
		f.mu.Lock()
		defer f.mu.Unlock()
		if n == f.stopRefusalErrAt {
			return "", errors.New("the destination's facts could not be read")
		}
		return f.stopRefusal, nil
	}
}

// fakeMovePauseGen is the stopped pause's generation: the row's paused_at in microseconds.
func fakeMovePauseGen(f *moveFakes) movePort[types.AgentRun, int64] {
	return func(_ context.Context, run types.AgentRun) (int64, error) {
		f.note("pause_gen")
		if run.PausedAt == nil {
			return 0, errors.New("the run is not paused")
		}
		return run.PausedAt.UnixMicro(), nil
	}
}

// fakeMoveStop marks the row stopped with a test-only statement and ends the source sandbox through the runner.
func fakeMoveStop(f *moveFakes) movePort[moveStopArgs, movePauseReceipt] {
	return func(ctx context.Context, a moveStopArgs) (movePauseReceipt, error) {
		f.note("stop")
		f.locked(ctx, "stop", a.Run.ID)
		f.mu.Lock()
		stopErr, onStop := f.stopErr, f.onStop
		f.mu.Unlock()
		if onStop != nil {
			onStop()
		}
		if stopErr != nil {
			return movePauseReceipt{}, stopErr
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		if err := f.rw.moveMarkStopped(ctx, a.Run.ID, at); err != nil {
			return movePauseReceipt{}, err
		}
		if e, ok := f.srv.cfg.Runner.(runner.SandboxEnder); ok {
			if err := e.EndSandbox(ctx, a.Run.SandboxRef); err != nil {
				return movePauseReceipt{}, fmt.Errorf("end the source sandbox: %w", err)
			}
		} else if err := f.srv.cfg.Runner.StopSandbox(ctx, a.Run.SandboxRef); err != nil {
			return movePauseReceipt{}, fmt.Errorf("stop the source sandbox: %w", err)
		}
		return movePauseReceipt{Gen: at.UnixMicro()}, nil
	}
}

func fakeMoveReceipt(f *moveFakes) movePort[moveGenArgs, moveRevokeState] {
	return func(ctx context.Context, a moveGenArgs) (moveRevokeState, error) {
		n := f.note("receipt")
		if n == 1 {
			f.locked(ctx, "receipt", a.RunID) // the locked sequence's read; commit's runs without the lock
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.receiptAt != nil {
			return f.receiptAt(n), f.receiptErr
		}
		return f.receipt, f.receiptErr
	}
}

func fakeMoveCut(f *moveFakes) movePort[uuid.UUID, bool] {
	return func(ctx context.Context, runID uuid.UUID) (bool, error) {
		f.note("cut")
		f.locked(ctx, "cut", runID)
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.cut, nil
	}
}

// fakeMoveStamp answers true and refreshes the token stamp with a test-only statement.
func fakeMoveStamp(f *moveFakes) movePort[uuid.UUID, bool] {
	return func(ctx context.Context, runID uuid.UUID) (bool, error) {
		f.note("stamp")
		f.locked(ctx, "stamp", runID)
		f.mu.Lock()
		stamp, err := f.stamp, f.stampErr
		f.mu.Unlock()
		if err != nil || !stamp {
			return false, err
		}
		return true, f.rw.moveStampToken(ctx, runID)
	}
}

func fakeMoveContained(f *moveFakes) movePort[uuid.UUID, bool] {
	return func(context.Context, uuid.UUID) (bool, error) {
		f.note("contained")
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.contained, f.containedErr
	}
}

// fakeMoveMint keeps every grant.
func fakeMoveMint(f *moveFakes) movePort[moveMintArgs, moveMintPlan] {
	return func(context.Context, moveMintArgs) (moveMintPlan, error) {
		f.note("mint")
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.mintPlan, f.mintErr
	}
}

func fakeMoveCarried(f *moveFakes) movePort[types.AgentRun, moveCarriedSet] {
	return func(context.Context, types.AgentRun) (moveCarriedSet, error) {
		f.note("carried")
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.carried, nil
	}
}

func fakeMoveRevokeAtPause(f *moveFakes) movePort[moveGenArgs, moveRevokeState] {
	return func(ctx context.Context, a moveGenArgs) (moveRevokeState, error) {
		f.note("revoke_at_pause")
		f.locked(ctx, "revoke_at_pause", a.RunID)
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.revokeAtPause, nil
	}
}

// fakeMoveWithPauseResume answers its context marked with a test key.
func fakeMoveWithPauseResume(f *moveFakes) movePort[struct{}, context.Context] {
	return func(ctx context.Context, _ struct{}) (context.Context, error) {
		f.note("with_pause_resume")
		return context.WithValue(ctx, moveFakeCtxKey{}, true), nil
	}
}

func fakeMoveResume(f *moveFakes) movePort[moveResumeArgs, struct{}] {
	return func(_ context.Context, a moveResumeArgs) (struct{}, error) {
		f.note("resume")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.resumed = append(f.resumed, a)
		return struct{}{}, f.resumeErr
	}
}

// The move's in-memory world: the revive fixture's run, store and runner, with the move's rows, the run's recorded
// spec, and a runner that exports and imports. A move between the organisation's own executors runs end to end on it.

var (
	_ store.RunMoveStore    = (*moveStore)(nil)
	_ store.RunSandboxSpecs = (*moveStore)(nil)
	_ store.RunTrustStore   = (*moveStore)(nil)
	_ runner.StateExporter  = (*moveRunner)(nil)
	_ runner.StateImporter  = (*moveRunner)(nil)
	_ runner.MoveUnbounder  = (*moveRunner)(nil)
	_ moveRowWriter         = (*moveStore)(nil)
)

// moveStore keeps run_moves, run_placements and run_sandbox_specs in memory, under PG's terms.
type moveStore struct {
	*reviveStore
	mmu        sync.Mutex
	site       types.SiteConfig
	moves      []*types.RunMove
	placements []types.RunPlacement
	specs      map[uuid.UUID][]byte
	runners    map[uuid.UUID]types.Runner
	kept       bool
	keptErr    error
	trust      types.RunnerTrustClass
	// skew is added to the moves' clock: a hold made stale.
	skew time.Duration
	// beforeCommit runs at the top of CommitRunMove.
	beforeCommit func()
	unsettled    bool
	stamps       int
	// failWrites names writes that fail once: "pause_gen" errors, "pause_gen:none" changes no row, and
	// "advance:<state>" errors the advance to that state.
	failWrites map[string]bool
}

// failing reports, once, whether the write named key is set to fail.
func (s *moveStore) failing(key string) bool {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	if !s.failWrites[key] {
		return false
	}
	delete(s.failWrites, key)
	return true
}

func (s *moveStore) now() time.Time { return time.Now().Add(s.skew) }

func (s *moveStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, nil }

func (s *moveStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	if r, ok := s.runners[id]; ok {
		return r, nil
	}
	return types.Runner{}, store.ErrNotFound
}

func (s *moveStore) ListRunnersByOwner(_ context.Context, owner string) ([]types.Runner, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	var out []types.Runner
	for _, r := range s.runners {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b types.Runner) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}

func (s *moveStore) GetKeptMachineByRun(context.Context, uuid.UUID) (types.KeptMachine, error) {
	switch {
	case s.keptErr != nil:
		return types.KeptMachine{}, s.keptErr
	case s.kept:
		return types.KeptMachine{}, nil
	}
	return types.KeptMachine{}, store.ErrNotFound
}

func (s *moveStore) SetRunRunnerTrustClass(_ context.Context, _ uuid.UUID, c types.RunnerTrustClass) error {
	s.trust = c
	return nil
}

func (s *moveStore) RunRunnerTrustClass(context.Context, uuid.UUID) (types.RunnerTrustClass, error) {
	return s.trust, nil
}

func (s *moveStore) PutRunSandboxSpec(_ context.Context, id uuid.UUID, sealed []byte) error {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	if _, ok := s.specs[id]; ok {
		return errors.New("a run's sandbox spec is written once")
	}
	s.specs[id] = sealed
	return nil
}

func (s *moveStore) GetRunSandboxSpec(_ context.Context, id uuid.UUID) ([]byte, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	if b, ok := s.specs[id]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}

// moveMarkStopped is the stop fake's test-only statement: the row in a stopped pause at at.
func (s *moveStore) moveMarkStopped(_ context.Context, _ uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.run.PausedAt, s.run.PausedReason = &at, movePauseStopped
	return nil
}

// moveStampToken is the stamp fake's test-only statement.
func (s *moveStore) moveStampToken(context.Context, uuid.UUID) error {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	s.stamps++
	return nil
}

// fresh is PG's hold: a row's heartbeat (or creation) within the orphan bound.
func (s *moveStore) fresh(m *types.RunMove, within time.Duration) bool {
	at := m.CreatedAt
	if m.HeartbeatAt != nil {
		at = *m.HeartbeatAt
	}
	return !at.Before(s.now().Add(-within))
}

func (s *moveStore) held(m *types.RunMove) bool {
	return (m.State.InFlight() || (m.State == types.MoveCommitted && m.ReleasedAt == nil)) && s.fresh(m, store.RunMoveOrphanAfter)
}

func (s *moveStore) find(id uuid.UUID) *types.RunMove {
	for _, m := range s.moves {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (s *moveStore) AdmitRunMove(_ context.Context, mv types.RunMove, limits store.RunMoveLimits) (types.RunMove, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	install, onto := 0, 0
	for _, m := range s.moves {
		if m.RunID == mv.RunID && s.held(m) {
			return types.RunMove{}, store.ErrRunMoveBusy
		}
		if m.State.InFlight() && s.fresh(m, store.RunMoveOrphanAfter) {
			install++
			if mv.ToRunnerID != nil && m.ToRunnerID != nil && *m.ToRunnerID == *mv.ToRunnerID {
				onto++
			}
		}
	}
	if (limits.Install > 0 && install >= limits.Install) || (limits.PerRunner > 0 && mv.ToRunnerID != nil && onto >= limits.PerRunner) {
		return types.RunMove{}, store.ErrRunMoveBusy
	}
	mv.ID, mv.State, mv.CreatedAt, mv.UpdatedAt = uuid.New(), types.MoveAdmitted, s.now(), s.now()
	mv.Cleanup, mv.SourceRevoke = types.MoveCleanupNone, types.MoveSourceRevokeNone
	stored := mv
	s.moves = append(s.moves, &stored)
	return mv, nil
}

func (s *moveStore) RunMoveInFlight(_ context.Context, runID uuid.UUID) (bool, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	for _, m := range s.moves {
		if m.RunID == runID && s.held(m) {
			return true, nil
		}
	}
	return false, nil
}

func (s *moveStore) GetRunMove(_ context.Context, id uuid.UUID) (types.RunMove, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	if m := s.find(id); m != nil {
		return *m, nil
	}
	return types.RunMove{}, store.ErrNotFound
}

func (s *moveStore) ListRunMoves(_ context.Context, runID uuid.UUID) ([]types.RunMove, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	var out []types.RunMove
	for _, m := range s.moves {
		if m.RunID == runID {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (s *moveStore) ListRunPlacements(_ context.Context, runID uuid.UUID) ([]types.RunPlacement, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	var out []types.RunPlacement
	for _, p := range s.placements {
		if p.RunID == runID {
			out = append(out, p)
		}
	}
	return out, nil
}

// update applies fn to row id when cond holds, PG's one-row conditional UPDATE.
func (s *moveStore) update(id uuid.UUID, cond func(*types.RunMove) bool, fn func(*types.RunMove)) (bool, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	m := s.find(id)
	if m == nil || !cond(m) {
		return false, nil
	}
	fn(m)
	m.UpdatedAt = s.now()
	return true, nil
}

func inFlight(m *types.RunMove) bool { return m.State.InFlight() }

func (s *moveStore) SetRunMovePauseGen(_ context.Context, id uuid.UUID, gen int64) (bool, error) {
	switch {
	case s.failing("pause_gen"):
		return false, errors.New("the move store refuses the write")
	case s.failing("pause_gen:none"):
		return false, nil
	}
	return s.update(id, inFlight, func(m *types.RunMove) { m.PauseGen = gen })
}

func (s *moveStore) SetRunMoveDestination(_ context.Context, mv types.RunMove) (bool, error) {
	return s.update(mv.ID, inFlight, func(m *types.RunMove) {
		m.ToPlacement, m.ToRunnerID, m.ToPoolID, m.ToExecutor, m.ToSandboxRef = mv.ToPlacement, mv.ToRunnerID, mv.ToPoolID, mv.ToExecutor, mv.ToSandboxRef
		m.ToEvidenceSource, m.ToTrustClass, m.ToTokenJTI, m.Narrowed = mv.ToEvidenceSource, mv.ToTrustClass, mv.ToTokenJTI, mv.Narrowed
	})
}

func (s *moveStore) AdvanceRunMove(_ context.Context, id uuid.UUID, from, to types.MoveState) (bool, error) {
	if s.failing("advance:" + string(to)) {
		return false, errors.New("the move store refuses the write")
	}
	return s.update(id, func(m *types.RunMove) bool { return m.State == from }, func(m *types.RunMove) { m.State = to })
}

func (s *moveStore) StampRunMoveHeartbeat(_ context.Context, id uuid.UUID) (bool, error) {
	return s.update(id, func(m *types.RunMove) bool {
		return m.ReleasedAt == nil && (m.State.InFlight() || m.State == types.MoveCommitted) &&
			s.fresh(m, store.RunMoveOrphanAfter-store.RunMoveHoldGuard)
	}, func(m *types.RunMove) { now := s.now(); m.HeartbeatAt = &now })
}

func (s *moveStore) EndRunMove(_ context.Context, id uuid.UUID, state types.MoveState, failure string, cleanup types.MoveCleanup) (bool, error) {
	if (state != types.MoveRefused && state != types.MoveFailed) || failure == "" {
		return false, errors.New("not an end")
	}
	return s.update(id, inFlight, func(m *types.RunMove) { m.State, m.Failure, m.Cleanup = state, failure, cleanup })
}

func (s *moveStore) FailStaleRunMove(_ context.Context, id uuid.UUID, failure string) (bool, error) {
	return s.update(id, func(m *types.RunMove) bool { return m.State.InFlight() && !s.fresh(m, store.RunMoveOrphanAfter) },
		func(m *types.RunMove) {
			m.State, m.Failure, m.Cleanup = types.MoveFailed, failure, types.MoveCleanupPending
		})
}

func (s *moveStore) ReleaseRunMove(_ context.Context, id uuid.UUID, onlyStale bool) (bool, error) {
	return s.update(id, func(m *types.RunMove) bool {
		return m.State == types.MoveCommitted && m.ReleasedAt == nil && (!onlyStale || !s.fresh(m, store.RunMoveOrphanAfter))
	}, func(m *types.RunMove) { now := s.now(); m.ReleasedAt = &now })
}

func (s *moveStore) SetRunMoveCleanup(_ context.Context, id uuid.UUID, c types.MoveCleanup) error {
	_, err := s.update(id, func(*types.RunMove) bool { return true }, func(m *types.RunMove) { m.Cleanup = c })
	return err
}

func (s *moveStore) SetRunMoveSourceRevoke(_ context.Context, id uuid.UUID, r types.MoveSourceRevoke) error {
	_, err := s.update(id, func(*types.RunMove) bool { return true }, func(m *types.RunMove) { m.SourceRevoke = r })
	return err
}

func (s *moveStore) ListRunMovesDue(_ context.Context, limit int) ([]store.RunMoveDue, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	var out []store.RunMoveDue
	for _, m := range s.moves {
		if m.State.InFlight() || (m.State == types.MoveCommitted && m.ReleasedAt == nil) ||
			m.Cleanup == types.MoveCleanupPending || m.SourceRevoke == types.MoveSourceRevokePending {
			out = append(out, store.RunMoveDue{Move: *m, Stale: !s.fresh(m, store.RunMoveOrphanAfter)})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *moveStore) ListRunMovesFromRunner(_ context.Context, runnerID uuid.UUID) ([]types.RunMove, error) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	var out []types.RunMove
	for _, m := range s.moves {
		if m.FromRunnerID != nil && *m.FromRunnerID == runnerID && m.State == types.MoveCommitted && m.Cleanup == types.MoveCleanupPending {
			out = append(out, *m)
		}
	}
	return out, nil
}

// CommitRunMove is PG's commit on the fake rows: the run RUNNING, not lost, in the stopped pause the move recorded,
// and settled at its source; then the run row bound to the destination and the placement history written.
func (s *moveStore) CommitRunMove(_ context.Context, c store.RunMoveCommit) error {
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mmu.Lock()
	defer s.mmu.Unlock()
	m := s.find(c.MoveID)
	if m == nil || m.State != types.MoveImporting {
		return store.ErrRunMoveNotImporting
	}
	if m.ToTokenJTI == "" || m.ToSandboxRef == "" {
		return errors.New("the move recorded no destination sandbox or token")
	}
	switch {
	case s.state != types.RunRunning || s.run.LostAt != nil:
		return store.ErrRunMoveRunEnded
	case s.run.PausedReason != movePauseStopped || s.run.PausedAt == nil || s.run.PausedAt.UnixMicro() != m.PauseGen:
		return store.ErrRunMovePauseChanged
	case s.unsettled:
		return store.ErrRunMoveSourceUnsettled
	}
	if len(s.placements) == 0 {
		s.placements = append(s.placements, types.RunPlacement{RunID: c.RunID, Seq: 1, Cause: types.PlacementCauseOrigin,
			Placement: s.run.Placement, RunnerID: s.run.RunnerID, EvidenceSource: s.run.EvidenceSource, TrustClass: s.trust})
	}
	s.placements = append(s.placements, types.RunPlacement{RunID: c.RunID, Seq: len(s.placements) + 1, Cause: types.PlacementCauseMove,
		Placement: m.ToPlacement, RunnerID: m.ToRunnerID, PoolID: m.ToPoolID, Executor: m.ToExecutor, EvidenceSource: m.ToEvidenceSource,
		TrustClass: m.ToTrustClass, MoveID: &m.ID})
	s.run.Placement, s.run.RunnerID, s.run.RunnerPoolID, s.run.RunnerPoolExecutor = m.ToPlacement, m.ToRunnerID, m.ToPoolID, m.ToExecutor
	s.run.SandboxRef, s.run.EvidenceSource, s.trust = m.ToSandboxRef, m.ToEvidenceSource, m.ToTrustClass
	if c.SandboxSpec != nil {
		s.specs[c.RunID] = c.SandboxSpec
	}
	now := s.now()
	m.State, m.SourceRevoke, m.ManifestSHA256, m.Bytes, m.Skipped = types.MoveCommitted, types.MoveSourceRevokePending, c.ManifestSHA256, c.Bytes, c.Skipped
	m.HeartbeatAt, m.CommittedAt = &now, &now
	return nil
}

// moveRunner is the revive fixture's runner that exports the run's files and imports them into a destination.
type moveRunner struct {
	*reviveRunner
	mmu      sync.Mutex
	files    []moveFile
	drained  bool
	exportFn func() (io.ReadCloser, error)
	// importFn, when set, replaces the import's read of the stream.
	importFn  func(spec runner.SandboxSpec, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error)
	onImport  func()
	exports   int
	imports   []runner.SandboxSpec
	imported  []string
	data      map[string]string
	importMax int64
	kills     []string
	unbound   []runner.MoveUnbound
}

// moveFile is one entry the source exports.
type moveFile struct {
	h       movepack.Header
	content string
}

func moveDir(p string) moveFile {
	return moveFile{h: movepack.Header{Path: p, Type: movepack.TypeDir, Mode: 0o755}}
}

func moveReg(p, content string) moveFile {
	return moveFile{h: movepack.Header{Path: p, Type: movepack.TypeFile, Mode: 0o644, Size: int64(len(content))}, content: content}
}

func (r *moveRunner) ExportState(_ context.Context, _ string, set runner.CarriedSet) (runner.StateExport, error) {
	r.mmu.Lock()
	r.exports++
	fn, files, drained := r.exportFn, slices.Clone(r.files), r.drained
	r.mmu.Unlock()
	// The stream's order: a path before everything beneath it, siblings by name ('/' sorts first).
	key := func(f moveFile) string { return strings.ReplaceAll(f.h.Path, "/", "\x00") }
	slices.SortFunc(files, func(a, b moveFile) int { return strings.Compare(key(a), key(b)) })
	if fn != nil {
		rc, err := fn()
		return runner.StateExport{Stream: rc, SpoolDrained: drained}, err
	}
	var buf bytes.Buffer
	w, err := movepack.NewWriter(&buf, set)
	if err != nil {
		return runner.StateExport{}, err
	}
	for _, f := range files {
		if movepack.Excluded(f.h.Path, set.Exclude) {
			continue
		}
		if err := w.Add(f.h, strings.NewReader(f.content)); err != nil {
			return runner.StateExport{}, err
		}
	}
	if _, err := w.Close(); err != nil {
		return runner.StateExport{}, err
	}
	return runner.StateExport{Stream: io.NopCloser(&buf), SpoolDrained: drained}, nil
}

func (r *moveRunner) ImportSandbox(_ context.Context, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	r.mmu.Lock()
	r.imports, r.importMax = append(r.imports, spec), maxBytes
	fn, on := r.importFn, r.onImport
	r.mmu.Unlock()
	if on != nil {
		on()
	}
	if err := runner.CheckImportSpec(spec); err != nil {
		return runner.Sandbox{}, runner.CarriedManifest{}, err
	}
	if fn != nil {
		return fn(spec, carried)
	}
	rd, err := movepack.NewReader(carried, movepack.Options{Set: set, Limits: movepack.Limits{MaxBytes: maxBytes}})
	if err != nil {
		return runner.Sandbox{}, runner.CarriedManifest{}, err
	}
	for {
		h, content, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return runner.Sandbox{}, runner.CarriedManifest{}, errors.Join(runner.ErrMoveFiles, err)
		}
		var b strings.Builder
		_, _ = io.Copy(&b, content)
		r.mmu.Lock()
		r.imported = append(r.imported, h.Path)
		if r.data == nil {
			r.data = map[string]string{}
		}
		r.data[h.Path] = b.String()
		r.mmu.Unlock()
	}
	return runner.Sandbox{Ref: "ref-dest-" + spec.RunID.String()}, rd.Manifest(), nil
}

func (r *moveRunner) MoveUnbound(_ context.Context, u runner.MoveUnbound) error {
	r.mmu.Lock()
	defer r.mmu.Unlock()
	r.unbound = append(r.unbound, u)
	return nil
}

func (r *moveRunner) KillSandbox(_ context.Context, ref string) error {
	r.mmu.Lock()
	defer r.mmu.Unlock()
	r.kills = append(r.kills, ref)
	return nil
}

func (r *moveRunner) killed() []string {
	r.mmu.Lock()
	defer r.mmu.Unlock()
	return slices.Clone(r.kills)
}

func (r *moveRunner) importedPaths() []string {
	r.mmu.Lock()
	defer r.mmu.Unlock()
	return slices.Clone(r.imported)
}

func (r *moveRunner) importedData(p string) string {
	r.mmu.Lock()
	defer r.mmu.Unlock()
	return r.data[p]
}

func (r *moveRunner) lastImport() runner.SandboxSpec {
	r.mmu.Lock()
	defer r.mmu.Unlock()
	if len(r.imports) == 0 {
		return runner.SandboxSpec{}
	}
	return r.imports[len(r.imports)-1]
}

func (r *moveRunner) importCount() int {
	r.mmu.Lock()
	defer r.mmu.Unlock()
	return len(r.imports)
}

func (r *moveRunner) exportCount() int {
	r.mmu.Lock()
	defer r.mmu.Unlock()
	return r.exports
}

// moveFixture is a RUNNING interactive run on the organisation's executor, recorded at dispatch, with run_move on
// and the gate open, the twelve ports set to their fakes, and a source holding a work tree with a git directory.
type moveFixture struct {
	*reviveFixture
	ms *moveStore
	mr *moveRunner
	fk *moveFakes
}

func newMoveFixture(t *testing.T) *moveFixture {
	t.Helper()
	rf := newReviveFixture(t)
	rf.st.mu.Lock()
	rf.st.run.LostAt, rf.st.run.LostReason, rf.st.run.Interactive = nil, "", true
	rf.st.run.Placement, rf.st.run.RunnerPoolExecutor = types.PlacementRemote, "kind-a"
	rf.st.mu.Unlock()
	f := &moveFixture{reviveFixture: rf}
	f.ms = &moveStore{reviveStore: rf.rs, site: siteFeaturesDoc(t, runMoveStoredOn), specs: map[uuid.UUID][]byte{},
		runners: map[uuid.UUID]types.Runner{}}
	f.mr = &moveRunner{reviveRunner: rf.rr, drained: true, files: []moveFile{
		moveDir("home/agent/work"), moveReg("home/agent/work/README.md", "hello"),
		moveDir("home/agent/work/.git"), moveReg("home/agent/work/.git/HEAD", "ref: refs/heads/main\n"),
	}}
	f.srv.cfg.Store, f.srv.cfg.Runner, f.srv.cfg.BaseCtx = f.ms, f.mr, t.Context()
	f.srv.cfg.RunnerOrgURL = admissionOrgURL
	withMoveGateOpen(t)
	f.fk = withMovePortFakes(t)
	f.fk.srv, f.fk.rw = f.srv, f.ms
	f.record(t, runSandboxSpecRecord{Image: "ghcr.io/example/agent:1", ConfinementClass: types.CC1, Interactive: true,
		Env: map[string]string{"WARDYN_RUN_ID": f.run.ID.String()}})
	f.run = f.current(t)
	t.Cleanup(f.srv.bg.Wait)
	return f
}

// record writes the run's recorded spec, as dispatch does.
func (f *moveFixture) record(t *testing.T, rec runSandboxSpecRecord) {
	t.Helper()
	f.ms.mmu.Lock()
	delete(f.ms.specs, f.run.ID)
	f.ms.mmu.Unlock()
	if err := f.srv.storeRunSandboxSpec(context.Background(), f.run.ID, rec); err != nil {
		t.Fatalf("record the run's spec: %v", err)
	}
}

func (f *moveFixture) current(t *testing.T) types.AgentRun {
	t.Helper()
	run, err := f.ms.GetRun(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// stop puts the run in a stopped pause, as a person's pause leaves it.
func (f *moveFixture) stop(t *testing.T) {
	t.Helper()
	if err := f.ms.moveMarkStopped(context.Background(), f.run.ID, time.Now().UTC().Truncate(time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	f.run = f.current(t)
}

// move POSTs /runs/{id}/move as the admin token and waits for the executor to finish.
func (f *moveFixture) move(t *testing.T, to string) (int, string) {
	t.Helper()
	w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/move", adminToken, `{"to":"`+to+`"}`)
	f.srv.bg.Wait()
	return w.Code, w.Body.String()
}

// only is the run's one move row.
func (f *moveFixture) only(t *testing.T) types.RunMove {
	t.Helper()
	moves, _ := f.ms.ListRunMoves(context.Background(), f.run.ID)
	if len(moves) != 1 {
		t.Fatalf("run_moves rows = %d, want 1: %+v", len(moves), moves)
	}
	return moves[0]
}

// moveRowsAll is the run's run_moves rows.
func (f *moveFixture) moveRowsAll(t *testing.T) []types.RunMove {
	t.Helper()
	moves, err := f.ms.ListRunMoves(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return moves
}

// moveRows counts the run.move audit rows with outcome.
func (f *moveFixture) moveRows(outcome string) int {
	n := 0
	for _, ev := range f.audit.snapshot() {
		if ev.Action == "run.move" && ev.Outcome == outcome {
			n++
		}
	}
	return n
}
