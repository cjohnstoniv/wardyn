// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance profiles and their subject assignments (migration 0052).
//
// This file is DUMB on purpose: it round-trips rows and owns exactly one
// piece of logic, ResolveGovernanceUnit's ORDER BY, so precedence is a
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

const governanceUnitCols = `id, name, ceiling, limits, created_at, updated_at, created_by, contact, parent_id, overlay, overlay_limits`

// governanceUnitColsP is governanceUnitCols qualified for a join or a recursive term.
const governanceUnitColsP = `p.id, p.name, p.ceiling, p.limits, p.created_at, p.updated_at, p.created_by, p.contact, p.parent_id, p.overlay, p.overlay_limits`

// GovernanceUnitBuild produces the row a graph write stores, given every profile and every assignment
// as the write transaction sees them under the graph lock. A refusal it returns aborts the write unchanged.
type GovernanceUnitBuild func(all []types.GovernanceUnit, assignments []types.GovernanceMembership) (types.GovernanceUnit, error)

// ErrProfileHasChildren is DeleteGovernanceUnit's refusal when other profiles still compose on
// the row: Names are those children, sorted.
type ErrProfileHasChildren struct{ Names []string }

func (e *ErrProfileHasChildren) Error() string {
	return "store: governance profile is the base of: " + strings.Join(e.Names, ", ")
}

const governanceMembershipCols = `id, subject_type, subject, unit_id, priority, created_at, created_by`

// UpsertGovernanceUnit writes one profile exactly as given. It is WriteGovernanceUnit with
// nothing to decide, so it takes the same graph lock and a write can never skip it.
func (s PG) UpsertGovernanceUnit(ctx context.Context, p types.GovernanceUnit) (types.GovernanceUnit, error) {
	return s.WriteGovernanceUnit(ctx, p.ID, func([]types.GovernanceUnit, []types.GovernanceMembership) (types.GovernanceUnit, error) {
		return p, nil
	})
}

// WriteGovernanceUnit writes one profile keyed on its PRIMARY KEY: an unminted id INSERTs, an
// existing one UPDATEs in place (name included, so a profile can be renamed while assignments
// still point at it, since ON DELETE RESTRICT makes delete-and-recreate impossible for an assigned
// profile). One statement serves POST (fresh id) and PUT (id from the path).
//
// The whole write is one transaction under a graph-wide advisory lock: build sees every profile as
// that transaction reads them, decides the row (merging what a PUT left absent, refusing a cycle,
// a depth overflow or an overlay its base does not permit) and the row is stored before the lock
// is released, so two concurrent writes cannot together make a cycle or a depth-4 chain.
//
// contact follows GovernanceUnit.ContactSet: written (or cleared, when empty) only when it is
// true, otherwise the stored value stays. A composed row (Overlay set) binds '{}' for ceiling and
// limits and sets them outright, since it stores no raw authority; its composition columns are
// always written from the row as given.
//
// Returns ErrConflict when UNIQUE(name) rejects the write. created_by and created_at are NOT
// touched on update: creation provenance stays with whoever authored the profile.
func (s PG) WriteGovernanceUnit(ctx context.Context, id uuid.UUID, build GovernanceUnitBuild) (types.GovernanceUnit, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.GovernanceUnit{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	out, err := WriteGovernanceUnitQ(ctx, tx, id, build)
	if err != nil {
		return types.GovernanceUnit{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.GovernanceUnit{}, err
	}
	return out, nil
}

// WriteGovernanceUnitQ is WriteGovernanceUnit's body on a caller's transaction: q must be a
// pgx.Tx, because the graph lock it takes is transaction-scoped. A held governance change applies
// through it inside the decision transaction.
func WriteGovernanceUnitQ(ctx context.Context, q Querier, id uuid.UUID, build GovernanceUnitBuild) (types.GovernanceUnit, error) {
	if id == uuid.Nil {
		id = uuid.New()
	}
	if err := LockGovernanceGraph(ctx, q); err != nil {
		return types.GovernanceUnit{}, err
	}
	all, err := ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return types.GovernanceUnit{}, err
	}
	assignments, err := ListGovernanceMembershipsQ(ctx, q)
	if err != nil {
		return types.GovernanceUnit{}, err
	}
	p, err := build(all, assignments)
	if err != nil {
		return types.GovernanceUnit{}, err
	}
	p.ID = id
	return upsertGovernanceUnitTx(ctx, q, p)
}

// LockGovernanceGraph takes the transaction-scoped lock every write to the governance profile graph
// holds, so one write at a time sees and changes it. Every assignment write takes it too, so a profile
// write reads the assignments no other write is changing. q must be a pgx.Tx.
func LockGovernanceGraph(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, db.GovernanceGraphLockClass); err != nil {
		return fmt.Errorf("store: lock the governance profile graph: %w", err)
	}
	return nil
}

// ListGovernanceUnitsQ reads every profile by name on q.
func ListGovernanceUnitsQ(ctx context.Context, q Querier) ([]types.GovernanceUnit, error) {
	rows, err := q.Query(ctx, `SELECT `+governanceUnitCols+` FROM governance_units ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list governance profiles: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (types.GovernanceUnit, error) { return scanGovernanceUnit(r) })
	if err != nil {
		return nil, fmt.Errorf("store: iterate governance profiles: %w", err)
	}
	return out, nil
}

func upsertGovernanceUnitTx(ctx context.Context, tx Querier, p types.GovernanceUnit) (types.GovernanceUnit, error) {
	// An empty contact is stored as NULL, never as an empty object.
	var contactJSON any
	if p.Contact != nil && !p.Contact.IsZero() {
		b, err := json.Marshal(p.Contact)
		if err != nil {
			return types.GovernanceUnit{}, fmt.Errorf("store: marshal governance contact: %w", err)
		}
		contactJSON = b
	}
	var q string
	var args []any
	if p.Composed() {
		overlayJSON, overlayLimitsJSON, err := marshalOverlays(p)
		if err != nil {
			return types.GovernanceUnit{}, err
		}
		const composed = `
		INSERT INTO governance_units (id, name, ceiling, limits, created_by, contact, parent_id, overlay, overlay_limits)
		VALUES ($1,$2,'{}'::jsonb,'{}'::jsonb,$3,$4,$5,$6,$7)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name,
			    ceiling = '{}'::jsonb,
			    limits = '{}'::jsonb,
			    contact = CASE WHEN $8::boolean THEN EXCLUDED.contact ELSE governance_units.contact END,
			    parent_id = EXCLUDED.parent_id,
			    overlay = EXCLUDED.overlay,
			    overlay_limits = EXCLUDED.overlay_limits,
			    updated_at = now()
		RETURNING ` + governanceUnitCols
		q, args = composed, []any{p.ID, p.Name, p.CreatedBy, contactJSON, p.ParentID, overlayJSON, overlayLimitsJSON, p.ContactSet}
	} else {
		ceilingJSON, err := json.Marshal(p.Ceiling)
		if err != nil {
			return types.GovernanceUnit{}, fmt.Errorf("store: marshal governance ceiling: %w", err)
		}
		limitsJSON, err := json.Marshal(p.Limits)
		if err != nil {
			return types.GovernanceUnit{}, fmt.Errorf("store: marshal governance limits: %w", err)
		}
		// ceiling and limits each keep the keys this binary's types don't
		// declare, so a field a newer wardynd set survives this binary's edit.
		const standalone = `
		INSERT INTO governance_units (id, name, ceiling, limits, created_by, contact)
		VALUES ($1,$2,$3,$4,$5,$8)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name,
			    ceiling = (governance_units.ceiling - $6::text[]) || EXCLUDED.ceiling,
			    limits = (governance_units.limits - $7::text[]) || EXCLUDED.limits,
			    contact = CASE WHEN $9::boolean THEN EXCLUDED.contact ELSE governance_units.contact END,
			    parent_id = NULL, overlay = NULL, overlay_limits = NULL,
			    updated_at = now()
		RETURNING ` + governanceUnitCols
		q, args = standalone, []any{p.ID, p.Name, ceilingJSON, limitsJSON, p.CreatedBy,
			declaredJSONKeys(p.Ceiling), declaredJSONKeys(p.Limits), contactJSON, p.ContactSet}
	}
	out, err := scanGovernanceUnit(tx.QueryRow(ctx, q, args...))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.GovernanceUnit{}, ErrConflict
		}
		return types.GovernanceUnit{}, err
	}
	return out, nil
}

// marshalOverlays is the two overlay columns of a composed row. An empty overlay_limits is stored
// NULL, never as an empty object.
func marshalOverlays(p types.GovernanceUnit) (overlay, overlayLimits any, err error) {
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

// GetGovernanceUnit returns one profile by id, or ErrNotFound. The RAW row: a composed profile
// carries no authority here (see GetGovernanceUnitChain).
func (s PG) GetGovernanceUnit(ctx context.Context, id uuid.UUID) (types.GovernanceUnit, error) {
	const q = `SELECT ` + governanceUnitCols + ` FROM governance_units WHERE id = $1`
	return scanGovernanceUnit(s.Pool.QueryRow(ctx, q, id))
}

// GetGovernanceUnitChain reads one profile and its ancestors in ONE statement, leaf first,
// bounded at depth 3 (the profile, its base, that base's base). ErrNotFound when the leaf is gone.
// It returns whatever the bound reached, cycle or overflow included: the caller refuses a chain
// whose last row still names a base or whose ids repeat, and never composes a partial one.
func (s PG) GetGovernanceUnitChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	const q = `
		WITH RECURSIVE chain AS (
			SELECT ` + governanceUnitColsP + `, 1 AS depth FROM governance_units p WHERE p.id = $1
			UNION ALL
			SELECT ` + governanceUnitColsP + `, c.depth + 1
			FROM governance_units p JOIN chain c ON p.id = c.parent_id
			WHERE c.depth < 3
		)
		SELECT ` + governanceUnitCols + ` FROM chain ORDER BY depth`
	out, err := collect(ctx, s.Pool, "read", "governance profile chain", q, []any{id}, scanGovernanceUnit)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// DeleteGovernanceUnit removes one profile by id. ErrNotFound when no row
// matched.
//
// ErrConflict when the row is still ASSIGNED: governance_memberships'
// ON DELETE RESTRICT refuses (23503) rather than cascading, since cascading
// would silently widen every assigned member back to the deployment ceiling.
// Translated into a sentinel so the route answers a caller-fixable 409
// instead of a 500. *ErrProfileHasChildren when other profiles still compose on it
// (named under the graph lock, so the answer matches what the delete saw).
func (s PG) DeleteGovernanceUnit(ctx context.Context, id uuid.UUID) error {
	_, err := s.DeleteGovernanceUnitNamed(ctx, id)
	return err
}

// DeleteGovernanceUnitNamed is DeleteGovernanceUnit that also returns the name of the row it
// removed, read in the same transaction under the graph lock, so an audit row names the profile that
// was deleted rather than whichever one a separate read saw. The name is empty on error.
func (s PG) DeleteGovernanceUnitNamed(ctx context.Context, id uuid.UUID) (string, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if err := LockGovernanceGraph(ctx, tx); err != nil {
		return "", err
	}
	row, err := GetGovernanceUnitQ(ctx, tx, id, false)
	if err != nil {
		return "", err
	}
	if err := DeleteGovernanceUnitQ(ctx, tx, id); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return row.Name, nil
}

// DeleteGovernanceUnitQ is DeleteGovernanceUnit's body on a caller's transaction (q must be a
// pgx.Tx: it takes the transaction-scoped graph lock) and answers the same errors.
func DeleteGovernanceUnitQ(ctx context.Context, q Querier, id uuid.UUID) error {
	if err := LockGovernanceGraph(ctx, q); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `SELECT name FROM governance_units WHERE parent_id = $1 ORDER BY name`, id)
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
	tag, err := q.Exec(ctx, `DELETE FROM governance_units WHERE id = $1`, id)
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

// ListGovernanceUnits returns every profile by name (the handle an admin
// looks for), unlike the creation order other List* calls use. Deployment-
// wide config, so there's no per-caller scoping to filter on.
func (s PG) ListGovernanceUnits(ctx context.Context) ([]types.GovernanceUnit, error) {
	const q = `SELECT ` + governanceUnitCols + ` FROM governance_units ORDER BY name`
	return collect(ctx, s.Pool, "list", "governance profiles", q, nil, scanGovernanceUnit)
}

// UpsertGovernanceMembership binds one subject to one profile, keyed on the
// natural UNIQUE (subject_type, subject): re-assigning a subject REPOINTS its
// single row rather than leaving a second one behind (two rows for one
// subject would make "which profile does Bob get" a tie-break instead of the
// admin's last write).
//
// The returned row carries the EXISTING id on conflict, not a.ID, so the
// caller gets the id the DELETE route needs.
//
// Returns ErrNotFound when unit_id names no profile (FK 23503 → 404).
func (s PG) UpsertGovernanceMembership(ctx context.Context, a types.GovernanceMembership) (types.GovernanceMembership, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.GovernanceMembership{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// The locks a held change's approval takes too, in its order: the upsert and that approval never
	// interleave, and neither does a profile write.
	if err := LockGovernanceGraph(ctx, tx); err != nil {
		return types.GovernanceMembership{}, err
	}
	if err := LockGovernanceMembershipKey(ctx, tx, string(a.SubjectType), a.Subject); err != nil {
		return types.GovernanceMembership{}, err
	}
	out, err := UpsertGovernanceMembershipQ(ctx, tx, a)
	if err != nil {
		return types.GovernanceMembership{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.GovernanceMembership{}, err
	}
	return out, nil
}

// LockGovernanceMembershipKey takes the transaction-scoped lock keyed to one assignment's natural
// key, so a write to that key and the approval of a held change to it serialize. q must be a pgx.Tx.
func LockGovernanceMembershipKey(ctx context.Context, q Querier, subjectType, subject string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		db.GovernanceMembershipLockClass, subjectType+"\x1f"+subject); err != nil {
		return fmt.Errorf("store: lock the governance assignment key: %w", err)
	}
	return nil
}

// UpsertGovernanceMembershipQ is UpsertGovernanceMembership's statement on q.
func UpsertGovernanceMembershipQ(ctx context.Context, q Querier, a types.GovernanceMembership) (types.GovernanceMembership, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	const stmt = `
		INSERT INTO governance_memberships (id, subject_type, subject, unit_id, priority, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (subject_type, subject) DO UPDATE
			SET unit_id = EXCLUDED.unit_id, priority = EXCLUDED.priority
		RETURNING ` + governanceMembershipCols
	out, err := scanGovernanceMembership(q.QueryRow(ctx, stmt,
		a.ID, a.SubjectType, a.Subject, a.UnitID, a.Priority, a.CreatedBy))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return types.GovernanceMembership{}, ErrNotFound
		}
		return types.GovernanceMembership{}, err
	}
	return out, nil
}

// DeleteGovernanceMembership removes one assignment by id and returns the row it
// removed, ErrNotFound when no row matched. Unassigning is the SUPPORTED way to
// widen someone back to the deployment ceiling.
func (s PG) DeleteGovernanceMembership(ctx context.Context, id uuid.UUID) (types.GovernanceMembership, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.GovernanceMembership{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if err := LockGovernanceGraph(ctx, tx); err != nil {
		return types.GovernanceMembership{}, err
	}
	a, err := scanGovernanceMembership(tx.QueryRow(ctx, `DELETE FROM governance_memberships WHERE id = $1 RETURNING `+governanceMembershipCols, id))
	if err != nil {
		return types.GovernanceMembership{}, err
	}
	return a, tx.Commit(ctx)
}

// DeleteGovernanceMembershipQ is the held-change apply's delete on q: ErrNotFound when no row
// matched, a statement failure as a plain error. It does not read the removed row back.
func DeleteGovernanceMembershipQ(ctx context.Context, q Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM governance_memberships WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete governance assignment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetGovernanceMembershipQ reads one assignment by id on q, ErrNotFound when none; forUpdate locks the
// row for the transaction.
func GetGovernanceMembershipQ(ctx context.Context, q Querier, id uuid.UUID, forUpdate bool) (types.GovernanceMembership, error) {
	stmt := `SELECT ` + governanceMembershipCols + ` FROM governance_memberships WHERE id = $1` + lockSuffix(forUpdate)
	return scanGovernanceMembership(q.QueryRow(ctx, stmt, id))
}

// GetGovernanceMembershipByKeyQ reads the assignment at one natural key on q, ErrNotFound when none.
func GetGovernanceMembershipByKeyQ(ctx context.Context, q Querier, subjectType, subject string, forUpdate bool) (types.GovernanceMembership, error) {
	stmt := `SELECT ` + governanceMembershipCols + ` FROM governance_memberships WHERE subject_type = $1 AND subject = $2` + lockSuffix(forUpdate)
	return scanGovernanceMembership(q.QueryRow(ctx, stmt, subjectType, subject))
}

// GetGovernanceUnitQ reads one profile row (raw, as GetGovernanceUnit does) on q.
func GetGovernanceUnitQ(ctx context.Context, q Querier, id uuid.UUID, forUpdate bool) (types.GovernanceUnit, error) {
	stmt := `SELECT ` + governanceUnitCols + ` FROM governance_units WHERE id = $1` + lockSuffix(forUpdate)
	return scanGovernanceUnit(q.QueryRow(ctx, stmt, id))
}

func lockSuffix(forUpdate bool) string {
	if forUpdate {
		return " FOR UPDATE"
	}
	return ""
}

// ListGovernanceMemberships returns every assignment in the order the
// resolver itself would rank them (tier, then priority, then subject), so
// the console's table reads top-down as the precedence rule.
func (s PG) ListGovernanceMemberships(ctx context.Context) ([]types.GovernanceMembership, error) {
	return ListGovernanceMembershipsQ(ctx, s.Pool)
}

// ListGovernanceMembershipsQ is ListGovernanceMemberships on q.
func ListGovernanceMembershipsQ(ctx context.Context, q Querier) ([]types.GovernanceMembership, error) {
	const stmt = `SELECT ` + governanceMembershipCols + ` FROM governance_memberships
		ORDER BY ` + subjectTierOrder + `, priority DESC, subject`
	return collect(ctx, q, "list", "governance assignments", stmt, nil, scanGovernanceMembership)
}

// ResolveGovernanceUnit returns THE ONE profile that applies to a caller, or
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
func (s PG) ResolveGovernanceUnit(ctx context.Context, userSubjects, groups []string, userType string) (*types.GovernanceUnit, types.CapabilitySubjectType, error) {
	if userSubjects == nil {
		userSubjects = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	q := `SELECT ` + governanceUnitColsP + `, a.subject_type
		FROM governance_memberships a
		JOIN governance_units p ON p.id = a.unit_id
		WHERE ` + subjectMatch("a") + `
		ORDER BY ` + subjectPrecedence("a", "p") + `
		LIMIT 1`
	var tier string
	p, err := scanGovernanceUnitInto(s.Pool.QueryRow(ctx, q, userSubjects, groups, userType), &tier)
	if err != nil {
		return nil, "", err
	}
	return &p, types.CapabilitySubjectType(tier), nil
}

// HasGroupTierMemberships reports whether ANY group-tier assignment exists:
// the gate on the stale-snapshot refusal (see hasGroupTierRows).
func (s PG) HasGroupTierMemberships(ctx context.Context) (bool, error) {
	return s.hasGroupTierRows(ctx, "governance_memberships", "governance assignments")
}

func scanGovernanceUnit(row pgx.Row) (types.GovernanceUnit, error) {
	return scanGovernanceUnitInto(row, nil)
}

// scanGovernanceUnitInto is scanGovernanceUnit plus the ONE extra
// column the resolver selects (subject_type), taken as a parameter rather
// than a second scan function so the ceiling/limits unmarshal never forks.
func scanGovernanceUnitInto(row pgx.Row, tier *string) (types.GovernanceUnit, error) {
	var p types.GovernanceUnit
	var ceilingRaw, limitsRaw, contactRaw, overlayRaw, overlayLimitsRaw []byte
	dest := []any{&p.ID, &p.Name, &ceilingRaw, &limitsRaw,
		&p.CreatedAt, &p.UpdatedAt, &p.CreatedBy, &contactRaw, &p.ParentID, &overlayRaw, &overlayLimitsRaw}
	if tier != nil {
		dest = append(dest, tier)
	}
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.GovernanceUnit{}, ErrNotFound
	}
	if err != nil {
		return types.GovernanceUnit{}, fmt.Errorf("store: scan governance profile: %w", err)
	}
	if err := json.Unmarshal(ceilingRaw, &p.Ceiling); err != nil {
		return types.GovernanceUnit{}, fmt.Errorf("store: unmarshal governance ceiling: %w", err)
	}
	if err := json.Unmarshal(limitsRaw, &p.Limits); err != nil {
		return types.GovernanceUnit{}, fmt.Errorf("store: unmarshal governance limits: %w", err)
	}
	// An overlay is authority, so unlike a contact one that will not decode is an error: the
	// resolver then fails closed rather than reading the profile as less narrowed than it is.
	if overlayRaw != nil {
		o, err := types.DecodeCeilingOverlay(overlayRaw)
		if err != nil {
			return types.GovernanceUnit{}, fmt.Errorf("store: decode governance overlay: %w", err)
		}
		p.Overlay = &o
		if overlayLimitsRaw != nil {
			l, err := types.DecodeLimitsOverlay(overlayLimitsRaw)
			if err != nil {
				return types.GovernanceUnit{}, fmt.Errorf("store: decode governance overlay limits: %w", err)
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

func scanGovernanceMembership(row pgx.Row) (types.GovernanceMembership, error) {
	var a types.GovernanceMembership
	err := row.Scan(&a.ID, &a.SubjectType, &a.Subject, &a.UnitID,
		&a.Priority, &a.CreatedAt, &a.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.GovernanceMembership{}, ErrNotFound
	}
	if err != nil {
		return types.GovernanceMembership{}, fmt.Errorf("store: scan governance assignment: %w", err)
	}
	return a, nil
}
