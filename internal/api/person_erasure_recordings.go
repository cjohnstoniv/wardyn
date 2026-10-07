// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// eraseRecordingsOf fences and deletes recordings of every run the person
// created before the run list was read. A deployment with no recording store has none to delete; a store
// that cannot delete is not available, never reported erased.
func (s *Server) eraseRecordingsOf(ctx context.Context, person string) (any, error) {
	if s.cfg.RecordingStore == nil {
		return map[string]any{"recordings": 0}, nil
	}
	del, ok := s.cfg.RecordingStore.(recording.RunDeleter)
	if !ok {
		return nil, fmt.Errorf("%w: the recording store cannot delete", erasure.ErrNotAvailable)
	}
	pe, err := s.personErasureStore()
	if err != nil {
		return nil, err
	}
	ids, err := pe.RunIDsCreatedBy(ctx, person)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, id := range ids {
		n, err := del.DeleteRun(ctx, id.String())
		if err != nil {
			return nil, err
		}
		total += n
	}
	return map[string]any{"recordings": total, "runs_fenced": len(ids)}, nil
}
