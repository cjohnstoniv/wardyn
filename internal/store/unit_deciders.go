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

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// UnitDeciderStore keeps each unit's decider rules (migration governance_unit_deciders). A side
// interface, as UnitAdminStore is: a store that does not implement it holds none, so no unit has a
// rule and holds are decided as they were without it.
type UnitDeciderStore interface {
	// ListUnitDeciders is every unit's rules, in unit order.
	ListUnitDeciders(ctx context.Context) ([]types.UnitDeciders, error)
}

const unitDeciderCols = `unit_id, rules, updated_by, updated_at`

func (s PG) ListUnitDeciders(ctx context.Context) ([]types.UnitDeciders, error) {
	return ListUnitDecidersQ(ctx, s.Pool)
}

// ListUnitDecidersQ is ListUnitDeciders on q.
func ListUnitDecidersQ(ctx context.Context, q Querier) ([]types.UnitDeciders, error) {
	return collect(ctx, q, "list", "unit deciders", `SELECT `+unitDeciderCols+` FROM governance_unit_deciders
		ORDER BY unit_id`, nil, scanUnitDeciders)
}

// GetUnitDecidersQ is one unit's rules, read for update when forUpdate. ErrNotFound when it has none.
func GetUnitDecidersQ(ctx context.Context, q Querier, unit uuid.UUID, forUpdate bool) (types.UnitDeciders, error) {
	d, err := scanUnitDeciders(q.QueryRow(ctx, `SELECT `+unitDeciderCols+` FROM governance_unit_deciders
		WHERE unit_id = $1`+lockSuffix(forUpdate), unit))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.UnitDeciders{}, ErrNotFound
	}
	if err != nil {
		return types.UnitDeciders{}, fmt.Errorf("store: get unit deciders: %w", err)
	}
	return d, nil
}

// SetUnitDecidersQ replaces a unit's rules on q; no rules clear the unit's row. ErrNotFound when the
// unit is unknown.
func SetUnitDecidersQ(ctx context.Context, q Querier, unit uuid.UUID, rules []types.DeciderRule, by string) (types.UnitDeciders, error) {
	if len(rules) == 0 {
		if _, err := q.Exec(ctx, `DELETE FROM governance_unit_deciders WHERE unit_id = $1`, unit); err != nil {
			return types.UnitDeciders{}, fmt.Errorf("store: clear unit deciders: %w", err)
		}
		return types.UnitDeciders{UnitID: unit, Rules: []types.DeciderRule{}, UpdatedBy: by}, nil
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return types.UnitDeciders{}, fmt.Errorf("store: encode unit deciders: %w", err)
	}
	d, err := scanUnitDeciders(q.QueryRow(ctx, `
		INSERT INTO governance_unit_deciders (unit_id, rules, updated_by) VALUES ($1, $2, $3)
		ON CONFLICT (unit_id) DO UPDATE SET rules = EXCLUDED.rules, updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING `+unitDeciderCols, unit, json.RawMessage(raw), by))
	switch {
	case isForeignKeyViolation(err):
		return types.UnitDeciders{}, ErrNotFound
	case err != nil:
		return types.UnitDeciders{}, fmt.Errorf("store: set unit deciders: %w", err)
	}
	return d, nil
}

// scanUnitDeciders reads one row. The rules decode without refusing an unknown field, kind or type: a
// row a newer build wrote reads back whole, and the resolver matches none of what it does not know.
func scanUnitDeciders(row pgx.Row) (types.UnitDeciders, error) {
	var d types.UnitDeciders
	var raw json.RawMessage
	if err := row.Scan(&d.UnitID, &raw, &d.UpdatedBy, &d.UpdatedAt); err != nil {
		return types.UnitDeciders{}, err
	}
	if err := json.Unmarshal(raw, &d.Rules); err != nil {
		return types.UnitDeciders{}, fmt.Errorf("decode rules: %w", err)
	}
	if d.Rules == nil {
		d.Rules = []types.DeciderRule{}
	}
	return d, nil
}
