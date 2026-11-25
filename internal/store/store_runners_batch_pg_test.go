// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GetRunnersByID reads several runners in one statement: the rows that exist, each equal to GetRunner's, and
// nothing for an unknown id or an empty list.
func TestPG_GetRunnersByID(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	var ids []uuid.UUID
	for _, owner := range []string{"alice", "bob", "carol"} {
		r := newRunner(owner)
		if _, err := pg.CreateRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	got, err := pg.GetRunnersByID(ctx, []uuid.UUID{ids[0], ids[2], uuid.New()})
	if err != nil || len(got) != 2 {
		t.Fatalf("GetRunnersByID = %d rows, %v; want 2", len(got), err)
	}
	byID := map[uuid.UUID]types.Runner{}
	for _, r := range got {
		byID[r.ID] = r
	}
	for _, id := range []uuid.UUID{ids[0], ids[2]} {
		want, err := pg.GetRunner(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(byID[id], want) {
			t.Fatalf("row %s differs from GetRunner:\n got %+v\nwant %+v", id, byID[id], want)
		}
	}
	if _, ok := byID[ids[1]]; ok {
		t.Fatal("a runner that was not asked for came back")
	}
	if got, err := pg.GetRunnersByID(ctx, nil); got != nil || err != nil {
		t.Fatalf("empty ids = %v, %v; want nil, nil", got, err)
	}
}
