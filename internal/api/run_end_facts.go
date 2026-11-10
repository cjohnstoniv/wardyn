// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A terminal run's sandbox can still hold changes its computer folder's pass back has not brought to the
// person's computer. Every decision about such a sandbox reads the same facts (folderPassFacts): whether a
// pass can happen (mayPass, for the run's end), and whether one is owed (pending, for every reader that
// could stop the sandbox). A fact that cannot be read counts as "a pass can happen": the sandbox is kept,
// contained, until a later read decides.

// pendingFloor is the shortest a pass back stays owed after its run ended (pendingCap).
const pendingFloor = 24 * time.Hour

// folderPassFacts is what decides a run's pass back, read once.
type folderPassFacts struct {
	run    types.AgentRun
	folder *types.RunLocalFolder
	seeded bool
	// dataOut is the run's data_out rule allowing its content out; a rule that cannot be read is err.
	dataOut bool
	status  types.RunLocalFolderStatus
	// statusErr is the record's read error, apart from err: a reader names it in its row.
	statusErr error
	// err is the first fact that could not be read.
	err error
}

// readFolderPassFacts reads run's facts; run is the caller's copy of the row, its ref the sandbox the
// caller acts on. A run with no folder reads nothing more.
func (s *Server) readFolderPassFacts(ctx context.Context, run types.AgentRun) folderPassFacts {
	f := folderPassFacts{run: run}
	fs, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return f
	}
	folder, seed, err := fs.RunLocalFolderSeed(ctx, run.ID)
	if err != nil {
		f.err = fmt.Errorf("its computer folder could not be read: %w", err)
		return f
	}
	if folder == nil {
		return f
	}
	f.folder, f.seeded = folder, seed.SeededAt != nil
	rule, _, derr := s.runDataOutRule(ctx, run.ID)
	f.dataOut = derr == nil && rule == types.TransferAllowed
	if derr != nil {
		f.err = fmt.Errorf("its data_out rule could not be read: %w", derr)
	}
	if st, ok := s.cfg.Store.(store.RunLocalFolderStatuses); ok {
		rec, serr := st.RunLocalFolderStatus(ctx, run.ID)
		switch {
		case errors.Is(serr, store.ErrNotFound):
		case serr != nil:
			f.statusErr = serr
			f.err = cmpErr(f.err, fmt.Errorf("its computer folder's record could not be read: %w", serr))
		default:
			f.status = rec
		}
	}
	return f
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// eligible is a folder whose changes a pass back brings to the person's computer: both ways, from a
// runner, seeded, and allowed out.
func (f folderPassFacts) eligible() bool {
	return f.folder != nil && f.folder.RunnerID != nil && f.folder.Direction == types.ComputerFolderBoth && f.seeded && f.dataOut
}

// owed is a pass back the record still owes: one kept for a resume, or an eligible folder's first, until a
// pass back is recorded (done: a recorded back is final).
func (f folderPassFacts) owed() bool {
	if folderStatusDone(f.status) {
		return false
	}
	return f.status.Resume != nil || (f.eligible() && f.status.Back == nil)
}

// mayPass is a run whose end may run a pass back: a sandbox, not killed, and a pass owed.
func (f folderPassFacts) mayPass() bool {
	return f.run.SandboxRef != "" && f.run.State != types.RunKilled && f.owed()
}

// capable is a run whose end must hold the run's lock before it passes: a pass may happen, or a fact
// could not be read. Any other run is proved ineligible and ends as any run does.
func (f folderPassFacts) capable() bool {
	return f.err != nil || f.mayPass()
}

// folderStatusDone is a record whose pass back is over: a back recorded and no resume kept. A pass is
// never run for it again (as store.GiveUpRunLocalFolder reads it).
func folderStatusDone(st types.RunLocalFolderStatus) bool {
	return st.Back != nil && st.Resume == nil
}

// pendingCap is how long after its run ended a pass back stays owed: WARDYN_ENDED_RUN_GRACE, the time an
// ended run keeps its files, and never less than pendingFloor.
func (s *Server) pendingCap() time.Duration {
	return max(s.cfg.EndedRunGrace, pendingFloor)
}

// pending is a terminal run whose sandbox a reader must keep, contained, for a pass back: its ref set,
// not killed, and a pass owed or a fact unread. pastCap reports one ended longer ago than pendingCap,
// whose owed pass is due to be given up; it is still pending until that give-up is recorded.
func (s *Server) pending(f folderPassFacts) (pending, pastCap bool) {
	if f.run.SandboxRef == "" || f.run.State == types.RunKilled {
		return false, false
	}
	if f.err == nil && !f.owed() {
		return false, false
	}
	ended := f.run.UpdatedAt
	if f.run.EndedAt != nil {
		ended = *f.run.EndedAt
	}
	return true, !s.cfg.Now().Before(ended.Add(s.pendingCap()))
}

// runPending is pending for a reader holding run (its own row) under the run's lock.
func (s *Server) runPending(ctx context.Context, run types.AgentRun) bool {
	p, _ := s.pending(s.readFolderPassFacts(ctx, run))
	return p
}

// holdsRunLock is the pass gate: ctx carries run id's lock, and the lock is still held. No pass back runs
// without it.
func (s *Server) holdsRunLock(ctx context.Context, id uuid.UUID) bool {
	return slices.Contains(db.HeldLocks(ctx), db.NewLockKey(db.RunOpLockClass, id.String())) && s.lockHeld(ctx) == nil
}

// sandboxQuiescable is a sandbox the server can keep with its egress cut and its agent stopped: on the
// server's own Docker, not a virtual machine, on a substrate that stops a proxy alone
// (quiesceKeptSandbox decides the rest by trying).
func (s *Server) sandboxQuiescable(run types.AgentRun) bool {
	_, ok := s.cfg.Runner.(runner.ProxyStopper)
	return ok && run.RunnerID == nil && !run.MachineKind.IsVM()
}

// keepForPass is the one decision every reader that could stop a terminal run's sandbox makes, under the
// run's lock; left reports a sandbox left in place.
//   - Not pending: the sandbox is stopped.
//   - Pending on a sandbox the server cannot keep with its egress cut (on a runner, a virtual machine,
//     a substrate that cannot stop a proxy alone): the sandbox is stopped (containment wins).
//   - Pending: its proxy is stopped, observed, every visit. Stopped: the sandbox is kept, and a record that
//     could not be read is action's failure row with folder_record_error. A proxy stop that failed, or
//     that the substrate refuses: the sandbox is stopped.
//   - A sandbox stop that failed is action's failure row and errProxyStopUnconfirmed: never contained.
func (s *Server) keepForPass(ctx context.Context, run types.AgentRun, action string) (left bool, err error) {
	if run.SandboxRef == "" {
		return false, nil
	}
	if kept, err := s.containPending(ctx, run, action); kept {
		return true, err
	}
	if !s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, action) {
		return false, errProxyStopUnconfirmed
	}
	return false, nil
}

// containPending is keepForPass's keep arm alone, for a reader with a teardown of its own: kept reports a
// pending sandbox kept with its proxy stopped (err then names a fact that could not be read); any other
// is the caller's to stop.
func (s *Server) containPending(ctx context.Context, run types.AgentRun, action string) (kept bool, err error) {
	f := s.readFolderPassFacts(ctx, run)
	if f.err != nil {
		slog.WarnContext(ctx, "wardynd: a terminal run's computer folder facts could not be read",
			slog.String("run_id", run.ID.String()), slog.String("action", action), slog.Any("err", f.err))
	}
	if p, _ := s.pending(f); !p || !s.sandboxQuiescable(run) || s.stopProxyOrAudit(ctx, run.ID, run.SandboxRef, action) != nil {
		return false, nil
	}
	if f.err != nil {
		data := map[string]any{"sandbox_ref": run.SandboxRef, "folder_record_error": f.err.Error()}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", action, run.SandboxRef, "failure", mustJSON(data)))
		return true, fmt.Errorf("a fact of its computer folder could not be read, so its sandbox is kept with its proxy stopped: %w", f.err)
	}
	return true, nil
}

// ownedFolderGen is a run's pass record as its owner moved it on (bumpComputerFolderGen): status.Gen is the
// owner's generation, and record writes only at it.
type ownedFolderGen struct {
	s      *Server
	id     uuid.UUID
	status types.RunLocalFolderStatus
}

// bumpComputerFolderGen moves run id's record from gen to the next and answers it: false when another
// owner moved it first, the store cannot, or the write failed.
func (s *Server) bumpComputerFolderGen(ctx context.Context, id uuid.UUID, gen int) (ownedFolderGen, bool) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return ownedFolderGen{}, false
	}
	rec, ok, err := st.BumpRunLocalFolderGen(ctx, id, store.FolderStatusGuard{Gen: gen})
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: move a computer folder's pass record on", slog.String("run_id", id.String()), slog.Any("err", err))
	}
	return ownedFolderGen{s: s, id: id, status: rec}, ok && err == nil
}

// record applies set at the owner's generation, and marks it settled when the runner answered for the
// pass or nothing was sent. It reports whether it was written.
func (g ownedFolderGen) record(ctx context.Context, settled bool, set func(*types.RunLocalFolderStatus)) bool {
	return g.s.writeComputerFolderStatus(ctx, g.id, g.status.Gen, func(st *types.RunLocalFolderStatus) {
		set(st)
		if settled {
			st.Settled = g.status.Gen
		}
	})
}

// folderRunnerPassOnce reports whether run's folder runner runs one pass per run at a time
// (Caps.FolderPassOnce), and whether it could be asked: one offline or not sharing folders is not, and
// the pass then sends nothing.
func (s *Server) folderRunnerPassOnce(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder) (once, reached bool) {
	if folder.RunnerID == nil {
		return false, false
	}
	sub, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID)
	if refusal != nil {
		return false, false
	}
	caps, ok := sub.Caps()
	return ok && caps.FolderPassOnce, ok
}
