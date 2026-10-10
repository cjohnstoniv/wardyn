// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

const templateYAML = "api_version: wardyn/v1\nkind: RunTemplate\nname: egress only\ncoverage: partial\nintent:\n  inline_policy:\n    allowed_domains: []\n"

func writeTemplateFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTemplateCommandsCallTheTemplateRoutes drives each leaf against a server
// that answers what the real one does until the store lands (501
// templates_unavailable) and checks the request each one made.
func TestTemplateCommandsCallTheTemplateRoutes(t *testing.T) {
	id := uuid.New().String()
	yamlFile := writeTemplateFile(t, "t.yaml", templateYAML)
	for _, tc := range []struct {
		name         string
		args         []string
		in           string
		method, path string
	}{
		{"list", []string{"template", "list"}, "", "GET", "/api/v1/templates"},
		{"show", []string{"template", "show", id}, "", "GET", "/api/v1/templates/" + id},
		{"show an old revision", []string{"template", "show", id, "--revision", "2"}, "", "GET", "/api/v1/templates/" + id + "/revisions/2"},
		{"save a personal template", []string{"template", "save", yamlFile}, "", "POST", "/api/v1/templates"},
		{"save to the organisation", []string{"template", "save", yamlFile, "--scope", "org"}, "", "POST", "/api/v1/templates"},
		{"save to a group", []string{"template", "save", yamlFile, "--scope", "group", "--group", "eng"}, "", "POST", "/api/v1/templates"},
		{"update with a revision", []string{"template", "save", yamlFile, "--update", id, "--expected-revision", "3"}, "", "PUT", "/api/v1/templates/" + id},
		{"import a file", []string{"template", "import", yamlFile}, "", "POST", "/api/v1/templates/import"},
		{"import stdin", []string{"template", "import", "-", "--format", "json"}, "{}", "POST", "/api/v1/templates/import"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := newCmdServer(t, http.StatusNotImplemented, map[string]string{"error": "Templates are not available on this server yet.", "reason": "templates_unavailable"})
			err := execCmdStdin(t, tc.in, append(tc.args, "--url", cs.URL, "--token", "tok")...)
			var apiErr *sdk.APIError
			if !errors.As(err, &apiErr) || apiErr.Reason != "templates_unavailable" {
				t.Fatalf("error = %v, want the server's templates_unavailable", err)
			}
			if got := cs.last(); got.method != tc.method || got.path != tc.path {
				t.Errorf("request = %s %s, want %s %s", got.method, got.path, tc.method, tc.path)
			}
		})
	}
}

func TestTemplateCommandsCheckTheirArguments(t *testing.T) {
	yamlFile := writeTemplateFile(t, "t.yaml", templateYAML)
	unnamed := writeTemplateFile(t, "u.yaml", strings.Replace(templateYAML, "name: egress only\n", "", 1))
	badYAML := writeTemplateFile(t, "b.yaml", "intent: [")
	unknownKey := writeTemplateFile(t, "k.json", `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":{},"owner_id":"x","name":"n"}`)
	noExt := writeTemplateFile(t, "doc", "{}")
	id := uuid.New().String()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"template", "show", "not-a-uuid"}, "invalid template id"},
		{[]string{"template", "show", id, "--revision", "-1"}, "--revision must be"},
		{[]string{"template", "save", yamlFile, "--scope", "team"}, `"team" is not person, org or group`},
		{[]string{"template", "save", yamlFile, "--scope", "group"}, "needs --group"},
		{[]string{"template", "save", yamlFile, "--scope", "org", "--group", "eng"}, "use it with --scope group"},
		{[]string{"template", "save", yamlFile, "--update", id}, "--update needs --expected-revision"},
		{[]string{"template", "save", yamlFile, "--expected-revision", "2"}, "only applies with --update"},
		{[]string{"template", "save", yamlFile, "--update", "nope", "--expected-revision", "1"}, "invalid template id"},
		{[]string{"template", "save", unnamed}, "needs a name"},
		{[]string{"template", "save", badYAML}, "parse template YAML"},
		{[]string{"template", "save", unknownKey}, "parse template document"},
		{[]string{"template", "save", filepath.Join(t.TempDir(), "missing.yaml")}, "read template document"},
		{[]string{"template", "import", "-"}, "needs --format"},
		{[]string{"template", "import", noExt, "--format", "toml"}, "is not json or yaml"},
		{[]string{"template", "bogus"}, "unknown command"},
	} {
		err := execCmd(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("wardyn %s = %v, want an error containing %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
}

func TestTemplateSaveBuildsTheRequest(t *testing.T) {
	cmd := templateSaveCmd(func() *sdk.Client { return nil })
	file := writeTemplateFile(t, "t.yaml", templateYAML)
	req, id, err := templateSaveRequest(cmd, file, "group", "eng", "", "d", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if id != uuid.Nil || req.Scope != sdk.TemplateScopeGroup || req.GroupID != "eng" || req.Name != "egress only" || req.Description != "d" || req.ExpectedRevision != nil {
		t.Errorf("request = %+v id=%s", req, id)
	}
	if !req.Document.Intent.Has("inline_policy") || !strings.Contains(string(req.Document.Intent.Raw("inline_policy")), `"allowed_domains":[]`) {
		t.Errorf("the explicit empty allowlist was lost: %s", req.Document.Intent.Raw("inline_policy"))
	}
	want := uuid.New()
	req, id, err = templateSaveRequest(cmd, file, "person", "", "flag name", "", want.String(), 4)
	if err != nil || id != want || req.Name != "flag name" || req.ExpectedRevision == nil || *req.ExpectedRevision != 4 {
		t.Errorf("update request = %+v id=%s err=%v", req, id, err)
	}
}

func TestTemplateImportPrintsEveryDiagnostic(t *testing.T) {
	var out strings.Builder
	err := printTemplateImport(&out, sdk.TemplateImportResult{Diagnostics: []sdk.TemplateDiagnostic{
		{Path: "intent.agnet", Reason: "template_field_unknown", Message: "intent.agnet is not a template field."},
		{Reason: "template_document_invalid", Message: "bad"},
	}})
	if err == nil || !strings.Contains(err.Error(), "2 problem") {
		t.Fatalf("error = %v", err)
	}
	for _, want := range []string{"intent.agnet\ttemplate_field_unknown\t", "document\ttemplate_document_invalid\tbad"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	doc := sdk.TemplateDocument{APIVersion: sdk.TemplateDocumentVersion, Kind: sdk.TemplateDocumentKind, Coverage: sdk.TemplateCoverageFull}
	out.Reset()
	if err := printTemplateImport(&out, sdk.TemplateImportResult{Document: &doc}); err != nil || !strings.Contains(out.String(), `"coverage": "full"`) {
		t.Errorf("a clean import: %v %s", err, out.String())
	}
	if err := printTemplateImport(&out, sdk.TemplateImportResult{}); err == nil {
		t.Error("an empty answer was read as success")
	}
}
