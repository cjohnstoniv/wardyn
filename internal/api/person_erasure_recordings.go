// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// eraseRecordingsOf fences and deletes recordings of every run the person
// created before the run list was read. Fence derived output first: an already
// open recording reader may outlive DeleteRun, even across backend reconfiguration.
// A run the run history retention sweep deleted is found by its tombstone (its
// recording stayed); once its recordings are gone, the tombstone's creator is
// blanked.
func (s *Server) eraseRecordingsOf(ctx context.Context, person string) (any, error) {
	pe, err := s.personErasureStore()
	if err != nil {
		return nil, err
	}
	var ids, deleted []uuid.UUID
	retained, _ := s.cfg.Store.(store.RetainedRunErasureStore)
	if retained != nil {
		ids, deleted, err = retained.PersonRunIDs(ctx, person)
	} else {
		ids, err = pe.RunIDsCreatedBy(ctx, person)
	}
	if err != nil {
		return nil, err
	}
	// Derived output has a row only while its run does.
	if st, ok := s.cfg.Store.(store.RunOutputStore); ok {
		if _, err := st.EraseRecordingRunOutputs(ctx, ids); err != nil {
			return nil, err
		}
	}
	if s.cfg.RecordingStore == nil {
		return map[string]any{"recordings": 0}, nil
	}
	del, ok := s.cfg.RecordingStore.(recording.RunDeleter)
	if !ok {
		return nil, fmt.Errorf("%w: the recording store cannot delete", erasure.ErrNotAvailable)
	}
	all := slices.Concat(ids, deleted)
	total := 0
	// One scan of the store for all the runs where it can, not one per run.
	if many, ok := s.cfg.RecordingStore.(recording.RunsDeleter); ok && len(all) > 0 {
		keys := make([]string, len(all))
		for i, id := range all {
			keys[i] = id.String()
		}
		if total, err = many.DeleteRuns(ctx, keys); err != nil {
			return nil, err
		}
	} else {
		for _, id := range all {
			n, err := del.DeleteRun(ctx, id.String())
			if err != nil {
				return nil, err
			}
			total += n
		}
	}
	out := map[string]any{"recordings": total, "runs_fenced": len(all)}
	if retained != nil {
		if _, err := retained.BlankRunTombstoneCreators(ctx, person, deleted); err != nil {
			return nil, err
		}
		out["tombstoned_runs"] = len(deleted)
	}
	return out, nil
}
