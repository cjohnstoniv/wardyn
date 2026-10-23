// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance units and their memberships (migrations 0052, 0207 and governance_unit_tree).
//
// This file is DUMB on purpose: it round-trips rows and owns exactly one
// piece of logic, ListMembershipsFor's ORDER BY, so precedence is a
// property of the single read, not a caller that could forget it.
// Every write is validated at the API boundary (internal/api/governance.go).
package store

import (
	"cmp"
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

const governanceUnitCols = `id, name, ceiling, limits, created_at, updated_at, created_by, contact, parent_id, overlay, overlay_limits, kind, mode, depth, locked_knobs`

// governanceUnitColsP is governanceUnitCols qualified for a join or a recursive term.
const governanceUnitColsP = `p.id, p.name, p.ceiling, p.limits, p.created_at, p.updated_at, p.created_by, p.contact, p.parent_id, p.overlay, p.overlay_limits, p.kind, p.mode, p.depth, p.locked_knobs`

// DefaultUnitID is the one Default unit's fixed id (unit-tree migration). Its walls are the deployment
// default, read at resolve; the row stores none.
var DefaultUnitID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// MaxUnitDepthCap is the deepest a unit path can be stored (the depth CHECK of the unit-tree migration), and
// the bound on the chain read: the configured limit (site_config.governance_max_depth) is never above it.
const MaxUnitDepthCap = 64

// GovernanceUnitBuild produces the row a graph write stores, given every profile and every assignment
// as the write transaction sees them under the graph lock. A refusal it returns aborts the write unchanged.
type GovernanceUnitBuild func(all []types.GovernanceUnit, assignments []types.GovernanceMembership) (types.GovernanceUnit, error)

// ErrProfileHasChildren is DeleteGovernanceUnit's refusal when other profiles still compose on
// the row: Names are those children, sorted.
type ErrProfileHasChildren struct{ Names []string }

func (e *ErrProfileHasChildren) Error() string {
	return "store: governance profile is the base of: " + strings.Join(e.Names, ", ")
}

// ErrUnitPinned is DeleteGovernanceUnit's refusal when workspaces are pinned to the unit
// (workspaces.owning_unit_id): Workspaces are their names, sorted. Deleting the unit would drop their
// walls, so they are unpinned first.
type ErrUnitPinned struct{ Workspaces []string }

func (e *ErrUnitPinned) Error() string {
	return "store: governance unit is pinned by workspaces: " + strings.Join(e.Workspaces, ", ")
}

const governanceMembershipCols = `id, subject_type, subject, unit_id, priority, created_at, created_by, home, legacy`

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
// is released, so two concurrent writes cannot together make a cycle or a path past the maximum depth.
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
	out, err := upsertGovernanceUnitTx(ctx, q, p)
	if err != nil {
		return types.GovernanceUnit{}, err
	}
	// A re-parent moves every descendant too, so the whole tree's depths are recomputed here,
	// under the graph lock, by the same walk the unit-tree migration runs.
	if _, err := q.Exec(ctx, refreshUnitDepths); err != nil {
		return types.GovernanceUnit{}, fmt.Errorf("store: refresh governance unit depths: %w", err)
	}
	if err := q.QueryRow(ctx, `SELECT depth FROM governance_units WHERE id = $1`, out.ID).Scan(&out.Depth); err != nil {
		return types.GovernanceUnit{}, fmt.Errorf("store: read governance unit depth: %w", err)
	}
	return out, nil
}

// refreshUnitDepths sets each unit's depth from the top: Default and every parent-less unit are 1.
const refreshUnitDepths = `
	WITH RECURSIVE tree AS (
		SELECT id, 1 AS d FROM governance_units WHERE parent_id IS NULL
		UNION ALL
		SELECT u.id, t.d + 1 FROM governance_units u JOIN tree t ON u.parent_id = t.id WHERE t.d < 64
	)
	UPDATE governance_units u SET depth = tree.d FROM tree WHERE u.id = tree.id AND u.depth <> tree.d`

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
		INSERT INTO governance_units (id, name, ceiling, limits, created_by, contact, parent_id, overlay, overlay_limits, mode, locked_knobs)
		VALUES ($1,$2,'{}'::jsonb,'{}'::jsonb,$3,$4,$5,$6,$7,$9,$10)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name,
			    ceiling = '{}'::jsonb,
			    limits = '{}'::jsonb,
			    contact = CASE WHEN $8::boolean THEN EXCLUDED.contact ELSE governance_units.contact END,
			    parent_id = EXCLUDED.parent_id,
			    overlay = EXCLUDED.overlay,
			    overlay_limits = EXCLUDED.overlay_limits,
			    mode = EXCLUDED.mode,
			    locked_knobs = EXCLUDED.locked_knobs,
			    updated_at = now()
		RETURNING ` + governanceUnitCols
		q, args = composed, []any{p.ID, p.Name, p.CreatedBy, contactJSON, p.ParentID, overlayJSON, overlayLimitsJSON, p.ContactSet,
			cmp.Or(p.Mode, types.UnitModeOwnWalls), nonNil(p.LockedKnobs)}
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
		INSERT INTO governance_units (id, name, ceiling, limits, created_by, contact, locked_knobs)
		VALUES ($1,$2,$3,$4,$5,$8,$10)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name,
			    ceiling = (governance_units.ceiling - $6::text[]) || EXCLUDED.ceiling,
			    limits = (governance_units.limits - $7::text[]) || EXCLUDED.limits,
			    contact = CASE WHEN $9::boolean THEN EXCLUDED.contact ELSE governance_units.contact END,
			    parent_id = NULL, overlay = NULL, overlay_limits = NULL, mode = 'own_walls',
			    locked_knobs = EXCLUDED.locked_knobs,
			    updated_at = now()
		RETURNING ` + governanceUnitCols
		q, args = standalone, []any{p.ID, p.Name, ceilingJSON, limitsJSON, p.CreatedBy,
			declaredJSONKeys(p.Ceiling), declaredJSONKeys(p.Limits), contactJSON, p.ContactSet, nonNil(p.LockedKnobs)}
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

// GetGovernanceUnitChain reads one unit and its ancestors in ONE statement, leaf first,
// bounded at MaxUnitDepthCap rows. ErrNotFound when the leaf is gone.
// It returns whatever the bound reached, cycle or overflow included: the caller refuses a chain
// whose last row still names a base or whose ids repeat, and never composes a partial one.
func (s PG) GetGovernanceUnitChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	const q = `
		WITH RECURSIVE chain AS (
			SELECT ` + governanceUnitColsP + `, 1 AS n FROM governance_units p WHERE p.id = $1
			UNION ALL
			SELECT ` + governanceUnitColsP + `, c.n + 1
			FROM governance_units p JOIN chain c ON p.id = c.parent_id
			WHERE c.n < 64
		)
		SELECT ` + governanceUnitCols + ` FROM chain ORDER BY n`
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
	rows, err = q.Query(ctx, `SELECT name FROM workspaces WHERE owning_unit_id = $1 ORDER BY name`, id)
	if err != nil {
		return fmt.Errorf("store: list workspaces pinned to a governance unit: %w", err)
	}
	if names, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return fmt.Errorf("store: list workspaces pinned to a governance unit: %w", err)
	}
	if len(names) > 0 {
		return &ErrUnitPinned{Workspaces: names}
	}
	tag, err := q.Exec(ctx, `DELETE FROM governance_units WHERE id = $1`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			if pgErr.ConstraintName == "workspaces_owning_unit_id_fkey" {
				return &ErrUnitPinned{} // pinned since the read above
			}
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

// UpsertGovernanceMembership adds one subject's membership of one unit, keyed on
// the natural UNIQUE (unit_id, subject_type, subject) of the unit-tree migration: a subject
// may be a member of several units, one row each, and re-posting the same unit and
// subject updates that row's priority and home. A legacy row stays legacy.
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
		INSERT INTO governance_memberships (id, subject_type, subject, unit_id, priority, created_by, home)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (unit_id, subject_type, subject) DO UPDATE
			SET priority = EXCLUDED.priority, home = EXCLUDED.home
		RETURNING ` + governanceMembershipCols
	out, err := scanGovernanceMembership(q.QueryRow(ctx, stmt,
		a.ID, a.SubjectType, a.Subject, a.UnitID, a.Priority, a.CreatedBy, a.Home))
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

// GetGovernanceMembershipByKeyQ reads the membership at one natural key (unit, subject type,
// subject) on q, ErrNotFound when none.
func GetGovernanceMembershipByKeyQ(ctx context.Context, q Querier, unitID uuid.UUID, subjectType, subject string, forUpdate bool) (types.GovernanceMembership, error) {
	stmt := `SELECT ` + governanceMembershipCols + ` FROM governance_memberships WHERE unit_id = $1 AND subject_type = $2 AND subject = $3` + lockSuffix(forUpdate)
	return scanGovernanceMembership(q.QueryRow(ctx, stmt, unitID, subjectType, subject))
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

// MembershipMatch is one membership that matches a caller, beside the unit it names.
type MembershipMatch struct {
	Membership types.GovernanceMembership
	Unit       types.GovernanceUnit
}

// ListMembershipsFor returns EVERY membership that matches a caller, each joined to its unit, in
// precedence order (the ORDER BY is the whole rule: tier, then sub over email, then priority, then
// unit name, then subject). No LIMIT: which of them are contexts is the resolver's (legacy rows past
// the first are dormant). subjectMatch and subjectPrecedence are the same fragments ResolveUserDrive
// splices, so the ceiling and the drive can't disagree about "most specific".
//
// SECURITY: users/groups are normalized from nil to empty for the same reason
// ListCapabilityGrantsFor does: a nil Go slice binds as SQL NULL and
// `x = ANY(NULL)` is NULL, not false. Fails closed either way, but a
// predicate whose behavior hinges on a driver detail has no place at an
// authorization boundary.
//
// userType is the caller's one type id; "" matches no row.
func (s PG) ListMembershipsFor(ctx context.Context, userSubjects, groups []string, userType string) ([]MembershipMatch, error) {
	if userSubjects == nil {
		userSubjects = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	q := `SELECT m.id, m.subject_type, m.subject, m.unit_id, m.priority, m.created_at, m.created_by, m.home, m.legacy, ` + governanceUnitColsP + `
		FROM governance_memberships m
		JOIN governance_units p ON p.id = m.unit_id
		WHERE ` + subjectMatch("m") + `
		ORDER BY ` + subjectPrecedence("m", "p")
	rows, err := s.Pool.Query(ctx, q, userSubjects, groups, userType)
	if err != nil {
		return nil, fmt.Errorf("store: list governance memberships for a caller: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MembershipMatch, error) {
		var m MembershipMatch
		a := &m.Membership
		u, err := scanGovernanceUnitPrefixed(r, &a.ID, &a.SubjectType, &a.Subject, &a.UnitID, &a.Priority, &a.CreatedAt, &a.CreatedBy, &a.Home, &a.Legacy)
		m.Unit = u
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: iterate governance memberships for a caller: %w", err)
	}
	return out, nil
}

// ResolveGovernanceUnit is the first row ListMembershipsFor returns (ErrNotFound when none matches)
// and the tier that matched it: the precedence winner over every membership, as the store's
// precedence tests read it. The resolver itself (internal/api governance_context.go) reads the list.
func (s PG) ResolveGovernanceUnit(ctx context.Context, userSubjects, groups []string, userType string) (*types.GovernanceUnit, types.CapabilitySubjectType, error) {
	ms, err := s.ListMembershipsFor(ctx, userSubjects, groups, userType)
	if err != nil {
		return nil, "", err
	}
	if len(ms) == 0 {
		return nil, "", ErrNotFound
	}
	return &ms[0].Unit, ms[0].Membership.SubjectType, nil
}

// CountOwnWallsUnits counts the units with walls of their own (kind unit, mode own_walls): the units
// an admin-token run stands outside. Default and a pass-through unit do not count.
func (s PG) CountOwnWallsUnits(ctx context.Context) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM governance_units WHERE kind = 'unit' AND mode = 'own_walls'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count governance units with their own walls: %w", err)
	}
	return n, nil
}

// MaxUnitDepth is MaxUnitDepthQ on the pool.
func (s PG) MaxUnitDepth(ctx context.Context) (int, []string, error) {
	return MaxUnitDepthQ(ctx, s.Pool)
}

// MaxUnitDepthQ returns the deepest unit path's depth and its unit names from the top down (Default
// or a parent-less unit first); 1 and one name for a tree with no child. The walk is the one
// the unit-tree migration and every write run, bounded at MaxUnitDepthCap.
func MaxUnitDepthQ(ctx context.Context, q Querier) (int, []string, error) {
	const stmt = `
		WITH RECURSIVE tree AS (
			SELECT id, 1 AS d, ARRAY[name] AS path FROM governance_units WHERE parent_id IS NULL
			UNION ALL
			SELECT u.id, t.d + 1, t.path || u.name FROM governance_units u JOIN tree t ON u.parent_id = t.id WHERE t.d < 64
		)
		SELECT d, path FROM tree ORDER BY d DESC, path LIMIT 1`
	var d int
	var path []string
	err := q.QueryRow(ctx, stmt).Scan(&d, &path)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("store: read the deepest governance unit path: %w", err)
	}
	return d, path, nil
}

// HasGroupTierMemberships reports whether ANY group-tier assignment exists:
// the gate on the stale-snapshot refusal (see hasGroupTierRows).
func (s PG) HasGroupTierMemberships(ctx context.Context) (bool, error) {
	return s.hasGroupTierRows(ctx, "governance_memberships", "governance assignments")
}

func scanGovernanceUnit(row pgx.Row) (types.GovernanceUnit, error) {
	return scanGovernanceUnitPrefixed(row)
}

// scanGovernanceUnitPrefixed is scanGovernanceUnit after the columns a join selects ahead of the
// unit's (ListMembershipsFor's membership row), taken as destinations rather than a second scan
// function so the ceiling/limits unmarshal never forks.
func scanGovernanceUnitPrefixed(row pgx.Row, prefix ...any) (types.GovernanceUnit, error) {
	var p types.GovernanceUnit
	var ceilingRaw, limitsRaw, contactRaw, overlayRaw, overlayLimitsRaw []byte
	dest := append(prefix, &p.ID, &p.Name, &ceilingRaw, &limitsRaw,
		&p.CreatedAt, &p.UpdatedAt, &p.CreatedBy, &contactRaw, &p.ParentID, &overlayRaw, &overlayLimitsRaw, &p.Kind, &p.Mode, &p.Depth, &p.LockedKnobs)
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
		&a.Priority, &a.CreatedAt, &a.CreatedBy, &a.Home, &a.Legacy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.GovernanceMembership{}, ErrNotFound
	}
	if err != nil {
		return types.GovernanceMembership{}, fmt.Errorf("store: scan governance assignment: %w", err)
	}
	return a, nil
}
