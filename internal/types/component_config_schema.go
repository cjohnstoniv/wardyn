// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The declarative configuration schema of a custom component (0.9): the typed
// fields a component author exposes, with labels, groups, bounds, defaults and
// one level of conditional visibility, so the New Run wizard can render the
// component's settings with its own controls. It is data, never code: no
// markup, no expressions, no references to fetch. The vocabulary is closed and
// versioned, and anything outside it is refused rather than ignored.
//
// A schema describes what may be asked. It does not widen what a component
// may do: every field is bound to one target the component already has (a
// config key, a delivered secret), under that target's existing rules
// (reserved names, secret-name shape), and the server validates the values
// whether or not the form showed the field. This file is the contract; a
// component row does not carry a schema until the components lane adds the
// column.

// ComponentConfigSchemaVersion is the one schema version this build reads.
const ComponentConfigSchemaVersion = 1

// Bounds on a schema and its values.
const (
	MaxConfigSchemaBytes       = 64 << 10
	MaxConfigSchemaFields      = 64
	MaxConfigSchemaGroups      = 16
	MaxConfigOptions           = 64
	MaxConfigListItems         = 32
	MaxConfigLabelRunes        = 80
	MaxConfigDescriptionRunes  = 500
	MaxConfigOptionValueRunes  = 128
	maxConfigIntegerMagnitude  = 1 << 53
	configFieldIDMaxBytes      = 64
	configSecretNameMaxBytes   = 128
	configVisibilityChainLimit = MaxConfigSchemaFields
)

// ConfigFieldKind is the type of one field's value.
type ConfigFieldKind string

// The closed field kinds. Object and nested kinds are deliberately absent:
// a field is one value, and layout is groups.
const (
	ConfigKindString     ConfigFieldKind = "string"
	ConfigKindBoolean    ConfigFieldKind = "boolean"
	ConfigKindInteger    ConfigFieldKind = "integer"
	ConfigKindNumber     ConfigFieldKind = "number"
	ConfigKindEnum       ConfigFieldKind = "enum"
	ConfigKindStringList ConfigFieldKind = "string_list"
	// ConfigKindSecretRef picks one of the person's stored secrets by name.
	// It never holds a secret value.
	ConfigKindSecretRef ConfigFieldKind = "secret_ref"
)

// ConfigBindTarget is what a field's value feeds on the component.
type ConfigBindTarget string

const (
	// ConfigBindConfig is a ComponentDefinition.Config key (a plain,
	// non-secret environment value).
	ConfigBindConfig ConfigBindTarget = "config"
	// ConfigBindSecret is the stored secret delivered into one environment
	// variable.
	ConfigBindSecret ConfigBindTarget = "secret"
)

// ComponentConfigSchema is a component's field schema.
type ComponentConfigSchema struct {
	SchemaVersion int           `json:"schema_version"`
	Groups        []ConfigGroup `json:"groups,omitempty"`
	Fields        []ConfigField `json:"fields"`
}

// ConfigGroup is a labelled section the form lays fields out in.
type ConfigGroup struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ConfigField is one setting.
type ConfigField struct {
	// ID is the field's structural name: lowercase letters, digits and
	// underscores. Values are keyed by it.
	ID          string          `json:"id"`
	Kind        ConfigFieldKind `json:"kind"`
	Label       string          `json:"label"`
	Description string          `json:"description,omitempty"`
	Group       string          `json:"group,omitempty"`
	// Required asks for a value whenever the field is visible and has no default.
	Required bool `json:"required,omitempty"`
	// ReadOnly shows a managed value. It needs a Default, and a value that
	// differs from it is refused.
	ReadOnly bool `json:"read_only,omitempty"`
	// Default is the value an untouched field takes, typed by Kind.
	Default json.RawMessage `json:"default,omitempty"`
	// Min and Max bound an integer or number.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	// MaxLen bounds a string or each string_list entry, in bytes.
	MaxLen int `json:"max_len,omitempty"`
	// MaxItems bounds a string_list.
	MaxItems int `json:"max_items,omitempty"`
	// Options are an enum's choices.
	Options []ConfigOption `json:"options,omitempty"`
	// VisibleWhen shows the field only while another field holds one value.
	VisibleWhen *ConfigCondition `json:"visible_when,omitempty"`
	Bind        ConfigBinding    `json:"bind"`
}

// ConfigOption is one enum choice.
type ConfigOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ConfigCondition is "Field equals Equals". It is the only expression the
// schema has.
type ConfigCondition struct {
	Field  string          `json:"field"`
	Equals json.RawMessage `json:"equals"`
}

// ConfigBinding names the component target a field feeds.
type ConfigBinding struct {
	Target ConfigBindTarget `json:"target"`
	// Key is the environment variable name: the config key, or the variable
	// the secret is delivered in. It follows the component env-name rules,
	// including the reserved names.
	Key string `json:"key"`
}

// ComponentConfigValues are the values an attachment supplies, keyed by field
// ID, each typed by its field's kind.
type ComponentConfigValues map[string]json.RawMessage

// ConfigIssue is one problem in a schema or a set of values.
type ConfigIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

var configSecretWordRE = regexp.MustCompile(`(?:^|[^a-z0-9])(?:pass(?:word|wd)?|secret_value|secret_key|api_?key|token|credential|private_?key|authorization|bearer|cookie)(?:[^a-z0-9]|$)`)

// ConfigKeyLooksSecret reports whether a config key names a secret by one of
// its words (GITHUB_TOKEN, apiKey, db-password). Whole words only: MAX_TOKENS
// and BYPASS_CACHE are plain settings. A config value is plain text by
// contract, so a key like this is refused by the schema and by a template; a
// secret travels through a secret_ref field.
func ConfigKeyLooksSecret(key string) bool {
	var b strings.Builder
	for i, r := range key {
		if i > 0 && r >= 'A' && r <= 'Z' && key[i-1] >= 'a' && key[i-1] <= 'z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return configSecretWordRE.MatchString(strings.ToLower(strings.ReplaceAll(b.String(), "-", "_")))
}

// DecodeComponentConfigSchema reads one schema from JSON: bounded size, one
// value, no unknown field, and the validation below. Anything else is refused,
// including an unknown schema version.
func DecodeComponentConfigSchema(data []byte) (ComponentConfigSchema, []ConfigIssue) {
	var s ComponentConfigSchema
	if len(data) > MaxConfigSchemaBytes {
		return s, []ConfigIssue{{"schema", fmt.Sprintf("is %d bytes; the limit is %d", len(data), MaxConfigSchemaBytes)}}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, []ConfigIssue{{"schema", err.Error()}}
	}
	if dec.More() {
		return s, []ConfigIssue{{"schema", "must be exactly one JSON value"}}
	}
	return s, ValidateComponentConfigSchema(s)
}

// ValidateComponentConfigSchema checks a schema against the closed vocabulary
// and its bounds.
func ValidateComponentConfigSchema(s ComponentConfigSchema) []ConfigIssue {
	var issues []ConfigIssue
	add := func(path, format string, args ...any) {
		issues = append(issues, ConfigIssue{path, fmt.Sprintf(format, args...)})
	}
	if s.SchemaVersion != ComponentConfigSchemaVersion {
		add("schema_version", "%d is not supported: this server reads version %d", s.SchemaVersion, ComponentConfigSchemaVersion)
		return issues
	}
	if n := len(s.Fields); n == 0 || n > MaxConfigSchemaFields {
		add("fields", "needs 1 to %d fields, has %d", MaxConfigSchemaFields, n)
		return issues
	}
	groups := validateConfigGroups(s.Groups, add)
	byID := map[string]ConfigField{}
	for i, f := range s.Fields {
		path := fmt.Sprintf("fields[%d]", i)
		if !validConfigID(f.ID) {
			add(path+".id", "%q must be lowercase letters, digits and underscores, starting with a letter, at most %d bytes", f.ID, configFieldIDMaxBytes)
		} else if _, dup := byID[f.ID]; dup {
			add(path+".id", "%q is used twice", f.ID)
		}
		byID[f.ID] = f
		if f.Group != "" && !groups[f.Group] {
			add(path+".group", "%q is not a group of this schema", f.Group)
		}
		validateConfigText(path+".label", f.Label, MaxConfigLabelRunes, true, add)
		validateConfigText(path+".description", f.Description, MaxConfigDescriptionRunes, false, add)
		validateConfigField(path, f, add)
	}
	validateConfigBindings(s.Fields, add)
	validateConfigVisibility(s.Fields, byID, add)
	return issues
}

func validateConfigGroups(groups []ConfigGroup, add func(string, string, ...any)) map[string]bool {
	seen := map[string]bool{}
	if len(groups) > MaxConfigSchemaGroups {
		add("groups", "has %d groups; the limit is %d", len(groups), MaxConfigSchemaGroups)
	}
	for i, g := range groups {
		path := fmt.Sprintf("groups[%d]", i)
		if !validConfigID(g.ID) {
			add(path+".id", "%q must be lowercase letters, digits and underscores, starting with a letter", g.ID)
		} else if seen[g.ID] {
			add(path+".id", "%q is used twice", g.ID)
		}
		seen[g.ID] = true
		validateConfigText(path+".label", g.Label, MaxConfigLabelRunes, true, add)
		validateConfigText(path+".description", g.Description, MaxConfigDescriptionRunes, false, add)
	}
	return seen
}

func validConfigID(id string) bool {
	if id == "" || len(id) > configFieldIDMaxBytes || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') }) < 0
}

// validateConfigText keeps labels and descriptions to plain single-line text:
// the console renders them as text, and no markup is ever interpreted.
func validateConfigText(path, s string, maxRunes int, required bool, add func(string, string, ...any)) {
	switch n := utf8.RuneCountInString(s); {
	case n == 0 && required:
		add(path, "is required")
	case n > maxRunes:
		add(path, "is %d characters; the limit is %d", n, maxRunes)
	case !printable(s) || strings.ContainsAny(s, "<>"):
		add(path, "must be plain printable text on one line, with no markup")
	}
}

func validateConfigField(path string, f ConfigField, add func(string, string, ...any)) {
	numeric := f.Kind == ConfigKindInteger || f.Kind == ConfigKindNumber
	switch f.Kind {
	case ConfigKindString, ConfigKindBoolean, ConfigKindInteger, ConfigKindNumber, ConfigKindEnum, ConfigKindStringList, ConfigKindSecretRef:
	default:
		add(path+".kind", "%q is not a supported kind", f.Kind)
		return
	}
	if !numeric && (f.Min != nil || f.Max != nil) {
		add(path, "min and max apply to integer and number fields only")
	}
	if f.MaxLen != 0 && f.Kind != ConfigKindString && f.Kind != ConfigKindStringList {
		add(path+".max_len", "applies to string and string_list fields only")
	}
	if f.MaxLen < 0 || f.MaxLen > MaxComponentConfigValueBytes {
		add(path+".max_len", "must be 0 to %d", MaxComponentConfigValueBytes)
	}
	if f.MaxItems != 0 && f.Kind != ConfigKindStringList {
		add(path+".max_items", "applies to string_list fields only")
	}
	if f.MaxItems < 0 || f.MaxItems > MaxConfigListItems {
		add(path+".max_items", "must be 0 to %d", MaxConfigListItems)
	}
	if numeric && f.Min != nil && f.Max != nil && *f.Min > *f.Max {
		add(path, "min is above max")
	}
	validateConfigOptions(path, f, add)
	if f.ReadOnly && len(f.Default) == 0 {
		add(path+".read_only", "a managed value needs a default")
	}
	if f.Kind == ConfigKindSecretRef && (len(f.Default) > 0 || f.ReadOnly) {
		add(path, "a secret reference is chosen per attachment: it has no default and is never read-only")
	}
	if len(f.Default) > 0 && f.Kind != ConfigKindSecretRef {
		if msg := checkConfigValue(f, f.Default); msg != "" {
			add(path+".default", "%s", msg)
		}
	}
}

func validateConfigOptions(path string, f ConfigField, add func(string, string, ...any)) {
	if f.Kind != ConfigKindEnum {
		if len(f.Options) > 0 {
			add(path+".options", "applies to enum fields only")
		}
		return
	}
	if n := len(f.Options); n == 0 || n > MaxConfigOptions {
		add(path+".options", "needs 1 to %d options, has %d", MaxConfigOptions, n)
	}
	seen := map[string]bool{}
	for i, o := range f.Options {
		p := fmt.Sprintf("%s.options[%d]", path, i)
		if o.Value == "" || utf8.RuneCountInString(o.Value) > MaxConfigOptionValueRunes || !printable(o.Value) {
			add(p+".value", "must be 1 to %d printable characters", MaxConfigOptionValueRunes)
		}
		if seen[o.Value] {
			add(p+".value", "%q is used twice", o.Value)
		}
		seen[o.Value] = true
		validateConfigText(p+".label", o.Label, MaxConfigLabelRunes, true, add)
	}
}

// validateConfigBindings holds each target to its component limit and to one
// field, and each key to the component env-name rules (reserved names too).
func validateConfigBindings(fields []ConfigField, add func(string, string, ...any)) {
	seen := map[ConfigBinding]bool{}
	counts := map[ConfigBindTarget]int{}
	for i, f := range fields {
		path := fmt.Sprintf("fields[%d].bind", i)
		switch f.Bind.Target {
		case ConfigBindConfig:
			if f.Kind == ConfigKindStringList || f.Kind == ConfigKindSecretRef {
				add(path, "a %s field cannot feed a config value", f.Kind)
			}
		case ConfigBindSecret:
			if f.Kind != ConfigKindSecretRef {
				add(path, "a secret target needs a secret_ref field")
			}
		default:
			add(path+".target", "%q is not a supported target", f.Bind.Target)
			continue
		}
		if f.Kind == ConfigKindSecretRef && f.Bind.Target != ConfigBindSecret {
			add(path, "a secret_ref field feeds a secret target")
		}
		if err := validComponentEnvName(f.Bind.Key); err != nil {
			add(path+".key", "%v", err)
		} else if f.Bind.Target == ConfigBindConfig && ConfigKeyLooksSecret(f.Bind.Key) {
			add(path+".key", "%q names a secret, and a config value is plain text: use a secret_ref field", f.Bind.Key)
		}
		if seen[f.Bind] {
			add(path, "%s %q is fed by two fields", f.Bind.Target, f.Bind.Key)
		}
		seen[f.Bind] = true
		counts[f.Bind.Target]++
	}
	if counts[ConfigBindConfig] > MaxComponentConfigKeys {
		add("fields", "%d config fields; a component holds at most %d config keys", counts[ConfigBindConfig], MaxComponentConfigKeys)
	}
	if counts[ConfigBindSecret] > MaxComponentSecrets {
		add("fields", "%d secret fields; a component holds at most %d secrets", counts[ConfigBindSecret], MaxComponentSecrets)
	}
}

// validateConfigVisibility checks each visible_when names another field, asks
// for a value that field can hold, and that the chain of conditions ends.
func validateConfigVisibility(fields []ConfigField, byID map[string]ConfigField, add func(string, string, ...any)) {
	for i, f := range fields {
		c := f.VisibleWhen
		if c == nil {
			continue
		}
		path := fmt.Sprintf("fields[%d].visible_when", i)
		ref, ok := byID[c.Field]
		switch {
		case !ok:
			add(path+".field", "%q is not a field of this schema", c.Field)
		case c.Field == f.ID:
			add(path+".field", "a field cannot depend on itself")
		case ref.Kind == ConfigKindStringList || ref.Kind == ConfigKindSecretRef:
			add(path+".field", "a %s field cannot be a condition", ref.Kind)
		case len(c.Equals) == 0:
			add(path+".equals", "is required")
		default:
			if msg := checkConfigValue(ref, c.Equals); msg != "" {
				add(path+".equals", "%s", msg)
			}
		}
	}
	for i, f := range fields {
		for hops, cur := 0, f; cur.VisibleWhen != nil; hops++ {
			next, ok := byID[cur.VisibleWhen.Field]
			if !ok || hops > configVisibilityChainLimit {
				break
			}
			if next.ID == f.ID {
				add(fmt.Sprintf("fields[%d].visible_when", i), "the conditions loop back to %q", f.ID)
				break
			}
			cur = next
		}
	}
}

// checkConfigValue types one value against its field and returns the problem,
// or "".
func checkConfigValue(f ConfigField, raw json.RawMessage) string {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() || v == nil {
		return "must be one JSON value of the field's kind (null is not a value)"
	}
	switch f.Kind {
	case ConfigKindString:
		s, ok := v.(string)
		return configStringProblem(s, ok, f.MaxLen)
	case ConfigKindBoolean:
		if _, ok := v.(bool); !ok {
			return "must be true or false"
		}
	case ConfigKindInteger, ConfigKindNumber:
		return configNumberProblem(f, v)
	case ConfigKindEnum:
		s, ok := v.(string)
		if !ok || !slices.ContainsFunc(f.Options, func(o ConfigOption) bool { return o.Value == s }) {
			return "must be one of the field's options"
		}
	case ConfigKindStringList:
		return configListProblem(f, v)
	case ConfigKindSecretRef:
		s, ok := v.(string)
		if !ok || len(s) > configSecretNameMaxBytes || !SecretNameRE.MatchString(s) {
			return "must be the name of a stored secret"
		}
	}
	return ""
}

func configStringProblem(s string, ok bool, maxLen int) string {
	if maxLen == 0 {
		maxLen = MaxComponentConfigValueBytes
	}
	if !ok {
		return "must be a string"
	}
	if len(s) > maxLen {
		return fmt.Sprintf("is %d bytes; the limit is %d", len(s), maxLen)
	}
	if !printable(s) {
		return "must be printable text on one line"
	}
	return ""
}

func configNumberProblem(f ConfigField, v any) string {
	n, ok := v.(json.Number)
	if !ok {
		return "must be a number"
	}
	x, err := n.Float64()
	if err != nil || math.IsInf(x, 0) || math.IsNaN(x) {
		return "must be a finite number"
	}
	if f.Kind == ConfigKindInteger && (x != math.Trunc(x) || math.Abs(x) > maxConfigIntegerMagnitude) {
		return "must be a whole number"
	}
	if f.Min != nil && x < *f.Min || f.Max != nil && x > *f.Max {
		return "is outside the field's bounds"
	}
	return ""
}

func configListProblem(f ConfigField, v any) string {
	list, ok := v.([]any)
	if !ok {
		return "must be a list of strings"
	}
	limit := cmp.Or(f.MaxItems, MaxConfigListItems)
	if len(list) > limit {
		return fmt.Sprintf("has %d entries; the limit is %d", len(list), limit)
	}
	for _, e := range list {
		s, ok := e.(string)
		if msg := configStringProblem(s, ok, f.MaxLen); msg != "" {
			return "entries " + msg
		}
	}
	return ""
}

// ValidateComponentConfigValues checks one attachment's values against a
// valid schema. Each supplied value is typed and bounded whether or not its
// field is visible. A required field is asked for only while visible and
// without a default; a managed (read-only) field accepts only its default.
// looksSecret refuses a string that is credential-shaped (a secret reference is
// a stored secret's name, never its value); this package cannot import the
// scanner, so the caller passes it, and a nil scanner is itself refused so no
// caller can opt out of the scan.
func ValidateComponentConfigValues(s ComponentConfigSchema, values ComponentConfigValues, looksSecret func(string) bool) []ConfigIssue {
	if looksSecret == nil {
		return []ConfigIssue{{"values", "cannot be checked without a secret scanner"}}
	}
	var issues []ConfigIssue
	byID := make(map[string]ConfigField, len(s.Fields))
	for _, f := range s.Fields {
		byID[f.ID] = f
	}
	for _, id := range sortedConfigKeys(values) {
		f, ok := byID[id]
		if !ok {
			issues = append(issues, ConfigIssue{"values." + id, "is not a field of this schema"})
			continue
		}
		if msg := checkConfigValue(f, values[id]); msg != "" {
			issues = append(issues, ConfigIssue{"values." + id, msg})
		} else if configValueLooksSecret(f, values[id], looksSecret) {
			issues = append(issues, ConfigIssue{"values." + id, "looks like a secret value: a template and a component keep the stored secret's name, never its value"})
		} else if f.ReadOnly && !jsonEqual(values[id], f.Default) {
			issues = append(issues, ConfigIssue{"values." + id, "is managed and cannot be changed"})
		}
	}
	effective := func(f ConfigField) json.RawMessage {
		if v, ok := values[f.ID]; ok {
			return v
		}
		return f.Default
	}
	for _, f := range s.Fields {
		if f.Required && len(effective(f)) == 0 && configVisible(f, byID, effective, 0) {
			issues = append(issues, ConfigIssue{"values." + f.ID, "is required"})
		}
	}
	return issues
}

// configValueLooksSecret applies the scanner to every string a value holds.
func configValueLooksSecret(f ConfigField, raw json.RawMessage, looksSecret func(string) bool) bool {
	if f.Kind == ConfigKindEnum || f.Kind == ConfigKindBoolean || f.Kind == ConfigKindInteger || f.Kind == ConfigKindNumber {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return looksSecret(x)
	case []any:
		return slices.ContainsFunc(x, func(e any) bool { s, ok := e.(string); return ok && looksSecret(s) })
	}
	return false
}

func sortedConfigKeys(m ComponentConfigValues) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// configVisible follows the visibility chain: a field is visible when its
// condition holds and the field it depends on is itself visible.
func configVisible(f ConfigField, byID map[string]ConfigField, effective func(ConfigField) json.RawMessage, depth int) bool {
	c := f.VisibleWhen
	if c == nil {
		return true
	}
	ref, ok := byID[c.Field]
	if !ok || depth > configVisibilityChainLimit {
		return false
	}
	return jsonEqual(effective(ref), c.Equals) && configVisible(ref, byID, effective, depth+1)
}

// jsonEqual compares two JSON values by meaning, so 1 and 1.0 agree.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
