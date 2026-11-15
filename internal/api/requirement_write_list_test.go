// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

	if refusal == nil || refusal.status != http.StatusInternalServerError || refusal.reason != reasonWorkspaceSourcesListUnavailable ||
		strings.Contains(refusal.Error(), "store down") {
		t.Fatalf("refusal = %v, want 500 %s without the store's own error", refusal, reasonWorkspaceSourcesListUnavailable)
	}
}

// The run and policy doors' own reads of the workspace list answer a list that cannot be read as the server fault
// it is, as the scan doors do, with the store's error in the log only.
func TestWorkspaceSources_AnUnreadableListIsAServerFault(t *testing.T) {
	spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/app", Target: "/home/agent/work"}}}
	srv := New(Config{Store: listFailStore{newOwnerStore()}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	for name, answer := range map[string]func() (int, string, error){
		"validateWorkspaceSources": func() (int, string, error) {
			return srv.validateWorkspaceSources(context.Background(), spec, runCarrierRule{})
		},
		"authorizeSpecWorkspaceSources": func() (int, string, error) {
			return srv.authorizeSpecWorkspaceSources(context.Background(), r, spec)
		},
	} {
		code, reason, err := answer()
		if code != http.StatusInternalServerError || reason != reasonWorkspaceSourcesListUnavailable || err == nil || strings.Contains(err.Error(), "store down") {
			t.Errorf("%s = %d %s %v, want 500 %s without the store's own error", name, code, reason, err, reasonWorkspaceSourcesListUnavailable)
		}
	}
}
