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
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const governanceProfileCols = `id, name, ceiling, limits, created_at, updated_at, created_by, contact, base_profile_id, overlay, overlay_limits`

// governanceProfileColsP is governanceProfileCols qualified for a join or a recursive term.
const governanceProfileColsP = `p.id, p.name, p.ceiling, p.limits, p.created_at, p.updated_at, p.created_by, p.contact, p.base_profile_id, p.overlay, p.overlay_limits`

// GovernanceProfileBuild produces the row a graph write stores, given every profile as the write
// transaction sees it under the graph lock. A refusal it returns aborts the write unchanged.
type GovernanceProfileBuild func(all []types.GovernanceProfile) (types.GovernanceProfile, error)

// ErrProfileHasChildren is DeleteGovernanceProfile's refusal when other profiles still compose on
// the row: Names are those children, sorted.
type ErrProfileHasChildren struct{ Names []string }

func (e *ErrProfileHasChildren) Error() string {
	return "store: governance profile is the base of: " + strings.Join(e.Names, ", ")
}

const governanceAssignmentCols = `id, subject_type, subject, profile_id, priority, created_at, created_by`

// UpsertGovernanceProfile writes one profile exactly as given. It is WriteGovernanceProfile with
// nothing to decide, so it takes the same graph lock and a write can never skip it.
func (s PG) UpsertGovernanceProfile(ctx context.Context, p types.GovernanceProfile) (types.GovernanceProfile, error) {
	return s.WriteGovernanceProfile(ctx, p.ID, func([]types.GovernanceProfile) (types.GovernanceProfile, error) { return p, nil })
}

// WriteGovernanceProfile writes one profile keyed on its PRIMARY KEY: an unminted id INSERTs, an
// existing one UPDATEs in place (name included, so a profile can be renamed while assignments
// still point at it, since ON DELETE RESTRICT makes delete-and-recreate impossible for an assigned
// profile). One statement serves POST (fresh id) and PUT (id from the path).
//
// The whole write is one transaction under a graph-wide advisory lock: build sees every profile as
// that transaction reads them, decides the row (merging what a PUT left absent, refusing a cycle,
// a depth overflow or an overlay its base does not permit) and the row is stored before the lock
// is released, so two concurrent writes cannot together make a cycle or a depth-4 chain.
//
// contact follows GovernanceProfile.ContactSet: written (or cleared, when empty) only when it is
// true, otherwise the stored value stays. A composed row (Overlay set) binds '{}' for ceiling and
// limits and sets them outright, since it stores no raw authority; its composition columns are
// always written from the row as given.
//
// Returns ErrConflict when UNIQUE(name) rejects the write. created_by and created_at are NOT
// touched on update: creation provenance stays with whoever authored the profile.
func (s PG) WriteGovernanceProfile(ctx context.Context, id uuid.UUID, build GovernanceProfileBuild) (types.GovernanceProfile, error) {
	if id == uuid.Nil {
		id = uuid.New()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.GovernanceProfile{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if err := lockGovernanceGraph(ctx, tx); err != nil {
		return types.GovernanceProfile{}, err
	}
	all, err := listGovernanceProfilesTx(ctx, tx)
	if err != nil {
		return types.GovernanceProfile{}, err
	}
	p, err := build(all)
	if err != nil {
		return types.GovernanceProfile{}, err
	}
	p.ID = id
	out, err := upsertGovernanceProfileTx(ctx, tx, p)
	if err != nil {
		return types.GovernanceProfile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.GovernanceProfile{}, err
	}
	return out, nil
}

func lockGovernanceGraph(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, db.GovernanceGraphLockClass); err != nil {
		return fmt.Errorf("store: lock the governance profile graph: %w", err)
	}
	return nil
}

func listGovernanceProfilesTx(ctx context.Context, tx pgx.Tx) ([]types.GovernanceProfile, error) {
	rows, err := tx.Query(ctx, `SELECT `+governanceProfileCols+` FROM governance_profiles ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list governance profiles: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (types.GovernanceProfile, error) { return scanGovernanceProfile(r) })
	if err != nil {
		return nil, fmt.Errorf("store: iterate governance profiles: %w", err)
	}
	return out, nil
}

func upsertGovernanceProfileTx(ctx context.Context, tx pgx.Tx, p types.GovernanceProfile) (types.GovernanceProfile, error) {
	// An empty contact is stored as NULL, never as an empty object.
	var contactJSON any
	if p.Contact != nil && !p.Contact.IsZero() {
		b, err := json.Marshal(p.Contact)
		if err != nil {
			return types.GovernanceProfile{}, fmt.Errorf("store: marshal governance contact: %w", err)
		}
		contactJSON = b
	}
	var q string
	var args []any
	if p.Composed() {
		overlayJSON, overlayLimitsJSON, err := marshalOverlays(p)
		if err != nil {
			return types.GovernanceProfile{}, err
		}
		const composed = `
		INSERT INTO governance_profiles (id, name, ceiling, limits, created_by, contact, base_profile_id, overlay, overlay_limits)
		VALUES ($1,$2,'{}'::jsonb,'{}'::jsonb,$3,$4,$5,$6,$7)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name,
			    ceiling = '{}'::jsonb,
			    limits = '{}'::jsonb,
			    contact = CASE WHEN $8::boolean THEN EXCLUDED.contact ELSE governance_profiles.contact END,
			    base_profile_id = EXCLUDED.base_profile_id,
			    overlay = EXCLUDED.overlay,
			    overlay_limits = EXCLUDED.overlay_limits,
			    updated_at = now()
		RETURNING ` + governanceProfileCols
		q, args = composed, []any{p.ID, p.Name, p.CreatedBy, contactJSON, p.BaseProfileID, overlayJSON, overlayLimitsJSON, p.ContactSet}
	} else {
		ceilingJSON, err := json.Marshal(p.Ceiling)
		if err != nil {
			return types.GovernanceProfile{}, fmt.Errorf("store: marshal governance ceiling: %w", err)
		}
		limitsJSON, err := json.Marshal(p.Limits)
		if err != nil {
			return types.GovernanceProfile{}, fmt.Errorf("store: marshal governance limits: %w", err)
		}
		// ceiling and limits each keep the keys this binary's types don't
		// declare, so a field a newer wardynd set survives this binary's edit.
		const standalone = `
		INSERT INTO governance_profiles (id, name, ceiling, limits, created_by, contact)
		VALUES ($1,$2,$3,$4,$5,$8)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name,
			    ceiling = (governance_profiles.ceiling - $6::text[]) || EXCLUDED.ceiling,
			    limits = (governance_profiles.limits - $7::text[]) || EXCLUDED.limits,
			    contact = CASE WHEN $9::boolean THEN EXCLUDED.contact ELSE governance_profiles.contact END,
			    base_profile_id = NULL, overlay = NULL, overlay_limits = NULL,
			    updated_at = now()
		RETURNING ` + governanceProfileCols
		q, args = standalone, []any{p.ID, p.Name, ceilingJSON, limitsJSON, p.CreatedBy,
			declaredJSONKeys(p.Ceiling), declaredJSONKeys(p.Limits), contactJSON, p.ContactSet}
	}
	out, err := scanGovernanceProfile(tx.QueryRow(ctx, q, args...))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.GovernanceProfile{}, ErrConflict
		}
		return types.GovernanceProfile{}, err
	}
	return out, nil
}

// marshalOverlays is the two overlay columns of a composed row. An empty overlay_limits is stored
// NULL, never as an empty object.
func marshalOverlays(p types.GovernanceProfile) (overlay, overlayLimits any, err error) {
	ob, err := json.Marshal(p.Overlay)
	if err != nil {
		return nil, nil, fmt.Errorf("store: marshal governance overlay: %w", err)
	}
	overlay = ob
	if p.OverlayLimits != nil {
		lb, err := json.Marshal(p.OverlayLimits)
		if err != nil {
			return nil, nil, fmt.Errorf("store: marshal governance overlay limits: %w", err)
		}
		if string(lb) != "{}" {
			overlayLimits = lb
		}
	}
	return overlay, overlayLimits, nil
}

// GetGovernanceProfile returns one profile by id, or ErrNotFound. The RAW row: a composed profile
// carries no authority here (see GetGovernanceProfileChain).
func (s PG) GetGovernanceProfile(ctx context.Context, id uuid.UUID) (types.GovernanceProfile, error) {
	const q = `SELECT ` + governanceProfileCols + ` FROM governance_profiles WHERE id = $1`
	return scanGovernanceProfile(s.Pool.QueryRow(ctx, q, id))
}

// GetGovernanceProfileChain reads one profile and its ancestors in ONE statement, leaf first,
// bounded at depth 3 (the profile, its base, that base's base). ErrNotFound when the leaf is gone.
// It returns whatever the bound reached, cycle or overflow included: the caller refuses a chain
// whose last row still names a base or whose ids repeat, and never composes a partial one.
func (s PG) GetGovernanceProfileChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	const q = `
		WITH RECURSIVE chain AS (
			SELECT ` + governanceProfileColsP + `, 1 AS depth FROM governance_profiles p WHERE p.id = $1
			UNION ALL
			SELECT ` + governanceProfileColsP + `, c.depth + 1
			FROM governance_profiles p JOIN chain c ON p.id = c.base_profile_id
			WHERE c.depth < 3
		)
		SELECT ` + governanceProfileCols + ` FROM chain ORDER BY depth`
	out, err := collect(ctx, s.Pool, "read", "governance profile chain", q, []any{id}, scanGovernanceProfile)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// DeleteGovernanceProfile removes one profile by id. ErrNotFound when no row
// matched.
//
// ErrConflict when the row is still ASSIGNED: governance_assignments'
// ON DELETE RESTRICT refuses (23503) rather than cascading, since cascading
// would silently widen every assigned member back to the deployment ceiling.
// Translated into a sentinel so the route answers a caller-fixable 409
// instead of a 500. *ErrProfileHasChildren when other profiles still compose on it
// (named under the graph lock, so the answer matches what the delete saw).
func (s PG) DeleteGovernanceProfile(ctx context.Context, id uuid.UUID) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if err := lockGovernanceGraph(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT name FROM governance_profiles WHERE base_profile_id = $1 ORDER BY name`, id)
	if err != nil {
		return fmt.Errorf("store: list governance profile children: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("store: list governance profile children: %w", err)
	}
	if len(names) > 0 {
		return &ErrProfileHasChildren{Names: names}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM governance_profiles WHERE id = $1`, id)
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
	return tx.Commit(ctx)
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
	q := `SELECT ` + governanceProfileColsP + `, a.subject_type
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
	var ceilingRaw, limitsRaw, contactRaw, overlayRaw, overlayLimitsRaw []byte
	dest := []any{&p.ID, &p.Name, &ceilingRaw, &limitsRaw,
		&p.CreatedAt, &p.UpdatedAt, &p.CreatedBy, &contactRaw, &p.BaseProfileID, &overlayRaw, &overlayLimitsRaw}
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
	// An overlay is authority, so unlike a contact one that will not decode is an error: the
	// resolver then fails closed rather than reading the profile as less narrowed than it is.
	if overlayRaw != nil {
		o, err := types.DecodeCeilingOverlay(overlayRaw)
		if err != nil {
			return types.GovernanceProfile{}, fmt.Errorf("store: decode governance overlay: %w", err)
		}
		p.Overlay = &o
		if overlayLimitsRaw != nil {
			l, err := types.DecodeLimitsOverlay(overlayLimitsRaw)
			if err != nil {
				return types.GovernanceProfile{}, fmt.Errorf("store: decode governance overlay limits: %w", err)
			}
			p.OverlayLimits = &l
		}
	}
	// A contact that will not decode (a direct write) reads as none: it is advice,
	// and must not take the profile's ceiling down with it.
	if len(contactRaw) > 0 {
		var c policyref.Contact
		if json.Unmarshal(contactRaw, &c) == nil && !c.IsZero() {
			p.Contact = &c
		}
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
