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
	`size_mib, writable, reclaim, created_at, updated_at, created_by, object_scheme`

const userDriveGrantCols = `id, subject_type, subject, drive_id, priority, ` +
	`size_mib_override, writable_override, home_override, enabled, created_at, created_by`

// shareBackend is the ONE backend whose object name carries no drive component
// (types.DriveObjectName: `<host_root>/<home>`), so the home_override
// uniqueness guard has to widen its namespace for it. Spliced from the Go
// constant, never a SQL literal, so the two cannot drift.
const shareBackend = string(types.DriveBackendHostPath)

// driveHomeNamespaceClashPred is the cross-row rule UpsertUserDrive is gated on
// — "another share on this root derives home directory NAMES by a different
// rule" — with its three parameter positions left open, shared by the writing
// statement and the attribution read below so the rule can't drift between them.
const driveHomeNamespaceClashPred = `EXISTS (
			SELECT 1
			FROM user_drives od
			WHERE od.id <> %s::uuid
			  AND od.backend = '` + shareBackend + `'
			  AND od.host_root = %s::text
			  AND od.home_template <> %s::text
		)`

// driveHomeNamespaceClashSQL asks that predicate on its own, for the attribution
// of a refused write (upsertUserDriveOn).
var driveHomeNamespaceClashSQL = `SELECT ` + fmt.Sprintf(driveHomeNamespaceClashPred, "$1", "$2", "$3")

// userDriveUpsertSQL is the write itself. Two guards ride its WHERE: the
// PRECONDITION the API's re-home gate hands in ($12, asserted under the row
// lock UpsertUserDrive takes), and the cross-row home-namespace rule above. An
// empty result means one of them refused, never that the row is missing.
// object_scheme is the ONE column this statement never takes a caller value
// for (types.DriveObjectScheme's doc): INSERT writes the literal 'id', and the
// ON CONFLICT SET references the TARGET table's own current value, never
// EXCLUDED.object_scheme — a row minted here is 'id' from creation and stays
// whatever it already was on every later edit.
var userDriveUpsertSQL = `
		INSERT INTO user_drives (id, name, backend, host_root, storage_class,
			home_template, size_mib, writable, reclaim, created_by, name_slug, object_scheme)
		SELECT $1::uuid,$2::text,$3::text,$4::text,$5::text,$6::text,$7::int,$8::boolean,$9::text,$10::text,$11::text,'id'::text
		WHERE (NOT $12::boolean OR NOT EXISTS (
			SELECT 1 FROM user_drive_grants WHERE drive_id = $1::uuid
		))
		  AND ($3::text <> '` + shareBackend + `' OR $4::text = '' OR NOT ` +
	fmt.Sprintf(driveHomeNamespaceClashPred, "$1", "$4", "$6") + `)
		ON CONFLICT (id) DO UPDATE
			SET name = EXCLUDED.name, backend = EXCLUDED.backend,
			    host_root = EXCLUDED.host_root, storage_class = EXCLUDED.storage_class,
			    home_template = EXCLUDED.home_template, size_mib = EXCLUDED.size_mib,
			    writable = EXCLUDED.writable, reclaim = EXCLUDED.reclaim,
			    name_slug = EXCLUDED.name_slug, updated_at = now(),
			    object_scheme = user_drives.object_scheme
		RETURNING ` + userDriveCols

// userDriveDest is the scan target list for userDriveCols, written ONCE so the
// column list and destinations cannot drift.
func userDriveDest(d *types.UserDrive) []any {
	return []any{&d.ID, &d.Name, &d.Backend, &d.HostRoot, &d.StorageClass, &d.HomeTemplate,
		&d.SizeMiB, &d.Writable, &d.Reclaim, &d.CreatedAt, &d.UpdatedAt, &d.CreatedBy, &d.ObjectScheme}
}

// userDriveGrantDest is the same for userDriveGrantCols. writable_override is
// scanned through a **bool because the column is NULLABLE and NULL is NOT
// false: NULL means "use the drive's posture", an explicit false narrows this
// subject to read-only.
func userDriveGrantDest(g *types.UserDriveGrant) []any {
	return []any{&g.ID, &g.SubjectType, &g.Subject, &g.DriveID, &g.Priority,
		&g.SizeMiBOverride, &g.WritableOverride, &g.HomeOverride, &g.Enabled,
		&g.CreatedAt, &g.CreatedBy}
}

// driveNameSlugIndex is the partial unique index migration 0061 puts on
// user_drives.name_slug — the fragment every minted object name is built from.
const driveNameSlugIndex = "user_drives_name_slug_uniq"

// userDriveUniqueConflict translates a unique-violation from the drive write
// into the sentinel that carries the right REMEDY, or nil when err is not one.
//
// Told apart by constraint name: UNIQUE(name) means the name is taken, pick
// another; the 0061 index means the name is free and its SLUG collides with
// an existing one ("Corp NAS" vs "corp nas") — an admin handed the "name
// already exists" message would go looking for a row that does not exist.
//
// A 23505 from neither index still maps to ErrConflict: the honest status is
// 409, and inventing a fourth outcome for a constraint nobody has added yet
// would be the guess this function exists to stop making.
func userDriveUniqueConflict(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return nil
	}
	if pgErr.ConstraintName == driveNameSlugIndex {
		return ErrDriveSlugConflict
	}
	return ErrConflict
}

// UpsertUserDrive writes one drive, keyed on its PRIMARY KEY: a caller-minted
// id that does not exist yet INSERTs, one that does UPDATEs in place (name
// included, so a drive can be renamed while grants still point at it, which
// they must be able to since ON DELETE RESTRICT makes delete-and-recreate
// impossible for an allocated drive).
//
// Renaming a drive moves a PVC's name — a documented consequence, not a bug:
// types.DriveObjectName folds the name into the object name, so a renamed
// drive's members bind an object that does not exist yet. The runbook
// (docs/OPERATIONS.md, "User drives on Kubernetes") is `kubectl get pvc -l
// wardyn.drive=<id>` plus the preview endpoint.
//
// An identity-affecting PUT on a drive that already has grants (backend,
// home_template, host_root or name) is refused 409 by driveRehomeGuard unless
// the request carries ?confirm=rehome; the rule stays at the API boundary, and
// what this statement carries is the guard's PRECONDITION (refuseIfAllocated)
// re-asserted where the write happens — without it, a grant created between
// the guard's read and this write would be re-homed silently. See
// ErrDriveAllocated.
//
// Returns ErrConflict when UNIQUE(name) rejects the write.
//
// And the cross-row guard: two shares over one root must agree on how a home
// is named. A share's object name (`<host_root>/<home>`) carries no drive
// component, so on host_path the NAMESPACE a home name must be unique in is
// the host_root, not the drive. UpsertUserDriveGrant already closes the half
// an admin can type (the same home_override on two same-root shares); this
// closes the DERIVED half.
//
// The refusal is template disagreement, not an equal root: two host_path
// drives on one root stay legal (a read-write and a read-only view of one
// tree, say). What's refused is a pair that makes the derivation
// non-injective ACROSS the two rows — equal templates keep each principal
// mapped to their own home, but different templates (`sub` on one,
// `email_local` on the other) can fold two different principals onto one
// name, handing them the same directory with every bind-time assertion
// passing. That's migration 0059's stated invariant ("one home directory
// belongs to one principal") failing on the backend its index can't reach.
//
// Residual: `email_local` folding two addresses sharing a local part onto one
// home is a property of the template itself, true on a SINGLE drive too, so
// it's not this guard's shape — it belongs to types.ValidateUserDrive, which
// already refuses `email_local` on a managed backend for the same reason.
//
// In the statement, not a read before it, so the window between "no other
// root-mate disagrees" and the write is one statement. Still not a
// constraint — two concurrent registrations can both pass NOT EXISTS under
// READ COMMITTED — but that's not the case that happens (one admin
// registering a second view of a tree).
//
// od.id <> the row being written, so an UPDATE never trips over itself; ON
// CONFLICT is gated by the same WHERE, so an edit that moves a drive onto a
// colliding root is refused on the same terms as a create.
//
// created_by/created_at are NOT touched on the update path: provenance stays
// with whoever registered the drive (the same rule UpsertGovernanceProfile
// follows).
func (s PG) UpsertUserDrive(ctx context.Context, d types.UserDrive, refuseIfAllocated bool) (types.UserDrive, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if !refuseIfAllocated {
		return s.upsertUserDriveOn(ctx, s.Pool, d, false)
	}
	// The predicate alone is not the guard — the row lock is.
	//
	// NOT EXISTS over user_drive_grants sees the statement's own snapshot; a
	// concurrent grant INSERT that hasn't committed isn't in it, and the FK's
	// FOR KEY SHARE plus this upsert's FOR NO KEY UPDATE don't conflict — so
	// without the lock below, a grant committing right after the predicate
	// ran was still re-homed silently.
	//
	// FOR UPDATE conflicts with FOR KEY SHARE, so after this line a
	// concurrent grant INSERT has either committed (and the predicate,
	// re-read under READ COMMITTED, sees it) or waits behind us until we
	// commit — either way the predicate becomes race-free.
	//
	// Read committed is pinned rather than inherited: under REPEATABLE READ
	// the snapshot is taken before the lock is granted, which would put the
	// pre-lock snapshot back in charge and undo the lock's purpose.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.UserDrive{}, fmt.Errorf("store: begin user drive write: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	// No row yet means a PUT that CREATES, and nothing can reference a drive
	// that doesn't exist (the FK), so an empty lock is correct.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM user_drives WHERE id = $1 FOR UPDATE`, d.ID); err != nil {
		return types.UserDrive{}, fmt.Errorf("store: lock user drive: %w", err)
	}
	out, err := s.upsertUserDriveOn(ctx, tx, d, true)
	if err != nil {
		return types.UserDrive{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.UserDrive{}, fmt.Errorf("store: commit user drive write: %w", err)
	}
	return out, nil
}

// driveQuerier is the pool and a transaction at the one method this file needs
// from either, so the statement below is written once and runs on both: the
// guarded path inside a tx holding the drive's row lock, the ordinary path
// straight on the pool.
type driveQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// upsertUserDriveOn is UpsertUserDrive's statement, plus the attribution of a
// refusal to whichever of its two guards emptied it.
func (s PG) upsertUserDriveOn(ctx context.Context, q driveQuerier, d types.UserDrive, refuseIfAllocated bool) (types.UserDrive, error) {
	out, err := scanUserDrive(q.QueryRow(ctx, userDriveUpsertSQL,
		d.ID, d.Name, d.Backend, d.HostRoot, d.StorageClass,
		d.HomeTemplate, d.SizeMiB, d.Writable, d.Reclaim, d.CreatedBy,
		types.DriveSlug(d.Name), refuseIfAllocated))
	if err != nil {
		if conflict := userDriveUniqueConflict(err); conflict != nil {
			return types.UserDrive{}, conflict
		}
		// No rows means one of the two WHERE guards fired, and nothing else
		// (23505 is handled above; the primary key is absorbed by ON
		// CONFLICT). scanUserDrive folds ErrNoRows to ErrNotFound for READ
		// callers, the wrong word here — the row was refused, not missing.
		//
		// Which guard refused costs one read on this already-failing path,
		// and the DURABLE blocker is asked FIRST: the allocation guard is
		// transient (re-send, confirm), the home-name guard is a stored
		// contradiction that would refuse the confirmed re-home too. This
		// read is not part of the DECISION (the statement above already made
		// it, under the row lock), so it can't reintroduce the window.
		if errors.Is(err, ErrNotFound) {
			// Namespace-first for a share: the durable blocker before the
			// transient one.
			namespacePossible := string(d.Backend) == shareBackend && d.HostRoot != ""
			if namespacePossible && s.driveHomeNamespaceClash(ctx, q, d) {
				return types.UserDrive{}, ErrDriveHomeNamespaceConflict
			}
			if refuseIfAllocated && s.driveHasGrants(ctx, q, d.ID) {
				return types.UserDrive{}, ErrDriveAllocated
			}
			// When only one guard was in the statement, the answer is that
			// guard's, whatever the re-read says (both fail closed to false
			// on error — the fallback decides the MESSAGE, never the write).
			if refuseIfAllocated && !namespacePossible {
				return types.UserDrive{}, ErrDriveAllocated
			}
			return types.UserDrive{}, ErrDriveHomeNamespaceConflict
		}
		return types.UserDrive{}, err
	}
	return out, nil
}

// driveHasGrants reports whether any allocation points at this drive — one
// half of the attribution above. A read that FAILS answers false, so the
// caller falls back to the other refusal's sentence: both are 409s, and the
// fallback decides the MESSAGE, never the write.
func (s PG) driveHasGrants(ctx context.Context, q driveQuerier, id uuid.UUID) bool {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_drive_grants WHERE drive_id = $1)`, id).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// driveHomeNamespaceClash is the other half, asked through the SAME predicate
// the writing statement is gated on, not a second spelling of the rule.
func (s PG) driveHomeNamespaceClash(ctx context.Context, q driveQuerier, d types.UserDrive) bool {
	var clash bool
	if err := q.QueryRow(ctx, driveHomeNamespaceClashSQL, d.ID, d.HostRoot, d.HomeTemplate).Scan(&clash); err != nil {
		return false
	}
	return clash
}

// GetUserDrive returns one drive by id, or ErrNotFound.
func (s PG) GetUserDrive(ctx context.Context, id uuid.UUID) (types.UserDrive, error) {
	const q = `SELECT ` + userDriveCols + ` FROM user_drives WHERE id = $1`
	return scanUserDrive(s.Pool.QueryRow(ctx, q, id))
}

// DeleteUserDrive removes one drive by id. ErrNotFound when no row matched.
//
// ErrConflict when the drive is still ALLOCATED: user_drive_grants.drive_id is
// ON DELETE RESTRICT, so Postgres refuses (23503) rather than cascading the
// grants away — those directories would still hold somebody's work, now
// unreachable and unaudited.
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
// bound to it — the console's whole table in one read. Name, not creation
// order, since that's the handle an admin looks for.
//
// The count rides along rather than a second round trip per row because it's
// what makes the delete affordance honest — a drive with grants answers 409,
// and an admin should see that before clicking. A grouped subquery (not a
// JOIN with GROUP BY over the product) keeps the drive row single, served by
// user_drive_grants_drive_id_idx.
func (s PG) ListUserDrives(ctx context.Context) ([]types.UserDriveListItem, error) {
	const q = `SELECT d.id, d.name, d.backend, d.host_root, d.storage_class, d.home_template,
			d.size_mib, d.writable, d.reclaim, d.created_at, d.updated_at, d.created_by, d.object_scheme,
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
// row rather than leaving a second one behind — two rows for one subject would
// make "which drive does Bob get" depend on tie-breaks instead of the admin's
// last write.
//
// The returned row carries the row's real id, which on a conflict is the
// EXISTING one, not g.ID.
//
// Returns ErrNotFound when drive_id names no drive (the FK rejects it, 23503).
//
// Returns ErrConflict when ANOTHER subject already holds the SAME
// home_override in the same OBJECT-NAME NAMESPACE. A home_override names ONE
// PERSON'S directory (ValidateUserDriveGrant refuses one on a group/all row
// for the same reason), and two user rows carrying one override is that same
// isolation loss with two rows instead of one. It matters most on a MANAGED
// backend, where the override is the only remaining way to name a minted
// object non-injectively — without this guard Wardyn creates one volume and
// binds it into two people's sandboxes.
//
// The namespace is the object name's, not the row's. Scoping to drive_id
// alone (what migration 0059's index does) states the rule over the row that
// happens to carry the name rather than over what the name must be unique IN
// — the two coincide only on a MANAGED backend, since a host_path (share)
// name (`<host_root>/<home>`) has no drive component. So on shares, drive_id
// scope was the wrong question: two share drives on ONE host_root (a shape
// internal/api's driveHostRootNesting deliberately permits) could carry the
// same override once each, handing two principals one absolute host
// directory with every bind-time assertion passing. The guard now asks the
// question in the namespace's own terms: same drive, OR two host_path drives
// over the same host_root.
//
// host_root is compared as the STORED STRING — the same comparison
// DriveObjectName makes, since types.ValidateUserDrive refuses a non-cleaned
// host_root. What it can't see is two textually different roots resolving to
// one tree through a symlink; that needs the filesystem, which
// driveHostRootNesting resolves on the drive-write path. Residual: a DERIVED
// home (no override) can still collide across two same-root shares with
// different templates, since a share's derived name is a claim substring, not
// a digest — refusing that here isn't possible (a group/all grant covers
// principals not known until sign-in); it's a design decision about whether
// two shares may share a root, owned elsewhere.
//
// In the statement, not a read before it: one round trip, so the window
// between "nobody else holds this name" and the write is one statement. Still
// not a constraint (two concurrent inserts can both pass NOT EXISTS under READ
// COMMITTED) — migration 0059's partial unique index is the race-free floor
// for the SAME-DRIVE half; the cross-drive half has no index form (a unique
// index can't span two tables), so on shares this statement is the whole
// guard, closing the case that actually happens: one admin, one name, typed
// twice.
//
// Enabled is written VERBATIM, so a zero-value grant is a DISABLED one — the
// fail-closed half, putting "new grants are on by default" at the API write
// boundary where the omission can still be seen.
//
// And the second guard in the same statement: a silent re-home. home_override
// IS an identity field — clearing one re-homes that person (their next run
// mounts the derived name instead, and the object holding their work is left
// behind unnamed). The DRIVE SLUG rides both halves because it's in the
// minted name; only <home> moves when an override clears, and writing the
// pair without the slug would read as though the whole name changed. This is
// the SAME act driveRehomeGuard answers 409 for on the drive row, with no
// counterpart here — ON CONFLICT replaced every override wholesale, so a POST
// that only meant to change priority cleared it too.
//
// homeOverrideStated is the request's tri-state, decided at the API boundary
// where "the client said nothing" is still visible. FALSE refuses the DO
// UPDATE on a row that pins a name — no row comes back, read as ErrConflict
// and answered 409 naming the remedy (state the field); stating it IS the
// confirmation, no ?confirm= to invent.
//
// In the statement, not a read before it, for the same reason as the
// uniqueness guard: one round trip — strictly better than driveRehomeGuard's
// own conceded read-then-write race.
//
// The zero value is the safe one: a caller that forgets the argument passes
// false, which REFUSES the clear rather than performing it.
func (s PG) UpsertUserDriveGrant(ctx context.Context, g types.UserDriveGrant, homeOverrideStated bool) (types.UserDriveGrant, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	const q = `
		INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, priority,
			size_mib_override, writable_override, home_override, enabled, created_by)
		SELECT $1::uuid,$2::text,$3::text,$4::uuid,$5::int,$6::int,$7::boolean,$8::text,$9::boolean,$10::text
		WHERE $8::text = '' OR NOT EXISTS (
			SELECT 1
			FROM user_drive_grants og
			JOIN user_drives od ON od.id = og.drive_id
			JOIN user_drives nd ON nd.id = $4::uuid
			WHERE og.home_override = $8::text
			  AND NOT (og.subject_type = $2::text AND og.subject = $3::text)
			  AND (og.drive_id = $4::uuid
			       OR (nd.backend = '` + shareBackend + `' AND od.backend = '` + shareBackend + `'
			           AND nd.host_root <> '' AND od.host_root = nd.host_root))
		)
		ON CONFLICT (subject_type, subject) DO UPDATE
			SET drive_id = EXCLUDED.drive_id, priority = EXCLUDED.priority,
			    size_mib_override = EXCLUDED.size_mib_override,
			    writable_override = EXCLUDED.writable_override,
			    home_override = EXCLUDED.home_override, enabled = EXCLUDED.enabled
			WHERE $11::boolean OR user_drive_grants.home_override = ''
		RETURNING ` + userDriveGrantCols
	out, err := scanUserDriveGrant(s.Pool.QueryRow(ctx, q,
		g.ID, g.SubjectType, g.Subject, g.DriveID, g.Priority,
		g.SizeMiBOverride, g.WritableOverride, g.HomeOverride, g.Enabled, g.CreatedBy,
		homeOverrideStated))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return types.UserDriveGrant{}, ErrNotFound
		}
		// 23505 is the guard above, won by the database instead: migration
		// 0059's partial unique index is what actually stops the second of
		// two concurrent inserts that both passed NOT EXISTS under READ
		// COMMITTED. This must answer the same refusal the single-threaded
		// path does, or the loser of the race gets a 500 and a raw driver
		// string.
		//
		// Not deterministically testable (same residual as the mount
		// TOCTOU): the guard closes every single-threaded case, so no
		// fixture can reach the index; internal/db's own test asserts the
		// SQLSTATE at the database, and this maps it.
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.UserDriveGrant{}, ErrConflict
		}
		// No rows is one of the two guards above and nothing else (the FK is
		// handled just above, the natural key absorbed by ON CONFLICT).
		// scanUserDriveGrant folds ErrNoRows to ErrNotFound for READ
		// callers, wrong here.
		//
		// Which of the two is decidable without a second read: they're
		// mutually exclusive by construction (the INSERT guard
		// short-circuits on an empty home_override, and an unstated one is
		// always empty; the DO UPDATE guard is disabled by a STATED one), so
		// homeOverrideStated alone tells them apart.
		if errors.Is(err, ErrNotFound) {
			return types.UserDriveGrant{}, ErrConflict
		}
		return types.UserDriveGrant{}, err
	}
	return out, nil
}

// DeleteUserDriveGrant removes one grant by id AND RETURNS IT, ErrNotFound when
// no row matched. De-allocating is the SUPPORTED way to take a drive away, and
// it removes only the BINDING: nothing deletes the volume, claim or
// directory.
//
// RETURNING rather than a bare Exec: the one caller has to describe what it
// just removed in an audit row, and the row is gone by then — replacing a
// full ListUserDriveGrants scan taken beforehand.
func (s PG) DeleteUserDriveGrant(ctx context.Context, id uuid.UUID) (types.UserDriveGrant, error) {
	const q = `DELETE FROM user_drive_grants WHERE id = $1 RETURNING ` + userDriveGrantCols
	return scanUserDriveGrant(s.Pool.QueryRow(ctx, q, id))
}

// ListUserDriveGrants returns every grant in the order the resolver itself
// would rank them (tier, then priority, then subject) so the console's table
// reads top-down as the precedence rule, not insertion order.
func (s PG) ListUserDriveGrants(ctx context.Context) ([]types.UserDriveGrant, error) {
	return collect(ctx, s.Pool, "list", "user drive grants", userDriveGrantList, nil, scanUserDriveGrant)
}

// userDriveGrantList is the grant read WITHOUT its window, written once so the
// whole-list and paged forms cannot drift into two different orders.
const userDriveGrantList = `SELECT ` + userDriveGrantCols + ` FROM user_drive_grants
		ORDER BY ` + subjectTierOrder + `, priority DESC, subject`

// ListUserDriveGrantsPage is ListUserDriveGrants bounded to one window.
//
// The limit changes the plan, not just the result size: this ORDER BY has no
// serving index, so unbounded it's a Seq Scan feeding a full sort (measured
// on Postgres 17, 50,000 rows: `external merge Disk: 5584kB`, 149.7 ms). The
// caller's LIMIT turns it into a top-N heapsort (`Memory: 301kB`, 37.7 ms, no
// temp file), since Postgres only keeps the best k rows. The Seq Scan itself
// stays; removing it needs an index, which is a migration.
func (s PG) ListUserDriveGrantsPage(ctx context.Context, p Page) ([]types.UserDriveGrant, error) {
	q, args := p.appendTo(userDriveGrantList, nil)
	return collect(ctx, s.Pool, "list", "user drive grants", q, args, scanUserDriveGrant)
}

// ResolveUserDrive returns THE ONE drive that applies to a caller, the grant
// that won, and the tier it won at — or ErrNotFound (the absent-row doctrine).
//
// The whole precedence rule is the ORDER BY: subjectMatch and
// subjectPrecedence, the fragments ResolveGovernanceProfile splices too,
// because it is the same rule about the same subject vocabulary. It is one
// indexed read on the UNIQUE(subject_type, subject) btree, so there is no second
// implementation in Go for a caller to skip, mis-order, or forget. The last key
// (grants.subject) matters more here than for the ceiling: THIS RESOLVER
// RETURNS THE GRANT, and the grant is what carries writable_override,
// size_mib_override, home_override and enabled.
//
// Disabled grants are in the query, and the winner's own `enabled` decides
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
//
// userType is the caller's one type id; "" matches no row.
func (s PG) ResolveUserDrive(ctx context.Context, userSubjects, groups []string, userType string) (
	*types.UserDrive, *types.UserDriveGrant, types.CapabilitySubjectType, error) {
	if userSubjects == nil {
		userSubjects = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	q := `SELECT d.id, d.name, d.backend, d.host_root, d.storage_class, d.home_template,
			d.size_mib, d.writable, d.reclaim, d.created_at, d.updated_at, d.created_by, d.object_scheme,
			g.id, g.subject_type, g.subject, g.drive_id, g.priority,
			g.size_mib_override, g.writable_override, g.home_override, g.enabled,
			g.created_at, g.created_by
		FROM user_drive_grants g
		JOIN user_drives d ON d.id = g.drive_id
		WHERE ` + subjectMatch("g") + `
		ORDER BY ` + subjectPrecedence("g", "d") + `
		LIMIT 1`
	var d types.UserDrive
	var g types.UserDriveGrant
	dest := append(userDriveDest(&d), userDriveGrantDest(&g)...)
	err := s.Pool.QueryRow(ctx, q, userSubjects, groups, userType).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, "", ErrNotFound
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("store: resolve user drive: %w", err)
	}
	return &d, &g, g.SubjectType, nil
}

// HasGroupTierDriveGrants reports whether ANY group-tier grant exists: the
// gate on the stale-snapshot refusal (see hasGroupTierRows).
func (s PG) HasGroupTierDriveGrants(ctx context.Context) (bool, error) {
	return s.hasGroupTierRows(ctx, "user_drive_grants", "user drive grants")
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
