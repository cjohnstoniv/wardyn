// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// listFailStore is an owner store whose workspace list cannot be read.
type listFailStore struct{ *ownerStore }

func (listFailStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, errors.New("store down")
}

// TestApplyWorkspaceRequirements_PolicyMountUnreadableListRefuses: a mount no
// attached workspace holds is held to every stored holder, so when the list
// cannot be read the write is refused, never granted unchecked.
func TestApplyWorkspaceRequirements_PolicyMountUnreadableListRefuses(t *testing.T) {
	const path = "/srv/app"
	ro := true
	spec := &types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: path, Target: "/home/agent/work", ReadOnly: &ro}}}
	supplier := types.Workspace{ID: uuid.New(), Requirements: map[string]types.WorkspaceRequirement{"write:" + path: {Level: "required", Provenance: "operator_set"}}}
	srv := New(Config{Store: listFailStore{newOwnerStore()}})

	_, refusal := srv.applyWorkspaceRequirementsFor(context.Background(), nil, spec, "claude-code", []types.Workspace{supplier}, nil, nil, nil)

	if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.reason != reasonWorkspaceSourcesListUnavailable {
		t.Fatalf("refusal = %v, want 422 %s", refusal, reasonWorkspaceSourcesListUnavailable)
	}
}
