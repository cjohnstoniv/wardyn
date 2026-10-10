// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func testTemplateOwner(scope, person, group string) types.TemplateOwner {
	return types.TemplateOwner{Scope: types.TemplateScope(scope), Person: person, Group: group}
}

// The census guard: every field reachable from CreateRunRequest (which reaches
// RunPolicySpec and ComponentDefinition) must be classified, at any depth, and
// every classification must name a real field. A new request field that nobody
// classified fails here, which is how it is made to choose between carried and
// excluded before a template can meet it.

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// reachableStructs returns every struct type reachable from root by its JSON
// fields, keyed by type name.
func reachableStructs(t *testing.T, root reflect.Type) map[string]reflect.Type {
	t.Helper()
	out := map[string]reflect.Type{}
	var visit func(reflect.Type)
	visit = func(ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || reflect.PointerTo(ty).Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
			return
		}
		if prev, seen := out[ty.Name()]; seen {
			if prev != ty {
				t.Fatalf("two reachable struct types are both named %s", ty.Name())
			}
			return
		}
		out[ty.Name()] = ty
		for i := range ty.NumField() {
			if jsonName(ty.Field(i)) != "" && jsonName(ty.Field(i)) != "-" {
				visit(ty.Field(i).Type)
			}
		}
	}
	visit(root)
	return out
}

func TestTemplateFieldRegistryCoversEveryReachableField(t *testing.T) {
	structs := reachableStructs(t, requestType)
	if len(structs) < 20 {
		t.Fatalf("reached only %d struct types; the walk is broken", len(structs))
	}
	classified := func(typeName, name string) int {
		n := 0
		switch typeName {
		case "CreateRunRequest":
			if _, ok := templateRule(templateRequestRules, name); ok {
				n++
			}
		case "RunPolicySpec":
			if _, ok := templateRule(templatePolicyRules, name); ok {
				n++
			}
		default:
			if slices.Contains(strings.Fields(templateNestedCarried[typeName]), name) {
				n++
			}
			if _, ok := templateNestedExcluded[typeName+"."+name]; ok {
				n++
			}
		}
		return n
	}
	for typeName, ty := range structs {
		for i := range ty.NumField() {
			name := jsonName(ty.Field(i))
			if name == "" || name == "-" {
				continue
			}
			if n := classified(typeName, name); n != 1 {
				t.Errorf("%s.%s is classified %d times; a template field is carried or excluded, exactly once (template_fields.go)", typeName, name, n)
			}
		}
	}
	for _, rules := range [][]TemplateFieldRule{templateRequestRules, templatePolicyRules} {
		seen := map[string]bool{}
		for _, r := range rules {
			if seen[r.Name] {
				t.Errorf("rule %s is listed twice", r.Name)
			}
			seen[r.Name] = true
		}
	}
	for _, r := range templatePolicyRules {
		if _, ok := templateStructField(structs["RunPolicySpec"], r.Name); !ok {
			t.Errorf("policy rule %s names no RunPolicySpec field", r.Name)
		}
	}
	for _, r := range templateRequestRules {
		if _, ok := templateStructField(requestType, r.Name); !ok && r.Name != "pool_id" {
			t.Errorf("request rule %s names no CreateRunRequest field", r.Name)
		}
	}
	for typeName, names := range templateNestedCarried {
		ty, ok := structs[typeName]
		if !ok {
			t.Errorf("nested table names %s, which no request field reaches", typeName)
			continue
		}
		for _, name := range strings.Fields(names) {
			if _, ok := templateStructField(ty, name); !ok {
				t.Errorf("%s lists %s, which is not one of its fields", typeName, name)
			}
		}
	}
	for key := range templateNestedExcluded {
		typeName, name, _ := strings.Cut(key, ".")
		ty, ok := structs[typeName]
		if !ok {
			t.Errorf("exclusion %s names an unreachable type", key)
			continue
		}
		if _, ok := templateStructField(ty, name); !ok {
			t.Errorf("exclusion %s names no field", key)
		}
	}
}

func TestTemplateRulesAreComplete(t *testing.T) {
	validOmit := []string{templateOmitBaseline, templateOmitRequired, templateOmitOptional}
	validEmpty := []string{templateEmptySame, templateEmptyValue, templateEmptyAll, templateEmptyNever}
	validTab := []string{templateTabInfo, templateTabRunner, templateTabRepoDrives, templateTabToolsImage, templateTabAccess}
	for _, r := range slices.Concat(templateRequestRules, templatePolicyRules) {
		if r.Excluded != "" {
			if r.Why == "" || r.Tab != "" || r.Omitted != "" {
				t.Errorf("excluded rule %s carries owner fields or lacks a reason", r.Name)
			}
			continue
		}
		if !slices.Contains(validOmit, r.Omitted) || !slices.Contains(validEmpty, r.Empty) || !slices.Contains(validTab, r.Tab) || r.Meaning == "" {
			t.Errorf("rule %s is incomplete: %+v", r.Name, r)
		}
		if r.Part == "" && r.Name != "inline_policy" {
			t.Errorf("rule %s has no refinement part", r.Name)
		}
	}
}

var templateFieldsGolden = filepath.Join("..", "..", "ui", "src", "app", "lib", "template-fields.golden.json")

// TestTemplateFieldsGolden pins the console's copy of the registry. Regenerate
// with WARDYN_UPDATE_GOLDEN=1 go test ./internal/api -run TestTemplateFieldsGolden.
func TestTemplateFieldsGolden(t *testing.T) {
	type nestedExcluded struct {
		Path     string `json:"path"`
		Excluded string `json:"excluded"`
		Why      string `json:"why"`
	}
	var nested []nestedExcluded
	for _, key := range sortedKeys(templateNestedExcluded) {
		r := templateNestedExcluded[key]
		nested = append(nested, nestedExcluded{key, r.Excluded, r.Why})
	}
	nestedCarried := map[string][]string{}
	for typeName, names := range templateNestedCarried {
		nestedCarried[typeName] = strings.Fields(names)
	}
	want, err := json.MarshalIndent(map[string]any{
		"request": templateRequestRules, "policy": templatePolicyRules, "nested_carried": nestedCarried, "nested_excluded": nested,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("WARDYN_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(templateFieldsGolden, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(templateFieldsGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not the field registry; run WARDYN_UPDATE_GOLDEN=1 go test ./internal/api -run TestTemplateFieldsGolden", templateFieldsGolden)
	}
}

// sampleJSON builds a non-empty JSON value for a type, leaving out the fields
// a template excludes; zeroJSON the empty, false or zero value of the same
// type, or nil where the type has none a template could carry.
func sampleJSON(ty reflect.Type) json.RawMessage {
	for ty.Kind() == reflect.Pointer {
		ty = ty.Elem()
	}
	switch {
	case ty == rawMessageType:
		return json.RawMessage(`{"k":"v"}`)
	case reflect.PointerTo(ty).Implements(textUnmarshalerType):
		return json.RawMessage(`"6f1c2a52-5b1e-4d45-9d6b-0c4f1b2a7e10"`)
	}
	switch ty.Kind() {
	case reflect.String:
		return json.RawMessage(`"x"`)
	case reflect.Bool:
		return json.RawMessage(`true`)
	case reflect.Int, reflect.Int64:
		return json.RawMessage(`1`)
	case reflect.Slice:
		return json.RawMessage("[" + string(sampleJSON(ty.Elem())) + "]")
	case reflect.Map:
		return json.RawMessage(`{"K":` + string(sampleJSON(ty.Elem())) + `}`)
	case reflect.Struct:
		obj := map[string]json.RawMessage{}
		for i := range ty.NumField() {
			name := jsonName(ty.Field(i))
			if _, isExcluded := templateExclusion(ty, name); name == "" || name == "-" || isExcluded {
				continue
			}
			obj[name] = sampleJSON(ty.Field(i).Type)
		}
		b, _ := json.Marshal(obj)
		return b
	}
	panic("no sample for " + ty.String())
}

func zeroJSON(ty reflect.Type) json.RawMessage {
	for ty.Kind() == reflect.Pointer {
		ty = ty.Elem()
	}
	if reflect.PointerTo(ty).Implements(textUnmarshalerType) {
		return nil
	}
	switch ty.Kind() {
	case reflect.String:
		return json.RawMessage(`""`)
	case reflect.Bool:
		return json.RawMessage(`false`)
	case reflect.Int, reflect.Int64:
		return json.RawMessage(`0`)
	case reflect.Slice:
		if ty == rawMessageType {
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(`[]`)
	case reflect.Map, reflect.Struct:
		return json.RawMessage(`{}`)
	}
	return nil
}

func templateDocJSON(intent map[string]json.RawMessage) []byte {
	b, _ := json.Marshal(map[string]any{
		"api_version": client.TemplateDocumentVersion, "kind": client.TemplateDocumentKind,
		"coverage": client.TemplateCoveragePartial, "intent": intent,
	})
	return b
}

func decodeJSONTemplate(t *testing.T, body []byte) (client.TemplateDocument, []client.TemplateDiagnostic) {
	t.Helper()
	return decodeTemplateDocument(body, client.TemplateFormatJSON)
}

func mustDecodeTemplate(t *testing.T, body []byte) client.TemplateDocument {
	t.Helper()
	doc, diags := decodeJSONTemplate(t, body)
	if len(diags) > 0 {
		t.Fatalf("diagnostics for %s: %+v", body, diags)
	}
	return doc
}

// fieldIntents yields, for every carried request and policy field, an intent
// holding that one field: with a sample value and, where the type has one,
// with its empty value.
func fieldIntents(t *testing.T, value func(reflect.Type) json.RawMessage) map[string]map[string]json.RawMessage {
	t.Helper()
	out := map[string]map[string]json.RawMessage{}
	policyType := reachableStructs(t, requestType)["RunPolicySpec"]
	for _, r := range templateRequestRules {
		field, ok := templateStructField(requestType, r.Name)
		if r.Excluded != "" || !ok || r.Name == "inline_policy" {
			continue
		}
		if v := value(field.Type); v != nil {
			out[r.Name] = map[string]json.RawMessage{r.Name: v}
		}
	}
	for _, r := range templatePolicyRules {
		field, _ := templateStructField(policyType, r.Name)
		if v := value(field.Type); v != nil {
			spec, _ := json.Marshal(map[string]json.RawMessage{r.Name: v})
			out["inline_policy."+r.Name] = map[string]json.RawMessage{"inline_policy": spec}
		}
	}
	return out
}

// TestTemplateRoundTripEveryCarriedField proves each carried field survives
// decode and encode with its presence and its value, as a sample and as the
// explicit empty, false or zero that is a choice and not an omission.
func TestTemplateRoundTripEveryCarriedField(t *testing.T) {
	for kind, value := range map[string]func(reflect.Type) json.RawMessage{"sample": sampleJSON, "explicit zero": zeroJSON} {
		intents := fieldIntents(t, value)
		if len(intents) < 40 {
			t.Fatalf("%s: only %d fields exercised", kind, len(intents))
		}
		for name, intent := range intents {
			body := templateDocJSON(intent)
			doc := mustDecodeTemplate(t, body)
			for key := range intent {
				if !doc.Intent.Has(key) {
					t.Errorf("%s %s: decoded intent lost %s", kind, name, key)
				}
			}
			again, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			var canonical any
			_ = json.Unmarshal(body, &canonical)
			want, _ := json.Marshal(canonical)
			// api_version/kind/coverage are in the document; the intent must come back byte for byte.
			if !bytes.Contains(again, []byte(`"intent":`+string(mustMarshalSorted(t, intent)))) {
				t.Errorf("%s %s: intent changed in a round trip\n got %s\nwant %s", kind, name, again, want)
			}
			if second := mustDecodeTemplate(t, again); !reflect.DeepEqual(second.Intent.Names(), doc.Intent.Names()) {
				t.Errorf("%s %s: second decode differs", kind, name)
			}
		}
	}
}

func mustMarshalSorted(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) == nil {
		b, _ = json.Marshal(m)
	}
	return b
}

func TestTemplateExcludedFieldsAreRefusedWithTheirReason(t *testing.T) {
	cases := []struct {
		name   string
		intent string
		path   string
		reason string
	}{
		{"integration_id", `{"integration_id":"x"}`, "intent.integration_id", reasonTemplateFieldExcluded},
		{"preset", `{"preset":"x"}`, "intent.preset", reasonTemplateFieldExcluded},
		{"preset_version", `{"preset_version":2}`, "intent.preset_version", reasonTemplateFieldExcluded},
		{"workspace secret values", `{"inline_policy":{"llm_inspection":{"mode":"alert","workspace_secret_values":["x"]}}}`,
			"intent.inline_policy.llm_inspection.workspace_secret_values", reasonTemplateSecretRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := decodeJSONTemplate(t, []byte(`{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":`+tc.intent+`}`))
			if len(diags) != 1 || diags[0].Path != tc.path || diags[0].Reason != tc.reason {
				t.Fatalf("diagnostics = %+v, want one %s at %s", diags, tc.reason, tc.path)
			}
		})
	}
}

// Canaries are assembled at run time so no credential-shaped literal sits in the source.
var (
	canaryGitHubToken = "gh" + "p_" + strings.Repeat("a", 36)
	canaryAWSKey      = "AK" + "IA" + strings.Repeat("A", 16)
	canaryPEM         = "-----BEGIN " + "RSA PRIVATE KEY-----"
)

func TestTemplateDecoderRefusals(t *testing.T) {
	const head = `"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial"`
	cases := []struct {
		name   string
		body   string
		path   string
		reason string
	}{
		{"unknown intent key", `{` + head + `,"intent":{"agnet":"claude"}}`, "intent.agnet", reasonTemplateFieldUnknown},
		{"unknown nested key", `{` + head + `,"intent":{"workspaces":[{"workspace_id":"w","taregt":"/home/agent/a"}]}}`, "intent.workspaces[0].taregt", reasonTemplateFieldUnknown},
		{"unknown document key", `{` + head + `,"intent":{"agent":"claude"},"extra":1}`, "extra", reasonTemplateFieldUnknown},
		{"secret-looking key", `{` + head + `,"intent":{"api_key":"abc"}}`, "intent.api_key", reasonTemplateSecretRefused},
		{"secret in config key", `{` + head + `,"intent":{"components":[{"inline":{"hosts":["a.example.com"],"config":{"GITHUB_TOKEN":"x"}}}]}}`, "intent.components[0].inline.config.GITHUB_TOKEN", reasonTemplateSecretRefused},
		{"token in text", `{` + head + `,"intent":{"task":"use ` + canaryGitHubToken + ` to push"}}`, "intent.task", reasonTemplateSecretRefused},
		{"token in grant scope", `{` + head + `,"intent":{"inline_policy":{"eligible_grants":[{"kind":"api_key","scope":{"v":"` + canaryAWSKey + `"},"requires_approval":false}]}}}`, "intent.inline_policy.eligible_grants[0].scope.v", reasonTemplateSecretRefused},
		{"credentials in a url", `{` + head + `,"intent":{"repo":"https://user:hunter2@github.com/acme/api"}}`, "intent.repo", reasonTemplateSecretRefused},
		{"private key", `{` + head + `,"intent":{"description":"` + canaryPEM + `"}}`, "intent.description", reasonTemplateSecretRefused},
		{"run id", `{` + head + `,"intent":{"agent":"claude","run_id":"x"}}`, "intent.run_id", reasonTemplateRunStateRefused},
		{"execution generation", `{` + head + `,"intent":{"execution_generation":3}}`, "intent.execution_generation", reasonTemplateRunStateRefused},
		{"claim", `{` + head + `,"intent":{"claim":{}}}`, "intent.claim", reasonTemplateRunStateRefused},
		{"admission", `{` + head + `,"intent":{"admission":{}}}`, "intent.admission", reasonTemplateRunStateRefused},
		{"effective policy", `{` + head + `,"intent":{"effective_policy":{}}}`, "intent.effective_policy", reasonTemplateRunStateRefused},
		{"owner in the document", `{` + head + `,"owner_id":"someone","intent":{"agent":"claude"}}`, "owner_id", reasonTemplateMetadataInContent},
		{"group in the document", `{` + head + `,"group_id":"eng","intent":{"agent":"claude"}}`, "group_id", reasonTemplateMetadataInContent},
		{"scope in the document", `{` + head + `,"scope":"org","intent":{"agent":"claude"}}`, "scope", reasonTemplateMetadataInContent},
		{"revision in the document", `{` + head + `,"revision":9,"intent":{"agent":"claude"}}`, "revision", reasonTemplateMetadataInContent},
		{"null", `{` + head + `,"intent":{"agent":null}}`, "intent.agent", reasonTemplateFieldInvalid},
		{"wrong type", `{` + head + `,"intent":{"interactive":"yes"}}`, "intent.interactive", reasonTemplateFieldInvalid},
		{"fractional integer", `{` + head + `,"intent":{"resources":{"cpu_millis":1.5}}}`, "intent.resources.cpu_millis", reasonTemplateFieldInvalid},
		{"wrong list", `{` + head + `,"intent":{"workspaces":{"workspace_id":"w"}}}`, "intent.workspaces", reasonTemplateFieldInvalid},
		{"bad id", `{` + head + `,"intent":{"policy_id":"not-a-uuid"}}`, "intent.policy_id", reasonTemplateFieldInvalid},
		{"redacted placeholder", `{` + head + `,"intent":{"inline_policy":{"workspace_mounts":[{"source":"<redacted>","target":"/home/agent/x"}]}}}`, "intent.inline_policy.workspace_mounts[0].source", reasonTemplateFieldInvalid},
		{"bad pool id", `{` + head + `,"intent":{"pool_id":"has space"}}`, "intent.pool_id", reasonTemplateFieldInvalid},
		{"bad coverage", `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"all","intent":{"agent":"claude"}}`, "coverage", reasonTemplateFieldInvalid},
		{"missing intent", `{` + head + `}`, "intent", reasonTemplateFieldInvalid},
		{"wrong version", `{"api_version":"wardyn/v2","kind":"RunTemplate","coverage":"partial","intent":{"agent":"claude"}}`, "api_version", reasonTemplateVersionUnsupported},
		{"wrong kind", `{"api_version":"wardyn/v1","kind":"RunPolicy","coverage":"partial","intent":{"agent":"claude"}}`, "api_version", reasonTemplateVersionUnsupported},
		{"duplicate key", `{` + head + `,"intent":{"agent":"a","agent":"b"}}`, "", reasonTemplateDocumentInvalid},
		{"two documents", `{` + head + `,"intent":{"agent":"a"}} {}`, "", reasonTemplateDocumentInvalid},
		{"not an object", `[1]`, "", reasonTemplateDocumentInvalid},
		{"not json", `{`, "", reasonTemplateDocumentInvalid},
		{"scope too big", `{` + head + `,"intent":{"inline_policy":{"eligible_grants":[{"kind":"k","scope":{"v":"` + strings.Repeat("a", 5000) + `"},"requires_approval":false}]}}}`, "intent.inline_policy.eligible_grants[0].scope", reasonTemplateFieldInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := decodeJSONTemplate(t, []byte(tc.body))
			if !slices.ContainsFunc(diags, func(d client.TemplateDiagnostic) bool { return d.Path == tc.path && d.Reason == tc.reason }) {
				t.Fatalf("diagnostics = %+v, want %s at %q", diags, tc.reason, tc.path)
			}
			for _, d := range diags {
				if d.Message == "" {
					t.Errorf("diagnostic %+v has no sentence", d)
				}
				if strings.Contains(d.Message, "hunter2") || strings.Contains(d.Message, canaryAWSKey) || strings.Contains(d.Message, canaryGitHubToken) {
					t.Errorf("a diagnostic echoes the secret it refused: %q", d.Message)
				}
			}
		})
	}
}

func TestTemplateDocumentSizeAndDepth(t *testing.T) {
	big := `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":{"description":"` + strings.Repeat("a", maxTemplateDocumentBytes) + `"}}`
	if _, diags := decodeJSONTemplate(t, []byte(big)); len(diags) != 1 || diags[0].Reason != reasonTemplateDocumentInvalid {
		t.Errorf("oversized document: %+v", diags)
	}
	deep := strings.Repeat(`{"a":`, maxTemplateDocumentDepth+2) + `1` + strings.Repeat(`}`, maxTemplateDocumentDepth+2)
	if _, diags := decodeJSONTemplate(t, []byte(deep)); len(diags) != 1 || diags[0].Reason != reasonTemplateDocumentInvalid {
		t.Errorf("deep document: %+v", diags)
	}
}

func TestTemplateYAMLIsReadStrictly(t *testing.T) {
	good := "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  agent: claude\n  interactive: false\n  inline_policy:\n    allowed_domains: []\n"
	doc, diags := decodeTemplateDocument([]byte(good), client.TemplateFormatYAML)
	if len(diags) > 0 {
		t.Fatalf("good yaml: %+v", diags)
	}
	if !doc.Intent.Has("interactive") || string(doc.Intent.Raw("interactive")) != "false" {
		t.Errorf("explicit false was lost: %s", doc.Intent.Raw("interactive"))
	}
	for name, src := range map[string]string{
		"alias":          "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  agent: &a claude\n  model_provider: *a\n",
		"anchor":         "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent: &i\n  agent: claude\n",
		"merge":          "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nx: &m {a: 1}\nintent:\n  <<: *m\n",
		"custom tag":     "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  agent: !custom claude\n",
		"timestamp":      "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  title: 2024-01-01\n",
		"octal":          "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  resources:\n    cpu_millis: 0755\n",
		"two documents":  good + "---\nintent: {}\n",
		"duplicate key":  "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  agent: a\n  agent: b\n",
		"non-string key": "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  1: a\n",
		"infinity":       "api_version: wardyn/v1\nkind: RunTemplate\ncoverage: partial\nintent:\n  resources:\n    cpu_millis: .inf\n",
		"not yaml":       "intent: [",
	} {
		t.Run(name, func(t *testing.T) {
			if _, diags := decodeTemplateDocument([]byte(src), client.TemplateFormatYAML); len(diags) == 0 {
				t.Fatal("accepted")
			}
		})
	}
	if _, diags := decodeTemplateDocument([]byte(good), "toml"); len(diags) != 1 || diags[0].Reason != reasonTemplateDocumentInvalid {
		t.Errorf("unknown format: %+v", diags)
	}
}

// TestTemplateOmittedIsNotEmpty is the presence contract: a field that is
// absent, one that is explicitly empty, and one that is zero are three
// different documents, and none becomes another on the way through.
func TestTemplateOmittedIsNotEmpty(t *testing.T) {
	omitted := mustDecodeTemplate(t, templateDocJSON(map[string]json.RawMessage{"agent": json.RawMessage(`"claude"`)}))
	empty := mustDecodeTemplate(t, templateDocJSON(map[string]json.RawMessage{
		"agent": json.RawMessage(`"claude"`), "workspaces": json.RawMessage(`[]`), "interactive": json.RawMessage(`false`),
		"inline_policy": json.RawMessage(`{"allowed_domains":[],"auto_stop_after_sec":0,"allow_all_egress":false}`),
	}))
	if omitted.Intent.Has("workspaces") || omitted.Intent.Has("interactive") {
		t.Fatal("omitted fields read as present")
	}
	for _, name := range []string{"workspaces", "interactive", "inline_policy"} {
		if !empty.Intent.Has(name) {
			t.Errorf("explicit empty %s read as omitted", name)
		}
	}
	out, _ := json.Marshal(empty)
	for _, want := range []string{`"workspaces":[]`, `"interactive":false`, `"allowed_domains":[]`, `"auto_stop_after_sec":0`, `"allow_all_egress":false`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("encoded document lost %s: %s", want, out)
		}
	}
	req, err := empty.Intent.Request()
	if err != nil || req.Workspaces == nil || len(req.Workspaces) != 0 {
		t.Errorf("an explicit empty list materialised as %v, %v; want empty and non-nil", req.Workspaces, err)
	}
	if req, _ = omitted.Intent.Request(); req.Workspaces != nil {
		t.Errorf("an omitted list materialised as %v", req.Workspaces)
	}
}

func TestTemplateImportValidation(t *testing.T) {
	const head = `"api_version":"wardyn/v1","kind":"RunTemplate",`
	run := func(body string) client.TemplateImportResult {
		return validateTemplateImport([]byte(body), client.TemplateFormatJSON)
	}
	if res := run(`{` + head + `"coverage":"partial","name":"egress only","intent":{"inline_policy":{"allowed_domains":["api.example.com"]}}}`); res.Document == nil || len(res.Diagnostics) != 0 {
		t.Fatalf("an egress-only partial template with no repository must import: %+v", res)
	}
	reasons := func(res client.TemplateImportResult) []string {
		var out []string
		for _, d := range res.Diagnostics {
			out = append(out, d.Reason+"@"+d.Path)
		}
		return out
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty intent", `{` + head + `"coverage":"partial","intent":{}}`, reasonTemplateCoverageInvalid + "@intent"},
		{"full with needs setup", `{` + head + `"coverage":"full","intent":{"agent":"a"},"needs_setup":[{"field":"task"}]}`, reasonTemplateCoverageInvalid + "@needs_setup"},
		{"dependency cut", `{` + head + `"coverage":"partial","intent":{"model_provider":"p"}}`, reasonTemplateDependencyMissing + "@intent.model_provider"},
		{"dependency listed", ``, ""},
		{"needs setup for a present field", `{` + head + `"coverage":"partial","intent":{"agent":"a"},"needs_setup":[{"field":"agent"}]}`, reasonTemplateFieldInvalid + "@needs_setup[0].field"},
		{"needs setup for an unknown field", `{` + head + `"coverage":"partial","intent":{"agent":"a"},"needs_setup":[{"field":"nope"}]}`, reasonTemplateFieldInvalid + "@needs_setup[0].field"},
		{"shape check shared with the run", `{` + head + `"coverage":"partial","intent":{"workspaces":[{"workspace_id":"w","target":"work"}]}}`, reasonWorkspaceTargetInvalid + "@intent"},
		{"unavailable: local placement", `{` + head + `"coverage":"partial","intent":{"placement":"local"}}`, reasonTemplateFieldUnavailable + "@intent.placement"},
		{"unavailable: allowed image", `{` + head + `"coverage":"partial","intent":{"allowed_image":"img"}}`, reasonTemplateFieldUnavailable + "@intent.allowed_image"},
		{"unavailable: overrides", `{` + head + `"coverage":"partial","intent":{"overrides":{"agent":{"add_hosts":["a.example.com"]}}}}`, reasonTemplateFieldUnavailable + "@intent.overrides"},
		{"unavailable: built-in component", `{` + head + `"coverage":"partial","intent":{"components":[{"builtin":"github","org":"acme","repos":["acme/api"]}]}}`, reasonTemplateFieldUnavailable + "@intent.components[0]"},
		{"unavailable: pool", `{` + head + `"coverage":"partial","intent":{"pool_id":"pool-1"}}`, reasonTemplateFieldUnavailable + "@intent.pool_id"},
		{"unavailable: reserved push rule", `{` + head + `"coverage":"partial","intent":{"inline_policy":{"push_rules":{"deny_new_executables":true}}}}`, reasonTemplateFieldUnavailable + "@intent.inline_policy.push_rules.deny_new_executables"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == "" {
				res := run(`{` + head + `"coverage":"partial","intent":{"model_provider":"p"},"needs_setup":[{"field":"agent","reason":"pick one"}]}`)
				if res.Document == nil {
					t.Fatalf("a dependency listed under needs_setup must import: %v", reasons(res))
				}
				return
			}
			res := run(tc.body)
			if res.Document != nil || !slices.Contains(reasons(res), tc.want) {
				t.Fatalf("got %v, want %s", reasons(res), tc.want)
			}
		})
	}
}

// TestTemplateUnavailableMirrorsRunRefusal keeps the template's "cannot honour
// yet" list and the run's together: a request field the run refuses as
// unapplied is refused in a template, and one it accepts is accepted.
func TestTemplateUnavailableMirrorsRunRefusal(t *testing.T) {
	for name, body := range map[string]string{
		"local placement":  `{"placement":"local"}`,
		"allowed image":    `{"allowed_image":"img"}`,
		"overrides":        `{"overrides":{"agent":{"add_hosts":["a.example.com"]}}}`,
		"built-in":         `{"components":[{"builtin":"github","org":"acme","repos":["acme/api"]}]}`,
		"remote placement": `{"placement":"remote"}`,
		"inline component": `{"components":[{"inline":{"hosts":["a.example.com"]}}]}`,
		"resources":        `{"resources":{"cpu_millis":1000}}`,
		"empty overrides":  `{"overrides":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var req createRunRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatal(err)
			}
			runRefuses := unappliedFieldsRefusal(req) != nil
			d := &templateDecoder{}
			d.checkAvailable(client.TemplateDocument{}, req)
			if templateRefuses := len(d.diags) > 0; templateRefuses != runRefuses {
				t.Fatalf("the run refuses=%v, the template refuses=%v", runRefuses, templateRefuses)
			}
		})
	}
}

func TestTemplateScopeRefusesPersonOnlyFields(t *testing.T) {
	doc := mustDecodeTemplate(t, templateDocJSON(map[string]json.RawMessage{
		"agent": json.RawMessage(`"claude"`), "placement": json.RawMessage(`"remote"`), "runner_id": json.RawMessage(`"r1"`),
	}))
	person := testTemplateOwner("person", "alice", "")
	if diags := validateTemplateForScope(doc, person); len(diags) != 0 {
		t.Errorf("a personal template may pin a runner: %+v", diags)
	}
	for _, owner := range []string{"org", "group"} {
		group := ""
		if owner == "group" {
			group = "eng"
		}
		diags := validateTemplateForScope(doc, testTemplateOwner(owner, "", group))
		if len(diags) != 1 || diags[0].Reason != reasonTemplateSharedFieldRefused || diags[0].Path != "intent.runner_id" {
			t.Errorf("%s template: diagnostics %+v, want runner_id refused", owner, diags)
		}
	}
	if diags := validateTemplateForScope(doc, testTemplateOwner("org", "alice", "")); len(diags) != 1 || diags[0].Reason != reasonTemplateScopeForbidden {
		t.Errorf("a malformed owner must be refused, not defaulted: %+v", diags)
	}
}

func TestTemplateIncludesNamesTheParts(t *testing.T) {
	doc := mustDecodeTemplate(t, templateDocJSON(map[string]json.RawMessage{
		"title": json.RawMessage(`"t"`), "drive": json.RawMessage(`{"enabled":true}`),
		"inline_policy": json.RawMessage(`{"allowed_domains":["a.example.com"],"eligible_grants":[]}`),
	}))
	want := []client.TemplatePart{client.TemplatePartCredentials, client.TemplatePartDrives, client.TemplatePartEgress, client.TemplatePartInfo}
	if got := templateIncludes(doc); !slices.Equal(got, want) {
		t.Errorf("includes = %v, want %v", got, want)
	}
}

// TestTemplateIntentFieldsMaterialiseIntoARequest proves the carried fields
// are the request's own: the sample of every one reads back through the
// canonical request decoder with no field dropped or unknown.
func TestTemplateIntentFieldsMaterialiseIntoARequest(t *testing.T) {
	for name, intent := range fieldIntents(t, sampleJSON) {
		doc := mustDecodeTemplate(t, templateDocJSON(intent))
		if _, err := doc.Intent.Request(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestTemplateWireTypesHoldNoAuthority walks every type a template travels in
// and refuses a field that could carry a credential, a session or a run's
// state. A published template is content: it has no field an admission could
// ride in.
func TestTemplateWireTypesHoldNoAuthority(t *testing.T) {
	roots := []reflect.Type{
		reflect.TypeFor[client.Template](), reflect.TypeFor[client.TemplateSummary](), reflect.TypeFor[client.TemplateSaveRequest](),
		reflect.TypeFor[client.TemplateImportResult](), reflect.TypeFor[client.TemplateRef](), reflect.TypeFor[client.TemplateGroupAdmin](),
	}
	n := 0
	for _, root := range roots {
		for name, ty := range reachableStructs(t, root) {
			for i := range ty.NumField() {
				field := jsonName(ty.Field(i))
				if field == "" || field == "-" {
					continue
				}
				n++
				if templateSecretKeyRE.MatchString(field) || slices.Contains(templateRunStateKey, field) {
					t.Errorf("%s.%s could carry authority or run state", name, field)
				}
			}
		}
	}
	if n < 30 {
		t.Fatalf("walked only %d fields", n)
	}
}

// TestTemplateCarriedFieldsNeverReadAsRunStateOrSecrets keeps the registry
// and the unknown-key classifier apart: a field a template carries is never
// one of the names the classifier refuses.
func TestTemplateCarriedFieldsNeverReadAsRunStateOrSecrets(t *testing.T) {
	check := func(owner, name string) {
		if templateSecretKeyRE.MatchString(name) || slices.Contains(templateRunStateKey, strings.ToLower(name)) {
			t.Errorf("%s.%s is carried but reads as a refused name", owner, name)
		}
	}
	for _, r := range slices.Concat(templateRequestRules, templatePolicyRules) {
		if r.Excluded == "" {
			check("field", r.Name)
		}
	}
	for typeName, names := range templateNestedCarried {
		for _, name := range strings.Fields(names) {
			check(typeName, name)
		}
	}
}
