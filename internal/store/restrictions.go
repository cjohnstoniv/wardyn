// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RestrictionStore keeps the restrictions (migration 0210). A side interface, as BoundaryStore is: a
// store that does not implement it holds none, so every run resolves as before.
type RestrictionStore interface {
	ListRestrictions(ctx context.Context) ([]types.Restriction, error)
	GetRestriction(ctx context.Context, id uuid.UUID) (types.Restriction, error)
	// PutRestriction writes r (members included, replacing the set) in one transaction. ErrConflict when
	// another restriction holds the name.
	PutRestriction(ctx context.Context, r types.Restriction, by string) (types.Restriction, error)
	// DeleteRestriction removes the restriction and its members and returns the row it removed.
	DeleteRestriction(ctx context.Context, id uuid.UUID) (types.Restriction, error)
	// ListRestrictionsFor is every restriction covering a caller, in name order: userSubjects and groups
	// are matched as ListMembershipsFor matches them, userType is the caller's one type.
	ListRestrictionsFor(ctx context.Context, userSubjects, groups []string, userType string) ([]types.Restriction, error)
	// HasGroupTierRestrictions gates the stale-snapshot refusal, as HasGroupTierMemberships does.
	HasGroupTierRestrictions(ctx context.Context) (bool, error)
	CountRestrictions(ctx context.Context) (int, error)
	// GetRestrictionsByID is the current restriction of each id that still exists, in name order.
	GetRestrictionsByID(ctx context.Context, ids []uuid.UUID) ([]types.Restriction, error)
}

const restrictionCols = `id, name, spec, limits, contact, created_by, created_at, updated_at`

func (s PG) ListRestrictions(ctx context.Context) ([]types.Restriction, error) {
	return ListRestrictionsQ(ctx, s.Pool)
}

func (s PG) GetRestriction(ctx context.Context, id uuid.UUID) (types.Restriction, error) {
	return GetRestrictionQ(ctx, s.Pool, id)
}

func (s PG) PutRestriction(ctx context.Context, r types.Restriction, by string) (types.Restriction, error) {
	var out types.Restriction
	err := s.inTx(ctx, func(q Querier) error {
		var err error
		out, err = PutRestrictionQ(ctx, q, r, by)
		return err
	})
	return out, err
}

func (s PG) DeleteRestriction(ctx context.Context, id uuid.UUID) (types.Restriction, error) {
	var out types.Restriction
	err := s.inTx(ctx, func(q Querier) error {
		var err error
		out, err = DeleteRestrictionQ(ctx, q, id)
		return err
	})
	return out, err
}

func (s PG) ListRestrictionsFor(ctx context.Context, userSubjects, groups []string, userType string) ([]types.Restriction, error) {
	if userSubjects == nil {
		userSubjects = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	return queryRestrictions(ctx, s.Pool, `SELECT `+restrictionCols+` FROM governance_restrictions r
		WHERE EXISTS (SELECT 1 FROM governance_restriction_members m WHERE m.restriction_id = r.id AND `+subjectMatch("m")+`)
		ORDER BY r.name`, userSubjects, groups, userType)
}

func (s PG) HasGroupTierRestrictions(ctx context.Context) (bool, error) {
	return s.hasGroupTierRows(ctx, "governance_restriction_members", "restrictions")
}

func (s PG) CountRestrictions(ctx context.Context) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM governance_restrictions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count restrictions: %w", err)
	}
	return n, nil
}

func (s PG) GetRestrictionsByID(ctx context.Context, ids []uuid.UUID) ([]types.Restriction, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return queryRestrictions(ctx, s.Pool, `SELECT `+restrictionCols+` FROM governance_restrictions WHERE id = ANY($1) ORDER BY name`, ids)
}

// ListRestrictionsQ is ListRestrictions on q.
func ListRestrictionsQ(ctx context.Context, q Querier) ([]types.Restriction, error) {
	return queryRestrictions(ctx, q, `SELECT `+restrictionCols+` FROM governance_restrictions ORDER BY name`)
}

// GetRestrictionQ is GetRestriction on q. ErrNotFound when none.
func GetRestrictionQ(ctx context.Context, q Querier, id uuid.UUID) (types.Restriction, error) {
	rs, err := queryRestrictions(ctx, q, `SELECT `+restrictionCols+` FROM governance_restrictions WHERE id = $1`, id)
	if err != nil {
		return types.Restriction{}, err
	}
	if len(rs) == 0 {
		return types.Restriction{}, ErrNotFound
	}
	return rs[0], nil
}

// PutRestrictionQ writes r on q: the row, then its members replaced. ErrConflict when the name is taken.
func PutRestrictionQ(ctx context.Context, q Querier, r types.Restriction, by string) (types.Restriction, error) {
	spec, err := json.Marshal(r.Spec)
	if err != nil {
		return types.Restriction{}, fmt.Errorf("store: marshal restriction spec: %w", err)
	}
	limits, err := json.Marshal(r.Limits)
	if err != nil {
		return types.Restriction{}, fmt.Errorf("store: marshal restriction limits: %w", err)
	}
	var contact []byte
	if r.Contact != nil {
		if contact, err = json.Marshal(r.Contact); err != nil {
			return types.Restriction{}, fmt.Errorf("store: marshal restriction contact: %w", err)
		}
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO governance_restrictions (id, name, spec, limits, contact, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name, spec = EXCLUDED.spec, limits = EXCLUDED.limits, contact = EXCLUDED.contact,
			    updated_at = now()`, r.ID, r.Name, spec, limits, contact, by); err != nil {
		if isUniqueViolation(err) {
			return types.Restriction{}, ErrConflict
		}
		return types.Restriction{}, fmt.Errorf("store: write restriction: %w", err)
	}
	if _, err := q.Exec(ctx, `DELETE FROM governance_restriction_members WHERE restriction_id = $1`, r.ID); err != nil {
		return types.Restriction{}, fmt.Errorf("store: replace restriction members: %w", err)
	}
	for _, m := range r.Members {
		if _, err := q.Exec(ctx, `
			INSERT INTO governance_restriction_members (id, restriction_id, subject_type, subject, created_by)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (restriction_id, subject_type, subject) DO NOTHING`,
			uuid.New(), r.ID, string(m.SubjectType), m.Subject, by); err != nil {
			return types.Restriction{}, fmt.Errorf("store: write restriction member: %w", err)
		}
	}
	return GetRestrictionQ(ctx, q, r.ID)
}

// DeleteRestrictionQ removes restriction id on q (its members cascade) and returns the row removed.
// ErrNotFound when none.
func DeleteRestrictionQ(ctx context.Context, q Querier, id uuid.UUID) (types.Restriction, error) {
	row, err := GetRestrictionQ(ctx, q, id)
	if err != nil {
		return types.Restriction{}, err
	}
	if _, err := q.Exec(ctx, `DELETE FROM governance_restrictions WHERE id = $1`, id); err != nil {
		return types.Restriction{}, fmt.Errorf("store: delete restriction: %w", err)
	}
	return row, nil
}

// queryRestrictions runs a restriction select (restrictionCols, in order) and joins each row's members.
// A stored spec or limits that does not decode is an error: a restriction is authority, so the resolve
// fails closed rather than read it as narrower than it is.
func queryRestrictions(ctx context.Context, q Querier, stmt string, args ...any) ([]types.Restriction, error) {
	rows, err := q.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list restrictions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (types.Restriction, error) {
		var x types.Restriction
		var spec, limits, contact []byte
		if err := r.Scan(&x.ID, &x.Name, &spec, &limits, &contact, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return x, err
		}
		if err := decodeStrictJSON(spec, &x.Spec); err != nil {
			return x, fmt.Errorf("decode restriction %s spec: %w", x.ID, err)
		}
		if err := decodeStrictJSON(limits, &x.Limits); err != nil {
			return x, fmt.Errorf("decode restriction %s limits: %w", x.ID, err)
		}
		if contact != nil {
			if err := json.Unmarshal(contact, &x.Contact); err != nil {
				return x, fmt.Errorf("decode restriction %s contact: %w", x.ID, err)
			}
		}
		x.Members = []types.RestrictionMember{}
		return x, nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: read restrictions: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(out))
	at := make(map[uuid.UUID]int, len(out))
	for i, x := range out {
		ids[i], at[x.ID] = x.ID, i
	}
	mrows, err := q.Query(ctx, `SELECT restriction_id, subject_type, subject FROM governance_restriction_members
		WHERE restriction_id = ANY($1) ORDER BY restriction_id, subject_type, subject`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list restriction members: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var id uuid.UUID
		var m types.RestrictionMember
		if err := mrows.Scan(&id, &m.SubjectType, &m.Subject); err != nil {
			return nil, fmt.Errorf("store: scan restriction member: %w", err)
		}
		out[at[id]].Members = append(out[at[id]].Members, m)
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate restriction members: %w", err)
	}
	return out, nil
}

func decodeStrictJSON(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// SetWorkspaceOwningUnit pins workspace id to unit, or unpins it when unit is nil. The column's only
// writer: UpdateWorkspace never touches it. ErrNotFound when the workspace or the unit is unknown.
func (s PG) SetWorkspaceOwningUnit(ctx context.Context, id uuid.UUID, unit *uuid.UUID) (types.Workspace, error) {
	out, err := s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET owning_unit_id=$1, updated_at=now() WHERE id=$2 RETURNING `+wsReadCols, unit, id))
	if isForeignKeyViolation(err) {
		return types.Workspace{}, ErrNotFound
	}
	return out, err
}

func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

// SetWorkspaceOwningUnitQ is the pin write on q, for a held change's approval. ErrNotFound for an
// unknown workspace or unit.
func SetWorkspaceOwningUnitQ(ctx context.Context, q Querier, id uuid.UUID, unit *uuid.UUID) error {
	tag, err := q.Exec(ctx, `UPDATE workspaces SET owning_unit_id=$1, updated_at=now() WHERE id=$2`, unit, id)
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: set workspace owning unit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RunRestrictionSetter records the restrictions a revive holds a run to, a side interface like
// RunLimitsReclamper so a store without it keeps the ids the run was created with.
type RunRestrictionSetter interface {
	SetRunRestrictionIDs(ctx context.Context, id uuid.UUID, ids []uuid.UUID) error
}

var _ RunRestrictionSetter = PG{}

// SetRunRestrictionIDs replaces the run's restriction_ids. ErrNotFound for an unknown run.
func (s PG) SetRunRestrictionIDs(ctx context.Context, id uuid.UUID, ids []uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET restriction_ids=$1 WHERE id=$2`, restrictionIDsParam(ids), id)
	if err != nil {
		return fmt.Errorf("store: set run restriction ids: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// WorkspaceOwningUnitQ is workspace id's pin on q (nil when unpinned), for a held change's approval to
// compare. ErrNotFound for an unknown workspace.
func WorkspaceOwningUnitQ(ctx context.Context, q Querier, id uuid.UUID) (*uuid.UUID, error) {
	var unit *uuid.UUID
	err := q.QueryRow(ctx, `SELECT owning_unit_id FROM workspaces WHERE id=$1`, id).Scan(&unit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read workspace owning unit: %w", err)
	}
	return unit, nil
}
