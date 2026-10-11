// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sweepLapsedRunners deletes unclaimed runners past their 24h wait. The runner lists already hide them,
// so without this pass the row would stay invisible and its key would stay registered, refusing the
// same identity for good (runners.key_fingerprint is unique). Idempotent across replicas.
func (s *Server) sweepLapsedRunners(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return nil
	}
	n, err := st.ExpireUnclaimedRunners(ctx, s.cfg.Now().UTC().Add(-types.RunnerUnclaimedTTL))
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: expired unclaimed runners", slog.Int("runners", n))
	}
	return err
}
