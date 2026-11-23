// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// alsoEraseWindowsSeats wraps the runner_pools erasure step: after it, the person's seats in Windows licence
// pools are deleted too. A seat a run still holds fails the scope, naming the run, and deletes none of them; the
// scope is retried once the run has ended, which frees the seat.
func (s *Server) alsoEraseWindowsSeats(step erasure.Step) erasure.Step {
	return func(ctx context.Context, person string) (any, error) {
		detail, err := step(ctx, person)
		ws, ok := s.cfg.Store.(store.WindowsLicenceStore)
		if err != nil || !ok {
			return detail, err
		}
		n, err := ws.DeleteWindowsLicenceHoldersOf(ctx, []string{person})
		if d, isMap := detail.(map[string]any); isMap {
			d["windows_licence_seats"] = n
		}
		return detail, err
	}
}
