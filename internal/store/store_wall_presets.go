// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Wall presets (migration 0248_wall_presets); see WallPresetStore (iface.go). The spec is validated at
// the API and stored as given. The built-ins are constants in internal/types, never rows.

const wallPresetCols = `id, scope, owner_id, group_id, name, description, spec, version, updated_at, updated_by`

func wallPresetAudienceKey(o types.TemplateOwner) string {
	return "wall_presets.audience\x1f" + string(o.Scope) + "\x1f" + o.Person + "\x1f" + o.Group
}

// ListWallPresetsVisible — see WallPresetStore.
func (s PG) ListWallPresetsVisible(ctx context.Context, person string, groups []string) ([]types.WallPreset, error) {
	if groups == nil {
		groups = []string{}
	}
	const q = `SELECT ` + wallPresetCols + ` FROM wall_presets
		WHERE scope = 'org' OR (scope = 'person' AND owner_id = $1) OR (scope = 'group' AND group_id = ANY($2))
		ORDER BY lower(name), id`
	return collect(ctx, s.Pool, "list", "wall presets", q, []any{person, groups}, scanWallPreset)
}

// GetWallPreset — see WallPresetStore.
func (s PG) GetWallPreset(ctx context.Context, id uuid.UUID) (types.WallPreset, error) {
	return scanWallPreset(s.Pool.QueryRow(ctx, `SELECT `+wallPresetCols+` FROM wall_presets WHERE id = $1`, id))
}

// CreateWallPreset — see WallPresetStore.
func (s PG) CreateWallPreset(ctx context.Context, p types.WallPreset, grant *TemplateGrantCheck) (types.WallPreset, error) {
	id, err := uuid.Parse(p.ID)
	owner := p.Owner()
	if err != nil || !owner.Valid() || p.UpdatedBy == "" {
		return types.WallPreset{}, errors.New("store: create wall preset: a uuid, a valid owner and a writer are required")
	}
	spec, err := json.Marshal(p.Spec)
	if err != nil {
		return types.WallPreset{}, fmt.Errorf("store: create wall preset: %w", err)
	}
	var out types.WallPreset
	err = s.inTx(ctx, func(q Querier) error {
		if err := checkTemplateGrant(ctx, q, grant); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, templateAudienceLockSQL, wallPresetAudienceKey(owner)); err != nil {
			return err
		}
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM wall_presets
			WHERE scope = $1 AND COALESCE(owner_id, '') = $2 AND COALESCE(group_id, '') = $3`,
			owner.Scope, owner.Person, owner.Group).Scan(&n); err != nil {
			return err
		}
		if limit := TemplateLimit(owner); n >= limit {
			return &TemplateLimitError{Limit: limit}
		}
		var err error
		out, err = scanWallPreset(q.QueryRow(ctx, `INSERT INTO wall_presets
			(id, scope, owner_id, group_id, name, description, spec, version, created_at, created_by, updated_at, updated_by)
			VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, 1, now(), $8, now(), $8)
			RETURNING `+wallPresetCols, id, owner.Scope, owner.Person, owner.Group, p.Name, p.Description, spec, p.UpdatedBy))
		return err
	})
	if err != nil {
		return types.WallPreset{}, wallPresetWriteErr("create", err)
	}
	return out, nil
}

// UpdateWallPreset — see WallPresetStore.
func (s PG) UpdateWallPreset(ctx context.Context, p types.WallPreset, expected int, grant *TemplateGrantCheck) (types.WallPreset, error) {
	id, err := uuid.Parse(p.ID)
	if err != nil || p.UpdatedBy == "" {
		return types.WallPreset{}, errors.New("store: update wall preset: a uuid and a writer are required")
	}
	spec, err := json.Marshal(p.Spec)
	if err != nil {
		return types.WallPreset{}, fmt.Errorf("store: update wall preset: %w", err)
	}
	var out types.WallPreset
	err = s.inTx(ctx, func(q Querier) error {
		if err := checkTemplateGrant(ctx, q, grant); err != nil {
			return err
		}
		var err error
		out, err = scanWallPreset(q.QueryRow(ctx, `UPDATE wall_presets
			SET name = $2, description = $3, spec = $4, version = version + 1, updated_at = now(), updated_by = $5
			WHERE id = $1 AND version = $6
			RETURNING `+wallPresetCols, id, p.Name, p.Description, spec, p.UpdatedBy, expected))
		if errors.Is(err, ErrNotFound) {
			var current int
			if err := q.QueryRow(ctx, `SELECT version FROM wall_presets WHERE id = $1`, id).Scan(&current); err != nil {
				return notFoundOr(err)
			}
			return &TemplateRevisionConflictError{Current: current}
		}
		return err
	})
	if err != nil {
		return types.WallPreset{}, wallPresetWriteErr("update", err)
	}
	return out, nil
}

// DeleteWallPreset — see WallPresetStore.
func (s PG) DeleteWallPreset(ctx context.Context, id uuid.UUID, grant *TemplateGrantCheck) (types.WallPreset, error) {
	var out types.WallPreset
	err := s.inTx(ctx, func(q Querier) error {
		if err := checkTemplateGrant(ctx, q, grant); err != nil {
			return err
		}
		var err error
		out, err = scanWallPreset(q.QueryRow(ctx, `DELETE FROM wall_presets WHERE id = $1 RETURNING `+wallPresetCols, id))
		return err
	})
	if err != nil {
		return types.WallPreset{}, wallPresetWriteErr("delete", err)
	}
	return out, nil
}

// wallPresetWriteErr keeps the store's own errors and names a unique-index violation as the name it is.
func wallPresetWriteErr(verb string, err error) error {
	var pgErr *pgconn.PgError
	var limit *TemplateLimitError
	var conflict *TemplateRevisionConflictError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return ErrTemplateNameTaken
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrTemplateGrantGone), errors.As(err, &limit), errors.As(err, &conflict):
		return err
	}
	return fmt.Errorf("store: %s wall preset: %w", verb, err)
}

// scanWallPreset reads one row. A spec this server cannot read whole is an error, never a narrower
// or empty preset: dropping a knob it does not know would widen every run launched with it.
func scanWallPreset(row pgx.Row) (types.WallPreset, error) {
	var p types.WallPreset
	var id uuid.UUID
	var owner, group *string
	var spec []byte
	var at time.Time
	err := row.Scan(&id, &p.Scope, &owner, &group, &p.Name, &p.Description, &spec, &p.Version, &at, &p.UpdatedBy)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return types.WallPreset{}, ErrNotFound
	case err != nil:
		return types.WallPreset{}, err
	}
	p.ID, p.UpdatedAt = id.String(), &at
	if owner != nil {
		p.OwnerID = *owner
	}
	if group != nil {
		p.GroupID = *group
	}
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.Spec); err != nil {
		return types.WallPreset{}, fmt.Errorf("store: read wall preset %s: %w", p.ID, err)
	}
	return p, nil
}
