// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// The preview names the set image whenever no attached workspace would replace it at launch: a
// workspace with no profile, or any workspace while no image builder is wired, leaves the run on
// the set image, so the preview must say so (#2570). A workspace that builds the image keeps the
// preview's image empty (two harnesses on one are refused instead: TestRunMode_TwoHarnesses...).
func TestPolicyPreview_SetImageSurvivesAWorkspaceThatBuildsNothing(t *testing.T) {
	images := map[string]string{"claude-code": "wardyn/agent-claude-code:local", "claude-code+codex-cli": setImage}
	profile, _ := json.Marshal(workspacescan.WorkspaceProfile{Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh,
		Source: workspacescan.SourceDeterministic})
	ws := func(p json.RawMessage) types.Workspace {
		return types.Workspace{ID: uuid.MustParse(wsID), Name: "w", Status: types.WorkspaceScanned, Profile: p,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: t.TempDir()}}}
	}
	attach := `"workspaces":[{"workspace_id":"` + wsID + `","target":"/home/agent/work"}]`
	two := `"experience":"interactive","tools":[` + claudeTool + `,` + codexTool + `],` + attach
	one := `"experience":"interactive","tools":[` + claudeTool + `],` + attach
	for _, tc := range []struct {
		name    string
		ws      types.Workspace
		builder bool
		body    string
		image   string
	}{
		{"a workspace with no profile", ws(nil), true, two, setImage},
		{"a profile and no image builder", ws(profile), false, two, setImage},
		{"one harness and a workspace that builds the image", ws(profile), true, one, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _ := harnessSetFixture(t, &capStore{}, images, nil)
			st.workspaces = []types.Workspace{tc.ws}
			if tc.builder {
				srv.cfg.ImageBuilder = buildEventBuilder{log: &buildEventLog{}}
			}
			w := harnessSetCall{"sub-plain", tc.body}.ask(t, srv, policyPreviewPath)
			if w.Code != http.StatusOK {
				t.Fatalf("preview = %d %s", w.Code, w.Body.String())
			}
			var got struct {
				Image *struct {
					Ref string `json:"ref"`
				} `json:"image"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			ref := ""
			if got.Image != nil {
				ref = got.Image.Ref
			}
			if ref != tc.image {
				t.Fatalf("preview image = %q, want %q", ref, tc.image)
			}
		})
	}
}
