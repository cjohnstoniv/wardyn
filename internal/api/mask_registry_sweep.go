// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The shared masking registry's side of the API (ha-l2.1): the leader's retention
// pass and the live writer's fail-closed read.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// sweepCommittedMasks is the leader's half of SweepRunSecrets: it deletes the
// committed per-run values and masking manifests of the runs that went cold, and
// tombstones the credential values retired or expired past the grace. A follower
// (lead false) does nothing.
func (s *Server) sweepCommittedMasks(ctx context.Context, lead bool, cold []uuid.UUID) error {
	if !lead {
		return nil
	}
	err := s.cfg.MaskRegistry.PurgeRuns(ctx, cold)
	if _, gerr := s.cfg.MaskRegistry.SweepPersisted(ctx, s.cfg.Now().Add(-RunSecretGrace)); gerr != nil {
		err = errors.Join(err, gerr)
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: the committed masking rows were not all swept", slog.Any("err", err))
		return fmt.Errorf("sweep the committed masking rows: %w", err)
	}
	return nil
}

// replaceIfStale is liveMaskWriter's fail-closed read of the shared registry: the
// chunk of n bytes is masked only against a corpus read after it arrived, so a
// value another replica committed before it is in. When that cannot be proven
// (Postgres does not answer) the chunk is replaced by the placeholder, as a
// recovered masker panic is: the writer never forwards bytes it cannot vouch for.
// stale reports that the chunk was dealt with and err is the destination's.
func (w *liveMaskWriter) replaceIfStale(n int) (stale bool, err error) {
	if w.reg.Fresh(time.Now()) {
		return false, nil
	}
	w.tail = nil
	w.capture.dropped, w.capture.uncovered = true, true
	_, err = w.dst.Write(secretmask.Placeholder())
	return true, err
}
