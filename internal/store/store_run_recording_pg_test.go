// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunRecordingLevelRoundTrips: a run resolved to off keeps its level and its policy_off mark
// (0201 admits both), and a run that wrote neither reads as the legacy empty values.
func TestPG_RunRecordingLevelRoundTrips(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPool(t))
	untouched, err := pg.CreateRun(ctx, newRun(types.RunStarting))
	if err != nil {
		t.Fatal(err)
	}
	run, err := pg.CreateRun(ctx, newRun(types.RunStarting))
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetRunRecordingLevel(ctx, run.ID, types.RecordingOff); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetRunNotRecorded(ctx, run.ID, types.NotRecordedPolicyOff); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RecordingLevel != types.RecordingOff || got.NotRecorded != types.NotRecordedPolicyOff {
		t.Fatalf("level %q not_recorded %q, want off policy_off", got.RecordingLevel, got.NotRecorded)
	}
	if err := pg.SetRunRecordingLevel(ctx, run.ID, "keystrokes"); err == nil {
		t.Fatal("the column took a level that is not one")
	}
	if got, err = pg.GetRun(ctx, untouched.ID); err != nil || got.RecordingLevel != "" || got.NotRecorded != "" {
		t.Fatalf("untouched run: level %q not_recorded %q err %v", got.RecordingLevel, got.NotRecorded, err)
	}
}

// TestPG_SetRunRecordingOff: the off decision is one write, the level and policy_off together, and a
// run that is not there is ErrNotFound.
func TestPG_SetRunRecordingOff(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPool(t))
	run, err := pg.CreateRun(ctx, newRun(types.RunStarting))
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetRunRecordingOff(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetRun(ctx, run.ID)
	if err != nil || got.RecordingLevel != types.RecordingOff || got.NotRecorded != types.NotRecordedPolicyOff {
		t.Fatalf("level %q not_recorded %q err %v, want off policy_off", got.RecordingLevel, got.NotRecorded, err)
	}
	if err := pg.SetRunRecordingOff(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing run: err %v, want ErrNotFound", err)
	}
}
