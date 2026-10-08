// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// componentStore is the store's component seam when it has one.
func (s *Server) componentStore() (store.ComponentStore, error) {
	if cs, ok := s.cfg.Store.(store.ComponentStore); ok {
		return cs, nil
	}
	return nil, fmt.Errorf("%w: the store has no components", erasure.ErrNotAvailable)
}

// eraseComponentsOf is the components scope: it deletes the components the person saved and clears
// what the snapshot of every run says about the ones they defined. It never deletes a snapshot
// row. That row is the run's record that it carried components, and revive reads it to ask again
// whether the run may keep what they reached; with the row gone, the run would look as if it
// never had any. The organisation's components are untouched. The GitHub connection's numeric id
// sits in the credential blob, so the credentials scope erases it and nothing here does.
func (s *Server) eraseComponentsOf(ctx context.Context, person string) (any, error) {
	cs, err := s.componentStore()
	if err != nil {
		return nil, err
	}
	saved, err := cs.DeleteComponentsByOwner(ctx, person)
	if err != nil {
		return nil, err
	}
	snapshots, err := cs.EraseRunComponentsByOwner(ctx, person)
	if err != nil {
		return nil, err
	}
	return map[string]any{"components": saved, "run_snapshots_cleared": snapshots}, nil
}
