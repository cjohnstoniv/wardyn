// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunDataOuts keeps a run's data_out rule on its row (agent_runs.data_out, migration 0291), where audit
// retention cannot drop it; GetRun reads it back as AgentRun.DataOut. Optional for RunLeaser's reason: the
// api layer type-asserts, and production is always PG.
type RunDataOuts interface {
	// RaiseRunDataOut stores rule as the run's data_out unless the row already holds a stricter one, so the
	// column is only ever raised. A run the table does not hold is ErrNotFound.
	RaiseRunDataOut(ctx context.Context, id uuid.UUID, rule types.TransferRule) error
}

var _ RunDataOuts = PG{}

// RaiseRunDataOut — see RunDataOuts. "" is stored as allowed.
func (s PG) RaiseRunDataOut(ctx context.Context, id uuid.UUID, rule types.TransferRule) error {
	rule = rule.DataOut()
	if !types.ValidDataOut(rule) {
		return fmt.Errorf("store: raise run data_out: %q is not a rule", rule)
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET data_out = CASE
			WHEN data_out IS NULL OR array_position(`+dataOutOrderSQL+`, data_out) < array_position(`+dataOutOrderSQL+`, $2::text)
			THEN $2::text ELSE data_out END
		WHERE id = $1`, id, string(rule))
	if err != nil {
		return fmt.Errorf("store: raise run data_out: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// dataOutOrderSQL is the data_out rules from least to most strict (types.TransferStrictness).
const dataOutOrderSQL = `ARRAY['allowed', 'review', 'blocked']::text[]`
