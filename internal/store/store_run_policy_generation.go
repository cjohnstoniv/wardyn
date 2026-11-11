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
	// fresh, keeps retired on the outgoing generation, and returns the new number. It lands only while
	// the run is RUNNING and not lost and its newest generation (NewestPolicyGeneration) is still expected, the one
	// the revive read before its source config; otherwise it changes nothing and is ErrRunPolicyMoved.
	// sealed, when set, is the revive's config, stored as the run's (run_proxy_configs) in the same
	// transaction, so no reader sees the new generation with the source config before it. A run the
	// table does not hold is ErrNotFound.
	AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, expected int, digest string, retired []string, fresh string, sealed []byte) (int, error)
	// AbandonRunPolicyGeneration marks gen abandoned when it is still the newest, putting the one before
	// it back as current: a revive that never replaced the old proxy. restore, when set, is the config the
	// revive started from, stored back in the same transaction and under the same condition, so a late
	// abandon never touches a newer generation or the config that generation stored. A gen that no longer
	// holds the newest slot, or is already abandoned, changes nothing and is no error; a restore that
	// cannot be written rolls the abandon back.
	AbandonRunPolicyGeneration(ctx context.Context, id uuid.UUID, gen int, restore []byte) error
	// RunPolicyState reads the run's policy state (RunPolicyStateOf), for the readers of its effective
	// policy: which generation is current, abandoned ones marked. found is false for a run the table does
	// not hold.
	RunPolicyState(ctx context.Context, id uuid.UUID) (st types.RunPolicyState, found bool, err error)
}

var _ RunPolicyGenerations = PG{}

// ErrRunPolicyMoved is AdvanceRunPolicyGeneration's refusal: another revive started a generation since
// the caller read the run's newest one, or the run was lost or left RUNNING since its claim. Nothing was
// written.
var ErrRunPolicyMoved = errors.New("store: the run's policy generation moved since it was read")

// NewestPolicyGeneration is the number of the run's last generation, abandoned ones included, or 1
// before any revive: the number a revive reads before its source config and AdvanceRunPolicyGeneration
// compares. Every advance moves it, so an abandoned revive cannot bring an older number back.
func NewestPolicyGeneration(st types.RunPolicyState) int {
	if n := len(st.Generations); n > 0 {
		return st.Generations[n-1].Generation
	}
	return 1
}

// newestPolicyGenerationSQL is NewestPolicyGeneration over the row: Advance appends each generation one
// past the last, so the array's last element is the newest.
const newestPolicyGenerationSQL = `COALESCE((policy_generations -> -1 ->> 'generation')::int, 1)`

// putRunProxyConfigSQL is PutRunProxyConfig's write, for the generation writes' own transaction.
const putRunProxyConfigSQL = `
	INSERT INTO run_proxy_configs (run_id, sealed) VALUES ($1, $2)
	ON CONFLICT (run_id) DO UPDATE SET sealed=$2, updated_at=now()`

// AdvanceRunPolicyGeneration — see RunPolicyGenerations. The row is locked for the read and the write,
// so two revives cannot take one number, and the write's own predicate decides the race.
func (s PG) AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, expected int, digest string, retired []string, fresh string, sealed []byte) (int, error) {
	var gen int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		st, err := readPolicyGenerations(ctx, tx, id)
		if err != nil {
			return err
		}
		st, gen = st.Advance(digest, retired, fresh)
		applied, err := writePolicyGenerations(ctx, tx, id, st,
			`state = $5 AND lost_at IS NULL AND `+newestPolicyGenerationSQL+` = $4`, expected, string(types.RunRunning))
		switch {
		case err != nil:
			return err
		case !applied:
			return ErrRunPolicyMoved
		case sealed == nil:
			return nil
		}
		if _, err := tx.Exec(ctx, putRunProxyConfigSQL, id, sealed); err != nil {
			return fmt.Errorf("store: put run proxy config: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return gen, nil
}

// AbandonRunPolicyGeneration — see RunPolicyGenerations. A gen that is no longer the newest, or already
// abandoned, changes nothing.
func (s PG) AbandonRunPolicyGeneration(ctx context.Context, id uuid.UUID, gen int, restore []byte) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		st, err := readPolicyGenerations(ctx, tx, id)
		if err != nil {
			return err
		}
		n := len(st.Generations)
		if n == 0 || st.Generations[n-1].Generation != gen || st.Generations[n-1].Abandoned {
			return nil
		}
		applied, err := writePolicyGenerations(ctx, tx, id, st.Abandon(gen), newestPolicyGenerationSQL+` = $4`, gen)
		if err != nil || !applied || restore == nil {
			return err
		}
		if _, err := tx.Exec(ctx, putRunProxyConfigSQL, id, restore); err != nil {
			return fmt.Errorf("store: restore run proxy config: %w", err)
		}
		return nil
	})
}

// RunPolicyState — see RunPolicyGenerations.
func (s PG) RunPolicyState(ctx context.Context, id uuid.UUID) (types.RunPolicyState, bool, error) {
	return RunPolicyStateOf(ctx, s.Pool, id)
}

// readPolicyGenerations reads the run's policy generations, its row locked for the transaction.
func readPolicyGenerations(ctx context.Context, tx pgx.Tx, id uuid.UUID) (types.RunPolicyState, error) {
	var st types.RunPolicyState
	var gens []byte
	err := tx.QueryRow(ctx, `SELECT policy_digest, policy_generations FROM agent_runs WHERE id = $1 FOR UPDATE`, id).Scan(&st.Digest, &gens)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return st, ErrNotFound
	case err != nil:
		return st, fmt.Errorf("store: read run policy generations: %w", err)
	}
	if err := json.Unmarshal(gens, &st.Generations); err != nil {
		return st, fmt.Errorf("store: read run policy generations: %w", err)
	}
	return st, nil
}

// writePolicyGenerations writes st over the run's row where cond holds (args are $4 on), and reports
// whether it did.
func writePolicyGenerations(ctx context.Context, tx pgx.Tx, id uuid.UUID, st types.RunPolicyState, cond string, args ...any) (bool, error) {
	out, err := json.Marshal(st.Generations)
	if err != nil {
		return false, fmt.Errorf("store: write run policy generations: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_runs SET policy_digest = $2, policy_generations = $3 WHERE id = $1 AND `+cond,
		append([]any{id, st.Digest, out}, args...)...)
	if err != nil {
		return false, fmt.Errorf("store: write run policy generations: %w", err)
	}
	return tag.RowsAffected() > 0, nil
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
