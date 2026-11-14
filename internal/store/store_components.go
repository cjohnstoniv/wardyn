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

// componentCols is every read's columns: the row, its unit pin (owning_unit_id) and the pinned unit's
// name, aliased so that ORDER BY name means the component's. The INSERT and UPDATE lists never name the pin:
// only SetComponentOwningUnitQ writes it.
const componentCols = `id, owner, name, definition, version, created_by, created_at, updated_at, ` +
	`owning_unit_id, (SELECT u.name FROM governance_units u WHERE u.id = components.owning_unit_id) AS owning_unit_name`

// CreateComponent — see ComponentStore.
func (s PG) CreateComponent(ctx context.Context, c types.Component) (types.Component, error) {
	return createComponentQ(ctx, s.Pool, c)
}

// createComponentQ is CreateComponent's statement on q, so a caller's transaction can hold it.
func createComponentQ(ctx context.Context, q Querier, c types.Component) (types.Component, error) {
	if c.ID == uuid.Nil {
		return types.Component{}, errors.New("store: create component: the caller chooses the id")
	}
	def, err := marshalComponentDefinition(c.Definition)
	if err != nil {
		return types.Component{}, err
	}
	const stmt = `
		INSERT INTO components (id, owner, name, definition, version, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, $5, now(), now())
		RETURNING ` + componentCols
	return scanComponent(q.QueryRow(ctx, stmt, c.ID, c.Owner, c.Name, def, c.CreatedBy))
}

// ErrComponentCap is CreateComponentCapped's answer when the owner already holds the cap.
var ErrComponentCap = errors.New("store: the owner already holds the most components allowed")

// CappedComponentCreator creates a component only while its owner holds fewer than max rows, the count
// and the insert under one lock, so concurrent saves cannot pass the cap. Optional beside ComponentStore.
type CappedComponentCreator interface {
	// CreateComponentCapped is CreateComponent, refused with ErrComponentCap when c.Owner already has max rows.
	CreateComponentCapped(ctx context.Context, c types.Component, max int) (types.Component, error)
}

var _ CappedComponentCreator = PG{}

// CreateComponentCapped — see CappedComponentCreator.
func (s PG) CreateComponentCapped(ctx context.Context, c types.Component, max int) (types.Component, error) {
	var out types.Component
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('components.create:' || $1))`, c.Owner); err != nil {
			return err
		}
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM components WHERE owner = $1`, c.Owner).Scan(&n); err != nil {
			return fmt.Errorf("store: count components: %w", err)
		}
		if n >= max {
			return ErrComponentCap
		}
		var err error
		out, err = createComponentQ(ctx, q, c)
		return err
	})
	if err != nil {
		return types.Component{}, err
	}
	return out, nil
}

// ErrComponentStale is UpdateComponent's answer when the row is not at the version the caller read.
var ErrComponentStale = errors.New("store: the component changed since the caller read it")

// UpdateComponent — see ComponentStore.
func (s PG) UpdateComponent(ctx context.Context, c types.Component, expected int) (types.Component, error) {
	def, err := marshalComponentDefinition(c.Definition)
	if err != nil {
		return types.Component{}, err
	}
	const q = `
		UPDATE components SET name = $3, definition = $4, version = version + 1, updated_at = now()
		WHERE id = $1 AND owner = $2 AND ($5::integer = 0 OR version = $5::integer)
		RETURNING ` + componentCols
	out, err := scanComponent(s.Pool.QueryRow(ctx, q, c.ID, c.Owner, c.Name, def, expected))
	if errors.Is(err, ErrNotFound) && expected != 0 {
		return types.Component{}, ErrComponentStale
	}
	return out, err
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

// DeleteComponentsByOwner — see ComponentStore. It locks the rows in id order, the order a snapshot
// takes its share locks in (lockSavedComponents), so the two never deadlock.
func (s PG) DeleteComponentsByOwner(ctx context.Context, owner string) (int, error) {
	return s.eraseComponentRowsOf(ctx, `DELETE FROM components WHERE id IN
		(SELECT id FROM components WHERE owner = $1 ORDER BY id FOR UPDATE)`, owner)
}

// EraseRunComponentsByOwner — see ComponentStore. It clears content and keeps
// the row: the row is the run's authorization tombstone.
func (s PG) EraseRunComponentsByOwner(ctx context.Context, owner string) (int, error) {
	return s.eraseComponentRowsOf(ctx, `UPDATE run_components
		SET owner = NULL, name = NULL, version = NULL, definition = NULL, component_id = NULL
		WHERE owner = $1`, owner)
}

// EraseRunComponentsOfRun — see ComponentStore. The same clear as
// EraseRunComponentsByOwner, on one run.
func (s PG) EraseRunComponentsOfRun(ctx context.Context, runID uuid.UUID, owner string) (int, error) {
	return s.eraseComponentRowsOf(ctx, `UPDATE run_components
		SET owner = NULL, name = NULL, version = NULL, definition = NULL, component_id = NULL
		WHERE owner = $1 AND run_id = $2`, owner, runID)
}

// eraseComponentRowsOf runs one person's erasure statement. Owner "" is the
// organisation's rows, which no person's erasure may take.
func (s PG) eraseComponentRowsOf(ctx context.Context, stmt, owner string, args ...any) (int, error) {
	if owner == "" {
		return 0, errors.New("store: erase components by owner: an owner is required")
	}
	tag, err := s.Pool.Exec(ctx, stmt, append([]any{owner}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("store: erase components by owner: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PutRunComponents — see ComponentStore.
func (s PG) PutRunComponents(ctx context.Context, runID uuid.UUID, comps []types.RunComponent) error {
	if len(comps) == 0 {
		return errors.New("store: put run components: no components; a run without any writes no snapshot")
	}
	err := s.inTx(ctx, func(q Querier) error { return insertRunComponents(ctx, q, runID, comps) })
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

// ErrRunComponentGone refuses a run's component snapshot that copies a person's saved component which
// is no longer stored: their components were erased, or they deleted it, after it was read for the run.
var ErrRunComponentGone = errors.New("store: a saved component the run copies was erased or deleted")

// insertRunComponents writes comps as runID's snapshot rows on q, then holds the saved components they
// copy (lockSavedComponents) to the transaction's commit.
func insertRunComponents(ctx context.Context, q Querier, runID uuid.UUID, comps []types.RunComponent) error {
	const ins = `
		INSERT INTO run_components (run_id, ordinal, component_id, owner, name, version, definition, self_defined, mcp_harnesses)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	for i, c := range comps {
		def, err := marshalComponentDefinition(c.Definition)
		if err != nil {
			return err
		}
		harnesses := c.MCPHarnesses
		if harnesses == nil {
			harnesses = []string{}
		}
		if _, err := q.Exec(ctx, ins, runID, i, c.ComponentID, c.Owner, c.Name, c.Version, def, c.SelfDefined, harnesses); err != nil {
			return err
		}
	}
	return lockSavedComponents(ctx, q, comps)
}

// lockSavedComponents takes a share lock, in id order, on every saved component a person's row of comps
// copies, and refuses with ErrRunComponentGone when one is no longer stored. The lock is held to the
// snapshot's commit, so an erasure of the person's components either deleted the row first, and the copy
// is never written, or waits for the commit and then clears the copy (EraseRunComponentsByOwner): the
// copy never outlives the erasure, whatever happens to this process after the commit.
func lockSavedComponents(ctx context.Context, q Querier, comps []types.RunComponent) error {
	want := map[uuid.UUID]bool{}
	for _, c := range comps {
		if c.SelfDefined && c.ComponentID != nil && c.Owner != "" {
			want[*c.ComponentID] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	rows, err := q.Query(ctx, `SELECT id FROM components WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, ids)
	if err != nil {
		return fmt.Errorf("store: lock the run's saved components: %w", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("store: lock the run's saved components: %w", err)
	}
	if len(got) != len(ids) {
		return ErrRunComponentGone
	}
	return nil
}

// ListRunComponents — see ComponentStore.
func (s PG) ListRunComponents(ctx context.Context, runID uuid.UUID) ([]types.RunComponent, error) {
	const q = `SELECT run_id, ordinal, self_defined, component_id, owner, name, version, definition, mcp_harnesses
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
	var owningUnitName *string
	err := row.Scan(&c.ID, &c.Owner, &c.Name, &def, &c.Version, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		&c.OwningUnitID, &owningUnitName)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return types.Component{}, ErrNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return types.Component{}, ErrConflict
	case err != nil:
		return types.Component{}, fmt.Errorf("store: scan component: %w", err)
	}
	if owningUnitName != nil {
		c.OwningUnitName = *owningUnitName
	}
	if err := json.Unmarshal(def, &c.Definition); err != nil {
		return types.Component{}, fmt.Errorf("store: unmarshal component definition: %w", err)
	}
	return c, nil
}

// ComponentOwningUnitQ is organisation component id's unit pin on q (nil when unpinned).
// ErrNotFound for an unknown id or a person's row.
func ComponentOwningUnitQ(ctx context.Context, q Querier, id uuid.UUID) (*uuid.UUID, error) {
	var unit *uuid.UUID
	err := q.QueryRow(ctx, `SELECT owning_unit_id FROM components WHERE id=$1 AND owner=''`, id).Scan(&unit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read component owning unit: %w", err)
	}
	return unit, nil
}

// SetComponentOwningUnitQ pins organisation component id to unit on q, or unpins it when unit is nil:
// the pin's only writer. The version does not move, since a pin is not a definition change.
// ErrNotFound for an unknown id, a person's row or an unknown unit.
func SetComponentOwningUnitQ(ctx context.Context, q Querier, id uuid.UUID, unit *uuid.UUID) error {
	tag, err := q.Exec(ctx, `UPDATE components SET owning_unit_id=$1, updated_at=now() WHERE id=$2 AND owner=''`, unit, id)
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: set component owning unit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanRunComponent(row pgx.Row) (types.RunComponent, error) {
	var c types.RunComponent
	var owner, name *string
	var version *int
	var def []byte
	if err := row.Scan(&c.RunID, &c.Ordinal, &c.SelfDefined, &c.ComponentID, &owner, &name, &version, &def, &c.MCPHarnesses); err != nil {
		return types.RunComponent{}, fmt.Errorf("store: scan run component: %w", err)
	}
	if len(c.MCPHarnesses) == 0 {
		c.MCPHarnesses = nil
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
