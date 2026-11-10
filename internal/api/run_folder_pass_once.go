// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner runs one pass per run at a time when it says so (Caps.FolderPassOnce), and refuses a second
// with folder_pass_busy. Every pass, the seed and each pass back, moves the run's record to a generation
// of its own first, and marks it settled (types.RunLocalFolderStatus.Settled) once the runner answered for
// it, or when nothing was sent and the last one was settled. While the last pass is unsettled, a runner
// not known to run one per run at a time (an older one, which runs whatever it is sent, or one that
// cannot be reached) gets no pass: one it may still be running would run beside the new one.

// folderPassBusyMsg is the record of a pass a runner refused while it runs another for the run.
const folderPassBusyMsg = "The runner is still running this run's last pass back; a later sweep tries again"

// errFolderPassUnconfirmed is a pass not sent: the run's last pass is unconfirmed, and the runner it would
// be sent to is not known to run one pass per run at a time (runsOnePassPerRun), or could not be reached.
var errFolderPassUnconfirmed = errors.New("no pass ran; the runner's last pass for this run was not confirmed, and another could run beside it. Update the Wardyn runner")

// runsOnePassPerRun is a runner whose caps say it runs one pass per run at a time. Caps not read yet count
// as an older runner's.
func runsOnePassPerRun(caps runnerwire.Caps, read bool) bool {
	return read && caps.FolderPassOnce
}

// bumpComputerFolderGen moves run id's record from gen to the next, and answers the record as it then
// stands, its Gen the generation the pass then owns: false when the record moved first or could not be
// written. A store with no records answers gen, settled.
func (s *Server) bumpComputerFolderGen(ctx context.Context, id uuid.UUID, gen int) (types.RunLocalFolderStatus, bool) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return types.RunLocalFolderStatus{Gen: gen, Settled: gen}, true
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	rec, ok, err := st.BumpRunLocalFolderGen(rctx, id, store.FolderStatusGuard{Gen: gen})
	if err != nil || !ok {
		slog.WarnContext(ctx, "wardynd: a computer folder's pass record moved before its pass",
			slog.String("run_id", id.String()), slog.Bool("moved", ok), slog.Any("err", err))
		return types.RunLocalFolderStatus{}, false
	}
	return rec, true
}

// recordFolderPassBusy records a pass back the runner refused as busy, at the generation it owned: settled,
// since the runner answered, and kept for a resume so a later sweep tries again.
func (s *Server) recordFolderPassBusy(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int) bool {
	return s.recordFolderPassHeld(ctx, id, folder, gen, folderPassBusyMsg, func(st *types.RunLocalFolderStatus) { st.Settled = gen })
}

// recordFolderPassUnconfirmed records a pass back not sent as the last one is unconfirmed
// (errFolderPassUnconfirmed), at the generation it owned: unsettled, named in the record with errText,
// and kept for a resume.
func (s *Server) recordFolderPassUnconfirmed(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int, errText string) bool {
	return s.recordFolderPassHeld(ctx, id, folder, gen, errText, func(st *types.RunLocalFolderStatus) {
		st.Back = folderPassRecord(s.cfg.Now(), runnerwire.FolderPass{}, errText, true)
	})
}

// recordFolderPassHeld records a pass back that did not run, at the generation it owned, with set applied
// and a resume kept, and audits it with errText. The sandbox stays up either way: a record that did not
// land is another owner's to decide.
func (s *Server) recordFolderPassHeld(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int, errText string, set func(*types.RunLocalFolderStatus)) bool {
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	if s.writeComputerFolderStatus(rctx, id, gen, func(st *types.RunLocalFolderStatus) {
		if st.Resume == nil {
			st.Resume = &types.FolderResume{}
		}
		set(st)
	}) {
		s.auditComputerFolderPass(rctx, id, folder, true, runnerwire.FolderPass{}, errText)
	}
	return true
}
