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

// shareBackend is the ONE backend whose object name carries no drive component
// (types.DriveObjectName: `<host_root>/<home>`), which is why the home_override
// uniqueness guard has to widen its namespace for it. Spliced from the Go
// constant rather than typed as a SQL literal so the two cannot drift — it is a
// compile-time constant from this repo's own closed enum, never caller input,
// and the column it is compared against is written from the same enum.
const shareBackend = string(types.DriveBackendHostPath)

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
// into a k8s claim name AND a Docker volume name, so a renamed drive's members
// bind an object that does not exist yet and a managed drive provisions a fresh
// empty one. The alternative
// — a second immutable slug column — buys stability for the one field an admin
// most needs to be able to correct, and the operator runbook for a rename is
// `kubectl get pvc -l wardyn.drive=<id>` (the label carries the row id, so the
// orphans stay findable) plus the preview endpoint, which prints the exact
// object name for a principal. docs/OPERATIONS.md, "User drives on Kubernetes",
// is that runbook.
//
// THE HANDLER NO LONGER WRITES THAT UNCONDITIONALLY. An identity-affecting PUT
// on a drive that already has grants — backend, home_template, host_root or
// name, the four columns every allocated person's storage object is derived
// from — is refused 409 by driveRehomeGuard unless the request carries
// ?confirm=rehome. This statement stays unconditional and must: the gate belongs
// at the API boundary, where the request that asked for it is, and a store that
// re-read the grants on every write would be a second, quieter copy of a rule
// that already has one.
//
// Returns ErrConflict when UNIQUE(name) rejects the write — a new drive taking
// a taken name, or a rename onto another row's name. The caller maps that to
// 409 with the name in the message, never a raw driver error (the CreatePolicy
// contract).
//
// ─── AND THE CROSS-ROW GUARD: TWO SHARES OVER ONE ROOT MUST AGREE ON HOW A
// HOME IS NAMED ─────────────────────────────────────────────────────────────
//
// A share's object name is `<host_root>/<home>` (types.DriveObjectName) — no
// drive component at all, because the directory was named by whoever owns the
// tree. So on host_path the NAMESPACE a home name has to be unique in is the
// host_root, not the drive. UpsertUserDriveGrant already closes the half an
// admin can type: the same home_override on two same-root shares. This closes
// the half nobody types — the DERIVED one, which that guard's own doc names as
// its residual and hands to "where the decision lives", i.e. here.
//
// THE REFUSAL IS TEMPLATE DISAGREEMENT, NOT AN EQUAL ROOT. Two host_path drives
// on one root stay legal, because that is a real deployment shape and the one
// driveHostRootNesting deliberately permits: a read-write and a read-only view
// of /srv/homes, or two size ceilings over it. What is refused is the pair that
// makes the derivation non-injective ACROSS the two rows. With equal templates
// it cannot be: `sub` maps each principal to its own subject, so equal homes
// mean the same person and the same directory, which is the correct answer.
// Give the two drives DIFFERENT templates and it collapses — drive A on `sub`
// and drive B on `email_local`, a member whose sub is "alice" and a member whose
// address is alice@corp.example both derive "alice", and two principals are
// handed /srv/homes/alice read-write with every bind-time assertion passing,
// because each allocation is individually legitimate. That is migration 0059's
// stated invariant ("one home directory belongs to one principal") failing on
// the backend its index cannot reach.
//
// RESIDUAL, stated rather than implied: `email_local` folds two principals whose
// addresses share the part before the "@" onto one home. That fold is a property
// of the template itself and happens on a SINGLE drive just as readily, so it is
// not this guard's shape and closing it here would leave `sub` as the only
// authorable share template; it belongs to the template rules in
// types.ValidateUserDrive, which already refuses `email_local` on a managed
// backend for exactly that reason.
//
// IN THE STATEMENT, not in a read before it, for the reason
// UpsertUserDriveGrant's guard is: one round trip rather than two, so the window
// between "no other root-mate names homes differently" and the write is a single
// statement. It is still not a constraint — a cross-row rule over one table
// cannot be an index — so two concurrent registrations can both pass NOT EXISTS
// under READ COMMITTED. The case that actually happens is one admin registering
// a second view of a tree, and that is closed.
//
// od.id <> the row being written, so an UPDATE never trips over itself; and the
// ON CONFLICT (id) DO UPDATE is gated by the same WHERE, so an EDIT that moves a
// drive onto a colliding root, or changes its template into disagreement with
// its root-mates, is refused on the same terms as a create.
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
		SELECT $1::uuid,$2::text,$3::text,$4::text,$5::text,$6::text,$7::int,$8::boolean,$9::text,$10::text
		WHERE $3::text <> '` + shareBackend + `' OR $4::text = '' OR NOT EXISTS (
			SELECT 1
			FROM user_drives od
			WHERE od.id <> $1::uuid
			  AND od.backend = '` + shareBackend + `'
			  AND od.host_root = $4::text
			  AND od.home_template <> $6::text
		)
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
		// NO ROWS IS THE CROSS-ROW GUARD ABOVE AND NOTHING ELSE. The SELECT is a
		// row of constants, so only its WHERE can empty it; UNIQUE(name) raises
		// 23505 (handled just above) and the primary key is absorbed by ON
		// CONFLICT. scanUserDrive folds pgx.ErrNoRows into ErrNotFound for the
		// READ callers that share it, which on THIS statement would be the wrong
		// word to hand a caller — the row it asked to write is not missing, it
		// was refused.
		if errors.Is(err, ErrNotFound) {
			return types.UserDrive{}, ErrDriveHomeNamespaceConflict
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
// Returns ErrConflict when ANOTHER subject already holds the SAME
// home_override in the same OBJECT-NAME NAMESPACE. A home_override names ONE
// PERSON'S directory — that is the whole reason a group or all row may not carry
// one (ValidateUserDriveGrant: "would hand every member of that group the SAME
// directory — the isolation a per-user subdirectory buys") — and two user rows
// carrying one override is that same loss spelled with two rows instead of one.
// It matters most on a MANAGED backend, where the override is the only
// remaining way to name a minted object non-injectively (the home_template arm
// is refused by types.ValidateUserDrive, and a hash home folds the subject), so
// without this guard Wardyn itself creates one volume and binds it into two
// people's sandboxes, read-write wherever the allocations are writable.
//
// ─── THE NAMESPACE IS THE OBJECT NAME'S, NOT THE ROW'S ────────────────────────
//
// Scoping this to drive_id — which is what it and migration 0059's index both
// did — states the rule over the row that happens to carry the name rather than
// over the thing the name has to be unique IN, and the two are only the same on
// a MANAGED backend. types.DriveObjectName is explicit about it: a managed name
// is `wardyn-drive-<drive-slug>-<home>`, so the drive is IN the name and
// drive_id is exactly right; a host_path (share) name is `<host_root>/<home>`,
// with no drive component at all, because the directory was named by whoever
// owns the tree and a slug there would name a directory that does not exist.
//
// So on shares the drive_id scope was the wrong question. Two share drives on
// ONE host_root — the shape internal/api's driveHostRootNesting deliberately
// permits, since equal roots are a naming question and only NESTED roots are a
// containment one — let the same override be typed once on each, and the two
// principals were handed one absolute host directory: both bind it at
// /home/agent/drive, read-write wherever their allocation is writable, and
// every bind-time assertion passes because each allocation is individually
// legitimate. The guard now asks the question in the namespace's own terms:
// same drive, OR two host_path drives over the same host_root.
//
// host_root is compared as the STORED STRING, which is the same comparison
// DriveObjectName makes: types.ValidateUserDrive refuses a host_root that is not
// already filepath.Clean'd, so equal names here are equal paths. What it does
// not see is two textually different roots that resolve to one tree through a
// symlink; that needs the filesystem, which the store has no access to, and it
// is driveHostRootNesting (which does resolve, on the drive-write path) that
// owns the resolved half. RESIDUAL, stated rather than implied: a DERIVED home
// — no override at all — can still collide across two same-root shares, because
// two drives may carry different home_templates and a share's derived name is a
// claim substring rather than a digest (types.ValidateUserDrive refuses `hash`
// on a share precisely because the directory is not Wardyn's to name). Refusing
// that at THIS boundary is not possible: a group- or all-tier grant covers
// principals whose claims are not known until they sign in. Closing it is a
// design decision about whether two shares may share a root at all, and it
// belongs where that decision lives, not here.
//
// THE GUARD IS IN THE STATEMENT, not in a read before it, so the window between
// "nobody else holds this name" and the write is one statement rather than two
// round trips. It is still not a constraint: two concurrent inserts can both
// pass NOT EXISTS under READ COMMITTED. Migration 0059's partial unique index on
// (drive_id, home_override) is the race-free floor for the SAME-DRIVE half and
// keeps working unchanged; the cross-drive half has no index form, because a
// unique index cannot span two tables (host_root lives on user_drives) — so on
// shares this statement is the whole guard, and the residual is the concurrent
// second writer, the same one driveHostRootNesting states for its own read-then-
// write. Both close the case that actually happens: one admin, one directory
// name, typed twice.
//
// Enabled is written VERBATIM, so a zero-value grant is a DISABLED one. That is
// the fail-closed half (an allocation nobody enabled mounts nothing) and it
// puts the "new grants are on by default" decision at the API write boundary,
// where the request that omitted the field can still be seen — the column's own
// DEFAULT true never applies, because this INSERT always supplies a value.
//
// ─── AND THE SECOND GUARD IN THE SAME STATEMENT: A SILENT RE-HOME ──────────
//
// home_override IS an identity field. The object a member binds is derived
// from it (DriveObjectName over the resolved home), so clearing one re-homes
// that person: on a managed backend their next run mounts
// `wardyn-drive-corp-nas-d-9f2a1c04` instead of `wardyn-drive-corp-nas-bsmith`,
// and the object holding their work is left behind with nothing in Wardyn
// naming it. The DRIVE SLUG is in both halves because it is in the minted name
// — types.DriveObjectName is `wardyn-drive-<drive-slug>-<home>`, and only the
// <home> half moves when an override is cleared; writing the pair without the
// slug read as though the whole name changed, which is a different (and
// larger) act than the one this guard refuses. On a share the same clearing
// re-homes them from `<host_root>/bsmith` to `<host_root>/<derived>`. That is the SAME act driveRehomeGuard answers
// 409 for one surface over, on the drive row — and this write had no
// counterpart, because ON CONFLICT replaced every override wholesale. A POST
// that only meant to change a priority, or to repoint a subject at another
// drive, cleared it.
//
// homeOverrideStated is the request's tri-state, decided at the API boundary
// where "the client said nothing" is still visible (the same thing Enabled's
// *bool exists for, two fields apart in the same request struct). When it is
// FALSE the DO UPDATE is refused on a row that pins a name — no row comes back,
// which the caller reads as ErrConflict and answers 409 naming the remedy:
// state the field. Stating it is the confirmation, so there is no ?confirm= to
// invent and no second spelling of the act.
//
// IN THE STATEMENT, not in a read before it, for the reason the uniqueness
// guard above is: the window between "does this row pin a name" and the write
// is one statement rather than two round trips — which is strictly better than
// driveRehomeGuard, whose own doc concedes its read-then-write race.
//
// THE ZERO VALUE IS THE SAFE ONE. A caller that forgets the argument passes
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
		// 23505 IS THE GUARD ABOVE, WON BY THE DATABASE INSTEAD. Migration 0059
		// added the partial unique index this statement's NOT EXISTS could only
		// approximate — two concurrent inserts can both pass NOT EXISTS under
		// READ COMMITTED, and the index is what actually stops the second. That
		// path must answer the caller with the SAME refusal the single-threaded
		// path does: without this arm the loser of the race gets a 500 and a raw
		// driver string, for the one request the index exists to refuse
		// correctly.
		//
		// NOT DETERMINISTICALLY TESTABLE and nothing here claims to cover it —
		// the same statement residual #25 makes about the mount TOCTOU. The
		// guard closes every single-threaded case, so no fixture can reach the
		// index; internal/db's own test asserts the SQLSTATE at the database,
		// and this maps it.
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.UserDriveGrant{}, ErrConflict
		}
		// NO ROWS is one of the two guards above and nothing else: the FK raises
		// 23503 (handled just above), the natural key is absorbed by ON
		// CONFLICT, and the SELECT is otherwise a row of constants that cannot
		// be empty. It arrives as ErrNotFound because scanUserDriveGrant folds
		// pgx.ErrNoRows into it for the READ callers that share the helper — on
		// THIS statement that reading would be wrong, and the reason is above.
		//
		// WHICH of the two is decidable by the caller without a second read, and
		// the API needs to decide because the two remedies are different
		// sentences. They are mutually exclusive by construction: the INSERT's
		// uniqueness guard short-circuits on an EMPTY home_override, and an
		// unstated one is always empty; the DO UPDATE's re-home guard is
		// disabled outright by a STATED one. So homeOverrideStated alone tells
		// them apart.
		if errors.Is(err, ErrNotFound) {
			return types.UserDriveGrant{}, ErrConflict
		}
		return types.UserDriveGrant{}, err
	}
	return out, nil
}

// DeleteUserDriveGrant removes one grant by id AND RETURNS IT, ErrNotFound when
// no row matched. De-allocating is the SUPPORTED way to take a drive away — the
// deliberate act the RESTRICT on the drive delete exists to force — and it
// removes only the BINDING: nothing in the control plane deletes the volume,
// the claim or the directory (reclaim is a declared intent executed by a
// documented operator command).
//
// RETURNING rather than a bare Exec, because the ONE caller has to describe
// what it just removed in an audit row and the row is gone by then. The
// alternative it replaced was a full ListUserDriveGrants scan taken beforehand:
// every allocation in the deployment loaded to describe one, and a second read
// that could disagree with the row the DELETE actually took. The same shape
// UpsertUserDriveGrant above already uses, over the same shared column list.
func (s PG) DeleteUserDriveGrant(ctx context.Context, id uuid.UUID) (types.UserDriveGrant, error) {
	const q = `DELETE FROM user_drive_grants WHERE id = $1 RETURNING ` + userDriveGrantCols
	return scanUserDriveGrant(s.Pool.QueryRow(ctx, q, id))
}

// ListUserDriveGrants returns every grant in the order the resolver itself
// would rank them (tier, then priority, then subject) so the console's "who
// gets what" table reads top-down as the precedence rule, not as insertion
// order an admin then has to re-sort in their head.
func (s PG) ListUserDriveGrants(ctx context.Context) ([]types.UserDriveGrant, error) {
	return collect(ctx, s.Pool, "list", "user drive grants", userDriveGrantList, nil, scanUserDriveGrant)
}

// userDriveGrantList is the grant read WITHOUT its window, written once so the
// whole-list and paged forms cannot drift into two different orders. A page
// whose ORDER BY differs from the list's is a page that omits rows the caller
// would have seen and repeats others across offsets — the failure a second copy
// of an ORDER BY makes silently.
const userDriveGrantList = `SELECT ` + userDriveGrantCols + ` FROM user_drive_grants
		ORDER BY ` + userDriveTierOrder + `, priority DESC, subject`

// ListUserDriveGrantsPage is ListUserDriveGrants bounded to one window — the
// seventh entry in Pager, and the one user_drive_grants was missing.
//
// THE LIMIT IS NOT A COURTESY, IT CHANGES THE PLAN. This ORDER BY has no index
// to serve it, so unbounded it is a Seq Scan feeding a full sort: measured on
// this deployment's own PostgreSQL 17 at work_mem=4MB with 50,000 allocations,
// `external merge Disk: 5584kB`, 149.7 ms, every row materialised and
// serialised. The same query with the caller's LIMIT becomes a top-N heapsort
// bounded by the window — `Memory: 301kB`, 37.7 ms, no temp file — because
// Postgres only has to keep the best k rows rather than sort all n. The Seq
// Scan (8-11 ms) stays; removing THAT needs an index on the sort keys, which is
// a migration.
//
// One row per SUBJECT (UNIQUE(subject_type, subject)) and two user subjects per
// person, so this table's row count is deployment headcount — the shape
// migration 0054's opening paragraph names as the target, not an edge case.
func (s PG) ListUserDriveGrantsPage(ctx context.Context, p Page) ([]types.UserDriveGrant, error) {
	q, args := p.appendTo(userDriveGrantList, nil)
	return collect(ctx, s.Pool, "list", "user drive grants", q, args, scanUserDriveGrant)
}

// userDriveTierOrder ranks the three subject tiers MOST SPECIFIC FIRST —
// user > group > all. Written once, as SQL, and spliced into BOTH the resolver
// and the console listing so the two can never disagree about what "most
// specific" means. Deliberately a SEPARATE constant from governanceTierOrder
// despite the identical text: these two are the same RULE over different
// tables, and sharing the string would make a future per-table divergence look
// like a typo in a shared const rather than the deliberate change it would have
// to be.
//
// subject_type is deliberately UNQUALIFIED so the one string works in the
// resolver's JOIN as well as the single-table listing. That is safe because
// user_drives has no subject_type column (migration 0054) — the only other
// table in that JOIN. A migration that added one would make this ambiguous, and
// Postgres would say so loudly rather than silently re-rank.
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
//  4. drives.name ASC — applied in EVERY tier, and a total order across
//     DISTINCT drives because drives.name is UNIQUE. It is what decides which
//     of two drives a member reaches when nothing above it separates them.
//  5. grants.subject ASC — THE FLOOR, and the drive's name is not one. Two
//     grants can name the SAME drive: UNIQUE(subject_type, subject) is per
//     SUBJECT, so two groups one member belongs to may each be granted one
//     drive, and priority DEFAULTS to 0 on both — every key above ties, and
//     LIMIT 1 falls to physical row order. That is not academic here, because
//     THIS RESOLVER RETURNS THE GRANT and the grant is what carries
//     writable_override, size_mib_override, home_override and enabled: the
//     losing coin-flip is an admin's explicit read-only narrowing silently not
//     applying, or a paused allocation mounting. Subject is a total order
//     within a tier (the UNIQUE key makes subjects distinct there) and it is
//     EXPLAINABLE, which a row id would not be — "the alphabetically first
//     group's allocation wins" is an answer to "why did Bob get that one".
//
// THE RULE WAS COPIED FROM ResolveGovernanceProfile, WHICH DOES NOT NEED THE
// LAST KEY. governance_assignments carries no per-assignment override — only
// profile_id and priority — so two assignments naming one profile are
// interchangeable and the tie is unobservable. Copying the ORDER BY into a
// table whose rows DO carry per-row overrides is what turned a benign gap into
// a decision. ListUserDriveGrants already ended with `subject`, so the console
// listing and the resolver now agree on the last key rather than only the
// first.
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
			` + userDriveTierOrder + `,
			CASE g.subject_type WHEN 'user'
				THEN COALESCE(array_position($1::text[], g.subject), 2147483647)
				ELSE 0 END,
			g.priority DESC,
			d.name ASC,
			g.subject ASC
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
