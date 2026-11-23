// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// runScheduleRow is a daily schedule of owner's, next due at next.
func runScheduleRow(owner string, next time.Time) store.RunSchedule {
	return store.RunSchedule{ID: uuid.New(), Owner: owner, Every: "daily", LocalTime: "09:00", TimeZone: "Europe/London",
		Request: json.RawMessage(`{"agent":"claude-code"}`), Authority: json.RawMessage(`{"sub":"` + owner + `","groups":null}`),
		AuthorityAt: next.Add(-time.Hour), NextAt: next}
}

// The schedule limit counts one owner's rows only: an owner at the limit is refused with
// ErrRunScheduleLimit and nothing is written, while another owner still creates.
func TestRunSchedules_LimitIsPerOwner_PG(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()
	next := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	for range 2 {
		if err := pg.CreateRunSchedule(ctx, runScheduleRow("sub-a", next), 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.CreateRunSchedule(ctx, runScheduleRow("sub-a", next), 2); !errors.Is(err, store.ErrRunScheduleLimit) {
		t.Fatalf("a third schedule at a limit of 2 = %v, want ErrRunScheduleLimit", err)
	}
	if got, err := pg.ListRunSchedules(ctx, "sub-a"); err != nil || len(got) != 2 {
		t.Fatalf("sub-a's schedules = %d, %v; want 2", len(got), err)
	}
	if err := pg.CreateRunSchedule(ctx, runScheduleRow("sub-b", next), 2); err != nil {
		t.Fatalf("another owner's schedule = %v, want it created", err)
	}
	if all, err := pg.ListRunSchedules(ctx, ""); err != nil || len(all) != 3 {
		t.Fatalf("every schedule = %d, %v; want 3", len(all), err)
	}
}

// The due read skips a paused schedule and one not yet due; the advance is a compare-and-set on next_at.
func TestRunSchedules_DueSkipsPaused_PG(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	due, paused, later := runScheduleRow("sub-a", now.Add(-time.Minute)), runScheduleRow("sub-a", now.Add(-time.Minute)), runScheduleRow("sub-a", now.Add(time.Hour))
	for _, sc := range []store.RunSchedule{due, paused, later} {
		if err := pg.CreateRunSchedule(ctx, sc, 20); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := pg.PauseRunSchedule(ctx, paused.ID, "by_owner", now); err != nil || !ok {
		t.Fatalf("pause = %v %v", ok, err)
	}
	got, err := pg.DueRunSchedules(ctx, now, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != due.ID {
		t.Fatalf("due schedules = %+v; want only %s", got, due.ID)
	}
	next := due.NextAt.Add(24 * time.Hour)
	if ok, err := pg.AdvanceRunSchedule(ctx, due.ID, due.NextAt, next, nil, "missed"); err != nil || !ok {
		t.Fatalf("advance = %v %v", ok, err)
	}
	if ok, err := pg.AdvanceRunSchedule(ctx, due.ID, due.NextAt, next, nil, "missed"); err != nil || ok {
		t.Fatalf("a second advance of the same occurrence = %v %v; want false", ok, err)
	}
	if got, err := pg.DueRunSchedules(ctx, now, 50); err != nil || len(got) != 0 {
		t.Fatalf("due after the advance = %d, %v; want none", len(got), err)
	}
}

// The erasure's delete is skipped for a role without INSERT on the table, which can have written no schedule, and
// fails loudly for a role that can insert but not delete.
func TestRunSchedules_EraseSkipsARoleWithoutTheGrant_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := t.Context()
	if err := pg.CreateRunSchedule(ctx, runScheduleRow("sub-a", time.Now().UTC().Add(time.Hour)), 20); err != nil {
		t.Fatal(err)
	}
	role := "wardyn_nosched_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := pool.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE IF EXISTS `+role) })
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["role"] = role // every connection of this pool runs as the role
	restricted, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restricted.Close)
	if n, err := store.NewPG(restricted).DeleteRunSchedulesOf(ctx, "sub-a"); err != nil || n != 0 {
		t.Fatalf("erase without INSERT = %d, %v; want 0 and no error", n, err)
	}
	if _, err := pool.Exec(ctx, `GRANT INSERT ON run_schedules TO `+role); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NewPG(restricted).DeleteRunSchedulesOf(ctx, "sub-a"); err == nil {
		t.Fatal("erase with INSERT and no DELETE succeeded; want it to fail loudly")
	}
	if n, err := pg.DeleteRunSchedulesOf(ctx, "sub-a"); err != nil || n != 1 {
		t.Fatalf("erase as the owner role = %d, %v; want 1", n, err)
	}
}
