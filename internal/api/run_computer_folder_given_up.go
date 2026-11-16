// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// setUnreadFolderFate is setFolderFate for a run whose folder record cannot be decoded. v says so
// (RecordUnreadable). A give-up the record holds, read without decoding it (store.RunLocalFolderGivenUps), is shown
// as given up, with its count, as the run's given_up row shows it; the sandbox copy follows the run's ref, as for any
// record. Nothing else the record holds is shown: the view cannot tell.
func (s *Server) setUnreadFolderFate(ctx context.Context, v *computerFolderRunView, id uuid.UUID, seeded bool) {
	v.RecordUnreadable = true
	gs, ok := s.cfg.Store.(store.RunLocalFolderGivenUps)
	if !ok {
		return
	}
	g, err := gs.RunLocalFolderGivenUp(ctx, id)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: read a computer folder's give-up", slog.String("run_id", id.String()), slog.Any("err", err))
		return
	}
	if g.At == nil {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil {
		return
	}
	st := types.RunLocalFolderStatus{Back: &types.FolderPassRecord{At: *g.At, GivenUpAt: g.At, GivenUpCount: g.Count}}
	f := folderFateOf(run, st, seeded, false, s.pendingCap())
	v.Fate, v.SandboxCopy, v.LastReport = f.fate, f.sandboxCopy, f.lastReport
}
