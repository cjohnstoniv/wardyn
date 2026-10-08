// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Custom components (migration 0136_components) and the snapshot of the ones a
// run launched with (migration 0137_run_components). Round-trips rows; a
// definition is validated at the API boundary. See ComponentStore (iface.go).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const componentCols = `id, owner, name, definition, version, created_by, created_at, updated_at`

// CreateComponent — see ComponentStore.
func (s PG) CreateComponent(ctx context.Context, c types.Component) (types.Component, error) {
	if c.ID == uuid.Nil {
		return types.Component{}, errors.New("store: create component: the caller chooses the id")
	}
	def, err := marshalComponentDefinition(c.Definition)
	if err != nil {
		return types.Component{}, err
	}
	const q = `
		INSERT INTO components (id, owner, name, definition, version, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, $5, now(), now())
		RETURNING ` + componentCols
	return scanComponent(s.Pool.QueryRow(ctx, q, c.ID, c.Owner, c.Name, def, c.CreatedBy))
}

// UpdateComponent — see ComponentStore.
func (s PG) UpdateComponent(ctx context.Context, c types.Component) (types.Component, error) {
	def, err := marshalComponentDefinition(c.Definition)
	if err != nil {
		return types.Component{}, err
	}
	const q = `
		UPDATE components SET name = $3, definition = $4, version = version + 1, updated_at = now()
		WHERE id = $1 AND owner = $2
		RETURNING ` + componentCols
	return scanComponent(s.Pool.QueryRow(ctx, q, c.ID, c.Owner, c.Name, def))
}

// DeleteComponent — see ComponentStore.
func (s PG) DeleteComponent(ctx context.Context, id uuid.UUID, owner string) (types.Component, error) {
	const q = `DELETE FROM components WHERE id = $1 AND owner = $2 RETURNING ` + componentCols
	return scanComponent(s.Pool.QueryRow(ctx, q, id, owner))
}

// GetComponent — see ComponentStore.
func (s PG) GetComponent(ctx context.Context, id uuid.UUID, owner string) (types.Component, error) {
	const q = `SELECT ` + componentCols + ` FROM components WHERE id = $1 AND owner = $2`
	return scanComponent(s.Pool.QueryRow(ctx, q, id, owner))
}

// ListComponents — see ComponentStore.
func (s PG) ListComponents(ctx context.Context, owner string) ([]types.Component, error) {
	const q = `SELECT ` + componentCols + ` FROM components WHERE owner = $1 ORDER BY name`
	return collect(ctx, s.Pool, "list", "components", q, []any{owner}, scanComponent)
}

// ListComponentsByIDs — see ComponentStore.
func (s PG) ListComponentsByIDs(ctx context.Context, owner string, ids []uuid.UUID) ([]types.Component, error) {
	if len(ids) == 0 {
		return []types.Component{}, nil
	}
	const q = `SELECT ` + componentCols + ` FROM components
		WHERE id = ANY($1) AND (owner = '' OR owner = $2) ORDER BY owner, name`
	return collect(ctx, s.Pool, "list", "components by id", q, []any{ids, owner}, scanComponent)
}

// CountComponents — see ComponentStore.
func (s PG) CountComponents(ctx context.Context, owner string) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM components WHERE owner = $1`, owner).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count components: %w", err)
	}
	return n, nil
}

// DeleteComponentsByOwner — see ComponentStore.
func (s PG) DeleteComponentsByOwner(ctx context.Context, owner string) (int, error) {
	return s.eraseComponentRowsOf(ctx, `DELETE FROM components WHERE owner = $1`, owner)
}

// EraseRunComponentsByOwner — see ComponentStore. It clears content and keeps
// the row: the row is the run's authorization tombstone.
func (s PG) EraseRunComponentsByOwner(ctx context.Context, owner string) (int, error) {
	return s.eraseComponentRowsOf(ctx, `UPDATE run_components
		SET owner = NULL, name = NULL, version = NULL, definition = NULL, component_id = NULL
		WHERE owner = $1`, owner)
}

// eraseComponentRowsOf runs one person's erasure statement. Owner "" is the
// organisation's rows, which no person's erasure may take.
func (s PG) eraseComponentRowsOf(ctx context.Context, stmt, owner string) (int, error) {
	if owner == "" {
		return 0, errors.New("store: erase components by owner: an owner is required")
	}
	tag, err := s.Pool.Exec(ctx, stmt, owner)
	if err != nil {
		return 0, fmt.Errorf("store: erase components by owner: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PutRunComponents — see ComponentStore.
func (s PG) PutRunComponents(ctx context.Context, runID uuid.UUID, comps []types.RunComponent) error {
	defs := make([][]byte, len(comps))
	for i, c := range comps {
		def, err := marshalComponentDefinition(c.Definition)
		if err != nil {
			return err
		}
		defs[i] = def
	}
	err := s.inTx(ctx, func(q Querier) error {
		const ins = `
			INSERT INTO run_components (run_id, ordinal, component_id, owner, name, version, definition, self_defined)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
		for i, c := range comps {
			if _, err := q.Exec(ctx, ins, runID, i, c.ComponentID, c.Owner, c.Name, c.Version, defs[i], c.SelfDefined); err != nil {
				return err
			}
		}
		return nil
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return ErrNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return ErrConflict
	case err != nil:
		return fmt.Errorf("store: put run components: %w", err)
	}
	return nil
}

// ListRunComponents — see ComponentStore.
func (s PG) ListRunComponents(ctx context.Context, runID uuid.UUID) ([]types.RunComponent, error) {
	const q = `SELECT run_id, ordinal, self_defined, component_id, owner, name, version, definition
		FROM run_components WHERE run_id = $1 ORDER BY ordinal`
	return collect(ctx, s.Pool, "list", "run components", q, []any{runID}, scanRunComponent)
}

// marshalComponentDefinition encodes d with hosts as [] rather than null, the
// empty-array shape every read of this package returns.
func marshalComponentDefinition(d types.ComponentDefinition) ([]byte, error) {
	if d.Hosts == nil {
		d.Hosts = []string{}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("store: marshal component definition: %w", err)
	}
	return raw, nil
}

func scanComponent(row pgx.Row) (types.Component, error) {
	var c types.Component
	var def []byte
	err := row.Scan(&c.ID, &c.Owner, &c.Name, &def, &c.Version, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return types.Component{}, ErrNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return types.Component{}, ErrConflict
	case err != nil:
		return types.Component{}, fmt.Errorf("store: scan component: %w", err)
	}
	if err := json.Unmarshal(def, &c.Definition); err != nil {
		return types.Component{}, fmt.Errorf("store: unmarshal component definition: %w", err)
	}
	return c, nil
}

func scanRunComponent(row pgx.Row) (types.RunComponent, error) {
	var c types.RunComponent
	var owner, name *string
	var version *int
	var def []byte
	if err := row.Scan(&c.RunID, &c.Ordinal, &c.SelfDefined, &c.ComponentID, &owner, &name, &version, &def); err != nil {
		return types.RunComponent{}, fmt.Errorf("store: scan run component: %w", err)
	}
	if owner == nil {
		c.Erased = true
		return c, nil
	}
	c.Owner, c.Name, c.Version = *owner, *name, *version
	if err := json.Unmarshal(def, &c.Definition); err != nil {
		return types.RunComponent{}, fmt.Errorf("store: unmarshal run component definition: %w", err)
	}
	return c, nil
}
