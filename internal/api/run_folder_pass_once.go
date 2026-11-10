// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner runs one pass per run at a time when it says so (Caps.FolderPassOnce), and refuses a second
// with folder_pass_busy. Every pass back moves the run's record to a generation of its own first, and
// marks it settled (types.RunLocalFolderStatus.Settled) once the runner answered for it or nothing was
// sent. An older runner, which runs whatever it is sent, gets a pass back only while the last one is
// settled: one it may still be running would run beside the new one.

// folderPassBusyMsg is the record of a pass a runner refused while it runs another for the run.
const folderPassBusyMsg = "The runner is still running this run's last pass back; a later sweep tries again"

// folderPassUnconfirmed reports a pass back that must not be sent: the run's folder runner does not run
// one pass per run at a time, and the last pass is not settled. A runner that cannot be asked sends
// nothing anyway, and is left to the pass's own refusal.
func (s *Server) folderPassUnconfirmed(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, rec types.RunLocalFolderStatus) bool {
	if folder.RunnerID == nil || rec.Gen == rec.Settled {
		return false
	}
	sub, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID)
	if refusal != nil {
		return false
	}
	caps, ok := sub.Caps()
	return ok && !caps.FolderPassOnce
}

// bumpComputerFolderGen moves run id's record from gen to the next, and answers the generation the pass
// back then owns: false when the record moved first or could not be written. A store with no records
// answers gen.
func (s *Server) bumpComputerFolderGen(ctx context.Context, id uuid.UUID, gen int) (int, bool) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return gen, true
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	rec, ok, err := st.BumpRunLocalFolderGen(rctx, id, store.FolderStatusGuard{Gen: gen})
	if err != nil || !ok {
		slog.WarnContext(ctx, "wardynd: a computer folder's pass record moved before its pass back",
			slog.String("run_id", id.String()), slog.Bool("moved", ok), slog.Any("err", err))
		return 0, false
	}
	return rec.Gen, true
}

// recordFolderPassBusy records a pass back the runner refused as busy, at the generation it owned: settled,
// since the runner answered, and kept for a resume so a later sweep tries again. The sandbox stays up
// either way: a record that did not land is another owner's to decide.
func (s *Server) recordFolderPassBusy(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int) bool {
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	if s.writeComputerFolderStatus(rctx, id, gen, func(st *types.RunLocalFolderStatus) {
		if st.Resume == nil {
			st.Resume = &types.FolderResume{}
		}
		st.Settled = gen
	}) {
		s.auditComputerFolderPass(rctx, id, folder, true, runnerwire.FolderPass{}, folderPassBusyMsg)
	}
	return true
}
