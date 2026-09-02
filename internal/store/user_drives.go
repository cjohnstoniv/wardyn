// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User drives and their subject grants (migration 0054). Kept out of store.go
// on purpose, mirroring the governance.go split.
//
// This file is DUMB on purpose: it round-trips rows and owns exactly one piece
// of logic, ResolveUserDrive's ORDER BY, because that precedence has to be a
// property of the single indexed read rather than of a caller that could forget
// it. Every write is validated at the API boundary (types.ValidateUserDrive and
// types.ValidateUserDriveGrant, called from internal/api), exactly as a
// governance profile's ceiling is.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const userDriveCols = `id, name, backend, host_root, storage_class, home_template, ` +
	`size_mib, writable, reclaim, created_at, updated_at, created_by`

const userDriveGrantCols = `id, subject_type, subject, drive_id, priority, ` +
	`size_mib_override, writable_override, home_override, enabled, created_at, created_by`

// userDriveDest is the scan target list for userDriveCols, written ONCE so the
// column list and the destinations cannot drift: the resolver selects a drive
// JOINed to its grant and would otherwise carry a second hand-written copy of
// twelve destinations in the same order.
func userDriveDest(d *types.UserDrive) []any {
	return []any{&d.ID, &d.Name, &d.Backend, &d.HostRoot, &d.StorageClass, &d.HomeTemplate,
		&d.SizeMiB, &d.Writable, &d.Reclaim, &d.CreatedAt, &d.UpdatedAt, &d.CreatedBy}
}

// userDriveGrantDest is the same for userDriveGrantCols. writable_override is
// scanned through a **bool because the column is NULLABLE and NULL is NOT
// false: NULL means "use the drive's posture", an explicit false is an admin
// narrowing this subject to read-only.
func userDriveGrantDest(g *types.UserDriveGrant) []any {
	return []any{&g.ID, &g.SubjectType, &g.Subject, &g.DriveID, &g.Priority,
		&g.SizeMiBOverride, &g.WritableOverride, &g.HomeOverride, &g.Enabled,
		&g.CreatedAt, &g.CreatedBy}
}

// UpsertUserDrive writes one drive, keyed on its PRIMARY KEY: a caller-minted
// id that does not exist yet INSERTs, one that does UPDATEs in place (name
// included, so a drive can be renamed while grants still point at it — which
// they must be able to be, since ON DELETE RESTRICT makes delete-and-recreate
// impossible for an allocated drive).
//
// RENAMING A DRIVE MOVES A PVC's NAME, and that is a documented consequence
// rather than a bug this store can fix: types.DriveObjectName folds the name
// into a k8s claim name, so a renamed drive's members bind a claim that does
// not exist yet and a managed drive provisions a fresh empty one. The admin
// surface says so at the write; the alternative — a second immutable slug
// column — buys stability for the one field an admin most needs to be able to
// correct, and the operator runbook for a rename is `kubectl get pvc` plus the
// preview endpoint, which prints the exact object name for a principal.
//
// Returns ErrConflict when UNIQUE(name) rejects the write — a new drive taking
// a taken name, or a rename onto another row's name. The caller maps that to
// 409 with the name in the message, never a raw driver error (the CreatePolicy
// contract).
//
// created_by and created_at are NOT touched on the update path: creation
// provenance stays with whoever registered the drive, even after a later edit
// by a different admin (the same rule UpsertGovernanceProfile follows).
func (s PG) UpsertUserDrive(ctx context.Context, d types.UserDrive) (types.UserDrive, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	const q = `
		INSERT INTO user_drives (id, name, backend, host_root, storage_class,
			home_template, size_mib, writable, reclaim, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name, backend = EXCLUDED.backend,
			    host_root = EXCLUDED.host_root, storage_class = EXCLUDED.storage_class,
			    home_template = EXCLUDED.home_template, size_mib = EXCLUDED.size_mib,
			    writable = EXCLUDED.writable, reclaim = EXCLUDED.reclaim,
			    updated_at = now()
		RETURNING ` + userDriveCols
	out, err := scanUserDrive(s.Pool.QueryRow(ctx, q,
		d.ID, d.Name, d.Backend, d.HostRoot, d.StorageClass,
		d.HomeTemplate, d.SizeMiB, d.Writable, d.Reclaim, d.CreatedBy))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.UserDrive{}, ErrConflict
		}
		return types.UserDrive{}, err
	}
	return out, nil
}

// GetUserDrive returns one drive by id, or ErrNotFound.
func (s PG) GetUserDrive(ctx context.Context, id uuid.UUID) (types.UserDrive, error) {
	const q = `SELECT ` + userDriveCols + ` FROM user_drives WHERE id = $1`
	return scanUserDrive(s.Pool.QueryRow(ctx, q, id))
}

// DeleteUserDrive removes one drive by id. ErrNotFound when no row matched
// (admin-only surface — no principal to scope to, no existence oracle to
// protect).
//
// ErrConflict when the drive is still ALLOCATED. user_drive_grants.drive_id is
// ON DELETE RESTRICT, so Postgres refuses with a foreign-key violation (23503)
// rather than cascading the grants away — and that refusal is the point: the
// directories those grants named would still hold somebody's work, now
// unreachable and unaudited. Translating the driver error into a sentinel here
// is what lets the route answer a caller-fixable 409 instead of a 500.
func (s PG) DeleteUserDrive(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM user_drives WHERE id = $1`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ErrConflict
		}
		return fmt.Errorf("store: delete user drive: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListUserDrives returns every drive by name, each with the number of grants
// bound to it — the console's whole table in one read. Name is the handle an
// admin actually looks for, so creation order (what the other List* calls sort
// by) would be the wrong axis. This is deployment-wide config; there is no
// per-caller scoping to filter on.
//
// The count rides along rather than being a second round trip per row because
// it is what makes the delete affordance honest: a drive with grants answers
// 409, and an admin should see that before clicking rather than after. A
// grouped subquery (not a JOIN with GROUP BY over the whole product) keeps the
// drive row single even when a drive holds many grants, and it is served by
// user_drive_grants_drive_id_idx.
func (s PG) ListUserDrives(ctx context.Context) ([]types.UserDriveListItem, error) {
	const q = `SELECT d.id, d.name, d.backend, d.host_root, d.storage_class, d.home_template,
			d.size_mib, d.writable, d.reclaim, d.created_at, d.updated_at, d.created_by,
			COALESCE(c.n, 0)
		FROM user_drives d
		LEFT JOIN (SELECT drive_id, COUNT(*) AS n FROM user_drive_grants GROUP BY drive_id) c
			ON c.drive_id = d.id
		ORDER BY d.name`
	return collect(ctx, s.Pool, "list", "user drives", q, nil,
		func(row pgx.Row) (types.UserDriveListItem, error) {
			var item types.UserDriveListItem
			dest := append(userDriveDest(&item.UserDrive), &item.GrantCount)
			if err := row.Scan(dest...); err != nil {
				return types.UserDriveListItem{}, fmt.Errorf("store: scan user drive: %w", err)
			}
			return item, nil
		})
}

// UpsertUserDriveGrant allocates one drive to one subject, keyed on the natural
// UNIQUE (subject_type, subject): re-allocating a subject REPOINTS its single
// row rather than leaving a second one behind. Two rows for one subject would
// make "which drive does Bob get" depend on the priority/name tie-breaks
// instead of on the admin's last write — resolvable, but not explainable, and
// an admin who cannot explain an allocation cannot trust it.
//
// The returned row carries the row's real id, which on a conflict is the
// EXISTING one, not g.ID: the caller needs the id the DELETE route will be
// given, and an admin re-submitting the same subject must not be handed an id
// that names no row.
//
// Returns ErrNotFound when drive_id names no drive — the FK rejects it (23503),
// and "the drive you named does not exist" is a 404 the caller can act on, not
// a 500.
//
// Enabled is written VERBATIM, so a zero-value grant is a DISABLED one. That is
// the fail-closed half (an allocation nobody enabled mounts nothing) and it
// puts the "new grants are on by default" decision at the API write boundary,
// where the request that omitted the field can still be seen — the column's own
// DEFAULT true never applies, because this INSERT always supplies a value.
func (s PG) UpsertUserDriveGrant(ctx context.Context, g types.UserDriveGrant) (types.UserDriveGrant, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	const q = `
		INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, priority,
			size_mib_override, writable_override, home_override, enabled, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (subject_type, subject) DO UPDATE
			SET drive_id = EXCLUDED.drive_id, priority = EXCLUDED.priority,
			    size_mib_override = EXCLUDED.size_mib_override,
			    writable_override = EXCLUDED.writable_override,
			    home_override = EXCLUDED.home_override, enabled = EXCLUDED.enabled
		RETURNING ` + userDriveGrantCols
	out, err := scanUserDriveGrant(s.Pool.QueryRow(ctx, q,
		g.ID, g.SubjectType, g.Subject, g.DriveID, g.Priority,
		g.SizeMiBOverride, g.WritableOverride, g.HomeOverride, g.Enabled, g.CreatedBy))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return types.UserDriveGrant{}, ErrNotFound
		}
		return types.UserDriveGrant{}, err
	}
	return out, nil
}

// DeleteUserDriveGrant removes one grant by id, ErrNotFound when no row
// matched. De-allocating is the SUPPORTED way to take a drive away — the
// deliberate act the RESTRICT on the drive delete exists to force — and it
// removes only the BINDING: nothing in the control plane deletes the volume,
// the claim or the directory (reclaim is a declared intent executed by a
// documented operator command).
func (s PG) DeleteUserDriveGrant(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM user_drive_grants WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete user drive grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListUserDriveGrants returns every grant in the order the resolver itself
// would rank them (tier, then priority, then subject) so the console's "who
// gets what" table reads top-down as the precedence rule, not as insertion
// order an admin then has to re-sort in their head.
func (s PG) ListUserDriveGrants(ctx context.Context) ([]types.UserDriveGrant, error) {
	const q = `SELECT ` + userDriveGrantCols + ` FROM user_drive_grants
		ORDER BY ` + userDriveTierOrder + `, priority DESC, subject`
	return collect(ctx, s.Pool, "list", "user drive grants", q, nil, scanUserDriveGrant)
}

// userDriveTierOrder ranks the three subject tiers MOST SPECIFIC FIRST —
// user > group > all. Written once, as SQL, and shared by the resolver and the
// console listing so the two can never disagree about what "most specific"
// means. Deliberately a SEPARATE constant from governanceTierOrder despite the
// identical text: these two are the same RULE over different tables, and
// sharing the string would make a future per-table divergence look like a typo
// in a shared const rather than the deliberate change it would have to be.
const userDriveTierOrder = `CASE subject_type WHEN 'user' THEN 0 WHEN 'group' THEN 1 ELSE 2 END`

// ResolveUserDrive returns THE ONE drive that applies to a caller, the grant
// that won, and the tier it won at — or ErrNotFound when no grant matches,
// which the caller reads as "this principal has no drive" (the absent-row
// doctrine).
//
// The whole precedence rule is the ORDER BY, copied from
// ResolveGovernanceProfile because it is the same rule about the same subject
// vocabulary, and it is one indexed read on the UNIQUE(subject_type, subject)
// btree so there is no second implementation in Go for a caller to skip,
// mis-order, or forget. Ranked, in order:
//
//  1. TIER — user > group > all. A grant is one admin explicitly naming one
//     principal, so the more specific naming wins outright; no priority in the
//     group tier can beat a user-tier row.
//  2. WITHIN THE USER TIER, a sub-keyed match beats an email-keyed one.
//     capabilitySubjects returns up to TWO user subjects (lowercased sub, then
//     email) and an admin may legitimately have written a grant against either.
//     Sub wins because it is the stable identifier — an email is reassignable,
//     and inheriting a departed colleague's DRIVE by taking their address is
//     emphatically not a thing this may permit. Encoded as the MATCH POSITION
//     in the caller's own userSubjects slice (array_position), so the caller's
//     documented ordering IS the precedence.
//  3. priority DESC — the admin's explicit tie-break, and the group tier's
//     working lever (a member is usually in several groups at once).
//  4. drives.name ASC — the deterministic total-order floor, applied in EVERY
//     tier. Without it two same-priority rows make LIMIT 1 depend on the plan,
//     and "why did Bob get the other drive today" has no answer.
//
// DISABLED GRANTS ARE IN THE QUERY, and the winner's own `enabled` decides
// PAUSED vs MOUNTED — a disabled row that wins its tier yields paused, never
// the wider row beneath it (DESIGN §2.2: turning Bob's row off cannot silently
// hand him the group's writable drive). Excluding them in the WHERE instead
// reads tidier and is the widening this must not do: the pause would fall
// through to whatever `all`-tier row exists, which is a drive no admin decided
// this member should have — and it would do so silently, because the member's
// only signal is a mount that appeared rather than an allocation that stopped.
// So there is ONE read, its ORDER BY is the whole precedence rule, and the
// caller reads g.Enabled to tell the two answers apart.
//
// The tier comes back as the winning grant's own subject_type rather than a
// separately selected column — this resolver returns the grant itself, so
// unlike the governance one it has the answer in hand and cannot disagree with
// it.
//
// userSubjects/groups are normalized from nil to empty for the same reason
// ResolveGovernanceProfile normalizes them: a nil Go slice binds as SQL NULL
// and `x = ANY(NULL)` is NULL rather than false. It fails closed either way,
// but a predicate whose behavior depends on a driver detail is not one to leave
// standing at an authorization boundary.
func (s PG) ResolveUserDrive(ctx context.Context, userSubjects, groups []string) (
	*types.UserDrive, *types.UserDriveGrant, types.CapabilitySubjectType, error) {
	if userSubjects == nil {
		userSubjects = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	const q = `SELECT d.id, d.name, d.backend, d.host_root, d.storage_class, d.home_template,
			d.size_mib, d.writable, d.reclaim, d.created_at, d.updated_at, d.created_by,
			g.id, g.subject_type, g.subject, g.drive_id, g.priority,
			g.size_mib_override, g.writable_override, g.home_override, g.enabled,
			g.created_at, g.created_by
		FROM user_drive_grants g
		JOIN user_drives d ON d.id = g.drive_id
		WHERE (g.subject_type = 'all'
		   OR (g.subject_type = 'user'  AND g.subject = ANY($1::text[]))
		   OR (g.subject_type = 'group' AND g.subject = ANY($2::text[])))
		ORDER BY
			CASE g.subject_type WHEN 'user' THEN 0 WHEN 'group' THEN 1 ELSE 2 END,
			CASE g.subject_type WHEN 'user'
				THEN COALESCE(array_position($1::text[], g.subject), 2147483647)
				ELSE 0 END,
			g.priority DESC,
			d.name ASC
		LIMIT 1`
	var d types.UserDrive
	var g types.UserDriveGrant
	dest := append(userDriveDest(&d), userDriveGrantDest(&g)...)
	err := s.Pool.QueryRow(ctx, q, userSubjects, groups).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, "", ErrNotFound
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("store: resolve user drive: %w", err)
	}
	return &d, &g, g.SubjectType, nil
}

// HasGroupTierDriveGrants reports whether ANY group-tier grant exists.
//
// It is the gate on the stale/truncated-snapshot refusal, and it is a separate,
// deliberately cheap read because that refusal must fire on exactly one
// deployment shape. A caller whose group snapshot is missing or truncated
// cannot have their group grants evaluated — but on a deployment with NO
// group-tier rows there is nothing an unknown group could have matched, so
// refusing there would break "no grant ⇒ no drive, exactly as before" for every
// pre-upgrade session and every deployment that allocates by user only.
// EXISTS, not a count: the answer is a boolean and Postgres stops at the first
// row.
func (s PG) HasGroupTierDriveGrants(ctx context.Context) (bool, error) {
	var has bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_drive_grants WHERE subject_type = 'group')`).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: check group-tier user drive grants: %w", err)
	}
	return has, nil
}

func scanUserDrive(row pgx.Row) (types.UserDrive, error) {
	var d types.UserDrive
	err := row.Scan(userDriveDest(&d)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.UserDrive{}, ErrNotFound
	}
	if err != nil {
		return types.UserDrive{}, fmt.Errorf("store: scan user drive: %w", err)
	}
	return d, nil
}

func scanUserDriveGrant(row pgx.Row) (types.UserDriveGrant, error) {
	var g types.UserDriveGrant
	err := row.Scan(userDriveGrantDest(&g)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.UserDriveGrant{}, ErrNotFound
	}
	if err != nil {
		return types.UserDriveGrant{}, fmt.Errorf("store: scan user drive grant: %w", err)
	}
	return g, nil
}
