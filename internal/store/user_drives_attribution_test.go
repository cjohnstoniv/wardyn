// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

// PIN for the refusal-attribution fallback answered
// ErrDriveHomeNamespaceConflict unconditionally, so a drive the ALLOCATION guard
// refused could be reported with the home-namespace sentence — which names a
// rule that does not exist for its backend ("another host_path drive on …" for a
// docker_volume drive rooted nowhere). An admin reading it goes looking for a
// second share over a host root the drive does not have.
//
// The arm is reachable whenever the attribution RE-READ cannot answer:
// driveHasGrants answers false on a read error by design (the fallback decides
// the MESSAGE, never the write), and a grant deleted between the refusing
// statement and the re-read answers false honestly. Either way the writing
// statement has already decided, under the drive's row lock — and when the
// namespace guard is STRUCTURALLY IMPOSSIBLE for this write, the allocation
// guard is the only thing that can have emptied it.
//
// No Postgres needed: the fallback is a decision over two reads, and the reads
// are what this drives.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errRow is a pgx.Row that always answers err — the re-read that cannot say
// whether the drive is allocated.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// scriptedQuerier answers the upsert with "no rows" (the refusal) and every
// re-read after it with rows[i], in order.
type scriptedQuerier struct {
	rows []pgx.Row
	n    int
}

func (q *scriptedQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	if q.n < len(q.rows) {
		r := q.rows[q.n]
		q.n++
		return r
	}
	return errRow{err: errors.New("scriptedQuerier: no scripted answer left")}
}

func TestUpsertAttributionNamesTheAllocationWhenTheNamespaceGuardCannotHaveFired(t *testing.T) {
	ctx := context.Background()
	pg := PG{}

	// A docker_volume drive: driveHomeNamespaceClashPred is host_path-only, so
	// the namespace guard could not have emptied this statement's result set.
	d := types.UserDrive{ID: uuid.New(), Name: "Corp Volume", Backend: types.DriveBackendDockerVolume}

	t.Run("the re-read cannot answer", func(t *testing.T) {
		q := &scriptedQuerier{rows: []pgx.Row{
			errRow{err: pgx.ErrNoRows},                  // the upsert: refused
			errRow{err: errors.New("connection reset")}, // driveHasGrants: cannot answer
		}}
		_, err := pg.upsertUserDriveOn(ctx, q, d, true)
		if !errors.Is(err, ErrDriveAllocated) {
			t.Errorf("err = %v, want ErrDriveAllocated.\n"+
				"refuseIfAllocated was set and this drive has no host_root and is not a share, so the home-namespace "+
				"guard is not even in the statement's WHERE — the allocation guard is the only thing that can have "+
				"refused it. Falling back to ErrDriveHomeNamespaceConflict renders as \"another host_path drive on "+
				"\\\"\\\" …\", which is a rule this drive cannot break and a row the admin will not find", err)
		}
	})

	t.Run("the grant vanished between the statement and the re-read", func(t *testing.T) {
		q := &scriptedQuerier{rows: []pgx.Row{
			errRow{err: pgx.ErrNoRows}, // the upsert: refused
			boolRow{false},             // driveHasGrants: honestly false now
		}}
		_, err := pg.upsertUserDriveOn(ctx, q, d, true)
		if !errors.Is(err, ErrDriveAllocated) {
			t.Errorf("err = %v, want ErrDriveAllocated — the write was refused under the row lock and only one "+
				"guard was in it", err)
		}
	})

	// The negative controls, and they are what keep this from becoming "every
	// refusal is an allocation".
	t.Run("a share keeps the namespace answer", func(t *testing.T) {
		share := types.UserDrive{ID: uuid.New(), Name: "Corp NAS",
			Backend: types.DriveBackendHostPath, HostRoot: "/srv/shares"}
		q := &scriptedQuerier{rows: []pgx.Row{
			errRow{err: pgx.ErrNoRows}, // the upsert: refused
			boolRow{true},              // driveHomeNamespaceClash: a real clash, asked FIRST
		}}
		_, err := pg.upsertUserDriveOn(ctx, q, share, true)
		if !errors.Is(err, ErrDriveHomeNamespaceConflict) {
			t.Errorf("err = %v, want ErrDriveHomeNamespaceConflict — the durable blocker is asked first, and "+
				"naming the transient one sends an admin to re-send with ?confirm=rehome into a refusal that was "+
				"there all along", err)
		}
	})

	t.Run("an unguarded write still falls back to the namespace answer", func(t *testing.T) {
		// refuseIfAllocated false: the allocation guard is NOT in the statement,
		// so it cannot be what refused it either.
		q := &scriptedQuerier{rows: []pgx.Row{
			errRow{err: pgx.ErrNoRows},
			boolRow{false},
		}}
		_, err := pg.upsertUserDriveOn(ctx, q, d, false)
		if !errors.Is(err, ErrDriveHomeNamespaceConflict) {
			t.Errorf("err = %v, want the unchanged fallback", err)
		}
	})
}

// boolRow scans a single bool.
type boolRow struct{ v bool }

func (r boolRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if p, ok := dest[0].(*bool); ok {
			*p = r.v
			return nil
		}
	}
	return errors.New("boolRow: unexpected scan destination")
}
