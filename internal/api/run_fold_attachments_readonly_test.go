// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestStepRequirements_AttachmentsShowTheFinalReadOnly (DR-0219): the mount map the dry doors return is
// read before the requirement fold, which narrows an attached workspace's write mount when its own write
// row is optional and not enabled; the map must show the mount the run gets.
func TestStepRequirements_AttachmentsShowTheFinalReadOnly(t *testing.T) {
	const path, target = "/srv/app", "/home/agent/work"
	ws := types.Workspace{ID: uuid.New(), Name: "app",
		Requirements: map[string]types.WorkspaceRequirement{"write:" + path: {Level: "optional", Provenance: "scan_seeded"}},
		Sources:      []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: path}}}
	writable := false
	f := &runFold{
		w: httptest.NewRecorder(), r: httptest.NewRequest("POST", "/api/v1/runs/preflight", nil),
		req:         &createRunRequest{Agent: "claude-code"},
		spec:        types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: path, Target: target, ReadOnly: &writable}}},
		wsRefs:      []types.Workspace{ws},
		seed:        workspaceSeed{attached: []types.Workspace{ws}},
		attachments: []client.RunAttachment{{Kind: attachmentFolder, Target: target, ReadOnly: false}},
	}
	if !New(Config{}).stepRequirements(f, &foldRecorder{}) {
		t.Fatalf("the requirement fold refused: %v", f.w.(*httptest.ResponseRecorder).Body)
	}
	if !f.spec.WorkspaceMounts[0].ReadOnlyOrDefault() {
		t.Fatal("the fold left the mount writable; the case no longer narrows")
	}
	if !f.attachments[0].ReadOnly {
		t.Errorf("attachments = %+v, want the narrowed mount shown read-only", f.attachments)
	}
}
