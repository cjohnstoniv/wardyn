// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// routeRig is an httptest server that answers by "METHOD /path" and records each
// request body under the same key.
type routeRig struct {
	*httptest.Server
	mu     sync.Mutex
	bodies map[string][]string
}

func newRouteRig(t *testing.T, routes map[string]func(w http.ResponseWriter, body []byte)) *routeRig {
	t.Helper()
	rig := &routeRig{bodies: map[string][]string{}}
	rig.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		rig.mu.Lock()
		rig.bodies[key] = append(rig.bodies[key], string(body))
		rig.mu.Unlock()
		h, ok := routes[key]
		if !ok {
			http.Error(w, `{"error":"no route `+key+`"}`, http.StatusNotFound)
			return
		}
		h(w, body)
	}))
	t.Cleanup(rig.Close)
	return rig
}

func (r *routeRig) sent(key string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies[key]...)
}

func (r *routeRig) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return execCmdCapture(t, append(args, "--url", r.URL, "--token", "tok")...)
}

func replyJSON(status int, v any) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
}

const grantsPath = "/api/v1/admin/template-group-admins"

// TestTemplateGroupAdminRoundTrip: grant, list, revoke against a server that
// keeps the grants, and the bodies the three commands sent.
func TestTemplateGroupAdminRoundTrip(t *testing.T) {
	var grants []sdk.TemplateGroupAdmin
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"PUT " + grantsPath: func(w http.ResponseWriter, body []byte) {
			var g sdk.TemplateGroupAdmin
			_ = json.Unmarshal(body, &g)
			grants = append(grants, g)
			replyJSON(http.StatusOK, g)(w, nil)
		},
		"GET " + grantsPath: func(w http.ResponseWriter, _ []byte) {
			replyJSON(http.StatusOK, sdk.TemplateGroupAdmins{Grants: grants})(w, nil)
		},
		"DELETE " + grantsPath: func(w http.ResponseWriter, _ []byte) {
			grants = nil
			w.WriteHeader(http.StatusNoContent)
		},
	})

	out, err := rig.run(t, "template", "group-admin", "grant", "devteam", "gina@corp.example", "--label", "Dev Team")
	if err != nil || !strings.Contains(out, `"person": "gina@corp.example"`) {
		t.Fatalf("grant: %v\n%s", err, out)
	}
	if got := rig.sent("PUT " + grantsPath); len(got) != 1 || got[0] != `{"group_id":"devteam","person":"gina@corp.example","label":"Dev Team"}` {
		t.Errorf("grant body = %q", got)
	}
	out, err = rig.run(t, "template", "group-admin", "list")
	if err != nil || !strings.Contains(out, `"group_id": "devteam"`) || !strings.Contains(out, `"label": "Dev Team"`) {
		t.Fatalf("list after grant: %v\n%s", err, out)
	}
	out, err = rig.run(t, "template", "group-admin", "revoke", "devteam", "gina@corp.example")
	if err != nil || !strings.Contains(out, "revoked gina@corp.example from devteam") {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	if got := rig.sent("DELETE " + grantsPath); len(got) != 1 || got[0] != `{"group_id":"devteam","person":"gina@corp.example"}` {
		t.Errorf("revoke body = %q", got)
	}
	out, err = rig.run(t, "template", "group-admin", "list")
	if err != nil || !strings.Contains(out, `"grants": null`) && !strings.Contains(out, `"grants": []`) {
		t.Errorf("list after revoke: %v\n%s", err, out)
	}
	if err := execCmd(t, "template", "group-admin", "grant", "devteam"); err == nil {
		t.Error("a grant with no person was accepted")
	}
}

// TestTemplateGroupAdminSurfacesTheServersRefusal: the person the grant names
// matches nobody, and the server's reason reaches the caller.
func TestTemplateGroupAdminSurfacesTheServersRefusal(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"PUT " + grantsPath: replyJSON(http.StatusUnprocessableEntity, map[string]string{"error": "No person has that email.", "reason": "owner_unresolved"}),
	})
	_, err := rig.run(t, "template", "group-admin", "grant", "devteam", "nobody@corp.example")
	var apiErr *sdk.APIError
	if !errors.As(err, &apiErr) || apiErr.Reason != "owner_unresolved" {
		t.Fatalf("error = %v, want owner_unresolved", err)
	}
}

const presetPath = "/api/v1/presets/ci-egress"

func presetRoutes(p sdk.Preset, saved *sdk.TemplateSaveRequest) map[string]func(http.ResponseWriter, []byte) {
	return map[string]func(http.ResponseWriter, []byte){
		"GET " + presetPath: replyJSON(http.StatusOK, p),
		"POST /api/v1/templates": func(w http.ResponseWriter, body []byte) {
			_ = json.Unmarshal(body, saved)
			replyJSON(http.StatusCreated, sdk.Template{Name: saved.Name, Scope: saved.Scope})(w, nil)
		},
	}
}

// TestTemplateFromPresetSavesTheConvertedDocumentToTheOrganisation: the preset's
// request becomes the template's intent, field by field, and nothing is saved
// for a preset limited to user types until --everyone says so.
func TestTemplateFromPresetSavesTheConvertedDocumentToTheOrganisation(t *testing.T) {
	policy := sdk.RunPolicySpec{AllowedDomains: []string{}}
	preset := sdk.Preset{Name: "ci-egress", Description: "egress only", Request: sdk.CreateRunRequest{
		Agent: "claude-code", ModeContract: sdk.RunModeContractV09, Experience: sdk.ExperienceBackground,
		NoRepositoriesOrDrives: true, InlinePolicy: &policy,
	}}
	var saved sdk.TemplateSaveRequest
	rig := newRouteRig(t, presetRoutes(preset, &saved))
	if _, err := rig.run(t, "template", "from-preset", "ci-egress"); err != nil {
		t.Fatal(err)
	}
	if saved.Scope != sdk.TemplateScopeOrg || saved.Name != "ci-egress" || saved.Description != "egress only" || saved.GroupID != "" {
		t.Errorf("saved = %+v", saved)
	}
	want, err := sdk.PresetDocument(preset, false)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(saved.Document)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("document = %s\nwant       %s", gotJSON, wantJSON)
	}
	if saved.Document.Intent.Has("mode_contract") {
		t.Error("mode_contract rode into the template")
	}

	preset.UserTypes = []string{"analyst"}
	saved = sdk.TemplateSaveRequest{}
	rig = newRouteRig(t, presetRoutes(preset, &saved))
	_, err = rig.run(t, "template", "from-preset", "ci-egress")
	if err == nil || !strings.Contains(err.Error(), "--everyone") {
		t.Fatalf("a limited preset was converted without --everyone: %v", err)
	}
	if got := rig.sent("POST /api/v1/templates"); len(got) != 0 {
		t.Fatalf("a refused conversion still saved: %v", got)
	}
	if _, err = rig.run(t, "template", "from-preset", "ci-egress", "--everyone"); err != nil {
		t.Fatal(err)
	}
	if saved.Name != "ci-egress" {
		t.Errorf("--everyone did not save: %+v", saved)
	}
}

// TestTemplateImportKindIsSentAndChecked: --kind rides the import request, and a
// value that is neither document kind is refused before any request.
func TestTemplateImportKindIsSentAndChecked(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"POST /api/v1/templates/import": replyJSON(http.StatusOK, sdk.TemplateImportResult{Diagnostics: []sdk.TemplateDiagnostic{}, Document: &sdk.TemplateDocument{Kind: sdk.RunDocumentKind}}),
	})
	file := writeTemplateFile(t, "run.yaml", "api_version: wardyn/v1\nkind: Run\nintent: {}\n")
	for _, kind := range []string{"Run", "RunTemplate"} {
		if _, err := rig.run(t, "template", "import", file, "--kind", kind); err != nil {
			t.Fatal(err)
		}
	}
	got := rig.sent("POST /api/v1/templates/import")
	if len(got) != 2 || !strings.Contains(got[0], `"kind":"Run"`) || !strings.Contains(got[1], `"kind":"RunTemplate"`) {
		t.Errorf("import bodies = %q", got)
	}
	if _, err := rig.run(t, "template", "import", file); err != nil {
		t.Fatal(err)
	}
	if last := rig.sent("POST /api/v1/templates/import")[2]; strings.Contains(last, `"kind"`) {
		t.Errorf("no --kind still sent a kind: %s", last)
	}
	if _, err := rig.run(t, "template", "import", file, "--kind", "Policy"); err == nil || !strings.Contains(err.Error(), "is not Run or RunTemplate") {
		t.Errorf("--kind Policy = %v", err)
	}
	if n := len(rig.sent("POST /api/v1/templates/import")); n != 3 {
		t.Errorf("a refused --kind still sent a request (%d)", n)
	}
}

// TestTemplateSaveFromStdin (R2-0045): save advertises '-' for stdin, so it takes
// --format like import does, and a document read from stdin is saved.
func TestTemplateSaveFromStdin(t *testing.T) {
	var saved sdk.TemplateSaveRequest
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"POST /api/v1/templates": func(w http.ResponseWriter, body []byte) {
			_ = json.Unmarshal(body, &saved)
			replyJSON(http.StatusCreated, sdk.Template{Name: saved.Name})(w, nil)
		},
	})
	err := execCmdStdin(t, templateYAML, "template", "save", "-", "--format", "yaml", "--url", rig.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("save - --format yaml: %v", err)
	}
	if saved.Name != "egress only" || saved.Scope != sdk.TemplateScopePerson || !saved.Document.Intent.Has("inline_policy") {
		t.Errorf("saved = %+v", saved)
	}
	saved = sdk.TemplateSaveRequest{}
	err = execCmdStdin(t, `{"api_version":"wardyn/v1","kind":"RunTemplate","name":"j","coverage":"partial","intent":{"agent":"claude-code"}}`,
		"template", "save", "-", "--format", "json", "--url", rig.URL, "--token", "tok")
	if err != nil || saved.Name != "j" {
		t.Errorf("save - --format json: %v %+v", err, saved)
	}
	if err := execCmdStdin(t, templateYAML, "template", "save", "-", "--url", rig.URL, "--token", "tok"); err == nil || !strings.Contains(err.Error(), "needs --format") {
		t.Errorf("save - with no format = %v, want the --format hint", err)
	}
}
