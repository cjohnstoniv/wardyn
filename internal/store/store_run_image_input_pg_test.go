// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.image_input (migration 0249): the image ref a run's create held to
// the digest rule. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunImageInput_RoundTrips: a recorded ref, the convention image ("") and no record (NULL, a run from
// before the column) each survive create and read, and stay apart.
func TestPG_RunImageInput_RoundTrips(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	tag, convention := "registry.example.com/team/app:1", ""
	for _, input := range []*string{&tag, &convention, nil} {
		r := newRun(types.RunRunning)
		r.ImageInput = input
		created := persistRun(t, ctx, pool, r)
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range []types.AgentRun{created, got} {
			if (run.ImageInput == nil) != (input == nil) || (input != nil && *run.ImageInput != *input) {
				t.Errorf("image input %v read back as %v", imageInputShown(input), imageInputShown(run.ImageInput))
			}
		}
	}
}

// TestPG_RunImageInput_StartRewritesIt: a queued run joins with the input its create checked, and the start,
// which folds the run afresh, writes the input it checked; a start that names none keeps the stored one.
func TestPG_RunImageInput_StartRewritesIt(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 2)
	now := time.Now().UTC()
	queuedRef, startRef := "registry.example.com/team/app:1", "registry.example.com/team/app@sha256:"+strings.Repeat("a", 64)
	rewritten, kept := poolRun(p, types.RunQueued, "alice"), poolRun(p, types.RunQueued, "bob")
	for _, r := range []types.AgentRun{rewritten, kept} {
		r.ImageInput = &queuedRef
		queued, err := pg.CreateQueuedRun(ctx, r, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
		if err != nil || queued.ImageInput == nil || *queued.ImageInput != queuedRef {
			t.Fatalf("queued run = %v, %v; want image input %q", imageInputShown(queued.ImageInput), err, queuedRef)
		}
	}
	if promoted, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll); err != nil || len(promoted) != 2 {
		t.Fatalf("promote = %+v, %v; want both waiting runs", promoted, err)
	}
	for _, tc := range []struct {
		run  types.AgentRun
		in   *string
		want string
	}{{rewritten, &startRef, startRef}, {kept, nil, queuedRef}} {
		started, ok, err := pg.StartQueuedRun(ctx, tc.run.ID, store.QueueStart{SPIFFEID: "spiffe://x/" + tc.run.ID.String(),
			ConfinementClass: types.CC2, ImageInput: tc.in})
		if err != nil || !ok || started.ImageInput == nil || *started.ImageInput != tc.want {
			t.Fatalf("start with input %s = %v ok=%v err=%v; want %q", imageInputShown(tc.in), imageInputShown(started.ImageInput), ok, err, tc.want)
		}
	}
}

func imageInputShown(p *string) string {
	if p == nil {
		return "NULL"
	}
	return "\"" + *p + "\""
}
