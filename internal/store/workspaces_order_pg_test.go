// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ListWorkspacesOrderIsTotal: workspaces onboarded at the same instant
// list in one order whatever order they were written in, since the run fold
// reads the list twice and credits a shared source from it (newest first, then
// by id).
func TestPG_ListWorkspacesOrderIsTotal(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	low, high := uuid.MustParse("00000000-0000-4000-8000-000000000001"), uuid.MustParse("ffffffff-ffff-4fff-bfff-ffffffffffff")
	for _, ids := range [][]uuid.UUID{{low, high}, {high, low}} {
		s := hydrateStore(t)
		for _, id := range ids {
			if _, err := s.CreateWorkspace(ctx, types.Workspace{ID: id, Name: id.String()[:8], Status: types.WorkspaceScanned, CreatedAt: at, UpdatedAt: at}); err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
		}
		all, err := s.ListWorkspaces(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got []uuid.UUID
		for _, ws := range all {
			got = append(got, ws.ID)
		}
		if want := []uuid.UUID{high, low}; !slices.Equal(got, want) {
			t.Fatalf("written %v, listed %v, want %v", ids, got, want)
		}
	}
}
