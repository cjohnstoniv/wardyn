// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestComponentImageMet(t *testing.T) {
	d := strings.Repeat("a", 64)
	for _, tc := range []struct {
		want, have string
		met        bool
	}{
		{"ghcr.io/acme/tools", "ghcr.io/acme/tools:1.2", true},
		{"ghcr.io/acme/tools:1.2", "ghcr.io/acme/tools:1.2", true},
		{"ghcr.io/acme/tools:1.2", "ghcr.io/acme/tools:1.3", false},
		{"ghcr.io/acme/tools:1.2", "ghcr.io/acme/tools:1.2@sha256:" + d, true},
		{"nginx", "docker.io/library/nginx:1.27", true},
		{"ghcr.io/acme/tools@sha256:" + d, "ghcr.io/acme/tools@sha256:" + d, true},
		{"ghcr.io/acme/tools@sha256:" + d, "ghcr.io/acme/tools:1.2", false},
		{"ghcr.io/acme/tools", "ghcr.io/acme/other", false},
		{"ghcr.io/acme/tools", "", false},
		{"ghcr.io/acme/tools", "not a ref", false},
		// A bare repository runs as :latest.
		{"nginx:latest", "nginx", true},
		{"nginx:1.27", "nginx", false},
		// A family requirement still takes a digest-only image.
		{"nginx", "nginx@sha256:" + d, true},
	} {
		if got := componentImageMet(tc.want, tc.have); got != tc.met {
			t.Errorf("componentImageMet(%q, %q) = %v, want %v", tc.want, tc.have, got, tc.met)
		}
	}
}

func TestComponentRunImage(t *testing.T) {
	const toolsRef = "ghcr.io/acme/tools:1.2"
	images := map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
	ws := func(b *types.WorkspaceBaseImage, profile string) types.Workspace {
		w := types.Workspace{BaseImage: b}
		if profile != "" {
			w.Profile = []byte(profile)
		}
		return w
	}
	registry := &types.WorkspaceBaseImage{Kind: "registry", Image: toolsRef}
	oldClient := createRunRequest{}
	runMode := createRunRequest{ModeContract: client.RunModeContractV09,
		Tools: []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness}}}
	// What launch starts a run-mode run from, for the same request.
	want, _ := harnessSetImage(harnessSet(runMode), images, false)
	if want.Ref != images["claude-code"] {
		t.Fatalf("harnessSetImage = %q, want the map's entry %q", want.Ref, images["claude-code"])
	}
	withImage := runMode
	withImage.Image = "ghcr.io/acme/other:9"
	dev := oldClient
	dev.DevcontainerRepo = "https://example.com/acme/repo"
	set := oldClient
	set.Image = toolsRef

	for name, tc := range map[string]struct {
		req     createRunRequest
		wsRefs  []types.Workspace
		builder bool
		want    string
	}{
		"the request's image, whatever the workspaces":    {set, []types.Workspace{ws(&types.WorkspaceBaseImage{Kind: "registry", Image: "ghcr.io/acme/other:1"}, "")}, false, toolsRef},
		"a dev container build, even beside a base image": {dev, []types.Workspace{ws(registry, "")}, false, ""},
		"a workspace's base image":                        {oldClient, []types.Workspace{ws(&types.WorkspaceBaseImage{Kind: "recommended"}, ""), ws(registry, "")}, false, toolsRef},
		"a recommended workspace, older client":           {oldClient, []types.Workspace{ws(&types.WorkspaceBaseImage{Kind: "recommended"}, "")}, false, ""},
		"no workspace, older client":                      {oldClient, nil, false, ""},
		"run mode, no image, no workspace":                {runMode, nil, false, want.Ref},
		"run mode with an image keeps its precedence":     {withImage, nil, false, "ghcr.io/acme/other:9"},
		"run mode, scanned workspace, built at launch":    {runMode, []types.Workspace{ws(nil, "{}")}, true, ""},
		"run mode, scanned workspace, no builder wired":   {runMode, []types.Workspace{ws(nil, "{}")}, false, want.Ref},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{cfg: Config{AgentImages: images}}
			if tc.builder {
				s.cfg.ImageBuilder = fakeImageBuilder{}
			}
			if got := s.componentRunImage(tc.req, tc.wsRefs); got != tc.want {
				t.Errorf("componentRunImage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestComponentImageRefusal_Sentences(t *testing.T) {
	id := uuid.New()
	comp := func(name, image, source string, cid *uuid.UUID) attachedComponent {
		return attachedComponent{source: source, snapshot: types.RunComponent{Name: name, ComponentID: cid,
			Definition: types.ComponentDefinition{Image: image}}}
	}
	const want = "ghcr.io/acme/tools:1.2"
	for name, tc := range map[string]struct {
		comps   runComponents
		have    string
		body    string
		ordinal int
		source  string
		id      *uuid.UUID
	}{
		"another tag": {
			comps:  runComponents{attached: []attachedComponent{comp("tools", want, componentSourceOrg, &id)}},
			have:   "ghcr.io/acme/tools:1.3",
			body:   "components[0]: tools needs the image ghcr.io/acme/tools:1.2, and this run uses ghcr.io/acme/tools:1.3. Use that image for the run, or remove the component.",
			source: componentSourceOrg, id: &id,
		},
		"no image known": {
			comps:  runComponents{attached: []attachedComponent{comp("tools", want, componentSourceSelf, &id)}},
			body:   "components[0]: tools needs the image ghcr.io/acme/tools:1.2, and this run uses the default image or builds one when it starts. Use that image for the run, or remove the component.",
			source: componentSourceSelf, id: &id,
		},
		"an unnamed inline one after a met one": {
			comps: runComponents{attached: []attachedComponent{
				comp("plain", "", componentSourceInline, nil),
				comp("", want, componentSourceInline, nil),
			}},
			have:    "ghcr.io/acme/tools:1.3",
			body:    "components[1]: This component needs the image ghcr.io/acme/tools:1.2, and this run uses ghcr.io/acme/tools:1.3. Use that image for the run, or remove the component.",
			ordinal: 1, source: componentSourceInline,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := componentImageRefusal(tc.comps, tc.have)
			if r == nil {
				t.Fatal("no refusal")
			}
			if r.refusal.status != http.StatusUnprocessableEntity || r.refusal.body.Reason != "component_image_unmet" || r.reason != "component_image_unmet" {
				t.Errorf("refusal = %d %q (reason %q), want 422 component_image_unmet", r.refusal.status, r.refusal.body.Reason, r.reason)
			}
			if r.refusal.body.Error != tc.body {
				t.Errorf("body = %q, want %q", r.refusal.body.Error, tc.body)
			}
			if r.ordinal != tc.ordinal || r.source != tc.source || (tc.id == nil) != (r.id == nil) || (tc.id != nil && *r.id != *tc.id) {
				t.Errorf("ordinal/source/id = %d %q %v, want %d %q %v", r.ordinal, r.source, r.id, tc.ordinal, tc.source, tc.id)
			}
		})
	}
	for name, comps := range map[string]runComponents{
		"none attached":     {},
		"no requirement":    {attached: []attachedComponent{comp("tools", "", componentSourceOrg, nil)}},
		"a requirement met": {attached: []attachedComponent{comp("tools", "ghcr.io/acme/tools", componentSourceOrg, nil)}},
	} {
		if r := componentImageRefusal(comps, want); r != nil {
			t.Errorf("%s: refusal %+v, want nil", name, r.refusal.body)
		}
	}
}

// A refused launch writes one run.component.refuse row and leaves no run.
func TestComponentImage_LaunchRefusalIsAudited(t *testing.T) {
	f := newComponentFixture(t)
	body := componentBody(f.org(compOrgID, types.ComponentDefinition{Hosts: []string{"org-api.example"}, Image: "ghcr.io/acme/tools:1.2"}))
	before := len(f.rec.snapshot())
	w := f.ask(t, "/api/v1/runs", body)
	if got := decodeErrorBody(t, w); w.Code != http.StatusUnprocessableEntity || got.Reason != "component_image_unmet" {
		t.Fatalf("launch = %d %s, want 422 component_image_unmet", w.Code, w.Body.String())
	}
	want := []string{` {"ordinal":0,"reason":"component_image_unmet","source":"org"}`}
	if rows := f.componentAudit(before, "run.component.refuse"); !slices.Equal(rows, want) {
		t.Errorf("run.component.refuse = %v, want %v", rows, want)
	}
	if len(f.st.snapshots) != 0 || len(f.st.runs) != 0 {
		t.Error("a refused launch left a run or a snapshot")
	}
}
