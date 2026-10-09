// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/recording"
)

func TestPGStore_FenceLockCancellation(t *testing.T) {
	pool, other := pgPool(t), pgPool(t)
	s := recording.NewPGStore(other)
	run := uuid.NewString()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1, hashtext($2))`, db.RecordingLockClass, run); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"save", "erase", "open", "stat"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
			defer cancel()
			var err error
			switch action {
			case "save":
				err = s.SaveCast(ctx, run+"~part-2", strings.NewReader("late"))
			case "erase":
				_, err = s.DeleteRun(ctx, run)
			case "open":
				_, err = s.OpenCast(ctx, run)
			case "stat":
				_, _, err = s.StatAndTail(ctx, run, 32)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancelled %s = %v", action, err)
			}
		})
	}
	if err := s.SaveCast(t.Context(), uuid.NewString(), strings.NewReader("distinct")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCast(t.Context(), run, strings.NewReader("after cancellation")); err != nil {
		t.Fatal(err)
	}
}

func TestPGStore_FenceSurvivesSweepAndRecreatedRow(t *testing.T) {
	pool := pgPool(t)
	s := recording.NewPGStore(pool)
	run := uuid.NewString()
	if _, err := s.DeleteRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sweep(time.Hour); err != nil {
		t.Fatal(err)
	}
	s = recording.NewPGStore(pgPool(t))
	for _, key := range []string{run, run + "~part-2"} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO recordings (cast_key,payload) VALUES ($1,$2)`, key, []byte("external writer")); err != nil {
			t.Fatal(err)
		}
		assertErased(t, s, key)
		if err := s.SaveCast(t.Context(), key, strings.NewReader("late")); !errors.Is(err, recording.ErrErased) {
			t.Fatalf("late save after sweep/restart = %v", err)
		}
	}
	if n, err := s.DeleteRun(t.Context(), run); err != nil || n != 2 {
		t.Fatalf("repeat erasure = %d, %v", n, err)
	}
}
