// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance profiles and their subject assignments (migration 0052).
//
// This file is DUMB on purpose: it round-trips rows and owns exactly one
// piece of logic, ResolveGovernanceProfile's ORDER BY, so precedence is a
// property of the single indexed read, not a caller that could forget it.
// Every write is validated at the API boundary (internal/api/governance.go).
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

const governanceProfileCols = `id, name, ceiling, limits, created_at, updated_at, created_by`

const governanceAssignmentCols = `id, subject_type, subject, profile_id, priority, created_at, created_by`

// UpsertGovernanceProfile writes one profile, keyed on its PRIMARY KEY: an
// unminted id INSERTs, an existing one UPDATEs in place (name included, so a
// profile can be renamed while assignments still point at it — ON DELETE
// RESTRICT makes delete-and-recreate impossible for an assigned profile).
// One statement serves both write routes: POST always inserts (fresh id),
// PUT always updates (id from the path).
//
// Returns ErrConflict when UNIQUE(name) rejects the write. created_by and
// created_at are NOT touched on update: creation provenance stays with
// whoever authored the profile.
func (s PG) UpsertGovernanceProfile(ctx context.Context, p types.GovernanceProfile) (types.GovernanceProfile, error) {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	ceilingJSON, err := json.Marshal(p.Ceiling)
	if err != nil {
		return types.GovernanceProfile{}, fmt.Errorf("store: marshal governance ceiling: %w", err)
	}
	limitsJSON, err := json.Marshal(p.Limits)
	if err != nil {
		return types.GovernanceProfile{}, fmt.Errorf("store: marshal governance limits: %w", err)
	}
	const q = `
		INSERT INTO governance_profiles (id, name, ceiling, limits, created_by)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name,
			    ceiling = (governance_profiles.ceiling - $6::text[]) || EXCLUDED.ceiling,
			    limits = (governance_profiles.limits - $7::text[]) || EXCLUDED.limits, updated_at = now()
		RETURNING ` + governanceProfileCols
	// ceiling and limits each keep the keys this binary's types don't
	// declare, so a field a newer wardynd set survives this binary's edit.
	out, err := scanGovernanceProfile(s.Pool.QueryRow(ctx, q,
		p.ID, p.Name, ceilingJSON, limitsJSON, p.CreatedBy, declaredJSONKeys(p.Ceiling), declaredJSONKeys(p.Limits)))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.GovernanceProfile{}, ErrConflict
		}
		return types.GovernanceProfile{}, err
	}
	return out, nil
}

// GetGovernanceProfile returns one profile by id, or ErrNotFound.
func (s PG) GetGovernanceProfile(ctx context.Context, id uuid.UUID) (types.GovernanceProfile, error) {
	const q = `SELECT ` + governanceProfileCols + ` FROM governance_profiles WHERE id = $1`
	return scanGovernanceProfile(s.Pool.QueryRow(ctx, q, id))
}

// DeleteGovernanceProfile removes one profile by id. ErrNotFound when no row
// matched.
//
// ErrConflict when the row is still ASSIGNED: governance_assignments'
// ON DELETE RESTRICT refuses (23503) rather than cascading, since cascading
// would silently widen every assigned member back to the deployment ceiling.
// Translated into a sentinel so the route answers a caller-fixable 409
// instead of a 500.
func (s PG) DeleteGovernanceProfile(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM governance_profiles WHERE id = $1`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ErrConflict
		}
		return fmt.Errorf("store: delete governance profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListGovernanceProfiles returns every profile by name (the handle an admin
// looks for), unlike the creation order other List* calls use. Deployment-
// wide config, so there's no per-caller scoping to filter on.
func (s PG) ListGovernanceProfiles(ctx context.Context) ([]types.GovernanceProfile, error) {
	const q = `SELECT ` + governanceProfileCols + ` FROM governance_profiles ORDER BY name`
	return collect(ctx, s.Pool, "list", "governance profiles", q, nil, scanGovernanceProfile)
}

// UpsertGovernanceAssignment binds one subject to one profile, keyed on the
// natural UNIQUE (subject_type, subject): re-assigning a subject REPOINTS its
// single row rather than leaving a second one behind (two rows for one
// subject would make "which profile does Bob get" a tie-break instead of the
// admin's last write).
//
// The returned row carries the EXISTING id on conflict, not a.ID, so the
// caller gets the id the DELETE route needs.
//
// Returns ErrNotFound when profile_id names no profile (FK 23503 → 404).
func (s PG) UpsertGovernanceAssignment(ctx context.Context, a types.GovernanceAssignment) (types.GovernanceAssignment, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	const q = `
		INSERT INTO governance_assignments (id, subject_type, subject, profile_id, priority, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (subject_type, subject) DO UPDATE
			SET profile_id = EXCLUDED.profile_id, priority = EXCLUDED.priority
		RETURNING ` + governanceAssignmentCols
	out, err := scanGovernanceAssignment(s.Pool.QueryRow(ctx, q,
		a.ID, a.SubjectType, a.Subject, a.ProfileID, a.Priority, a.CreatedBy))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return types.GovernanceAssignment{}, ErrNotFound
		}
		return types.GovernanceAssignment{}, err
	}
	return out, nil
}

// DeleteGovernanceAssignment removes one assignment by id, ErrNotFound when
// no row matched. Unassigning is the SUPPORTED way to widen someone back to
// the deployment ceiling.
func (s PG) DeleteGovernanceAssignment(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM governance_assignments WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete governance assignment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListGovernanceAssignments returns every assignment in the order the
// resolver itself would rank them (tier, then priority, then subject), so
// the console's table reads top-down as the precedence rule.
func (s PG) ListGovernanceAssignments(ctx context.Context) ([]types.GovernanceAssignment, error) {
	const q = `SELECT ` + governanceAssignmentCols + ` FROM governance_assignments
		ORDER BY ` + subjectTierOrder + `, priority DESC, subject`
	return collect(ctx, s.Pool, "list", "governance assignments", q, nil, scanGovernanceAssignment)
}

// ResolveGovernanceProfile returns THE ONE profile that applies to a caller, or
// ErrNotFound when no assignment matches (which the caller reads as "fall
// through to the deployment ceiling", the absent-row back-compat rule).
//
// The whole precedence rule is the ORDER BY, deliberately: one indexed read
// on the UNIQUE(subject_type, subject) btree, so there's no second
// implementation in Go to skip, mis-order, or forget. subjectMatch and
// subjectPrecedence are the same fragments ResolveUserDrive splices, so the
// ceiling and the drive can't disagree about "most specific".
//
// SECURITY: users/groups are normalized from nil to empty for the same reason
// ListCapabilityGrantsFor does: a nil Go slice binds as SQL NULL and
// `x = ANY(NULL)` is NULL, not false. Fails closed either way, but a
// predicate whose behavior hinges on a driver detail has no place at an
// authorization boundary.
//
// userType is the caller's one type id; "" matches no row.
func (s PG) ResolveGovernanceProfile(ctx context.Context, userSubjects, groups []string, userType string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	if userSubjects == nil {
		userSubjects = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	q := `SELECT p.id, p.name, p.ceiling, p.limits, p.created_at, p.updated_at, p.created_by, a.subject_type
		FROM governance_assignments a
		JOIN governance_profiles p ON p.id = a.profile_id
		WHERE ` + subjectMatch("a") + `
		ORDER BY ` + subjectPrecedence("a", "p") + `
		LIMIT 1`
	var tier string
	p, err := scanGovernanceProfileInto(s.Pool.QueryRow(ctx, q, userSubjects, groups, userType), &tier)
	if err != nil {
		return nil, "", err
	}
	return &p, types.CapabilitySubjectType(tier), nil
}

// HasGroupTierAssignments reports whether ANY group-tier assignment exists:
// the gate on the stale-snapshot refusal (see hasGroupTierRows).
func (s PG) HasGroupTierAssignments(ctx context.Context) (bool, error) {
	return s.hasGroupTierRows(ctx, "governance_assignments", "governance assignments")
}

func scanGovernanceProfile(row pgx.Row) (types.GovernanceProfile, error) {
	return scanGovernanceProfileInto(row, nil)
}

// scanGovernanceProfileInto is scanGovernanceProfile plus the ONE extra
// column the resolver selects (subject_type), taken as a parameter rather
// than a second scan function so the ceiling/limits unmarshal never forks.
func scanGovernanceProfileInto(row pgx.Row, tier *string) (types.GovernanceProfile, error) {
	var p types.GovernanceProfile
	var ceilingRaw, limitsRaw []byte
	dest := []any{&p.ID, &p.Name, &ceilingRaw, &limitsRaw,
		&p.CreatedAt, &p.UpdatedAt, &p.CreatedBy}
	if tier != nil {
		dest = append(dest, tier)
	}
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.GovernanceProfile{}, ErrNotFound
	}
	if err != nil {
		return types.GovernanceProfile{}, fmt.Errorf("store: scan governance profile: %w", err)
	}
	if err := json.Unmarshal(ceilingRaw, &p.Ceiling); err != nil {
		return types.GovernanceProfile{}, fmt.Errorf("store: unmarshal governance ceiling: %w", err)
	}
	if err := json.Unmarshal(limitsRaw, &p.Limits); err != nil {
		return types.GovernanceProfile{}, fmt.Errorf("store: unmarshal governance limits: %w", err)
	}
	return p, nil
}

func scanGovernanceAssignment(row pgx.Row) (types.GovernanceAssignment, error) {
	var a types.GovernanceAssignment
	err := row.Scan(&a.ID, &a.SubjectType, &a.Subject, &a.ProfileID,
		&a.Priority, &a.CreatedAt, &a.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.GovernanceAssignment{}, ErrNotFound
	}
	if err != nil {
		return types.GovernanceAssignment{}, fmt.Errorf("store: scan governance assignment: %w", err)
	}
	return a, nil
}
