// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func structDoc(t *testing.T, intent string) client.TemplateDocument {
	t.Helper()
	var doc client.TemplateDocument
	src := `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":` + intent + `}`
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func diagReasons(diags []client.TemplateDiagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Reason+"@"+d.Path)
	}
	return out
}

// TestTemplateStructDoorHoldsWhatTheBytesDoorHolds is the single-door proof: a
// document a handler decoded into the struct (permissively, as a JSON body or
// the CLI's own parse arrives) is held to everything the text door holds it to.
func TestTemplateStructDoorHoldsWhatTheBytesDoorHolds(t *testing.T) {
	group := testTemplateOwner("group", "", "eng")
	person := testTemplateOwner("person", "alice", "")
	cases := []struct {
		name, intent, want string
		owner              types.TemplateOwner
	}{
		{"secret in text", `{"task":"` + canaryAWSKey + `"}`, reasonTemplateSecretRefused + "@intent.task", person},
		{"runtime-only secret values", `{"inline_policy":{"llm_inspection":{"mode":"alert","workspace_secret_values":["` + canaryGitHubToken + `"]}}}`,
			reasonTemplateSecretRefused + "@intent.inline_policy.llm_inspection.workspace_secret_values", person},
		{"case-variant key is not materialised as runner_id", `{"Runner_ID":"r-1"}`, reasonTemplateFieldUnknown + "@intent.Runner_ID", group},
		{"run state", `{"run_id":"x"}`, reasonTemplateRunStateRefused + "@intent.run_id", person},
		{"redacted placeholder", `{"inline_policy":{"workspace_mounts":[{"source":"<redacted>","target":"/home/agent/x"}]}}`,
			reasonTemplateFieldInvalid + "@intent.inline_policy.workspace_mounts[0].source", person},
		{"secret-named config key", `{"components":[{"inline":{"hosts":["a.example.com"],"config":{"GITHUB_TOKEN":"x"}}}]}`,
			reasonTemplateSecretRefused + "@intent.components[0].inline.config.GITHUB_TOKEN", person},
		{"launch-time field", `{"title":"nightly"}`, reasonTemplateFieldExcluded + "@intent.title", person},
		{"person-only field in a shared template", `{"placement":"local","runner_id":"r1"}`, reasonTemplateSharedFieldRefused + "@intent.runner_id", group},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := structDoc(t, tc.intent)
			if len(doc.Intent.Names()) == 0 {
				t.Fatal("the struct probe is empty")
			}
			if _, diags := validateTemplateDocument(doc, tc.owner); !slices.Contains(diagReasons(diags), tc.want) {
				t.Fatalf("diagnostics %v do not hold %s", diagReasons(diags), tc.want)
			}
		})
	}
	good := structDoc(t, `{"inline_policy":{"allowed_domains":["api.example.com"]}}`)
	norm, diags := validateTemplateDocument(good, person)
	if len(diags) != 0 {
		t.Fatalf("a good document was refused: %v", diagReasons(diags))
	}
	if !norm.Intent.Has("inline_policy") {
		t.Error("the normalised document lost its intent")
	}
}

// TestTemplateStructValidatorsAreReachableOnlyThroughTheDoor keeps the second
// door shut: the struct-level validators and the decoder are named only inside
// validateTemplateBytes (and that function only inside the two doors). Every
// identifier of every non-test file is checked, package-level declarations
// included, so a function value (`var loose = validateTemplateContentDecoded`,
// `scope := validateTemplateScopeDecoded`) is a second door as much as a call.
func TestTemplateStructValidatorsAreReachableOnlyThroughTheDoor(t *testing.T) {
	allowedIn := map[string][]string{
		"validateTemplateContentDecoded": {"validateTemplateBytes"},
		"validateTemplateScopeDecoded":   {"validateTemplateBytes"},
		"decodeTemplateDocument":         {"validateTemplateBytes"},
		"validateTemplateBytes":          {"validateTemplateImport", "validateTemplateDocument"},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			enclosing := ""
			fn, isFunc := decl.(*ast.FuncDecl)
			if isFunc {
				enclosing = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				allowed, guarded := allowedIn[id.Name]
				if !guarded || (isFunc && id == fn.Name) {
					return true
				}
				seen++
				if !slices.Contains(allowed, enclosing) {
					where := enclosing
					if where == "" {
						where = "a package-level declaration"
					}
					t.Errorf("%s: %s names %s; only %v may (the single save door)", file, where, id.Name, allowed)
				}
				return true
			})
		}
	}
	if seen < 5 {
		t.Fatalf("saw only %d guarded names; the scan is broken", seen)
	}
}

func TestTemplatePolicyOverlayKeepsAbsentKeysAndAppliesPresentOnes(t *testing.T) {
	source := types.RunPolicySpec{
		AllowedDomains: []string{"a.example.com"}, AllowedMethods: []string{"GET"}, AutoStopAfterSec: 600, AllowAllEgress: true,
		MinConfinementClass: "gvisor", FirstUseApproval: "always_deny", DeniedDomains: []string{"evil.example.com"},
	}
	overlay := json.RawMessage(`{"allowed_methods":[],"auto_stop_after_sec":0,"allow_all_egress":false}`)
	got, err := templatePolicyOverlay(source, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AllowedMethods) != 0 || got.AutoStopAfterSec != 0 || got.AllowAllEgress {
		t.Errorf("present empty/zero/false keys did not override the source: %+v", got)
	}
	if !slices.Equal(got.AllowedDomains, []string{"a.example.com"}) || !slices.Equal(got.DeniedDomains, []string{"evil.example.com"}) ||
		got.MinConfinementClass != "gvisor" || got.FirstUseApproval != "always_deny" {
		t.Errorf("absent keys did not keep the source's values: %+v", got)
	}
	kept, err := templatePolicyOverlay(source, json.RawMessage(`{"denied_domains":["x.example.com"]}`))
	if err != nil || !slices.Equal(kept.AllowedMethods, []string{"GET"}) || kept.AutoStopAfterSec != 600 || !kept.AllowAllEgress {
		t.Errorf("an absent allowed_methods must keep the source's GET: %+v %v", kept, err)
	}
	if !slices.Equal(kept.DeniedDomains, []string{"x.example.com"}) {
		t.Errorf("a present list replaces the source's: %v", kept.DeniedDomains)
	}
	// An inline_policy sent as the request's own would replace the source: this is the difference.
	var viaRequest types.RunPolicySpec
	_ = json.Unmarshal(overlay, &viaRequest)
	if viaRequest.AllowedDomains != nil || viaRequest.MinConfinementClass != "" {
		t.Fatalf("the replacement reading is not the zero-filled one: %+v", viaRequest)
	}
}

func TestTemplatePolicyOverlayRefusesWhatSaysNothingOrIsNotAPolicy(t *testing.T) {
	source := types.RunPolicySpec{AllowedDomains: []string{"a.example.com"}}
	for name, overlay := range map[string]string{
		"empty": `{}`, "not an object": `[]`, "unknown key": `{"allowed_domian":[]}`, "wrong type": `{"allowed_domains":"x"}`,
		"null list would clear the source": `{"denied_domains":null}`, "null object": `{"llm_inspection":null}`, "null scalar": `{"auto_stop_after_sec":null}`,
	} {
		if _, err := templatePolicyOverlay(source, json.RawMessage(overlay)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestTemplateIntentBytesDoNotDependOnKeyOrderOrFormat(t *testing.T) {
	a := mustDecodeTemplate(t, []byte(`{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":{"inline_policy":{"allowed_domains":["a.example.com"],"allow_all_egress":false},"workspaces":[{"workspace_id":"w","target":"/home/agent/a"}]}}`))
	b := mustDecodeTemplate(t, []byte(`{"intent":{"workspaces":[{"target":"/home/agent/a","workspace_id":"w"}],"inline_policy":{"allow_all_egress":false,"allowed_domains":["a.example.com"]}},"coverage":"partial","kind":"RunTemplate","api_version":"wardyn/v1"}`))
	doc, diags := decodeTemplateDocument([]byte("api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  workspaces:\n    - {target: /home/agent/a, workspace_id: w}\n  inline_policy:\n    allowed_domains: [a.example.com]\n    allow_all_egress: false\n"), client.TemplateFormatYAML)
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	jc, _ := json.Marshal(doc)
	if string(ja) != string(jb) || string(ja) != string(jc) {
		t.Errorf("the same document stored different bytes:\n%s\n%s\n%s", ja, jb, jc)
	}
	if string(a.Intent.Raw("inline_policy")) != `{"allow_all_egress":false,"allowed_domains":["a.example.com"]}` {
		t.Errorf("nested keys are not sorted: %s", a.Intent.Raw("inline_policy"))
	}
}

// TestTemplateDependencyEdges holds every row of templateDependencies: the
// dependent field alone is refused, and it is satisfied by the field it needs
// or by a needs_setup entry.
func TestTemplateDependencyEdges(t *testing.T) {
	// The expected edges are written out, so deleting a row fails here as well as editing one.
	var got []string
	for _, dep := range templateDependencies {
		got = append(got, dep.field+">"+dep.needs)
	}
	want := []string{
		"devcontainer_ref>devcontainer_repo", "model_provider>agent", "tool_approvals>agent", "seed_auto_tools>agent",
		"inline_policy.tool_rules>agent", "interactive_start>interactive", "task_mode>interactive", "inline_policy.ui_apps>interactive",
		"runner_id>placement", "runner_id>runner_pool_id",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("dependency edges = %v, want %v", got, want)
	}
	policyType := reachableStructs(t, requestType)["RunPolicySpec"]
	value := func(name string) json.RawMessage {
		if policy, ok := strings.CutPrefix(name, "inline_policy."); ok {
			field, _ := templateStructField(policyType, policy)
			return sampleJSON(field.Type)
		}
		field, ok := templateStructField(requestType, name)
		if !ok {
			return json.RawMessage(`"x"`)
		}
		return sampleJSON(field.Type)
	}
	build := func(names ...string) client.TemplateDocument {
		var doc client.TemplateDocument
		policy := map[string]json.RawMessage{}
		for _, name := range names {
			if p, ok := strings.CutPrefix(name, "inline_policy."); ok {
				policy[p] = value(name)
			} else if err := doc.Intent.Set(name, value(name)); err != nil {
				t.Fatal(err)
			}
		}
		if len(policy) > 0 {
			if err := doc.Intent.Set("inline_policy", policy); err != nil {
				t.Fatal(err)
			}
		}
		return doc
	}
	for _, dep := range templateDependencies {
		t.Run(dep.field+" needs "+dep.needs, func(t *testing.T) {
			check := func(doc client.TemplateDocument) []client.TemplateDiagnostic {
				d := &templateDecoder{}
				d.checkDependencies(doc)
				var mine []client.TemplateDiagnostic
				for _, diag := range d.diags {
					if diag.Path == "intent."+dep.field && strings.Contains(diag.Message, " needs "+dep.needs+".") {
						mine = append(mine, diag)
					}
				}
				return mine
			}
			if diags := check(build(dep.field)); len(diags) != 1 || diags[0].Reason != reasonTemplateDependencyMissing {
				t.Errorf("the dependent field alone: %v", diagReasons(diags))
			}
			if diags := check(build(dep.field, dep.needs)); len(diags) != 0 {
				t.Errorf("with the field it needs: %v", diagReasons(diags))
			}
			listed := build(dep.field)
			listed.NeedsSetup = []client.TemplateSetupNeed{{Field: dep.needs}}
			if diags := check(listed); len(diags) != 0 {
				t.Errorf("listed under needs_setup: %v", diagReasons(diags))
			}
			if !templateKnownField(dep.field) || !templateKnownField(dep.needs) {
				t.Errorf("an edge names a field the registry does not know")
			}
		})
	}
}

func TestTemplateFullCoverage(t *testing.T) {
	const head = `"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"full","intent":`
	check := func(intent string) []string {
		return diagReasons(validateTemplateImport([]byte(`{`+head+intent+`}`), client.TemplateFormatJSON).Diagnostics)
	}
	gap := reasonTemplateCoverageInvalid + "@intent"
	cases := []struct {
		name, intent string
		gaps         bool
	}{
		{"agent only", `{"agent":"claude-code"}`, true},
		{"no mode chosen", `{"agent":"claude-code","drive":{"enabled":true}}`, true},
		{"only the mode is missing", `{"task_mode":"exec","image":"img","drive":{"enabled":true}}`, true},
		{"background without a task kind", `{"interactive":false,"agent":"claude-code","drive":{"enabled":true}}`, true},
		{"background agent task without its agent", `{"interactive":false,"task_mode":"harness","drive":{"enabled":true}}`, true},
		{"nothing attached", `{"interactive":true}`, true},
		{"background agent task", `{"interactive":false,"task_mode":"harness","agent":"claude-code","workspaces":[{"workspace_id":"w"}]}`, false},
		{"background command needs no agent", `{"interactive":false,"task_mode":"exec","image":"img","drive":{"enabled":true}}`, false},
		{"interactive environment needs no agent", `{"interactive":true,"drive":{"enabled":true}}`, false},
		{"policy mounts count as an attachment", `{"interactive":true,"inline_policy":{"workspace_repos":[{"repo":"acme/api"}]}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Contains(check(tc.intent), gap)
			if got != tc.gaps {
				t.Fatalf("coverage gap = %v, want %v: %v", got, tc.gaps, check(tc.intent))
			}
		})
	}
}

// TestTemplatePendingRulesAreStillRefusedByTheRun keeps a request field's
// Pending flag honest. A pending row whose field now exists on CreateRunRequest
// must still be refused by the run itself; when its lane honours the field, the
// flag is stale and a template would refuse something the run accepts.
func TestTemplatePendingRulesAreStillRefusedByTheRun(t *testing.T) {
	for _, r := range templateRequestRules {
		field, ok := templateStructField(requestType, r.Name)
		if r.Pending == "" || !ok {
			continue
		}
		var req createRunRequest
		body := `{"` + r.Name + `":` + string(sampleJSON(field.Type)) + `}`
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		if unappliedFieldsRefusal(req) == nil {
			t.Errorf("%s is pending %s but the run accepts it: clear the flag (template_fields.go)", r.Name, r.Pending)
		}
	}
	for _, r := range templatePolicyRules {
		if _, ok := templateStructField(reachableStructs(t, requestType)["RunPolicySpec"], r.Name); !ok {
			t.Errorf("policy rule %s names no field", r.Name)
		}
	}
}

func TestTemplateRegistryFollowsTheOwnersAccessAndModeRulings(t *testing.T) {
	section := func(name string) (string, string) {
		if policy, ok := strings.CutPrefix(name, "inline_policy."); ok {
			r, _ := templateRule(templatePolicyRules, policy)
			return r.Section, r.KeyedBy
		}
		r, _ := templateRule(templateRequestRules, name)
		return r.Section, r.KeyedBy
	}
	for name, want := range map[string][2]string{
		"inline_policy.push_rules":                {templateSectionSCMEntry, "provider+org"},
		"inline_policy.git_push_any_branch":       {templateSectionSCMEntry, "provider+org"},
		"inline_policy.github_capabilities":       {templateSectionSCMEntry, "provider+org"},
		"inline_policy.azure_devops_capabilities": {templateSectionSCMEntry, "provider+org"},
		"inline_policy.tool_rules":                {templateSectionHarness, "harness"},
		"tool_approvals":                          {templateSectionHarness, "harness"},
		"seed_auto_tools":                         {templateSectionHarness, "harness"},
		"model_provider":                          {templateSectionHarness, "harness"},
		"components":                              {templateSectionComp, "component"},
		"inline_policy.allowed_domains":           {templateSectionRunWide, ""},
	} {
		if sec, keyed := section(name); sec != want[0] || keyed != want[1] {
			t.Errorf("%s: section %s keyed by %q, want %s keyed by %q", name, sec, keyed, want[0], want[1])
		}
	}
	for name, omitted := range map[string]string{
		"interactive": templateOmitRequired, "task_mode": templateOmitRequired, "agent": templateOmitOptional, "interactive_start": templateOmitOptional,
	} {
		if r, _ := templateRule(templateRequestRules, name); r.Omitted != omitted {
			t.Errorf("%s omission = %s, want %s", name, r.Omitted, omitted)
		}
	}
	for _, name := range []string{"experience", "included_tools", "startup", "starting_folder", "no_repositories_or_drives", "runner_pool_id"} {
		if r, ok := templateRule(templateRequestRules, name); !ok || r.Pending == "" {
			t.Errorf("%s must be a pending carrier", name)
		}
	}
	for _, name := range []string{"title", "description"} {
		if r, _ := templateRule(templateRequestRules, name); r.Excluded != templateExcludeLaunch {
			t.Errorf("%s must be excluded as a launch-time field", name)
		}
	}
	if _, ok := templateRule(templateRequestRules, "pool_id"); ok {
		t.Error("pool_id is the C-pools carrier's working name no more: it is runner_pool_id")
	}
	for _, r := range slices.Concat(templateRequestRules, templatePolicyRules) {
		if r.Tab == "info" || r.Part == "info" {
			t.Errorf("%s still belongs to the removed Info tab", r.Name)
		}
	}
}
