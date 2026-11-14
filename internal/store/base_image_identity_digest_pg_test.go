// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_UpsertBaseImage_LargeStepsKeepOneIdentity: custom steps far past a btree entry's size, in text
// that does not compress, upsert to one row: the same steps again return the first row, other steps make
// another, and no steps and an empty list stay one identity. Guarded by WARDYN_TEST_PG through hydrateStore.
func TestPG_UpsertBaseImage_LargeStepsKeepOneIdentity(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	big := func() []string {
		b := make([]byte, 6<<10)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return []string{"RUN echo " + base64.StdEncoding.EncodeToString(b)}
	}
	up := func(steps []string) types.BaseImageEntry {
		t.Helper()
		got, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{
			ID: uuid.New(), Kind: "custom", Name: "big", Image: "ubuntu:24.04", Steps: steps,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("upsert with %d steps: %v", len(steps), err)
		}
		return got
	}
	steps := big()
	first := up(steps)
	if again := up(steps); again.ID != first.ID {
		t.Errorf("the same large steps upserted to %s, want the first row %s", again.ID, first.ID)
	}
	if other := up(big()); other.ID == first.ID {
		t.Error("different large steps upserted to the first row")
	}
	up(nil)
	if _, err := s.Pool.Exec(ctx, `INSERT INTO base_images (id, kind, name, image, steps)
		VALUES ($1, 'custom', 'empty', 'ubuntu:24.04', '[]'::jsonb)`, uuid.New()); err == nil {
		t.Error("a row with an empty steps list beside one with none was accepted, want one identity")
	}
}
