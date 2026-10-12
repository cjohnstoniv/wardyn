// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	importPath = "POST /api/v1/templates/import"
	runsPath   = "POST /api/v1/runs"
)

const runDocYAML = `api_version: wardyn/v1
kind: Run
intent:
  shape: environment
  experience: interactive
  tools:
    - {id: claude-code, kind: harness}
  placement: remote
`

const runDocPolicyYAML = runDocYAML + `  inline_policy:
    allowed_domains: [api.example.test]
`

// runFileRig answers the import door the way the server does for a document that
// decodes (the normalised document is the one it read) and records the create.
func runFileRig(t *testing.T) *routeRig {
	t.Helper()
	return newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		importPath: func(w http.ResponseWriter, body []byte) {
			var req sdk.TemplateImportRequest
			_ = json.Unmarshal(body, &req)
			var doc sdk.TemplateDocument
			if err := json.Unmarshal(yamlToJSONForTest(t, req.Source), &doc); err != nil {
				t.Errorf("stub door: %v", err)
			}
			replyJSON(http.StatusOK, sdk.TemplateImportResult{Document: &doc, Diagnostics: []sdk.TemplateDiagnostic{}})(w, nil)
		},
		runsPath: replyJSON(http.StatusCreated, sdk.CreateRunResult{AgentRun: types.AgentRun{ID: uuid.New(), State: types.RunPending}}),
	})
}

func yamlToJSONForTest(t *testing.T, src string) []byte {
	t.Helper()
	out, err := policyToJSON([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRunFileSendsTheDocumentAsTheRequest: the intent is the request, and the
// title and description come from the flags.
func TestRunFileSendsTheDocumentAsTheRequest(t *testing.T) {
	rig := runFileRig(t)
	file := writeTemplateFile(t, "run.yaml", runDocYAML)
	if _, err := rig.run(t, "run", "--file", file, "--title", "Dev box", "--description", "for Gina"); err != nil {
		t.Fatal(err)
	}
	if got := rig.sent(importPath); len(got) != 1 || !strings.Contains(got[0], `"kind":"Run"`) || !strings.Contains(got[0], `"format":"yaml"`) {
		t.Errorf("the door was not asked for a Run document: %q", got)
	}
	sent := rig.sent(runsPath)
	if len(sent) != 1 {
		t.Fatalf("create requests = %d", len(sent))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sent[0]), &body); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"title": `"Dev box"`, "description": `"for Gina"`, "shape": `"environment"`,
		"experience": `"interactive"`, "placement": `"remote"`, "mode_contract": `"0.9"`,
	} {
		if string(body[key]) != want {
			t.Errorf("%s = %s, want %s", key, body[key], want)
		}
	}
	if !strings.Contains(string(body["tools"]), `"claude-code"`) {
		t.Errorf("tools = %s", body["tools"])
	}
}

// TestRunFileNeverSendsInlinePolicy: a document's inline_policy is an overlay onto
// the caller's default policy. Sent as inline_policy it would replace that policy,
// so until the request can carry policy_overlay the command refuses and sends nothing.
func TestRunFileNeverSendsInlinePolicy(t *testing.T) {
	rig := runFileRig(t)
	_, err := rig.run(t, "run", "--file", writeTemplateFile(t, "run.yaml", runDocPolicyYAML))
	if err == nil || !strings.Contains(err.Error(), "policy_overlay") {
		t.Fatalf("error = %v", err)
	}
	if got := rig.sent(runsPath); len(got) != 0 {
		t.Errorf("a create was sent: %s", got)
	}
}

// TestRunFileCarriesWhatTheFlagsMayNot: the placement a document names is not
// overwritten by the placement flags' empty defaults, and a document with no
// inline_policy sends no overlay.
func TestRunFileCarriesWhatTheFlagsMayNot(t *testing.T) {
	rig := runFileRig(t)
	doc := "api_version: wardyn/v1\nkind: Run\nintent:\n  experience: background\n  workload: {kind: command, command: make test}\n  no_repositories_or_drives: true\n  runner_id: r1\n  placement: local\n"
	if _, err := rig.run(t, "run", "--file", writeTemplateFile(t, "run.yaml", doc)); err != nil {
		t.Fatal(err)
	}
	var req sdk.CreateRunRequest
	if err := json.Unmarshal([]byte(rig.sent(runsPath)[0]), &req); err != nil {
		t.Fatal(err)
	}
	if req.Placement != sdk.PlacementLocal || req.RunnerID != "r1" || req.Workload == nil || req.Workload.Command != "make test" {
		t.Errorf("request = %+v", req)
	}
}

// TestRunFileStandsAloneBesideItsAllowedFlags: no per-field flag rides beside
// --file (A16), and nothing is sent when one does.
func TestRunFileStandsAloneBesideItsAllowedFlags(t *testing.T) {
	rig := runFileRig(t)
	file := writeTemplateFile(t, "run.yaml", runDocYAML)
	for _, flags := range [][]string{
		{"--agent", "claude-code"}, {"--task", "x"}, {"--policy-file", "p.yaml"}, {"--experience", "interactive"},
		{"--placement", "remote"}, {"--ends-in", "1h"}, {"--workspace", uuid.NewString()},
	} {
		_, err := rig.run(t, append([]string{"run", "--file", file}, flags...)...)
		if err == nil || !strings.Contains(err.Error(), "--file carries the whole run: remove "+flags[0]) {
			t.Errorf("--file with %v = %v", flags, err)
		}
	}
	if n := len(rig.sent(importPath)) + len(rig.sent(runsPath)); n != 0 {
		t.Errorf("a refused combination still sent %d request(s)", n)
	}
	if _, err := rig.run(t, "run", "--file", file, "--title", "t", "--json"); err != nil {
		t.Errorf("--file with --title and --json = %v", err)
	}
}

// TestRunFileReportsTheDoorsDiagnosticsAndLaunchesNothing.
func TestRunFileReportsTheDoorsDiagnosticsAndLaunchesNothing(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		importPath: replyJSON(http.StatusOK, sdk.TemplateImportResult{Diagnostics: []sdk.TemplateDiagnostic{
			{Path: "intent.title", Reason: "template_field_excluded", Message: "A run is named when it is launched."},
		}}),
	})
	_, err := rig.run(t, "run", "--file", writeTemplateFile(t, "run.json", `{"api_version":"wardyn/v1","kind":"Run","intent":{"title":"t"}}`))
	if err == nil || !strings.Contains(err.Error(), "1 problem") {
		t.Fatalf("error = %v", err)
	}
	if n := len(rig.sent(runsPath)); n != 0 {
		t.Errorf("a refused document still created a run (%d)", n)
	}
}

// TestRunFileReadsStdin: '-' reads the document from stdin without --format.
func TestRunFileReadsStdin(t *testing.T) {
	rig := runFileRig(t)
	if err := execCmdStdin(t, runDocYAML, "run", "--file", "-", "--url", rig.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	if n := len(rig.sent(runsPath)); n != 1 {
		t.Errorf("creates = %d", n)
	}
}
