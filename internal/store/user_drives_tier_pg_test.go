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
			_, err := st.UpsertUserDriveGrantWithinTier(ctx, g, false) // the unchanged re-send of true
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
		if _, err := st.UpsertUserDriveGrantWithinTier(ctx, g, false); err != nil {
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
			_, err := st.UpsertUserDriveGrantWithinTier(ctx, types.UserDriveGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: subject, DriveID: d.ID, Enabled: true,
			}, false)
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
