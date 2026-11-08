// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// EnvelopeStore reads the platform envelopes (table governance_envelopes). A side interface, as
// UnitAdminStore is: a store that does not implement it holds none, so no write is widened. The
// writes are the Q forms below, run inside a governance transaction.
type EnvelopeStore interface {
	// ListEnvelopes is every envelope, by unit id.
	ListEnvelopes(ctx context.Context) ([]types.GovernanceEnvelope, error)
	// GetEnvelope is the envelope on unit itself, or ErrNotFound.
	GetEnvelope(ctx context.Context, unit uuid.UUID) (types.GovernanceEnvelope, error)
	// NearestEnvelope is the envelope on unit or the nearest unit above it; strict leaves unit's own out.
	NearestEnvelope(ctx context.Context, unit uuid.UUID, strict bool) (types.GovernanceEnvelope, bool, error)
}

var _ EnvelopeStore = PG{}

// ErrEnvelopeNested is a write refused because an envelope already sits on a unit above or below the
// target. The nearest envelope at or above a unit is what applies, so two on one path would be
// ambiguous.
var ErrEnvelopeNested = errors.New("store: an envelope already covers this unit's path")

// ErrEnvelopeGroupTaken is a write refused because a group is already approved on another unit's
// envelope.
type ErrEnvelopeGroupTaken struct {
	Group  string
	UnitID uuid.UUID
}

func (e *ErrEnvelopeGroupTaken) Error() string {
	return fmt.Sprintf("store: group %q is already approved on the envelope of unit %s", e.Group, e.UnitID)
}

const envelopeCols = `unit_id, approved_groups, set_by, set_at`

func scanEnvelope(row pgx.Row) (types.GovernanceEnvelope, error) {
	var e types.GovernanceEnvelope
	if err := row.Scan(&e.UnitID, &e.ApprovedGroups, &e.SetBy, &e.SetAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return types.GovernanceEnvelope{}, ErrNotFound
		}
		return types.GovernanceEnvelope{}, err
	}
	if e.ApprovedGroups == nil {
		e.ApprovedGroups = []string{}
	}
	return e, nil
}

func (s PG) ListEnvelopes(ctx context.Context) ([]types.GovernanceEnvelope, error) {
	return ListEnvelopesQ(ctx, s.Pool)
}

func (s PG) GetEnvelope(ctx context.Context, unit uuid.UUID) (types.GovernanceEnvelope, error) {
	return GetEnvelopeQ(ctx, s.Pool, unit)
}

func (s PG) NearestEnvelope(ctx context.Context, unit uuid.UUID, strict bool) (types.GovernanceEnvelope, bool, error) {
	return NearestEnvelopeQ(ctx, s.Pool, unit, strict)
}

// ListEnvelopesQ is ListEnvelopes on q.
func ListEnvelopesQ(ctx context.Context, q Querier) ([]types.GovernanceEnvelope, error) {
	return collect(ctx, q, "list", "envelopes", `SELECT `+envelopeCols+` FROM governance_envelopes ORDER BY unit_id`, nil, scanEnvelope)
}

// GetEnvelopeQ is GetEnvelope on q.
func GetEnvelopeQ(ctx context.Context, q Querier, unit uuid.UUID) (types.GovernanceEnvelope, error) {
	e, err := scanEnvelope(q.QueryRow(ctx, `SELECT `+envelopeCols+` FROM governance_envelopes WHERE unit_id = $1`, unit))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return types.GovernanceEnvelope{}, fmt.Errorf("store: get envelope: %w", err)
	}
	return e, err
}

// NearestEnvelopeQ is NearestEnvelope on q: one recursive statement walking parent_id up from unit
// (bounded at MaxUnitDepthCap rows). An unknown unit has none.
func NearestEnvelopeQ(ctx context.Context, q Querier, unit uuid.UUID, strict bool) (types.GovernanceEnvelope, bool, error) {
	e, err := scanEnvelope(q.QueryRow(ctx, `
		WITH RECURSIVE chain AS (
			SELECT u.id, u.parent_id, 0 AS n FROM governance_units u WHERE u.id = $1
			UNION ALL
			SELECT u.id, u.parent_id, c.n + 1 FROM governance_units u JOIN chain c ON u.id = c.parent_id WHERE c.n < 63
		)
		SELECT `+envelopeCols+` FROM chain c JOIN governance_envelopes e ON e.unit_id = c.id
		WHERE (NOT $2 OR c.n > 0) ORDER BY c.n LIMIT 1`, unit, strict))
	if errors.Is(err, ErrNotFound) {
		return types.GovernanceEnvelope{}, false, nil
	}
	if err != nil {
		return types.GovernanceEnvelope{}, false, fmt.Errorf("store: read nearest envelope: %w", err)
	}
	return e, true, nil
}

// CheckEnvelopeQ reports whether groups could be set as unit's envelope now: ErrNotFound for an unknown
// unit, ErrEnvelopeNested when an envelope sits on a unit above or below it, and *ErrEnvelopeGroupTaken
// when another unit's envelope approves one of groups. It does not lock; SetEnvelopeQ does.
func CheckEnvelopeQ(ctx context.Context, q Querier, unit uuid.UUID, groups []string) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM governance_units WHERE id = $1)`, unit).Scan(&exists); err != nil {
		return fmt.Errorf("store: read governance unit: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	var nested bool
	if err := q.QueryRow(ctx, `
		WITH RECURSIVE up AS (
			SELECT parent_id AS id, 1 AS n FROM governance_units WHERE id = $1 AND parent_id IS NOT NULL
			UNION ALL
			SELECT u.parent_id, up.n + 1 FROM governance_units u JOIN up ON u.id = up.id WHERE u.parent_id IS NOT NULL AND up.n < 64
		), down AS (
			SELECT id, 1 AS n FROM governance_units WHERE parent_id = $1
			UNION ALL
			SELECT u.id, d.n + 1 FROM governance_units u JOIN down d ON u.parent_id = d.id WHERE d.n < 64
		)
		SELECT EXISTS (
			SELECT 1 FROM governance_envelopes e
			WHERE e.unit_id <> $1 AND (e.unit_id IN (SELECT id FROM up) OR e.unit_id IN (SELECT id FROM down))
		)`, unit).Scan(&nested); err != nil {
		return fmt.Errorf("store: read envelopes on the unit's path: %w", err)
	}
	if nested {
		return ErrEnvelopeNested
	}
	var taken ErrEnvelopeGroupTaken
	err := q.QueryRow(ctx, `
		SELECT g, e.unit_id FROM governance_envelopes e, unnest(e.approved_groups) AS g
		WHERE e.unit_id <> $1 AND g = ANY($2::text[]) ORDER BY g LIMIT 1`, unit, nonNil(groups)).Scan(&taken.Group, &taken.UnitID)
	if err == nil {
		return &taken
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: read approved groups: %w", err)
	}
	return nil
}

// SetEnvelopeQ makes groups (already canonical, ValidateEnvelope) the envelope of unit, replacing any,
// under the governance graph lock, so the checks of CheckEnvelopeQ and the write see one tree. q must
// be a pgx.Tx.
func SetEnvelopeQ(ctx context.Context, q Querier, unit uuid.UUID, groups []string, by string) (types.GovernanceEnvelope, error) {
	if err := LockGovernanceGraph(ctx, q); err != nil {
		return types.GovernanceEnvelope{}, err
	}
	if err := CheckEnvelopeQ(ctx, q, unit, groups); err != nil {
		return types.GovernanceEnvelope{}, err
	}
	e, err := scanEnvelope(q.QueryRow(ctx, `
		INSERT INTO governance_envelopes (unit_id, approved_groups, set_by) VALUES ($1, $2::text[], $3)
		ON CONFLICT (unit_id) DO UPDATE
			SET approved_groups = EXCLUDED.approved_groups, set_by = EXCLUDED.set_by, set_at = now()
		RETURNING `+envelopeCols, unit, nonNil(groups), by))
	if err != nil {
		return types.GovernanceEnvelope{}, fmt.Errorf("store: set envelope: %w", err)
	}
	return e, nil
}

// DeleteEnvelopeQ removes unit's envelope under the governance graph lock and returns it as it was, or
// ErrNotFound. q must be a pgx.Tx.
func DeleteEnvelopeQ(ctx context.Context, q Querier, unit uuid.UUID) (types.GovernanceEnvelope, error) {
	if err := LockGovernanceGraph(ctx, q); err != nil {
		return types.GovernanceEnvelope{}, err
	}
	e, err := scanEnvelope(q.QueryRow(ctx, `DELETE FROM governance_envelopes WHERE unit_id = $1 RETURNING `+envelopeCols, unit))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return types.GovernanceEnvelope{}, fmt.Errorf("store: delete envelope: %w", err)
	}
	return e, err
}
