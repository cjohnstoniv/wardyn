// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User drives and their subject grants (migration 0054), split out of store.go
// like governance.go. Kept DUMB: round-trips rows, owns only
// ResolveUserDrive's ORDER BY (precedence must live in the one indexed read,
// not a caller). Writes are validated at the API boundary
// (types.ValidateUserDrive, types.ValidateUserDriveGrant).
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

// driveRowCols is userDriveCols plus the member-drive columns: every read that
// can return a member drive scans these, so the owner always travels with it.
// The resolvers keep userDriveCols, since a member drive has no grant to win.
const driveRowCols = userDriveCols + `, owner_subject, delete_started_at, kept_from_run`

const userDriveGrantCols = `id, subject_type, subject, drive_id, priority, ` +
	`size_mib_override, writable_override, home_override, enabled, created_at, created_by`

// shareBackend's object name has no drive component (`<host_root>/<home>`),
// so home_override uniqueness must widen to the host_root namespace for it.
// Spliced from the Go constant so SQL and Go can't drift.
const shareBackend = string(types.DriveBackendHostPath)

// driveHomeNamespaceClashPred: another share on this root derives home names
// by a different template. Parameterized and shared by the write and the
// attribution read below so the rule can't drift between them.
const driveHomeNamespaceClashPred = `EXISTS (
			SELECT 1
			FROM user_drives od
			WHERE od.id <> %s::uuid
			  AND od.backend = '` + shareBackend + `'
			  AND od.host_root = %s::text
			  AND od.home_template <> %s::text
		)`

// driveHomeNamespaceClashSQL runs the predicate alone, to attribute a refused
// write (upsertUserDriveOn).
var driveHomeNamespaceClashSQL = `SELECT ` + fmt.Sprintf(driveHomeNamespaceClashPred, "$1", "$2", "$3")

// userDriveUpsertSQL: WHERE carries two guards — the re-home precondition
// ($12, asserted under UpsertUserDrive's row lock) and the home-namespace rule
// above. An empty result means a guard refused, not that the row is missing.
// object_scheme is never a caller value: INSERT writes 'id', and ON CONFLICT
// SET keeps the target row's existing value rather than EXCLUDED's, so it's
// fixed at creation and never changes on later edits. The DO UPDATE's WHERE
// refuses a member drive (owner_subject set): the admin write path never edits one.
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
			WHERE user_drives.owner_subject = ''
		RETURNING ` + userDriveCols

// userDriveDest is the scan target list for userDriveCols, written ONCE so the
// column list and destinations cannot drift.
func userDriveDest(d *types.UserDrive) []any {
	return []any{&d.ID, &d.Name, &d.Backend, &d.HostRoot, &d.StorageClass, &d.HomeTemplate,
		&d.SizeMiB, &d.Writable, &d.Reclaim, &d.CreatedAt, &d.UpdatedAt, &d.CreatedBy, &d.ObjectScheme}
}

// driveRowDest is the scan target list for driveRowCols.
func driveRowDest(d *types.UserDrive) []any {
	return append(userDriveDest(d), &d.OwnerSubject, &d.DeleteStartedAt, &d.KeptFromRun)
}

// userDriveGrantDest is the same for userDriveGrantCols. writable_override
// scans as **bool: NULL means "use the drive's posture", false narrows to
// read-only — NULL is not false.
func userDriveGrantDest(g *types.UserDriveGrant) []any {
	return []any{&g.ID, &g.SubjectType, &g.Subject, &g.DriveID, &g.Priority,
		&g.SizeMiBOverride, &g.WritableOverride, &g.HomeOverride, &g.Enabled,
		&g.CreatedAt, &g.CreatedBy}
}

// driveNameSlugIndex is the partial unique index migration 0061 puts on
// user_drives.name_slug — the fragment every minted object name is built from.
const driveNameSlugIndex = "user_drives_name_slug_uniq"

// userDriveUniqueConflict maps a unique-violation to the sentinel carrying the
// right remedy, or nil if err isn't one. Told apart by constraint: UNIQUE(name)
// means the name is taken; the 0061 index means the name is free but its slug
// collides ("Corp NAS" vs "corp nas") — a different message, since "name
// exists" would send an admin looking for a row that isn't there. A 23505
// from neither index still maps to ErrConflict (409) rather than guessing a
// fourth outcome for a constraint that doesn't exist yet.
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

// UpsertUserDrive writes one drive keyed on ID: INSERT if absent, else UPDATE in place — renaming
// is allowed while grants still point at it, since ON DELETE RESTRICT rules out delete-and-recreate
// for an allocated drive. A rename moves the PVC's name; existing members bind an object that no
// longer exists until they re-mount (docs/OPERATIONS.md "User drives on Kubernetes"). Returns
// ErrConflict on UNIQUE(name).
//
// An identity-affecting PUT on an already-allocated drive is refused 409 (ErrDriveAllocated) unless
// ?confirm=rehome; the precondition is re-asserted in this statement, under the row lock
// UpsertUserDrive takes, so a grant created between the guard's read and this write can't be
// silently re-homed.
//
// SECURITY: two host_path shares over the same root must derive home names the same way (namespace
// is the host_root, not the drive — see shareBackend). Different home_templates (`sub` vs
// `email_local`) could otherwise fold two principals onto one directory name, breaking migration
// 0059's one-home-per-principal invariant on a backend its index can't reach; same-template drives
// on one root stay legal. Enforced in the statement itself, not a prior read — not a hard constraint
// under READ COMMITTED, but the case that actually happens (one admin registering a second view)
// can't race itself.
//
// created_by/created_at are untouched on update: provenance stays with whoever registered the drive.
func (s PG) UpsertUserDrive(ctx context.Context, d types.UserDrive, refuseIfAllocated bool) (types.UserDrive, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if !refuseIfAllocated {
		return s.upsertUserDriveOn(ctx, s.Pool, d, false)
	}
	// NOT EXISTS alone isn't the guard — the lock is. It sees only the
	// statement's own snapshot; a concurrent grant INSERT that hasn't
	// committed evades it (FOR KEY SHARE and FOR NO KEY UPDATE don't
	// conflict). FOR UPDATE below forces that INSERT to have already
	// committed or wait behind us, making the predicate race-free. READ
	// COMMITTED is pinned, not inherited: REPEATABLE READ would take its
	// snapshot before the lock, undoing the lock's purpose.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.UserDrive{}, fmt.Errorf("store: begin user drive write: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	// No row yet means this PUT creates; nothing can reference a drive that
	// doesn't exist yet, so an empty lock is fine.
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

// driveQuerier is the one method this file needs from either the pool or a
// tx, so the statement below runs unchanged on both the locked and unlocked
// paths.
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
		// No rows means a WHERE guard fired (23505 handled above; PK absorbed by ON CONFLICT) — the
		// row was refused, not missing. Attributing which guard costs one extra read here, on the
		// failing path only; the durable blocker (home-name) is checked before the transient one
		// (allocation) since it would refuse a confirmed re-home too. The decision was already made
		// under the row lock, so this read can't reopen the race window.
		if errors.Is(err, ErrNotFound) {
			namespacePossible := string(d.Backend) == shareBackend && d.HostRoot != ""
			if namespacePossible && s.driveHomeNamespaceClash(ctx, q, d) {
				return types.UserDrive{}, ErrDriveHomeNamespaceConflict
			}
			if refuseIfAllocated && s.driveHasGrants(ctx, q, d.ID) {
				return types.UserDrive{}, ErrDriveAllocated
			}
			// If only one guard applied, that's the answer regardless of the re-read (both fail
			// closed to false on error — this only picks the message, never the write).
			if refuseIfAllocated && !namespacePossible {
				return types.UserDrive{}, ErrDriveAllocated
			}
			return types.UserDrive{}, ErrDriveHomeNamespaceConflict
		}
		return types.UserDrive{}, err
	}
	return out, nil
}

// driveHasGrants reports whether any allocation points at this drive. A
// failed read answers false so the caller falls back to the other 409's
// message — never the write.
func (s PG) driveHasGrants(ctx context.Context, q driveQuerier, id uuid.UUID) bool {
	var exists bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_drive_grants WHERE drive_id = $1)`, id).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// driveHomeNamespaceClash is the other half, using the SAME predicate the
// write is gated on — not a second spelling of the rule.
func (s PG) driveHomeNamespaceClash(ctx context.Context, q driveQuerier, d types.UserDrive) bool {
	var clash bool
	if err := q.QueryRow(ctx, driveHomeNamespaceClashSQL, d.ID, d.HostRoot, d.HomeTemplate).Scan(&clash); err != nil {
		return false
	}
	return clash
}

// GetUserDrive returns one drive by id, or ErrNotFound.
func (s PG) GetUserDrive(ctx context.Context, id uuid.UUID) (types.UserDrive, error) {
	const q = `SELECT ` + driveRowCols + ` FROM user_drives WHERE id = $1`
	return scanDriveRow(s.Pool.QueryRow(ctx, q, id))
}

// DeleteUserDrive removes one admin drive by id. ErrNotFound if no row matched,
// including a member drive, which only the member-drive delete removes;
// ErrConflict if it's still allocated (user_drive_grants.drive_id is ON
// DELETE RESTRICT — Postgres refuses rather than cascading away grants whose
// directories may still hold someone's work).
func (s PG) DeleteUserDrive(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM user_drives WHERE id = $1 AND owner_subject = ''`, id)
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

// ListUserDrives returns every drive by name (the handle an admin looks for)
// with its grant count, in one read — the console's whole table. The count
// rides along so the delete affordance is honest (a drive with grants answers
// 409, visible before clicking); a grouped subquery, not a JOIN, keeps the
// drive row single. Member drives are listed too, for the recovery rehearsal;
// the admin handler filters them out.
func (s PG) ListUserDrives(ctx context.Context) ([]types.UserDriveListItem, error) {
	const q = `SELECT d.id, d.name, d.backend, d.host_root, d.storage_class, d.home_template,
			d.size_mib, d.writable, d.reclaim, d.created_at, d.updated_at, d.created_by, d.object_scheme,
			d.owner_subject, d.delete_started_at, d.kept_from_run, COALESCE(c.n, 0)
		FROM user_drives d
		LEFT JOIN (SELECT drive_id, COUNT(*) AS n FROM user_drive_grants GROUP BY drive_id) c
			ON c.drive_id = d.id
		ORDER BY d.name`
	return collect(ctx, s.Pool, "list", "user drives", q, nil,
		func(row pgx.Row) (types.UserDriveListItem, error) {
			var item types.UserDriveListItem
			dest := append(driveRowDest(&item.UserDrive), &item.GrantCount)
			if err := row.Scan(dest...); err != nil {
				return types.UserDriveListItem{}, fmt.Errorf("store: scan user drive: %w", err)
			}
			return item, nil
		})
}

// UpsertUserDriveGrant allocates one drive to one subject and REPLACES whatever the subject was
// allocated before: the subject ends with exactly this drive, and an existing row is repointed
// rather than a second left beside it, so the admin's last write decides which drive the subject
// gets. Since migration 0169 a subject may hold several rows (AddUserDriveGrant); this keeps one,
// repoints it, and removes the others, in one transaction. On a repoint the returned id is the
// EXISTING row's, not g.ID. Returns ErrNotFound when drive_id names no drive (FK, 23503).
//
// SECURITY: refuses (ErrConflict) if another subject already holds the same home_override in the
// same object-name NAMESPACE (host_root, not drive — see shareBackend) — one home_override must
// name one person's directory, or two people end up sharing a minted volume with every bind-time
// check passing. Guard checks same-drive OR two host_path drives on the same host_root (compared as
// the stored string; driveHostRootNesting handles the symlink case on the drive-write path).
// Enforced in the statement itself, one round trip: migration 0059's partial unique index is the
// race-free floor for the same-drive case; the cross-drive case has none, so here the statement is
// the whole guard — sufficient for the case that happens, one admin typing one name twice. Residual:
// a DERIVED (non-override) home can still collide across same-root shares with different templates;
// not refusable here since a group/all grant doesn't know its principals until sign-in.
//
// enabled is written verbatim, so a zero-value grant is disabled by default — fail-closed at the API
// boundary, where the omission is still visible.
//
// SECURITY: also guards a silent re-home. Clearing home_override moves that person to the derived
// name next run, stranding their old directory unnamed. homeOverrideStated is the API's tri-state for
// "the client said nothing": false refuses the update on a row with a stated override (no row
// returned, read as ErrConflict); stating it IS the confirmation, no ?confirm= to invent. The zero
// value is safe — a caller that forgets the argument gets a refusal, not a silent clear.
func (s PG) UpsertUserDriveGrant(ctx context.Context, g types.UserDriveGrant, homeOverrideStated bool) (types.UserDriveGrant, error) {
	out, _, err := s.ReplaceUserDriveGrant(ctx, g, homeOverrideStated)
	return out, err
}

// ReplaceUserDriveGrant is UpsertUserDriveGrant that also returns the drives the subject lost:
// the drive ids of the rows it removed and of the kept row's drive before the repoint.
func (s PG) ReplaceUserDriveGrant(ctx context.Context, g types.UserDriveGrant, homeOverrideStated bool) (types.UserDriveGrant, []uuid.UUID, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.UserDriveGrant{}, nil, fmt.Errorf("store: begin drive grant write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, removed, err := replaceUserDriveGrantOn(ctx, tx, g, homeOverrideStated, nil)
	if err != nil {
		return types.UserDriveGrant{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.UserDriveGrant{}, nil, fmt.Errorf("store: commit drive grant write: %w", err)
	}
	return out, removed, nil
}

// lockUserDriveGrants takes the subject's allocation lock for the rest of tx.
// Serialises replacing writes for one subject, including the first one for
// a subject with no row yet, which a FOR UPDATE has nothing to lock:
// without it two concurrent replaces both insert and the subject keeps both.
func lockUserDriveGrants(ctx context.Context, tx pgx.Tx, g types.UserDriveGrant) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('user_drive_grants:' || $1 || '/' || $2, 0))`,
		g.SubjectType, g.Subject); err != nil {
		return fmt.Errorf("store: lock drive grants: %w", err)
	}
	return nil
}

// grantJudge is shown the row a tier-judged write is about to change (nil: there is none) and
// refuses the write by returning an error.
type grantJudge func(prev *types.UserDriveGrant) error

// replaceUserDriveGrantOn is ReplaceUserDriveGrant's body on tx. judge, when non-nil, sees the row
// the write keeps and repoints, under the subject's lock and that row's lock and before anything
// is removed. A write judged against the subject having no row is written only while it still has
// none (ErrUserDriveGrantChanged).
func replaceUserDriveGrantOn(ctx context.Context, tx pgx.Tx, g types.UserDriveGrant, homeOverrideStated bool, judge grantJudge) (types.UserDriveGrant, []uuid.UUID, error) {
	if err := lockUserDriveGrants(ctx, tx, g); err != nil {
		return types.UserDriveGrant{}, nil, err
	}
	var prev *types.UserDriveGrant
	var removed []uuid.UUID
	kept, err := scanUserDriveGrant(tx.QueryRow(ctx, `SELECT `+userDriveGrantCols+` FROM user_drive_grants WHERE subject_type = $1 AND subject = $2
		ORDER BY (drive_id = $3) DESC, created_at, id LIMIT 1 FOR UPDATE`, g.SubjectType, g.Subject, g.DriveID))
	switch {
	case err == nil:
		prev = &kept
	case !errors.Is(err, ErrNotFound):
		return types.UserDriveGrant{}, nil, fmt.Errorf("store: read drive grants: %w", err)
	}
	if judge != nil {
		if err := judge(prev); err != nil {
			return types.UserDriveGrant{}, nil, err
		}
	}
	if prev != nil {
		rows, err := tx.Query(ctx, `DELETE FROM user_drive_grants WHERE subject_type = $1 AND subject = $2 AND id <> $3 RETURNING drive_id`,
			g.SubjectType, g.Subject, kept.ID)
		if err == nil {
			removed, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		}
		if err != nil {
			return types.UserDriveGrant{}, nil, fmt.Errorf("store: replace drive grants: %w", err)
		}
		if kept.DriveID != g.DriveID {
			removed = append(removed, kept.DriveID)
			// The stated home_override moves with the drive in one statement: the (drive_id, home_override)
			// index (migration 0059) is checked per statement, so a repoint carrying the old override could
			// collide with another subject's row on the new drive although the requested pair is free.
			if _, err := tx.Exec(ctx, `UPDATE user_drive_grants SET drive_id = $2,
				home_override = CASE WHEN $4::boolean THEN $3::text ELSE home_override END WHERE id = $1`,
				kept.ID, g.DriveID, g.HomeOverride, homeOverrideStated); err != nil {
				return types.UserDriveGrant{}, nil, grantWriteErr(err)
			}
		}
	}
	out, err := upsertUserDriveGrant(ctx, tx, g, homeOverrideStated, judge != nil && prev == nil)
	if err != nil {
		return types.UserDriveGrant{}, nil, err
	}
	return out, removed, nil
}

// AddUserDriveGrant allocates one drive to one subject BESIDE the subject's other drives, keyed
// on UNIQUE(subject_type, subject, drive_id) (migration 0169): writing the same subject and drive
// again updates that row and returns its id; another drive is another row. The guards and error
// mapping are UpsertUserDriveGrant's.
func (s PG) AddUserDriveGrant(ctx context.Context, g types.UserDriveGrant, homeOverrideStated bool) (types.UserDriveGrant, error) {
	return upsertUserDriveGrant(ctx, s.Pool, g, homeOverrideStated, false)
}

// ErrUserDriveGrantChanged is UpsertUserDriveGrantWithinTier's answer when an allocation for the
// subject appeared while the write was judged against there being none.
var ErrUserDriveGrantChanged = errors.New("store: the allocation changed while it was being written")

// UserDriveGrantWidensError is UpsertUserDriveGrantWithinTier's refusal: Field is what the write
// would widen (types.DriveGrantWidens). Nothing was written.
type UserDriveGrantWidensError struct{ Field string }

func (e *UserDriveGrantWidensError) Error() string {
	return "store: the allocation write widens " + e.Field
}

// UserDriveGrantTierWriter is the allocation write for a caller whose authority stops at the
// drive's own posture. Optional like PersonStore; a caller without it must refuse the write.
type UserDriveGrantTierWriter interface {
	UpsertUserDriveGrantWithinTier(ctx context.Context, g types.UserDriveGrant, homeOverrideStated, additional bool) (types.UserDriveGrant, []uuid.UUID, error)
}

var _ UserDriveGrantTierWriter = PG{}

// UpsertUserDriveGrantWithinTier is the allocation write of either verb, ReplaceUserDriveGrant or
// with additional AddUserDriveGrant, judged by types.DriveGrantWidens in the write's own
// transaction. The drive is read FOR SHARE, then the subject's allocation lock is taken, then the
// judged row is read FOR UPDATE: with additional the row for this subject and drive, otherwise the
// row the replace keeps and repoints. A concurrent edit of the drive or of that row therefore lands
// wholly before the judgement or waits for the write, and nothing is removed before the judgement.
// A write judged against there being no row is written only while there is still none
// (ErrUserDriveGrantChanged otherwise). The removed drives are ReplaceUserDriveGrant's. READ
// COMMITTED is pinned for UpsertUserDrive's reason. ErrNotFound for an unknown drive.
func (s PG) UpsertUserDriveGrantWithinTier(ctx context.Context, g types.UserDriveGrant, homeOverrideStated, additional bool) (types.UserDriveGrant, []uuid.UUID, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.UserDriveGrant{}, nil, fmt.Errorf("store: begin user drive grant write: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	d, err := scanUserDrive(tx.QueryRow(ctx, `SELECT `+userDriveCols+` FROM user_drives WHERE id = $1 FOR SHARE`, g.DriveID))
	if err != nil {
		return types.UserDriveGrant{}, nil, err
	}
	judge := func(prev *types.UserDriveGrant) error {
		if field := types.DriveGrantWidens(d, prev, g); field != "" {
			return &UserDriveGrantWidensError{Field: field}
		}
		return nil
	}
	var out types.UserDriveGrant
	var removed []uuid.UUID
	if additional {
		out, err = addUserDriveGrantOn(ctx, tx, g, homeOverrideStated, judge)
	} else {
		out, removed, err = replaceUserDriveGrantOn(ctx, tx, g, homeOverrideStated, judge)
	}
	if err != nil {
		return types.UserDriveGrant{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.UserDriveGrant{}, nil, fmt.Errorf("store: commit user drive grant write: %w", err)
	}
	return out, removed, nil
}

// addUserDriveGrantOn is AddUserDriveGrant on tx for a tier-judged write: judge sees the row for
// this subject and drive, the one row the write can change, under the subject's lock and that
// row's lock.
func addUserDriveGrantOn(ctx context.Context, tx pgx.Tx, g types.UserDriveGrant, homeOverrideStated bool, judge grantJudge) (types.UserDriveGrant, error) {
	if err := lockUserDriveGrants(ctx, tx, g); err != nil {
		return types.UserDriveGrant{}, err
	}
	var prev *types.UserDriveGrant
	row, err := scanUserDriveGrant(tx.QueryRow(ctx, `SELECT `+userDriveGrantCols+` FROM user_drive_grants
		WHERE subject_type = $1 AND subject = $2 AND drive_id = $3 FOR UPDATE`, g.SubjectType, g.Subject, g.DriveID))
	switch {
	case err == nil:
		prev = &row
	case !errors.Is(err, ErrNotFound):
		return types.UserDriveGrant{}, err
	}
	if err := judge(prev); err != nil {
		return types.UserDriveGrant{}, err
	}
	return upsertUserDriveGrant(ctx, tx, g, homeOverrideStated, prev == nil)
}

// rowQuerier is the one call upsertUserDriveGrant makes, on a pool or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// upsertUserDriveGrant is the guarded per-(subject, drive) write every grant verb shares. mustBeNew
// refuses to update a row (ErrUserDriveGrantChanged): the caller judged the write against there
// being none.
func upsertUserDriveGrant(ctx context.Context, q rowQuerier, g types.UserDriveGrant, homeOverrideStated, mustBeNew bool) (types.UserDriveGrant, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	const stmt = `
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
		ON CONFLICT (subject_type, subject, drive_id) DO UPDATE
			SET priority = EXCLUDED.priority,
			    size_mib_override = EXCLUDED.size_mib_override,
			    writable_override = EXCLUDED.writable_override,
			    home_override = EXCLUDED.home_override, enabled = EXCLUDED.enabled
			WHERE ($11::boolean OR user_drive_grants.home_override = '') AND NOT $12::boolean
		RETURNING ` + userDriveGrantCols
	out, err := scanUserDriveGrant(q.QueryRow(ctx, stmt,
		g.ID, g.SubjectType, g.Subject, g.DriveID, g.Priority,
		g.SizeMiBOverride, g.WritableOverride, g.HomeOverride, g.Enabled, g.CreatedBy,
		homeOverrideStated, mustBeNew))
	if err != nil {
		// No row from a write that had to be new: a row for the subject and drive appeared after
		// the judgement (a stated home_override was refused by the judgement before this).
		if errors.Is(err, ErrNotFound) && mustBeNew && g.HomeOverride == "" {
			return types.UserDriveGrant{}, ErrUserDriveGrantChanged
		}
		return types.UserDriveGrant{}, grantWriteErr(err)
	}
	return out, nil
}

// grantWriteErr maps a grant write's failure onto the store's refusals.
func grantWriteErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrNotFound
	}
	// 23505 is the same guard, won by the database on a race between two concurrent inserts
	// that both passed NOT EXISTS. Must map to the same refusal as the single-threaded path, or
	// the race's loser gets a raw driver error. Not reachable by a Go fixture; internal/db's own
	// test asserts the SQLSTATE and this maps it.
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	// No rows means one of the two guards fired (FK handled above, natural key absorbed by ON
	// CONFLICT). The two are mutually exclusive by construction — the INSERT guard needs a
	// non-empty home_override, the UPDATE guard is disabled by a stated one — so
	// homeOverrideStated alone tells them apart without a second read.
	if errors.Is(err, ErrNotFound) {
		return ErrConflict
	}
	return fmt.Errorf("store: write drive grant: %w", err)
}

// DeleteUserDriveGrant removes one grant by id and returns it (ErrNotFound if
// no match) — removing only the binding, never the volume/claim/directory.
// RETURNING lets the caller audit-log what was removed without a prior scan.
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

// ListUserDriveGrantsPage is ListUserDriveGrants bounded to one window. The
// LIMIT changes the plan, not just the size: this ORDER BY has no serving
// index, so unbounded it's a Seq Scan + full sort (50k rows, PG17: 149.7ms
// disk sort); LIMIT turns it into a top-N heapsort (37.7ms, in memory). The
// Seq Scan itself needs an index (a migration) to remove.
func (s PG) ListUserDriveGrantsPage(ctx context.Context, p Page) ([]types.UserDriveGrant, error) {
	q, args := p.appendTo(userDriveGrantList, nil)
	return collect(ctx, s.Pool, "list", "user drive grants", q, args, scanUserDriveGrant)
}

// ResolveUserDrive returns THE ONE drive that applies to a caller, the winning grant, and its tier
// — or ErrNotFound. The whole precedence rule lives in the ORDER BY (subjectMatch/subjectPrecedence,
// shared with ResolveGovernanceProfile since both rank the same subject vocabulary): one indexed
// read on UNIQUE(subject_type, subject), so there's no second Go implementation to mis-order.
//
// Disabled grants stay in the query; the winner's own `enabled` decides PAUSED vs MOUNTED. Excluding
// them in the WHERE would let a paused row fall through to a wider `all`-tier drive no admin chose
// for this member — silently, since the only visible signal would be a mount appearing rather than
// one stopping (DESIGN §2.2).
//
// userSubjects/groups are normalized from nil to empty: a nil Go slice binds as SQL NULL, and
// `x = ANY(NULL)` is NULL rather than false — same reasoning as ResolveGovernanceProfile.
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

func scanDriveRow(row pgx.Row) (types.UserDrive, error) {
	var d types.UserDrive
	err := row.Scan(driveRowDest(&d)...)
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
