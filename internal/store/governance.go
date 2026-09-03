// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance profiles and their subject assignments (migration 0052). Kept out
// of store.go on purpose, mirroring store_capabilities.go's split.
//
// This file is DUMB on purpose: it round-trips rows and owns exactly one piece
// of logic, ResolveGovernanceProfile's ORDER BY, because that precedence has to
// be a property of the single indexed read rather than of a caller that could
// forget it. Every write is validated at the API boundary (internal/api/
// governance.go) — the ceiling through validatePolicySpec, the eligible grants
// through the monotone-⊆ bound — exactly as run_policies is.
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

// UpsertGovernanceProfile writes one profile, keyed on its PRIMARY KEY: a
// caller-minted id that does not exist yet INSERTs, and one that does UPDATEs
// in place (name included, so a profile can be renamed while assignments still
// point at it — which they must be able to be, since ON DELETE RESTRICT makes
// delete-and-recreate impossible for an assigned profile).
//
// That single statement serves both write routes: POST mints a fresh id and
// therefore always inserts, PUT passes the id from the path and therefore
// always updates. A PUT naming an id no row has creates it at that id, which is
// what PUT means and costs no existence read.
//
// Returns ErrConflict when the UNIQUE(name) index rejects the write — a NEW
// profile taking a taken name, or a rename onto another row's name. The caller
// maps that to 409 with the name in the message, never a raw driver error
// (the CreatePolicy contract, W20-S1-3).
//
// created_by and created_at are NOT touched on the update path: creation
// provenance stays with whoever authored the profile, even after a later edit
// by a different admin (the A-9 rule UpsertRoleMapping follows).
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
			SET name = EXCLUDED.name, ceiling = EXCLUDED.ceiling,
			    limits = EXCLUDED.limits, updated_at = now()
		RETURNING ` + governanceProfileCols
	out, err := scanGovernanceProfile(s.Pool.QueryRow(ctx, q,
		p.ID, p.Name, ceilingJSON, limitsJSON, p.CreatedBy))
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
// matched (admin-only surface — no principal to scope to, no existence oracle
// to protect).
//
// ErrConflict when the row is still ASSIGNED. governance_assignments.profile_id
// is ON DELETE RESTRICT, so Postgres refuses with a foreign-key violation
// (23503) rather than cascading the assignments away — and that refusal is the
// point: cascading would silently widen every member of the deleted profile
// back to the deployment ceiling with nothing said. Translating the driver
// error into a sentinel here is what lets the route answer a caller-fixable
// 409 ("unassign it first") instead of a 500.
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

// ListGovernanceProfiles returns every profile by name — the console's whole
// table in one read, and name is the handle an admin actually looks for, so
// creation order (what the other List* calls sort by) would be the wrong axis
// here. This is deployment-wide config; there is no per-caller scoping to
// filter on.
func (s PG) ListGovernanceProfiles(ctx context.Context) ([]types.GovernanceProfile, error) {
	const q = `SELECT ` + governanceProfileCols + ` FROM governance_profiles ORDER BY name`
	return collect(ctx, s.Pool, "list", "governance profiles", q, nil, scanGovernanceProfile)
}

// UpsertGovernanceAssignment binds one subject to one profile, keyed on the
// natural UNIQUE (subject_type, subject): re-assigning a subject REPOINTS its
// single row rather than leaving a second one behind. Two rows for one subject
// would make "which profile does Bob get" depend on the priority/name
// tie-breaks instead of on the admin's last write — resolvable, but not
// explainable, and an admin who cannot explain a ceiling cannot trust it.
//
// The returned row carries the row's real id, which on a conflict is the
// EXISTING one, not a.ID: the caller needs the id the DELETE route will be
// given, and an admin re-submitting the same subject must not be handed an id
// that names no row (the UpsertCapabilityGrant contract).
//
// Returns ErrNotFound when profile_id names no profile — the FK rejects it
// (23503), and "the profile you named does not exist" is a 404 the caller can
// act on, not a 500.
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

// DeleteGovernanceAssignment removes one assignment by id, ErrNotFound when no
// row matched. Unassigning is the SUPPORTED way to widen someone back to the
// deployment ceiling — the deliberate act the RESTRICT on the profile delete
// exists to force.
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

// ListGovernanceAssignments returns every assignment in the order the resolver
// itself would rank them (tier, then priority, then subject) so the console's
// "who gets what" table reads top-down as the precedence rule, not as insertion
// order that an admin then has to re-sort in their head.
func (s PG) ListGovernanceAssignments(ctx context.Context) ([]types.GovernanceAssignment, error) {
	const q = `SELECT ` + governanceAssignmentCols + ` FROM governance_assignments
		ORDER BY ` + governanceTierOrder + `, priority DESC, subject`
	return collect(ctx, s.Pool, "list", "governance assignments", q, nil, scanGovernanceAssignment)
}

// governanceTierOrder ranks the three subject tiers MOST SPECIFIC FIRST —
// user > group > all. Written once, as SQL, and spliced into BOTH the resolver
// and the console listing so the two can never disagree about what "most
// specific" means.
//
// subject_type is deliberately UNQUALIFIED so the one string works in the
// resolver's JOIN as well as the single-table listing. That is safe because
// governance_profiles has no subject_type column (migration 0052) — the only
// other table in that JOIN. A migration that added one would make this
// ambiguous, and Postgres would say so loudly rather than silently re-rank.
const governanceTierOrder = `CASE subject_type WHEN 'user' THEN 0 WHEN 'group' THEN 1 ELSE 2 END`

// ResolveGovernanceProfile returns THE ONE profile that applies to a caller, or
// ErrNotFound when no assignment matches (which the caller reads as "fall
// through to the deployment ceiling", the absent-row back-compat rule).
//
// The whole precedence rule is the ORDER BY, and that is deliberate: it is one
// indexed read on the UNIQUE(subject_type, subject) btree, so there is no
// second implementation in Go for a caller to skip, mis-order, or forget.
// Ranked, in order:
//
//  1. TIER — user > group > all. An assignment is one admin explicitly naming
//     one principal, so the more specific naming wins outright; no priority in
//     the group tier can beat a user-tier row.
//
//  2. WITHIN THE USER TIER, a sub-keyed match beats an email-keyed one.
//     capabilitySubjects returns up to TWO user subjects (lowercased sub, then
//     email) and an admin may legitimately have written an assignment against
//     either, so dueling rows on the two are reachable and LIMIT 1 must not
//     pick arbitrarily. Sub wins because it is the stable identifier — an email
//     is reassignable, and inheriting a departed colleague's ceiling by taking
//     their address is not a thing this should permit. Encoded as the MATCH
//     POSITION in the caller's own userSubjects slice (array_position), so the
//     caller's documented ordering IS the precedence and this query needs no
//     opinion about which identity kind sits at which index.
//
//  3. priority DESC — the admin's explicit tie-break, and the group tier's
//     working lever (a member is usually in several groups at once).
//
//  4. profiles.name ASC — applied in EVERY tier. Without it two same-priority
//     rows make LIMIT 1 depend on the plan, and "why did Bob get profile B
//     today" has no answer.
//
//  5. assignments.subject ASC — the deterministic total-order FLOOR, and the
//     same last key ListGovernanceAssignments already ends on (:182), so the
//     two orderings in this file now agree.
//
//     It changes no answer today, and the reason is worth writing down because
//     it is a DEPENDENCY rather than a coincidence: the tier is the first key,
//     so two rows still tied after (4) necessarily share a tier AND a profile,
//     and this SELECT returns profile columns plus a.subject_type and nothing
//     else per assignment — so LIMIT 1 picking either row yields byte-identical
//     output. That held only while the projection carried no per-assignment
//     column. The moment anyone adds a.subject, a.priority or a new assignment
//     field to the SELECT (an audit line naming WHICH assignment matched is the
//     obvious next ask), the tie becomes observable and the answer starts
//     depending on the plan. One key removes the dependency instead of
//     documenting it, so nothing has to notice when that day comes.
//
// users/groups are normalized from nil to empty for the same reason
// ListCapabilityGrantsFor normalizes them: a nil Go slice binds as SQL NULL and
// `x = ANY(NULL)` is NULL rather than false. It fails closed either way, but a
// predicate whose behavior depends on a driver detail is not one to leave
// standing at an authorization boundary.
func (s PG) ResolveGovernanceProfile(ctx context.Context, userSubjects, groups []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	if userSubjects == nil {
		userSubjects = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	const q = `SELECT p.id, p.name, p.ceiling, p.limits, p.created_at, p.updated_at, p.created_by, a.subject_type
		FROM governance_assignments a
		JOIN governance_profiles p ON p.id = a.profile_id
		WHERE a.subject_type = 'all'
		   OR (a.subject_type = 'user'  AND a.subject = ANY($1::text[]))
		   OR (a.subject_type = 'group' AND a.subject = ANY($2::text[]))
		ORDER BY
			` + governanceTierOrder + `,
			CASE a.subject_type WHEN 'user'
				THEN COALESCE(array_position($1::text[], a.subject), 2147483647)
				ELSE 0 END,
			a.priority DESC,
			p.name ASC,
			a.subject ASC
		LIMIT 1`
	var tier string
	p, err := scanGovernanceProfileInto(s.Pool.QueryRow(ctx, q, userSubjects, groups), &tier)
	if err != nil {
		return nil, "", err
	}
	return &p, types.CapabilitySubjectType(tier), nil
}

// HasGroupTierAssignments reports whether ANY group-tier assignment exists.
//
// It is the gate on the stale/truncated-snapshot refusal, and it is a separate,
// deliberately cheap read because that refusal must fire on exactly one
// deployment shape. A caller whose group snapshot is missing or truncated
// cannot have its group assignments evaluated — but on a deployment with NO
// group-tier rows there is nothing an unknown group could have matched, so
// refusing there would break "no assignment ⇒ byte-for-byte today" for every
// pre-upgrade session and every deployment that never adopted group profiles.
// EXISTS, not a count: the answer is a boolean and Postgres stops at the first
// row.
func (s PG) HasGroupTierAssignments(ctx context.Context) (bool, error) {
	var has bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM governance_assignments WHERE subject_type = 'group')`).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: check group-tier governance assignments: %w", err)
	}
	return has, nil
}

func scanGovernanceProfile(row pgx.Row) (types.GovernanceProfile, error) {
	return scanGovernanceProfileInto(row, nil)
}

// scanGovernanceProfileInto is scanGovernanceProfile plus the ONE extra column
// the resolver selects: the matched assignment's subject_type. It is a
// parameter rather than a second scan function because the ceiling/limits
// unmarshal below is the part that must never fork — a second copy of it is
// how one of the two paths quietly stops validating.
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
