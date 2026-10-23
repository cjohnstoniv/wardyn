// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const runDocHead = `"api_version":"wardyn/v1","kind":"Run"`

func importReasons(res client.TemplateImportResult) []string { return diagReasons(res.Diagnostics) }

// TestRunDocumentHeaderPerKind: the header is read by kind. A Run document has
// no name, coverage or needs_setup and no run title or description; a
// RunTemplate keeps its own description and coverage; a wrong kind is reported
// at kind and a wrong api_version at api_version.
func TestRunDocumentHeaderPerKind(t *testing.T) {
	const intent = `"intent":{"agent":"claude-code"}`
	for _, tc := range []struct {
		name, body, want string
	}{
		{"title at the top", `{` + runDocHead + `,"title":"nightly",` + intent + `}`, reasonTemplateFieldExcluded + "@title"},
		{"description at the top", `{` + runDocHead + `,"description":"d",` + intent + `}`, reasonTemplateFieldExcluded + "@description"},
		{"title in the intent", `{` + runDocHead + `,"intent":{"title":"nightly"}}`, reasonTemplateFieldExcluded + "@intent.title"},
		{"description in the intent", `{` + runDocHead + `,"intent":{"description":"d"}}`, reasonTemplateFieldExcluded + "@intent.description"},
		{"a Run has no name", `{` + runDocHead + `,"name":"n",` + intent + `}`, reasonTemplateFieldUnknown + "@name"},
		{"a Run has no coverage", `{` + runDocHead + `,"coverage":"partial",` + intent + `}`, reasonTemplateFieldUnknown + "@coverage"},
		{"a Run has no needs_setup", `{` + runDocHead + `,"needs_setup":[{"field":"agent"}],` + intent + `}`, reasonTemplateFieldUnknown + "@needs_setup"},
		{"a Run has no other top key", `{` + runDocHead + `,"extra":1,` + intent + `}`, reasonTemplateFieldUnknown + "@extra"},
		{"a RunTemplate has no title either", `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","title":"t",` + intent + `}`, reasonTemplateFieldExcluded + "@title"},
		{"wrong kind is reported at kind", `{"api_version":"wardyn/v1","kind":"Job",` + intent + `}`, reasonTemplateVersionUnsupported + "@kind"},
		{"missing kind is reported at kind", `{"api_version":"wardyn/v1",` + intent + `}`, reasonTemplateVersionUnsupported + "@kind"},
		{"wrong api_version is reported at api_version", `{"api_version":"wardyn/v2","kind":"Run",` + intent + `}`, reasonTemplateVersionUnsupported + "@api_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := decodeJSONTemplate(t, []byte(tc.body))
			if !slices.Contains(diagReasons(diags), tc.want) {
				t.Fatalf("diagnostics %v do not hold %s", diagReasons(diags), tc.want)
			}
		})
	}

	// A header value that is present but not a string is a wrong type, never "(missing)".
	for _, body := range []string{
		`{"api_version":"wardyn/v1","kind":5,` + intent + `}`,
		`{"api_version":2,"kind":"Run",` + intent + `}`,
	} {
		_, diags := decodeJSONTemplate(t, []byte(body))
		if len(diags) != 1 || diags[0].Reason != reasonTemplateFieldInvalid {
			t.Errorf("%s: diagnostics %+v, want one %s", body, diags, reasonTemplateFieldInvalid)
		}
	}
	if _, diags := decodeJSONTemplate(t, []byte(`{"api_version":"wardyn/v1",`+intent+`}`)); len(diags) != 1 || !strings.Contains(diags[0].Message, "kind (missing)") {
		t.Errorf("an absent kind is called missing: %+v", diags)
	}

	if doc, diags := decodeJSONTemplate(t, []byte(`{`+runDocHead+`,`+intent+`}`)); len(diags) != 0 || doc.Kind != client.RunDocumentKind {
		t.Fatalf("a bare Run document is refused: %v", diagReasons(diags))
	}
	if _, diags := decodeJSONTemplate(t, []byte(`{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","description":"mine",`+intent+`}`)); len(diags) != 0 {
		t.Fatalf("a RunTemplate keeps its own description: %v", diagReasons(diags))
	}
	res := validateTemplateImport([]byte(`{`+runDocHead+`,"intent":{}}`), client.TemplateFormatJSON, "")
	if res.Document == nil || strings.Contains(docJSON(t, res.Document), `"coverage"`) {
		t.Fatalf("an empty Run draft needs no coverage and writes none: %+v", res)
	}
}

func docJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRunDocumentCanonTriggers fires each of the four reasons the editor shows a
// canon sentence for, on the mock's own example translated to the real schema
// (ui/src/app/lib/run-document-canon.ts maps them).
func TestRunDocumentCanonTriggers(t *testing.T) {
	const yamlRun = "api_version: wardyn/v1\nkind: Run\n"
	for _, tc := range []struct {
		name, src string
		format    client.TemplateFormat
		want      string
	}{
		{"title and description are not carried", yamlRun + "title: Carry this title\nintent:\n  agent: claude-code\n", client.TemplateFormatYAML, reasonTemplateFieldExcluded + "@title"},
		{"ends_after_sec beside no_end", yamlRun + "intent:\n  agent: claude-code\n  ends_after_sec: 28800\n  no_end: true\n", client.TemplateFormatYAML, reasonRunEndConflictingFields + "@intent"},
		{"json that is not json", `{"api_version":"wardyn/v1","kind":"Run","intent":{`, client.TemplateFormatJSON, reasonTemplateDocumentInvalid + "@"},
		{"api_version must be wardyn/v1", "api_version: wardyn/v2\nkind: Run\nintent:\n  agent: claude-code\n", client.TemplateFormatYAML, reasonTemplateVersionUnsupported + "@api_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := validateTemplateImport([]byte(tc.src), tc.format, "")
			if res.Document != nil || !slices.Contains(importReasons(res), tc.want) {
				t.Fatalf("diagnostics %v do not hold %s", importReasons(res), tc.want)
			}
		})
	}
}

// docIntent turns a create body's fields into a kind Run document, as the editor
// would write it.
func docIntent(fields string) string { return `{` + runDocHead + `,"intent":{` + fields + `}}` }

// TestRunDocumentDoorAgreesWithCreate: one request, two ways in. What the run's
// dry doors accept the import door accepts, and a request the run refuses with a
// contract reason is refused at the import door with the same reason.
func TestRunDocumentDoorAgreesWithCreate(t *testing.T) {
	const ws = "11111111-1111-1111-1111-111111111111"
	base := `"agent":"claude-code","task":"t"`
	for _, tc := range []struct{ name, fields, reason string }{
		{"valid request", base, ""},
		{"valid shape", `"shape":"environment",` + interactiveOK, ""},
		{"resources accepted", base + `,"resources":{"cpu_millis":4000,"memory_mib":8192}`, ""},
		{"shape that does not fit", `"shape":"application",` + bgAgentTask, reasonRunShapeConflict},
		{"shape without an experience", `"shape":"environment",` + base, reasonRunShapeConflict},
		{"unknown placement", base + `,"placement":"cloud"`, reasonPlacementInvalid},
		{"pool id not a uuid", base + `,"runner_pool_id":"build-farm"`, "runner_pool_invalid"},
		{"workspace target outside home", base + `,"workspaces":[{"workspace_id":"` + ws + `","target":"/etc/x"}]`, reasonWorkspaceTargetInvalid},
		{"negative cpu", base + `,"resources":{"cpu_millis":-1}`, reasonResourcesInvalid},
		{"ends_after_sec beside no_end", base + `,"ends_after_sec":600,"no_end":true`, reasonRunEndConflictingFields},
		{"run-mode carriers beside older fields", `"experience":"interactive","tools":[` + claudeTool + `],"agent":"claude-code",` + noRepos, reasonRunModeConflict},
		{"malformed run-mode carrier", `"experience":"batch"`, reasonRunModeInvalid},
	} {
		for _, door := range contractDoors {
			if tc.reason == "" && door.name == "create" {
				continue
			}
			t.Run(tc.name+"/"+door.name, func(t *testing.T) {
				h := newHarness(t)
				h.srv.cfg.AgentImages = composeAgentImages
				w := do(t, h.srv, http.MethodPost, door.path, adminToken, `{"title":"t",`+tc.fields+`}`)
				if got := errorReason(w); got != tc.reason {
					t.Fatalf("the run answered %d reason %q, want %q: %s", w.Code, got, tc.reason, w.Body.String())
				}
				res := validateTemplateImport([]byte(docIntent(tc.fields)), client.TemplateFormatJSON, client.RunDocumentKind)
				switch {
				case tc.reason == "" && res.Document == nil:
					t.Fatalf("the run accepts it and the door refuses it: %v", importReasons(res))
				case tc.reason != "" && !slices.Contains(importReasons(res), tc.reason+"@intent"):
					t.Fatalf("the run refuses it with %s and the door answers %v", tc.reason, importReasons(res))
				}
			})
		}
	}
}

// TestRunShapeConflict pins shape's one rule per shape at create, preflight,
// preview and the import door: the run answers 422 run_shape_conflict with the
// shared sentence, and the door the same reason at intent.
func TestRunShapeConflict(t *testing.T) {
	// A command with no agent runs in an image the run names; a workspace is the
	// attachment that supplies one without a builder.
	const cmdWorkload = `"experience":"background","workload":{"kind":"command","command":"make"},"workspaces":[{"workspace_id":"` + wsID + `","target":"/home/agent/work"}]`
	// An application server (#1994): an interactive run that starts a command and
	// declares the app it serves.
	const (
		serverStart = `"startup":{"kind":"command","command":"npm run dev"}`
		serverApp   = `"inline_policy":{"ui_apps":[{"name":"web","port":3000}]}`
		notAnApp    = "an application is a background run of a command, or an interactive run that starts a command and declares its app: " +
			"set experience background and workload kind command, or experience interactive with startup kind command and ui_apps"
	)
	for _, tc := range []struct {
		name, fields, detail string
	}{
		{"application: a command", `"shape":"application",` + cmdWorkload, ""},
		{"application: an interactive server", `"shape":"application",` + interactiveOK + `,` + serverStart + `,` + serverApp, ""},
		{"application: an agent task is not an application", `"shape":"application",` + bgAgentTask, notAnApp},
		{"application: interactive is not an application", `"shape":"application",` + interactiveOK, notAnApp},
		{"application: a server needs its app", `"shape":"application",` + interactiveOK + `,` + serverStart, notAnApp},
		{"application: a server needs its command", `"shape":"application",` + interactiveOK + `,` + serverApp, notAnApp},
		{"environment: interactive", `"shape":"environment",` + interactiveOK, ""},
		{"environment: background is not an environment", `"shape":"environment",` + bgAgentTask, "an environment is interactive: set experience interactive"},
		{"agent_task: background agent task", `"shape":"agent_task",` + bgAgentTask, ""},
		{"agent_task: interactive with a harness", `"shape":"agent_task",` + interactiveOK, ""},
		{"agent_task: a background command is not an agent task", `"shape":"agent_task",` + cmdWorkload, "a background agent task needs workload kind agent_task"},
		{"a shape needs an experience", `"shape":"environment","agent":"claude-code","task":"t"`, "it needs experience, which says whether the run is background or interactive"},
	} {
		shape := strings.SplitN(strings.SplitN(tc.fields, `"shape":"`, 2)[1], `"`, 2)[0]
		for _, door := range contractDoors {
			if tc.detail == "" && door.name == "create" {
				continue
			}
			t.Run(tc.name+"/"+door.name, func(t *testing.T) {
				h := newHarness(t)
				w := do(t, h.srv, http.MethodPost, door.path, adminToken, modeBody(tc.fields))
				if tc.detail == "" {
					if errorReason(w) == reasonRunShapeConflict {
						t.Fatalf("a fitting shape answered %d %s", w.Code, w.Body.String())
					}
					return
				}
				var body struct{ Error string }
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusUnprocessableEntity ||
					errorReason(w) != reasonRunShapeConflict || body.Error != runShapeConflictMsg(shape, tc.detail) {
					t.Fatalf("got %d %s, want 422 %s %q", w.Code, w.Body.String(), reasonRunShapeConflict, runShapeConflictMsg(shape, tc.detail))
				}
			})
		}
		t.Run(tc.name+"/door", func(t *testing.T) {
			res := validateTemplateImport([]byte(docIntent(tc.fields)), client.TemplateFormatJSON, "")
			if got := slices.Contains(importReasons(res), reasonRunShapeConflict+"@intent"); got != (tc.detail != "") {
				t.Fatalf("door diagnostics %v, shape conflict wanted: %v", importReasons(res), tc.detail != "")
			}
		})
	}
	// A value outside the three is malformed, as an unknown experience is: 400 run_mode_invalid.
	for _, door := range contractDoors {
		t.Run("unknown shape/"+door.name, func(t *testing.T) {
			h := newHarness(t)
			w := do(t, h.srv, http.MethodPost, door.path, adminToken, modeBody(`"shape":"service",`+interactiveOK))
			if w.Code != http.StatusBadRequest || errorReason(w) != reasonRunModeInvalid {
				t.Fatalf("got %d %s, want 400 %s", w.Code, w.Body.String(), reasonRunModeInvalid)
			}
		})
	}
	if res := validateTemplateImport([]byte(docIntent(`"shape":"service",`+interactiveOK)), client.TemplateFormatJSON, ""); !slices.Contains(importReasons(res), reasonRunModeInvalid+"@intent") {
		t.Errorf("the door does not call an unknown shape malformed: %v", importReasons(res))
	}
	// An interactive agent task needs a harness in tools. The run's decode asks
	// for a tool first (request_field_unavailable), so the rule is reached by the
	// door and by the contract check itself.
	noHarness := createRunRequest{Shape: client.RunShapeAgentTask, Experience: client.ExperienceInteractive,
		Tools: []client.IncludedTool{{ID: "lint", Kind: client.IncludedToolComponent}}}
	if refusal := runShapeRefusal(noHarness); refusal == nil || refusal.body.Reason != reasonRunShapeConflict {
		t.Fatalf("an interactive agent task with no harness was not refused: %+v", refusal)
	}
	if refusal := runShapeRefusal(createRunRequest{}); refusal != nil {
		t.Fatalf("a request with no shape was refused: %+v", refusal)
	}
	// The server's app may come in a policy_overlay, as New Run sends one.
	server := createRunRequest{Shape: client.RunShapeApplication, Experience: client.ExperienceInteractive,
		Startup: &client.RunStartup{Kind: client.StartupCommand, Command: "npm run dev"}, PolicyOverlay: json.RawMessage(`{"ui_apps":[{"name":"web","port":3000}]}`)}
	if refusal := runShapeRefusal(server); refusal != nil {
		t.Fatalf("an application server with its app in policy_overlay was refused: %+v", refusal)
	}
	server.Startup = &client.RunStartup{Kind: client.StartupHarness, Tool: "claude-code"}
	if refusal := runShapeRefusal(server); refusal == nil || refusal.body.Reason != reasonRunShapeConflict {
		t.Fatalf("an application that starts a harness, not a command, was not refused: %+v", refusal)
	}
}

// TestRunCreateAuditRecordsShape: run.create names the shape a run was set up
// from, so an application run is recorded as one whether or not its launch
// intent is kept; a request with no shape writes the row it always did.
func TestRunCreateAuditRecordsShape(t *testing.T) {
	audit := func(req createRunRequest) map[string]any {
		return createRunAuditData(req, nil, types.CC2, "", "jti", nil, types.AutonomyResolution{}, false, runProviderChoice{})
	}
	if got := audit(createRunRequest{Shape: client.RunShapeApplication})["shape"]; got != client.RunShapeApplication {
		t.Fatalf("run.create shape = %v, want application", got)
	}
	if _, present := audit(createRunRequest{Agent: "claude-code"})["shape"]; present {
		t.Fatal("run.create carries a shape the request did not name")
	}
}

// failingStore panics on any call: the import door reads no store, so a handler
// that touched one would turn the 200 into a 500.
type failingStore struct{ store.Store }

// TestTemplateImportDoorIsLitAndStoresNothing: POST /templates/import answers
// 200 with the normalised document for a valid Run document and with
// diagnostics for an invalid one; it touches no store, and on a store that
// keeps no templates every other template route answers 501.
func TestTemplateImportDoorIsLitAndStoresNothing(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, failingStore{}))
	post := func(req client.TemplateImportRequest) (int, client.TemplateImportResult, string) {
		w := do(t, srv, http.MethodPost, "/api/v1/templates/import", adminToken, docJSON(t, req))
		var res client.TemplateImportResult
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return w.Code, res, w.Body.String()
	}

	code, res, raw := post(client.TemplateImportRequest{Format: client.TemplateFormatYAML, Kind: client.RunDocumentKind,
		Source: "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: environment\n  experience: interactive\n  tools:\n    - {id: claude-code, kind: harness}\n  no_repositories_or_drives: true\n"})
	if code != http.StatusOK || res.Document == nil || len(res.Diagnostics) != 0 || res.Document.Kind != client.RunDocumentKind ||
		!res.Document.Intent.Has("shape") || !strings.Contains(raw, `"diagnostics":[]`) {
		t.Fatalf("a valid Run document = %d %s", code, raw)
	}

	for fields, want := range map[string]string{
		`"title":"t"`: reasonTemplateFieldExcluded + "@intent.title",
		`"shape":"application","experience":"interactive","tools":[` + claudeTool + `]`: reasonRunShapeConflict + "@intent",
	} {
		code, res, raw = post(client.TemplateImportRequest{Format: client.TemplateFormatJSON, Source: docIntent(fields)})
		if code != http.StatusOK || res.Document != nil || !slices.Contains(importReasons(res), want) {
			t.Errorf("an invalid Run document %s = %d %s, want 200 with %s", fields, code, raw, want)
		}
		for _, d := range res.Diagnostics {
			if d.Message == "" {
				t.Errorf("diagnostic %+v has no sentence", d)
			}
		}
	}

	// A valid document of the other kind: its own sentence, naming both kinds, and the
	// document's other problems are reported beside it.
	template := `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":{"agent":"claude-code"}}`
	for _, tc := range []struct{ name, source, want, got string }{
		{"a RunTemplate at the Run door", template, client.RunDocumentKind, client.TemplateDocumentKind},
		{"a Run at the RunTemplate door", docIntent(`"agent":"claude-code"`), client.TemplateDocumentKind, client.RunDocumentKind},
	} {
		_, res, _ = post(client.TemplateImportRequest{Format: client.TemplateFormatJSON, Kind: tc.want, Source: tc.source})
		i := slices.IndexFunc(res.Diagnostics, func(d client.TemplateDiagnostic) bool { return d.Path == "kind" })
		if res.Document != nil || i < 0 || res.Diagnostics[i].Reason != reasonTemplateVersionUnsupported || res.Diagnostics[i].Message != templateKindMismatchMsg(tc.want, tc.got) {
			t.Errorf("%s: %+v, want the kind-mismatch sentence", tc.name, res.Diagnostics)
		}
	}
	_, res, _ = post(client.TemplateImportRequest{Format: client.TemplateFormatJSON, Kind: client.TemplateDocumentKind, Source: `{` + runDocHead + `,"title":"t","intent":{"agent":"claude-code"}}`})
	for _, want := range []string{reasonTemplateVersionUnsupported + "@kind", reasonTemplateFieldExcluded + "@title"} {
		if !slices.Contains(importReasons(res), want) {
			t.Errorf("a mismatched kind with another problem: %v do not hold %s", importReasons(res), want)
		}
	}
	if _, res, _ = post(client.TemplateImportRequest{Format: client.TemplateFormatJSON, Source: `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":{"agent":"claude-code"}}`}); res.Document == nil {
		t.Errorf("a RunTemplate imports through the same door: %v", importReasons(res))
	}
	if code, _, _ = post(client.TemplateImportRequest{Format: client.TemplateFormatJSON, Kind: "Job", Source: "{}"}); code != http.StatusBadRequest {
		t.Errorf("an unknown expected kind = %d, want 400", code)
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/templates/import", adminToken, `{"format":"json","source":"{}","surprise":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown request field = %d, want 400", w.Code)
	}

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/templates"},
		{http.MethodPost, "/api/v1/templates"},
		{http.MethodGet, "/api/v1/templates/" + wsID},
		{http.MethodDelete, "/api/v1/templates/" + wsID},
	} {
		if w := do(t, srv, route.method, route.path, adminToken, `{}`); w.Code != http.StatusNotImplemented || errorReason(w) != reasonTemplatesUnavailable {
			t.Errorf("%s %s = %d %s, want 501 %s", route.method, route.path, w.Code, w.Body.String(), reasonTemplatesUnavailable)
		}
	}
}

// TestPresetDoorHoldsTheRunContract: a preset is held at write to the same
// shape and availability checks a document is at the import door, so an admin
// cannot store one no member can ever launch.
func TestPresetDoorHoldsTheRunContract(t *testing.T) {
	f := newMemPresetFixture(t)
	for _, tc := range []struct {
		name   string
		req    client.CreateRunRequest
		reason string
	}{
		{"a pool id that is not an id", client.CreateRunRequest{Agent: "claude-code", RunnerPoolID: "nope"}, "runner_pool_invalid"},
		{"an image choice no lane applies yet", client.CreateRunRequest{Agent: "claude-code", AllowedImage: "ghcr.io/acme/dev:1"}, reasonRequestFieldUnavailable},
		{"a built-in component no lane applies yet", client.CreateRunRequest{Agent: "claude-code", Components: []client.ComponentRef{{Builtin: "azure_devops", Org: "acme"}}}, reasonRequestFieldUnavailable},
		{"a workspace target outside home", client.CreateRunRequest{Agent: "claude-code", Workspaces: []client.WorkspaceSelection{{WorkspaceID: wsID, Target: "/etc/x"}}}, reasonWorkspaceTargetInvalid},
		{"a wildcard override", client.CreateRunRequest{Agent: "claude-code", Overrides: &client.RunOverrides{Agent: &client.AgentOverrides{AddHosts: []string{"*.example.com"}}}}, reasonOverrideRefused},
		// A preset never carries experience, so a shape would be told to set one and then
		// refused for it: it is refused as a run-mode field instead.
		{"a shape", client.CreateRunRequest{Agent: "claude-code", Shape: client.RunShapeEnvironment}, reasonRequestFieldUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.put(t, "team-x", client.PresetRequest{Request: tc.req})
			if reasonOf(t, w) != tc.reason || w.Code < 400 {
				t.Fatalf("= %d %s, want reason %s", w.Code, w.Body.String(), tc.reason)
			}
		})
	}
	// placement local is the launch door's to judge (stepLocalEligibility), as for a request sent directly.
	if w := f.put(t, "team-local", client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code", Placement: client.PlacementLocal}}); w.Code != http.StatusCreated {
		t.Errorf("a preset carrying placement local = %d %s, want it stored", w.Code, w.Body.String())
	}
	if w := f.put(t, "team-x", client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code", Shape: client.RunShapeEnvironment}}); !strings.Contains(w.Body.String(), "request.shape cannot be stored in a preset") {
		t.Errorf("a preset carrying shape = %d %s, want request.shape named", w.Code, w.Body.String())
	}
	if w := f.put(t, "team-x", client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code", Resources: &client.RunResources{CPUMillis: 2000}}}); w.Code != http.StatusCreated {
		t.Fatalf("a preset that holds the contract = %d %s", w.Code, w.Body.String())
	}
}

// TestTemplateSaveDoorRefusesARunDocument: the save door reads templates, so a
// kind Run document is refused at kind.
func TestTemplateSaveDoorRefusesARunDocument(t *testing.T) {
	doc := structDoc(t, `{"agent":"claude-code"}`)
	doc.Kind = client.RunDocumentKind
	if _, diags := validateTemplateDocument(doc, testTemplateOwner("person", "alice", "")); !slices.Contains(diagReasons(diags), reasonTemplateVersionUnsupported+"@kind") {
		t.Fatalf("a Run document was accepted as a template: %v", diagReasons(diags))
	}
}
