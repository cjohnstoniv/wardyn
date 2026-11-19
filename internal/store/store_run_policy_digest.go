// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrPolicyDigestSet is returned by SetRunPolicyDigest when the run already holds a different digest.
var ErrPolicyDigestSet = errors.New("store: run policy digest already set")

// RunPolicyDigests stores the digest of a run's effective policy (agent_runs.policy_digest). Optional for
// RunLeaser's reason: the api layer type-asserts, and production is always PG.
type RunPolicyDigests interface {
	// SetRunPolicyDigest writes digest once. The same digest again is nil; a different one is
	// ErrPolicyDigestSet and changes nothing; a run the table does not hold is ErrNotFound.
	SetRunPolicyDigest(ctx context.Context, id uuid.UUID, digest string) error
}

var _ RunPolicyDigests = PG{}

// SetRunPolicyDigest — see RunPolicyDigests.
func (s PG) SetRunPolicyDigest(ctx context.Context, id uuid.UUID, digest string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET policy_digest = $2 WHERE id = $1 AND policy_digest = ''`, id, digest)
	if err != nil {
		return fmt.Errorf("store: set run policy digest: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	held, found, err := RunPolicyDigest(ctx, s.Pool, id)
	switch {
	case err != nil:
		return err
	case !found:
		return ErrNotFound
	case held == digest:
		return nil
	}
	return ErrPolicyDigestSet
}

// RunPolicyDigest reads a run's policy digest for the audit recorder, which stamps it on the run's rows.
// found is false for a run the table does not hold; the digest is "" for a run with none recorded.
func RunPolicyDigest(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID) (digest string, found bool, err error) {
	// ponytail: one PK read per run row, outside the chain lock; cache if audit latency shows it.
	err = pool.QueryRow(ctx, `SELECT policy_digest FROM agent_runs WHERE id = $1`, runID).Scan(&digest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store: read run policy digest: %w", err)
	}
	return digest, true, nil
}
