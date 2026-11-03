// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.image_input (migration 0249): the image ref a run's create held to
// the digest rule. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"testing"

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

func imageInputShown(p *string) string {
	if p == nil {
		return "NULL"
	}
	return "\"" + *p + "\""
}
