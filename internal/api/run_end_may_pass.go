// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// endMayPass reports whether run id's end may run a pass back (bringComputerFolderBack), so it must hold
// the run's lock: a both-ways folder from a runner, seeded, allowed out, with no pass back brought home
// yet. A fact that cannot be read counts as a pass that can happen. Any other run is proved to have none.
func (s *Server) endMayPass(ctx context.Context, id uuid.UUID) bool {
	fs, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return false
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	folder, seed, err := fs.RunLocalFolderSeed(rctx, id)
	switch {
	case err != nil:
		return true
	case folder == nil || folder.RunnerID == nil || folder.Direction != types.ComputerFolderBoth || seed.SeededAt == nil:
		return false
	}
	if rule, _, err := s.runDataOutRule(rctx, id); err != nil {
		return true
	} else if rule != types.TransferAllowed {
		return false
	}
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return true
	}
	rec, err := st.RunLocalFolderStatus(rctx, id)
	if err != nil {
		// No record yet (no pass ran), or one that cannot be read.
		return true
	}
	return rec.Back == nil || rec.Back.Error != ""
}
