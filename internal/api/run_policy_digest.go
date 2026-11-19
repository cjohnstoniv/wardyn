// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordRunPolicyDigest stores the digest of the policy dispatch is about to record as
// run.policy.resolve, before that row is written, so the resolve row and every later row of the run carry
// it (audit.PolicyStampRecorder reads it back). Dispatch is the one writer and runs once per run, so a
// second, different value is a bug and is logged as an error. The digest is evidence: a store without the
// column, or a failed write, logs and leaves the run's rows unstamped; it never fails a dispatch.
func (s *Server) recordRunPolicyDigest(ctx context.Context, runID uuid.UUID, spec types.RunPolicySpec) {
	st, ok := s.cfg.Store.(store.RunPolicyDigests)
	if !ok {
		return
	}
	err := st.SetRunPolicyDigest(ctx, runID, spec.Digest())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrPolicyDigestSet):
		slog.Error("api: run already holds a different policy digest", slog.String("run_id", runID.String()))
	default:
		slog.Warn("api: run policy digest not stored, its audit rows go unstamped", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}
