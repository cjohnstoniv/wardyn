// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"

	yaml "gopkg.in/yaml.v3"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The strict reading of a template document. Text becomes one JSON value
// (bounded, one document, no duplicate key, no YAML alias, anchor, tag, merge
// or ambiguous number), and that value is read against the real request and
// policy types and the field registry: an unknown key, a secret, run state
// and a field the template schema excludes are each refused with the path and
// a closed reason. Nothing is trimmed and nothing is guessed; one problem
// does not hide the next, so the console can show them all.

const (
	maxTemplateDocumentBytes   = 1 << 20
	maxTemplateDocumentDepth   = 64
	maxTemplateGrantScopeBytes = 4096
	maxTemplateNameRunes       = 128
	maxTemplateDescriptionRune = 2000
)

var (
	templateSecretKeyRE = regexp.MustCompile(`(?i)(pass(word|wd)?|secret_value|api[_-]?key|token|credential|private[_-]?key|authorization|bearer|cookie)`)
	templateURLUserRE   = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`)
	templatePoolIDRE    = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	templateYAMLOctalRE = regexp.MustCompile(`^[-+]?0[0-9_xXoObB]`)
	templateMetadataKey = []string{"id", "template_id", "owner", "owner_id", "group", "group_id", "scope", "revision", "version", "visibility", "writable", "created_at", "updated_at", "created_by", "updated_by"}
	templateRunStateKey = []string{
		"run_id", "run", "status", "state", "phase", "generation", "execution_generation", "execution", "handle", "claim", "claimed_by", "runner_key", "lease", "epoch",
		"session", "session_id", "admission", "admitted", "resolved", "resolved_policy", "effective_policy", "sandbox_ref", "sandbox", "container_id", "started_at", "ended_at", "exit_code", "provenance",
	}
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	requestType         = reflect.TypeFor[client.CreateRunRequest]()
	rawMessageType      = reflect.TypeFor[json.RawMessage]()
)

// templateDecoder collects the diagnostics of one document.
type templateDecoder struct{ diags []client.TemplateDiagnostic }

func (d *templateDecoder) add(path, reason, message string) {
	d.diags = append(d.diags, client.TemplateDiagnostic{Path: path, Reason: reason, Message: message})
}

func (d *templateDecoder) invalid(path, detail string) {
	d.add(path, reasonTemplateFieldInvalid, templateFieldInvalidMsg(path, detail))
}

// unknownKey says why a key the schema does not have is refused: trusted
// metadata (owner, scope, group, revision: at the document's top level only),
// run state and secret-looking names each get their own reason, so the
// console can say what is wrong rather than only that something is.
func (d *templateDecoder) unknownKey(parent, key string) {
	path := joinTemplatePath(parent, key)
	lower := strings.ToLower(key)
	switch {
	case parent == "" && slices.Contains(templateMetadataKey, lower):
		d.add(path, reasonTemplateMetadataInContent, templateMetadataInContentMsg(path))
	case slices.Contains(templateRunStateKey, lower):
		d.add(path, reasonTemplateRunStateRefused, templateRunStateRefusedMsg(path))
	case templateSecretKeyRE.MatchString(key):
		d.add(path, reasonTemplateSecretRefused, templateSecretRefusedMsg(path))
	default:
		d.add(path, reasonTemplateFieldUnknown, templateFieldUnknownMsg(path))
	}
}

func joinTemplatePath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// templateSourceJSON turns the text into one JSON value, or says why not.
func templateSourceJSON(src []byte, format client.TemplateFormat) ([]byte, error) {
	if len(src) > maxTemplateDocumentBytes {
		return nil, fmt.Errorf("it is %d bytes and the limit is %d", len(src), maxTemplateDocumentBytes)
	}
	switch format {
	case client.TemplateFormatJSON:
		return src, nil
	case client.TemplateFormatYAML:
		return templateYAMLToJSON(src)
	}
	return nil, fmt.Errorf("format %q is not json or yaml", format)
}

// templateYAMLToJSON reads one YAML document as plain data. Aliases, anchors,
// custom tags, merge keys, timestamps and numbers written in another base are
// refused because a second parser (the console's) would read them differently.
func templateYAMLToJSON(src []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var node yaml.Node
	if err := dec.Decode(&node); err != nil {
		return nil, fmt.Errorf("the YAML does not parse: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("it must contain exactly one document")
	}
	if err := checkYAMLNode(&node, 0); err != nil {
		return nil, err
	}
	var v any
	if err := node.Decode(&v); err != nil {
		return nil, fmt.Errorf("the YAML does not parse: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("the YAML holds a value JSON cannot (a non-string key, or infinity): %w", err)
	}
	return out, nil
}

func checkYAMLNode(n *yaml.Node, depth int) error {
	if depth > maxTemplateDocumentDepth {
		return fmt.Errorf("it nests deeper than %d levels", maxTemplateDocumentDepth)
	}
	switch {
	case n.Kind == yaml.AliasNode || n.Anchor != "":
		return errors.New("YAML aliases and anchors are not accepted")
	case n.Kind == yaml.ScalarNode && n.ShortTag() == "!!merge":
		return errors.New("YAML merge keys are not accepted")
	case n.Kind == yaml.ScalarNode && n.ShortTag() == "!!timestamp":
		return fmt.Errorf("%q reads as a timestamp: quote it to make it text", n.Value)
	case n.Kind == yaml.ScalarNode && n.ShortTag() == "!!int" && templateYAMLOctalRE.MatchString(n.Value) && n.Value != "0":
		return fmt.Errorf("the number %q is ambiguous between YAML versions: write it in decimal", n.Value)
	case n.Tag != "" && !strings.HasPrefix(n.Tag, "!!"):
		return fmt.Errorf("the YAML tag %s is not accepted", n.Tag)
	}
	for _, c := range n.Content {
		if err := checkYAMLNode(c, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// checkTemplateJSON refuses what encoding/json would quietly accept: more
// than one value, a key written twice, and nesting deeper than the limit.
func checkTemplateJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanTemplateJSON(dec, "the document", 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("it must contain exactly one document")
	}
	return nil
}

func scanTemplateJSON(dec *json.Decoder, path string, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("it is not valid JSON: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxTemplateDocumentDepth {
		return fmt.Errorf("it nests deeper than %d levels", maxTemplateDocumentDepth)
	}
	seen := map[string]bool{}
	for i := 0; dec.More(); i++ {
		child := fmt.Sprintf("%s[%d]", path, i)
		if delim == '{' {
			kt, err := dec.Token()
			if err != nil {
				return fmt.Errorf("it is not valid JSON: %w", err)
			}
			key, _ := kt.(string)
			if seen[key] {
				return fmt.Errorf("%s has the key %q twice", path, key)
			}
			seen[key] = true
			child = path + "." + key
		}
		if err := scanTemplateJSON(dec, child, depth+1); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// walkIntent reads the intent object against CreateRunRequest and the registry.
func (d *templateDecoder) walkIntent(raw json.RawMessage, path string) {
	d.walkValue(requestType, raw, path)
}

// walkValue reads one JSON value against the Go type it will become.
func (d *templateDecoder) walkValue(t reflect.Type, raw json.RawMessage, path string) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		d.invalid(path, "is null, which is not a value: leave the field out")
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == rawMessageType:
		d.walkRaw(raw, path)
	case reflect.PointerTo(t).Implements(textUnmarshalerType):
		d.walkText(t, raw, path)
	case t.Kind() == reflect.Struct:
		d.walkStruct(t, raw, path)
	case t.Kind() == reflect.Slice:
		d.walkSlice(t, raw, path)
	case t.Kind() == reflect.Map:
		d.walkMap(t, raw, path)
	default:
		d.walkScalar(t, raw, path)
	}
}

func (d *templateDecoder) walkStruct(t reflect.Type, raw json.RawMessage, path string) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		d.invalid(path, "must be an object")
		return
	}
	for _, key := range sortedKeys(obj) {
		keyPath := joinTemplatePath(path, key)
		if t == requestType && key == "pool_id" {
			d.walkPoolID(obj[key], keyPath)
			continue
		}
		field, ok := templateStructField(t, key)
		if !ok {
			d.unknownKey(path, key)
			continue
		}
		if rule, isExcluded := templateExclusion(t, key); isExcluded {
			d.excluded(keyPath, rule)
			continue
		}
		d.walkValue(field.Type, obj[key], keyPath)
	}
}

// templateExclusion looks a field up in the registry's exclusions.
func templateExclusion(t reflect.Type, key string) (TemplateFieldRule, bool) {
	switch t.Name() {
	case "CreateRunRequest":
		r, ok := templateRule(templateRequestRules, key)
		return r, ok && r.Excluded != ""
	case "RunPolicySpec":
		r, ok := templateRule(templatePolicyRules, key)
		return r, ok && r.Excluded != ""
	}
	r, ok := templateNestedExcluded[t.Name()+"."+key]
	return r, ok
}

func (d *templateDecoder) excluded(path string, rule TemplateFieldRule) {
	if rule.Excluded == templateExcludeSecret {
		d.add(path, reasonTemplateSecretRefused, templateSecretRefusedMsg(path))
		return
	}
	d.add(path, reasonTemplateFieldExcluded, templateFieldExcludedMsg(path))
}

func (d *templateDecoder) walkPoolID(raw json.RawMessage, path string) {
	var id string
	if json.Unmarshal(raw, &id) != nil || !templatePoolIDRE.MatchString(id) {
		d.invalid(path, "must be a pool id: 1 to 128 letters, digits and . _ : -")
	}
}

func (d *templateDecoder) walkSlice(t reflect.Type, raw json.RawMessage, path string) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		d.invalid(path, "must be a list")
		return
	}
	for i, item := range items {
		d.walkValue(t.Elem(), item, fmt.Sprintf("%s[%d]", path, i))
	}
}

// walkMap reads a string map. A component's config is plain, non-secret
// configuration by contract, so a key that names a secret is refused there.
func (d *templateDecoder) walkMap(t reflect.Type, raw json.RawMessage, path string) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		d.invalid(path, "must be an object")
		return
	}
	for _, key := range sortedKeys(obj) {
		keyPath := joinTemplatePath(path, key)
		if strings.HasSuffix(path, ".config") && templateSecretKeyRE.MatchString(key) {
			d.add(keyPath, reasonTemplateSecretRefused, templateSecretRefusedMsg(keyPath))
			continue
		}
		d.walkValue(t.Elem(), obj[key], keyPath)
	}
}

func (d *templateDecoder) walkText(t reflect.Type, raw json.RawMessage, path string) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		d.invalid(path, "must be a string")
		return
	}
	if err := reflect.New(t).Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)); err != nil {
		d.invalid(path, "is not a valid id")
	}
}

func (d *templateDecoder) walkScalar(t reflect.Type, raw json.RawMessage, path string) {
	switch t.Kind() {
	case reflect.String:
		var s string
		if json.Unmarshal(raw, &s) != nil {
			d.invalid(path, "must be a string")
			return
		}
		d.scanString(path, s)
	case reflect.Bool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			d.invalid(path, "must be true or false")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if json.Unmarshal(raw, reflect.New(t).Interface()) != nil {
			d.invalid(path, "must be a whole number")
		}
	default:
		d.invalid(path, "is a kind of value templates do not carry")
	}
}

// scanString refuses a string that is, or holds, credential material, or that
// is a redaction placeholder standing where a real value belongs.
func (d *templateDecoder) scanString(path, s string) {
	switch {
	case isRedactedPlaceholder(s):
		d.invalid(path, "is a redacted placeholder, not a value: set the real value or leave the field out")
	case templateURLUserRE.MatchString(s):
		d.add(path, reasonTemplateSecretRefused, templateSecretRefusedMsg(path))
	default:
		if _, found := contentscan.MatchSecretFormat(s); found {
			d.add(path, reasonTemplateSecretRefused, templateSecretRefusedMsg(path))
		}
	}
}

func isRedactedPlaceholder(s string) bool {
	return strings.Contains(strings.ToLower(s), "redacted") && len(s) <= 32
}

// walkRaw reads a free-form value (a grant's scope): one bounded object whose
// strings are scanned. Its fields are checked per grant kind where the grant
// is used.
func (d *templateDecoder) walkRaw(raw json.RawMessage, path string) {
	if len(raw) > maxTemplateGrantScopeBytes {
		d.invalid(path, fmt.Sprintf("is %d bytes and the limit is %d", len(raw), maxTemplateGrantScopeBytes))
		return
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		d.invalid(path, "must be an object")
		return
	}
	d.scanAny(v, path)
}

func (d *templateDecoder) scanAny(v any, path string) {
	switch x := v.(type) {
	case string:
		d.scanString(path, x)
	case []any:
		for i, e := range x {
			d.scanAny(e, fmt.Sprintf("%s[%d]", path, i))
		}
	case map[string]any:
		for _, k := range sortedKeys(x) {
			d.scanAny(x[k], joinTemplatePath(path, k))
		}
	}
}
