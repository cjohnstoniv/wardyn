// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A workspace name is unique per owner (0192): two members, or a member and the operator, may use the
// same name; one owner may not use it twice, by create, rename or a reassignment to that owner.
func TestPG_WorkspaceNameIsUniquePerOwner(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	name := "frontend-" + uuid.NewString()
	create := func(owner, name string) (types.Workspace, error) {
		now := time.Now().UTC()
		return pg.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: name, OwnedBy: owner,
			Status: types.WorkspaceScanned, CreatedAt: now, UpdatedAt: now})
	}
	alice, err := create("alice", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create("bob", name); err != nil {
		t.Errorf("bob's %q beside alice's: %v", name, err)
	}
	if _, err := create("", name); err != nil {
		t.Errorf("an operator %q beside the members': %v", name, err)
	}
	if _, err := create("alice", name); !errors.Is(err, store.ErrConflict) {
		t.Errorf("alice's second %q = %v, want ErrConflict", name, err)
	}
	other, err := create("alice", name+"-2")
	if err != nil {
		t.Fatal(err)
	}
	other.Name = name
	if _, err := pg.UpdateWorkspace(ctx, other.ID, other, false); !errors.Is(err, store.ErrConflict) {
		t.Errorf("alice renaming onto her own %q = %v, want ErrConflict", name, err)
	}
	if _, err := pg.SetWorkspaceOwner(ctx, alice.ID, ""); !errors.Is(err, store.ErrConflict) {
		t.Errorf("reassigning alice's %q to the operator, who has one = %v, want ErrConflict", name, err)
	}
}
