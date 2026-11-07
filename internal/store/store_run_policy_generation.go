// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunPolicyGenerations records a revive's policy generation (agent_runs.policy_generations, with
// policy_digest as the current one). Optional for RunLeaser's reason: the api layer type-asserts, and
// production is always PG.
type RunPolicyGenerations interface {
	// AdvanceRunPolicyGeneration starts the run's next generation under digest, its proxy on the token
	// fresh, keeps retired on the outgoing generation, and returns the new number. A run the table does
	// not hold is ErrNotFound.
	AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, digest string, retired []string, fresh string) (int, error)
	// AbandonRunPolicyGeneration marks gen abandoned when it is still the newest, putting the one before
	// it back as current: a revive that never replaced the old proxy.
	AbandonRunPolicyGeneration(ctx context.Context, id uuid.UUID, gen int) error
}

var _ RunPolicyGenerations = PG{}

// AdvanceRunPolicyGeneration — see RunPolicyGenerations. The row is locked for the read and the write,
// so two revives cannot take one number (a revive also holds the run's lock).
func (s PG) AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, digest string, retired []string, fresh string) (int, error) {
	var gen int
	err := s.changePolicyGenerations(ctx, id, func(st types.RunPolicyState) types.RunPolicyState {
		st, gen = st.Advance(digest, retired, fresh)
		return st
	})
	return gen, err
}

// AbandonRunPolicyGeneration — see RunPolicyGenerations.
func (s PG) AbandonRunPolicyGeneration(ctx context.Context, id uuid.UUID, gen int) error {
	return s.changePolicyGenerations(ctx, id, func(st types.RunPolicyState) types.RunPolicyState { return st.Abandon(gen) })
}

func (s PG) changePolicyGenerations(ctx context.Context, id uuid.UUID, change func(types.RunPolicyState) types.RunPolicyState) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var st types.RunPolicyState
		var gens []byte
		err := tx.QueryRow(ctx, `SELECT policy_digest, policy_generations FROM agent_runs WHERE id = $1 FOR UPDATE`, id).Scan(&st.Digest, &gens)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("store: read run policy generations: %w", err)
		}
		if err := json.Unmarshal(gens, &st.Generations); err != nil {
			return fmt.Errorf("store: read run policy generations: %w", err)
		}
		st = change(st)
		out, err := json.Marshal(st.Generations)
		if err != nil {
			return fmt.Errorf("store: write run policy generations: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_runs SET policy_digest = $2, policy_generations = $3 WHERE id = $1`, id, st.Digest, out); err != nil {
			return fmt.Errorf("store: write run policy generations: %w", err)
		}
		return nil
	})
}

// RunPolicyStateOf reads a run's policy state for the audit recorder, which stamps it on the run's rows:
// the current digest, the generations and the live token lineage, in one row read. found is false for a
// run the table does not hold, whose state is the zero value.
func RunPolicyStateOf(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID) (st types.RunPolicyState, found bool, err error) {
	// ponytail: one PK read per run row, outside the chain lock; cache if audit latency shows it.
	var gens []byte
	var jti, prev string
	err = pool.QueryRow(ctx, `SELECT policy_digest, policy_generations, COALESCE(token_jti, ''), COALESCE(token_prev_jti, '')
		FROM agent_runs WHERE id = $1`, runID).Scan(&st.Digest, &gens, &jti, &prev)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return types.RunPolicyState{}, false, nil
	case err != nil:
		return types.RunPolicyState{}, false, fmt.Errorf("store: read run policy state: %w", err)
	}
	if err := json.Unmarshal(gens, &st.Generations); err != nil {
		return types.RunPolicyState{}, false, fmt.Errorf("store: read run policy state: %w", err)
	}
	for _, id := range []string{jti, prev} {
		if id != "" {
			st.LiveTokenIDs = append(st.LiveTokenIDs, id)
		}
	}
	return st, true, nil
}
