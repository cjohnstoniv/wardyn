// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_UserDriveGrantWithinTier_JudgesTheRowAsItIsWritten: a security-tier
// allocation write is judged against the subject's row and the drive as they are
// when it writes, not as they were when it read. Each case holds an operator's
// edit open until the tier write is waiting on it, then commits it: the tier
// write must see the edit.
func TestPG_UserDriveGrantWithinTier_JudgesTheRowAsItIsWritten(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seed := func(writable bool) types.UserDrive {
		d, err := st.UpsertUserDrive(ctx, types.UserDrive{
			Name: "tier-" + uuid.NewString(), Backend: types.DriveBackendDockerVolume,
			HomeTemplate: types.HomeTemplateHash, SizeMiB: 1024, Writable: writable,
			Reclaim: types.DriveReclaimRetain, CreatedBy: "admin@example.com",
		}, false)
		if err != nil {
			t.Fatalf("seed drive: %v", err)
		}
		return d
	}
	grantOf := func(subject string) (types.UserDriveGrant, bool) {
		all, err := st.ListUserDriveGrants(ctx)
		if err != nil {
			t.Fatalf("list grants: %v", err)
		}
		for _, g := range all {
			if g.SubjectType == types.CapabilitySubjectUser && g.Subject == subject {
				return g, true
			}
		}
		return types.UserDriveGrant{}, false
	}
	t.Cleanup(func() {
		all, _ := st.ListUserDriveGrants(context.Background())
		for _, g := range all {
			_, _ = st.DeleteUserDriveGrant(context.Background(), g.ID)
		}
	})
	tru, fls := true, false

	t.Run("a writable exception revoked meanwhile is not written back", func(t *testing.T) {
		d := seed(false)
		g := types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: "sub-" + uuid.NewString(),
			DriveID: d.ID, Enabled: true, WritableOverride: &tru}
		if _, err := st.UpsertUserDriveGrant(ctx, g, false); err != nil {
			t.Fatalf("seed the super admin's exception: %v", err)
		}
		err := whileOperatorEditIsOpen(ctx, t, pool, func(ctx context.Context, q pgx.Tx) error {
			_, err := q.Exec(ctx, `UPDATE user_drive_grants SET writable_override = false
				WHERE subject_type = 'user' AND subject = $1`, g.Subject)
			return err
		}, func() error {
			_, _, err := st.UpsertUserDriveGrantWithinTier(ctx, g, false, false) // the unchanged re-send of true
			return err
		})
		var widens *store.UserDriveGrantWidensError
		if !errors.As(err, &widens) || widens.Field != "writable_override" {
			t.Errorf("tier re-send of true after the revocation = %v, want a writable_override refusal", err)
		}
		if got, _ := grantOf(g.Subject); got.WritableOverride == nil || *got.WritableOverride {
			t.Errorf("writable_override = %v, want the operator's false kept", got.WritableOverride)
		}
		// Re-sent against the row as it now is, the same priority edit passes.
		g.WritableOverride, g.Priority = &fls, 4
		if _, _, err := st.UpsertUserDriveGrantWithinTier(ctx, g, false, false); err != nil {
			t.Errorf("unchanged re-send of false = %v, want it written", err)
		}
	})

	t.Run("a read-only allocation added meanwhile is not overwritten", func(t *testing.T) {
		d := seed(true)
		subject := "sub-" + uuid.NewString()
		err := whileOperatorEditIsOpen(ctx, t, pool, func(ctx context.Context, q pgx.Tx) error {
			_, err := q.Exec(ctx, `INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, priority,
				size_mib_override, writable_override, home_override, enabled, created_by)
				VALUES ($1, 'user', $2, $3, 0, 0, false, '', true, 'root')`, uuid.New(), subject, d.ID)
			return err
		}, func() error {
			_, _, err := st.UpsertUserDriveGrantWithinTier(ctx, types.UserDriveGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: subject, DriveID: d.ID, Enabled: true,
			}, false, false)
			return err
		})
		if !errors.Is(err, store.ErrUserDriveGrantChanged) {
			t.Errorf("tier write over a row added meanwhile = %v, want ErrUserDriveGrantChanged", err)
		}
		if got, _ := grantOf(subject); got.WritableOverride == nil || *got.WritableOverride {
			t.Errorf("writable_override = %v, want the operator's false kept", got.WritableOverride)
		}
	})
}

// whileOperatorEditIsOpen runs edit in an open transaction, starts write, waits
// until write is blocked behind that transaction, commits it, and returns
// write's result. The wait is on Postgres's own lock graph, so the order is the
// same on every run.
func whileOperatorEditIsOpen(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	edit func(context.Context, pgx.Tx) error, write func() error) error {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin operator edit: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // committed below on the happy path
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	if err := edit(ctx, tx); err != nil {
		t.Fatalf("operator edit: %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- write() }()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatalf("lock graph: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("the tier write finished (%v) without waiting on the open edit", err)
		case <-ctx.Done():
			t.Fatal("the tier write never waited on the open edit")
		case <-time.After(time.Millisecond):
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit operator edit: %v", err)
	}
	return <-result
}

// tierDrive seeds one managed drive for the tier-judged write tests.
func tierDrive(ctx context.Context, t *testing.T, st store.PG, writable bool) types.UserDrive {
	t.Helper()
	d, err := st.UpsertUserDrive(ctx, types.UserDrive{
		Name: "tier-" + uuid.NewString(), Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, SizeMiB: 1024, Writable: writable,
		Reclaim: types.DriveReclaimRetain, CreatedBy: "admin@example.com",
	}, false)
	if err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	return d
}

// tierGrants are the subject's allocations keyed by drive, removed again when the test ends.
func tierGrants(ctx context.Context, t *testing.T, st store.PG, subject string) map[uuid.UUID]types.UserDriveGrant {
	t.Helper()
	all, err := st.ListUserDriveGrants(ctx)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	out := map[uuid.UUID]types.UserDriveGrant{}
	for _, g := range all {
		if g.SubjectType == types.CapabilitySubjectUser && g.Subject == subject {
			out[g.DriveID] = g
		}
	}
	return out
}

func tierSubject(t *testing.T, st store.PG) string {
	subject := "sub-" + uuid.NewString()
	t.Cleanup(func() {
		for _, g := range tierGrants(context.Background(), t, st, subject) {
			_, _ = st.DeleteUserDriveGrant(context.Background(), g.ID)
		}
	})
	return subject
}

func wantWidens(t *testing.T, what string, err error, field string) {
	t.Helper()
	var widens *store.UserDriveGrantWidensError
	if !errors.As(err, &widens) || widens.Field != field {
		t.Errorf("%s = %v, want a %s refusal", what, err, field)
	}
}

// TestPG_UserDriveGrantWithinTier_AddBeside: a security-tier write that allocates a drive beside
// the subject's others is judged against the subject's row for THAT drive. One that widens is
// refused and writes nothing; one that does not is written and leaves the other rows alone.
func TestPG_UserDriveGrantWithinTier_AddBeside(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, b, c := tierDrive(ctx, t, st, true), tierDrive(ctx, t, st, false), tierDrive(ctx, t, st, true)
	subject := tierSubject(t, st)
	tru, fls := true, false
	grant := func(d types.UserDrive, mut func(*types.UserDriveGrant)) types.UserDriveGrant {
		g := types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: subject, DriveID: d.ID, Enabled: true}
		if mut != nil {
			mut(&g)
		}
		return g
	}
	held, err := st.AddUserDriveGrant(ctx, grant(a, func(g *types.UserDriveGrant) { g.WritableOverride = &fls }), false)
	if err != nil {
		t.Fatalf("seed the super admin's read-only allocation: %v", err)
	}

	_, _, err = st.UpsertUserDriveGrantWithinTier(ctx, grant(b, func(g *types.UserDriveGrant) { g.WritableOverride = &tru }), false, true)
	wantWidens(t, "a writable allocation beside", err, "writable_override")
	_, _, err = st.UpsertUserDriveGrantWithinTier(ctx, grant(c, func(g *types.UserDriveGrant) { g.HomeOverride = "alice" }), true, true)
	wantWidens(t, "a directory name beside", err, "home_override")
	if rows := tierGrants(ctx, t, st, subject); len(rows) != 1 {
		t.Fatalf("the subject holds %d rows after two refused adds, want 1", len(rows))
	}

	// The read-only override on drive a is not the row this write changes.
	added, removed, err := st.UpsertUserDriveGrantWithinTier(ctx, grant(c, nil), false, true)
	if err != nil || len(removed) != 0 || added.ID == held.ID {
		t.Fatalf("a plain allocation beside = %+v, removed %v, %v; want a new row and nothing removed", added, removed, err)
	}
	// On drive a that override IS the row: dropping it there is refused, an unchanged re-send passes.
	_, _, err = st.UpsertUserDriveGrantWithinTier(ctx, grant(a, nil), false, true)
	wantWidens(t, "dropping the read-only override with an add", err, "writable_override")
	again, _, err := st.UpsertUserDriveGrantWithinTier(ctx, grant(a, func(g *types.UserDriveGrant) { g.WritableOverride, g.Priority = &fls, 3 }), false, true)
	if err != nil || again.ID != held.ID {
		t.Errorf("unchanged re-send of the read-only row = %+v, %v; want the same row written", again, err)
	}
	rows := tierGrants(ctx, t, st, subject)
	if got := rows[a.ID]; len(rows) != 2 || got.WritableOverride == nil || *got.WritableOverride || got.Priority != 3 {
		t.Errorf("the subject holds %d rows and drive a's is %+v, want 2 and a's still read-only at priority 3", len(rows), got)
	}
}

// TestPG_UserDriveGrantWithinTier_ReplaceJudgesTheKeptRow: a subject that holds several rows is
// judged against the row the replace keeps and repoints (the one on the drive, else the oldest),
// before anything is removed; a replace that passes reports the drives it took away.
func TestPG_UserDriveGrantWithinTier_ReplaceJudgesTheKeptRow(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, b, c := tierDrive(ctx, t, st, true), tierDrive(ctx, t, st, false), tierDrive(ctx, t, st, true)
	subject := tierSubject(t, st)
	tru, fls := true, false
	base := types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: subject, Enabled: true}
	older, exception := base, base
	older.DriveID, older.WritableOverride = a.ID, &fls
	exception.DriveID, exception.WritableOverride = b.ID, &tru
	if _, err := st.AddUserDriveGrant(ctx, older, false); err != nil {
		t.Fatalf("seed the older read-only allocation: %v", err)
	}
	kept, err := st.AddUserDriveGrant(ctx, exception, false)
	if err != nil {
		t.Fatalf("seed the super admin's writable exception: %v", err)
	}

	// Moving to drive c keeps and repoints the OLDER row, whose read-only override a plain
	// allocation of a writable drive would drop.
	plain := base
	plain.DriveID = c.ID
	_, _, err = st.UpsertUserDriveGrantWithinTier(ctx, plain, false, false)
	wantWidens(t, "a replace that drops the kept row's read-only override", err, "writable_override")
	if rows := tierGrants(ctx, t, st, subject); len(rows) != 2 {
		t.Fatalf("a refused replace left %d rows, want both", len(rows))
	}

	// The unchanged re-send of the exception on its own drive keeps THAT row.
	out, removed, err := st.UpsertUserDriveGrantWithinTier(ctx, exception, false, false)
	if err != nil || out.ID != kept.ID || len(removed) != 1 || removed[0] != a.ID {
		t.Fatalf("unchanged re-send on the kept row's drive = %+v, removed %v, %v; want row %s kept and drive %s removed", out, removed, err, kept.ID, a.ID)
	}
	rows := tierGrants(ctx, t, st, subject)
	if got := rows[b.ID]; len(rows) != 1 || got.WritableOverride == nil || !*got.WritableOverride {
		t.Errorf("after the replace the subject holds %d rows (%+v), want the exception alone", len(rows), rows)
	}
}

// TestPG_UserDriveGrantWithinTier_WaitsForTheSubjectsOtherWriter: either verb's judgement waits
// for a writer that holds the row or the subject's allocation lock, and judges what it committed.
func TestPG_UserDriveGrantWithinTier_WaitsForTheSubjectsOtherWriter(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tru := true

	t.Run("an add beside waits for an open edit of its row", func(t *testing.T) {
		a, b := tierDrive(ctx, t, st, true), tierDrive(ctx, t, st, false)
		subject := tierSubject(t, st)
		g := types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: subject, DriveID: b.ID, Enabled: true, WritableOverride: &tru}
		other := g
		other.DriveID, other.WritableOverride = a.ID, nil
		for _, seed := range []types.UserDriveGrant{other, g} {
			if _, err := st.AddUserDriveGrant(ctx, seed, false); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		err := whileOperatorEditIsOpen(ctx, t, pool, func(ctx context.Context, q pgx.Tx) error {
			_, err := q.Exec(ctx, `UPDATE user_drive_grants SET writable_override = false
				WHERE subject_type = 'user' AND subject = $1 AND drive_id = $2`, subject, b.ID)
			return err
		}, func() error {
			_, _, err := st.UpsertUserDriveGrantWithinTier(ctx, g, false, true) // the unchanged re-send of true
			return err
		})
		wantWidens(t, "add re-sending true after the revocation", err, "writable_override")
		if got := tierGrants(ctx, t, st, subject)[b.ID]; got.WritableOverride == nil || *got.WritableOverride {
			t.Errorf("writable_override = %v, want the operator's false kept", got.WritableOverride)
		}
	})

	t.Run("a replace waits for the subject's allocation lock", func(t *testing.T) {
		a := tierDrive(ctx, t, st, true)
		subject := tierSubject(t, st)
		// The other writer is a replace in flight for a subject with no row yet: it holds the
		// subject's lock and has inserted a read-only allocation it has not committed.
		err := whileOperatorEditIsOpen(ctx, t, pool, func(ctx context.Context, q pgx.Tx) error {
			if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('user_drive_grants:' || $1 || '/' || $2, 0))`,
				string(types.CapabilitySubjectUser), subject); err != nil {
				return err
			}
			_, err := q.Exec(ctx, `INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, priority,
				size_mib_override, writable_override, home_override, enabled, created_by)
				VALUES ($1, 'user', $2, $3, 0, 0, false, '', true, 'root')`, uuid.New(), subject, a.ID)
			return err
		}, func() error {
			_, _, err := st.UpsertUserDriveGrantWithinTier(ctx, types.UserDriveGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: subject, DriveID: a.ID, Enabled: true,
			}, false, false)
			return err
		})
		// It waited for the lock, so it judged the committed row instead of colliding with it.
		wantWidens(t, "replace behind another replace", err, "writable_override")
		if got := tierGrants(ctx, t, st, subject)[a.ID]; got.WritableOverride == nil || *got.WritableOverride {
			t.Errorf("writable_override = %v, want the other writer's false kept", got.WritableOverride)
		}
	})
}
