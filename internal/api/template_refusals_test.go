// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

var templateRefusalGolden = filepath.Join("..", "..", "ui", "src", "app", "lib", "template-refusals.golden.json")

type templateSentence struct {
	Key  string   `json:"key"`
	Args []string `json:"args"`
	Text string   `json:"text"`
}

// templateSentences are the Go sentences with the arguments the golden pins.
func templateSentences() []templateSentence {
	return []templateSentence{
		{"DOCUMENT_INVALID", []string{"it has the key \"agent\" twice"}, templateDocumentInvalidMsg("it has the key \"agent\" twice")},
		{"VERSION_UNSUPPORTED", []string{"wardyn/v2 RunTemplate"}, templateVersionUnsupportedMsg("wardyn/v2 RunTemplate")},
		{"FIELD_UNKNOWN", []string{"intent.agnet"}, templateFieldUnknownMsg("intent.agnet")},
		{"FIELD_INVALID", []string{"intent.interactive", "must be true or false"}, templateFieldInvalidMsg("intent.interactive", "must be true or false")},
		{"FIELD_EXCLUDED", []string{"intent.preset"}, templateFieldExcludedMsg("intent.preset")},
		{"SECRET_REFUSED", []string{"intent.task"}, templateSecretRefusedMsg("intent.task")},
		{"RUN_STATE_REFUSED", []string{"intent.run_id"}, templateRunStateRefusedMsg("intent.run_id")},
		{"METADATA_IN_CONTENT", []string{"group_id"}, templateMetadataInContentMsg("group_id")},
		{"FIELD_UNAVAILABLE", []string{"intent.overrides"}, templateFieldUnavailableMsg("intent.overrides")},
		{"DEPENDENCY_MISSING", []string{"intent.model_provider", "agent"}, templateDependencyMissingMsg("intent.model_provider", "agent")},
		{"COVERAGE_INVALID", []string{"the intent specifies no field"}, templateCoverageInvalidMsg("the intent specifies no field")},
		{"SHARED_FIELD_REFUSED", []string{"intent.runner_id", "group"}, templateSharedFieldRefusedMsg("intent.runner_id", "group")},
		{"SCOPE_FORBIDDEN_ORG", []string{"org"}, templateScopeForbiddenMsg("org")},
		{"SCOPE_FORBIDDEN_GROUP", []string{"group"}, templateScopeForbiddenMsg("group")},
		{"SCOPE_FORBIDDEN_PERSON", []string{"person"}, templateScopeForbiddenMsg("person")},
		{"GROUP_UNVERIFIED", nil, templateGroupUnverifiedMsg()},
		{"NOT_FOUND", nil, templateNotFoundMsg()},
		{"REVISION_CONFLICT", []string{"4"}, templateRevisionConflictMsg(4)},
	}
}

// TestTemplateRefusalSentencesMatchGolden pins the Go sentences to the table
// the console's twin is pinned to. Regenerate with WARDYN_UPDATE_GOLDEN=1.
func TestTemplateRefusalSentencesMatchGolden(t *testing.T) {
	want, err := json.MarshalIndent(templateSentences(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(bytes.ReplaceAll(want, []byte(`"args": null`), []byte(`"args": []`)), '\n')
	if os.Getenv("WARDYN_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(templateRefusalGolden, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(templateRefusalGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s does not hold the Go sentences; run WARDYN_UPDATE_GOLDEN=1 go test ./internal/api -run TestTemplateRefusalSentencesMatchGolden", templateRefusalGolden)
	}
}

// templateReasonConsts reads the template reason block of reasons.go.
func templateReasonConsts(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "reasons.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || !strings.HasPrefix(spec.Names[0].Name, "reasonTemplate") {
			return true
		}
		lit, ok := spec.Values[0].(*ast.BasicLit)
		if !ok {
			t.Fatalf("%s is not a string literal", spec.Names[0].Name)
		}
		v, _ := strconv.Unquote(lit.Value)
		out[spec.Names[0].Name] = v
		return false
	})
	return out
}

// TestTemplateReasonsAreTheTypeScriptOnes pins the wire reasons to the console's table.
func TestTemplateReasonsAreTheTypeScriptOnes(t *testing.T) {
	goReasons := templateReasonConsts(t)
	if len(goReasons) != 16 {
		t.Fatalf("reasons.go declares %d template reasons, want 16", len(goReasons))
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "app", "lib", "template-refusals.ts"))
	if err != nil {
		t.Fatal(err)
	}
	var tsReasons []string
	block := regexp.MustCompile(`(?s)TEMPLATE_REASON = \{(.*?)\} as const`).FindSubmatch(raw)
	if block == nil {
		t.Fatal("template-refusals.ts has no TEMPLATE_REASON table")
	}
	for _, m := range regexp.MustCompile(`"(template_[a-z_]+)"`).FindAllSubmatch(block[1], -1) {
		tsReasons = append(tsReasons, string(m[1]))
	}
	var goValues []string
	for _, v := range goReasons {
		goValues = append(goValues, v)
	}
	slices.Sort(goValues)
	slices.Sort(tsReasons)
	if !slices.Equal(goValues, tsReasons) {
		t.Fatalf("Go reasons %v\nTS reasons %v", goValues, tsReasons)
	}
	for name, v := range goReasons {
		if status := templateReasonStatus(v); status < 400 || status > 499 {
			t.Errorf("%s (%s) has status %d", name, v, status)
		}
	}
}

func TestTemplateDenialRefusals(t *testing.T) {
	group := types.TemplateOwner{Scope: types.TemplateScopeGroup, Group: "eng"}
	org := types.TemplateOwner{Scope: types.TemplateScopeOrg}
	person := types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "alice"}
	tests := []struct {
		name   string
		denial types.TemplateDenial
		owner  types.TemplateOwner
		status int
		reason string
	}{
		{"allowed", types.TemplateAllowed, org, 0, ""},
		{"someone else's personal template is not found", types.TemplateDenyNotOwner, person, http.StatusNotFound, reasonTemplateNotFound},
		{"a group the caller is not in is not found", types.TemplateDenyNotGroupMember, group, http.StatusNotFound, reasonTemplateNotFound},
		{"a member who is not the administrator is forbidden", types.TemplateDenyNotGroupAdmin, group, http.StatusForbidden, reasonTemplateScopeForbidden},
		{"org writes need the administrator", types.TemplateDenyNotOrgAdmin, org, http.StatusForbidden, reasonTemplateScopeForbidden},
		{"an unverifiable snapshot says so", types.TemplateDenyGroupUnverified, group, http.StatusForbidden, reasonTemplateGroupUnverified},
		{"a malformed owner is not found", types.TemplateDenyBadOwner, group, http.StatusNotFound, reasonTemplateNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := templateDenialRefusal(tc.denial, tc.owner)
			if tc.status == 0 {
				if got != nil {
					t.Fatalf("an allowed action was refused: %+v", got)
				}
				return
			}
			if got == nil || got.status != tc.status || got.body.Reason != tc.reason {
				t.Fatalf("got %+v, want %d %s", got, tc.status, tc.reason)
			}
			if got.status == http.StatusNotFound && strings.Contains(got.body.Error, tc.owner.Group) && tc.owner.Group != "" {
				t.Errorf("a not-found answer names the group: %q", got.body.Error)
			}
		})
	}
	// The group-admin sentence must not say which group.
	if msg := templateScopeForbiddenMsg("group"); strings.Contains(msg, "eng") {
		t.Errorf("sentence names a group: %q", msg)
	}
}

// TestTemplateRoutesAreNotRegisteredYet is the tripwire for the storage lane.
// The route and tier table (types.TemplateRoutes) describes routes that do not
// exist. When the lane registers them, each must be classified in routeMatrix
// (authz_test.go) and this test deleted: the walk then holds them.
func TestTemplateRoutesAreNotRegisteredYet(t *testing.T) {
	srv, _, _, _ := newAuthzMatrixServer(t)
	registered := map[string]bool{}
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, route := range types.TemplateRoutes {
		if registered[route.Route] {
			t.Errorf("%s is registered: classify it in routeMatrix with its tier, then delete this test", route.Route)
		}
		if _, ok := routeMatrix[route.Route]; ok {
			t.Errorf("%s is in routeMatrix but the router does not register it", route.Route)
		}
	}
	for key := range registered {
		if strings.Contains(key, "/templates") || strings.Contains(key, "template-group-admins") {
			t.Errorf("%s is registered but TemplateRoutes does not classify it", key)
		}
	}
}
