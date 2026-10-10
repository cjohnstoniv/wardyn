// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const goodSchema = `{
  "schema_version": 1,
  "groups": [{"id": "conn", "label": "Connection"}],
  "fields": [
    {"id": "region", "kind": "enum", "label": "Region", "group": "conn", "required": true,
     "options": [{"value": "eu", "label": "Europe"}, {"value": "us", "label": "United States"}],
     "default": "eu", "bind": {"target": "config", "key": "SERVICE_REGION"}},
    {"id": "verbose", "kind": "boolean", "label": "Verbose logging", "default": false, "bind": {"target": "config", "key": "SERVICE_VERBOSE"}},
    {"id": "retries", "kind": "integer", "label": "Retries", "min": 0, "max": 5, "default": 2,
     "visible_when": {"field": "verbose", "equals": true}, "bind": {"target": "config", "key": "SERVICE_RETRIES"}},
    {"id": "endpoint", "kind": "string", "label": "Endpoint", "max_len": 64, "bind": {"target": "config", "key": "SERVICE_ENDPOINT"}},
    {"id": "mode", "kind": "string", "label": "Mode", "read_only": true, "default": "managed", "bind": {"target": "config", "key": "SERVICE_MODE"}},
    {"id": "key", "kind": "secret_ref", "label": "API key", "required": true, "bind": {"target": "secret", "key": "SERVICE_API_KEY"}}
  ]
}`

func mustSchema(t *testing.T, src string) ComponentConfigSchema {
	t.Helper()
	s, issues := DecodeComponentConfigSchema([]byte(src))
	if len(issues) > 0 {
		t.Fatalf("issues: %+v", issues)
	}
	return s
}

func TestComponentConfigSchemaAcceptsAGoodSchema(t *testing.T) { mustSchema(t, goodSchema) }

func TestComponentConfigSchemaRefusals(t *testing.T) {
	field := func(extra string) string {
		return `{"schema_version":1,"fields":[{"id":"a","kind":"string","label":"A","bind":{"target":"config","key":"A_KEY"}` + extra + `}]}`
	}
	tests := []struct{ name, src, want string }{
		{"unknown version", `{"schema_version":2,"fields":[]}`, "not supported"},
		{"no fields", `{"schema_version":1,"fields":[]}`, "needs 1 to"},
		{"unknown key", field(`,"onclick":"x"`), "unknown field"},
		{"two values", goodSchema + goodSchema, "exactly one JSON value"},
		{"unknown kind", strings.Replace(field(""), `"string"`, `"object"`, 1), "not a supported kind"},
		{"bad id", strings.Replace(field(""), `"id":"a"`, `"id":"A-b"`, 1), "lowercase letters"},
		{"markup in a label", strings.Replace(field(""), `"label":"A"`, `"label":"<b>A</b>"`, 1), "no markup"},
		{"unknown group", field(`,"group":"nope"`), "not a group"},
		{"min on a string", field(`,"min":1`), "integer and number"},
		{"options on a string", field(`,"options":[{"value":"a","label":"A"}]`), "enum fields only"},
		{"read only without default", field(`,"read_only":true`), "needs a default"},
		{"default of the wrong kind", field(`,"default":3`), "must be a string"},
		{"null default", field(`,"default":null`), ""},
		{"unknown target", strings.Replace(field(""), `"config"`, `"host"`, 1), "not a supported target"},
		{"reserved env name", strings.Replace(field(""), `A_KEY`, `LD_PRELOAD`, 1), "reserved"},
		{"sandbox harness env name", strings.Replace(field(""), `A_KEY`, `WARDYN_X`, 1), "reserved"},
		{"lowercase env name", strings.Replace(field(""), `A_KEY`, `a_key`, 1), "must match"},
		{"secret into config", `{"schema_version":1,"fields":[{"id":"a","kind":"secret_ref","label":"A","bind":{"target":"config","key":"A_KEY"}}]}`, "secret target"},
		{"secret default", `{"schema_version":1,"fields":[{"id":"a","kind":"secret_ref","label":"A","default":"token","bind":{"target":"secret","key":"A_KEY"}}]}`, "no default"},
		{"list into config", `{"schema_version":1,"fields":[{"id":"a","kind":"string_list","label":"A","bind":{"target":"config","key":"A_KEY"}}]}`, "cannot feed a config"},
		{"enum without options", `{"schema_version":1,"fields":[{"id":"a","kind":"enum","label":"A","bind":{"target":"config","key":"A_KEY"}}]}`, "needs 1 to"},
		{"enum default outside options", `{"schema_version":1,"fields":[{"id":"a","kind":"enum","label":"A","options":[{"value":"x","label":"X"}],"default":"y","bind":{"target":"config","key":"A_KEY"}}]}`, "one of the field's options"},
		{"min above max", `{"schema_version":1,"fields":[{"id":"a","kind":"integer","label":"A","min":5,"max":1,"bind":{"target":"config","key":"A_KEY"}}]}`, "min is above max"},
		{"integer default fractional", `{"schema_version":1,"fields":[{"id":"a","kind":"integer","label":"A","default":1.5,"bind":{"target":"config","key":"A_KEY"}}]}`, "whole number"},
		{"two fields one key", `{"schema_version":1,"fields":[{"id":"a","kind":"string","label":"A","bind":{"target":"config","key":"K"}},{"id":"b","kind":"string","label":"B","bind":{"target":"config","key":"K"}}]}`, "fed by two fields"},
		{"duplicate id", `{"schema_version":1,"fields":[{"id":"a","kind":"string","label":"A","bind":{"target":"config","key":"K"}},{"id":"a","kind":"string","label":"B","bind":{"target":"config","key":"J"}}]}`, "used twice"},
		{"condition on a missing field", field(`,"visible_when":{"field":"ghost","equals":true}`), "not a field"},
		{"condition on itself", field(`,"visible_when":{"field":"a","equals":"x"}`), "depend on itself"},
		{"condition value of the wrong kind", `{"schema_version":1,"fields":[{"id":"b","kind":"boolean","label":"B","bind":{"target":"config","key":"J"}},{"id":"a","kind":"string","label":"A","visible_when":{"field":"b","equals":"yes"},"bind":{"target":"config","key":"K"}}]}`, "true or false"},
		{"condition value not an option", `{"schema_version":1,"fields":[{"id":"b","kind":"enum","label":"B","options":[{"value":"x","label":"X"}],"bind":{"target":"config","key":"J"}},{"id":"a","kind":"string","label":"A","visible_when":{"field":"b","equals":"z"},"bind":{"target":"config","key":"K"}}]}`, "options"},
		{"condition cycle", `{"schema_version":1,"fields":[{"id":"a","kind":"boolean","label":"A","visible_when":{"field":"b","equals":true},"bind":{"target":"config","key":"K"}},{"id":"b","kind":"boolean","label":"B","visible_when":{"field":"a","equals":true},"bind":{"target":"config","key":"J"}}]}`, "loop back"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, issues := DecodeComponentConfigSchema([]byte(tc.src))
			if len(issues) == 0 {
				t.Fatal("accepted")
			}
			if tc.want == "" {
				return
			}
			var all []string
			for _, i := range issues {
				all = append(all, i.Message)
			}
			if !strings.Contains(strings.Join(all, " | "), tc.want) {
				t.Fatalf("issues %q do not mention %q", all, tc.want)
			}
		})
	}
}

func TestComponentConfigSchemaBounds(t *testing.T) {
	build := func(n int) string {
		var fields []string
		for i := range n {
			fields = append(fields, fmt.Sprintf(`{"id":"f%d","kind":"string","label":"F","bind":{"target":"config","key":"K_%d"}}`, i, i))
		}
		return `{"schema_version":1,"fields":[` + strings.Join(fields, ",") + `]}`
	}
	if _, issues := DecodeComponentConfigSchema([]byte(build(MaxComponentConfigKeys))); len(issues) != 0 {
		t.Errorf("a schema at the limit: %+v", issues)
	}
	if _, issues := DecodeComponentConfigSchema([]byte(build(MaxComponentConfigKeys + 1))); len(issues) == 0 {
		t.Error("more config fields than a component holds config keys was accepted")
	}
	if _, issues := DecodeComponentConfigSchema([]byte(build(MaxConfigSchemaFields + 1))); len(issues) == 0 {
		t.Error("more fields than the schema bound was accepted")
	}
	huge := `{"schema_version":1,"fields":[{"id":"a","kind":"string","label":"A","description":"` + strings.Repeat("a", MaxConfigSchemaBytes) + `","bind":{"target":"config","key":"K"}}]}`
	if _, issues := DecodeComponentConfigSchema([]byte(huge)); len(issues) != 1 || !strings.Contains(issues[0].Message, "limit") {
		t.Errorf("an oversized schema: %+v", issues)
	}
	secrets := strings.Replace(build(MaxComponentSecrets+1), `"kind":"string"`, `"kind":"secret_ref"`, -1)
	secrets = strings.Replace(secrets, `"target":"config"`, `"target":"secret"`, -1)
	if _, issues := DecodeComponentConfigSchema([]byte(secrets)); len(issues) == 0 {
		t.Error("more secret fields than a component holds secrets was accepted")
	}
}

func values(pairs ...string) ComponentConfigValues {
	v := ComponentConfigValues{}
	for i := 0; i < len(pairs); i += 2 {
		v[pairs[i]] = json.RawMessage(pairs[i+1])
	}
	return v
}

func issuePaths(issues []ConfigIssue) string {
	var out []string
	for _, i := range issues {
		out = append(out, i.Path+": "+i.Message)
	}
	return strings.Join(out, " | ")
}

func TestComponentConfigValues(t *testing.T) {
	s := mustSchema(t, goodSchema)
	if issues := ValidateComponentConfigValues(s, values("key", `"my-key"`)); len(issues) != 0 {
		t.Fatalf("defaults and one secret choice: %s", issuePaths(issues))
	}
	tests := []struct {
		name string
		v    ComponentConfigValues
		want string
	}{
		{"required secret reference missing", values(), "values.key: is required"},
		{"unknown field", values("key", `"k"`, "ghost", `1`), "values.ghost: is not a field"},
		{"enum outside options", values("key", `"k"`, "region", `"asia"`), "values.region"},
		{"integer out of bounds", values("key", `"k"`, "verbose", `true`, "retries", `9`), "values.retries: is outside"},
		{"hidden field is still validated", values("key", `"k"`, "retries", `"many"`), "values.retries: must be a number"},
		{"string too long", values("key", `"k"`, "endpoint", `"`+strings.Repeat("a", 65)+`"`), "values.endpoint"},
		{"multi-line string", values("key", `"k"`, "endpoint", `"a\nb"`), "printable text on one line"},
		{"managed value cannot be changed", values("key", `"k"`, "mode", `"open"`), "managed and cannot be changed"},
		{"secret reference must be a stored name", values("key", `"NOT A NAME"`), "name of a stored secret"},
		{"secret reference is a name, not a value", values("key", `{"value":"x"}`), "name of a stored secret"},
		{"null", values("key", `"k"`, "endpoint", `null`), "not a value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := issuePaths(ValidateComponentConfigValues(s, tc.v)); !strings.Contains(got, tc.want) {
				t.Fatalf("issues %q do not mention %q", got, tc.want)
			}
		})
	}
	if issues := ValidateComponentConfigValues(s, values("key", `"k"`, "mode", `"managed"`)); len(issues) != 0 {
		t.Errorf("restating the managed value: %s", issuePaths(issues))
	}
}

func TestComponentConfigVisibilityGatesRequiredNotValidation(t *testing.T) {
	s := mustSchema(t, `{"schema_version":1,"fields":[
	  {"id":"adv","kind":"boolean","label":"Advanced","bind":{"target":"config","key":"ADV"}},
	  {"id":"level","kind":"integer","label":"Level","required":true,"visible_when":{"field":"adv","equals":true},"bind":{"target":"config","key":"LEVEL"}}]}`)
	if issues := ValidateComponentConfigValues(s, values()); len(issues) != 0 {
		t.Errorf("a hidden required field asked for a value: %s", issuePaths(issues))
	}
	if issues := ValidateComponentConfigValues(s, values("adv", `true`)); len(issues) != 1 {
		t.Errorf("a visible required field was not asked for: %s", issuePaths(issues))
	}
	if issues := ValidateComponentConfigValues(s, values("level", `"x"`)); len(issues) == 0 {
		t.Error("hiding a field removed it from validation")
	}
}
