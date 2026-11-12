// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macosguest_test

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// plistValue is a parsed property list value: map[string]any, []any, string, or int64/bool as text.
func plistValue(d *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "dict":
		m := map[string]any{}
		var key string
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					if err := d.DecodeElement(&key, &t); err != nil {
						return nil, err
					}
					continue
				}
				if key == "" {
					return nil, fmt.Errorf("a dict value <%s> has no key", t.Name.Local)
				}
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				m[key], key = v, ""
			case xml.EndElement:
				if key != "" {
					return nil, fmt.Errorf("key %q has no value", key)
				}
				return m, nil
			}
		}
	case "array":
		var a []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := plistValue(d, t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			case xml.EndElement:
				return a, nil
			}
		}
	case "string", "integer", "true", "false", "real", "date", "data":
		var s string
		if err := d.DecodeElement(&s, &start); err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, fmt.Errorf("unknown plist element <%s>", start.Name.Local)
}

func parsePlist(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := xml.NewDecoder(strings.NewReader(string(b)))
	d.Strict = true
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			t.Fatalf("%s: no <plist> element", path)
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if s, ok := tok.(xml.StartElement); ok && s.Name.Local == "plist" {
			for {
				tok, err := d.Token()
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				if s, ok := tok.(xml.StartElement); ok {
					v, err := plistValue(d, s)
					root, isDict := v.(map[string]any)
					if err != nil || !isDict {
						t.Fatalf("%s: the plist's root is not a well-formed dict: %v", path, err)
					}
					return root
				}
			}
		}
	}
}

// placeholders is every __NAME__ in the strings of v.
func placeholders(v any, into map[string]bool) {
	re := regexp.MustCompile(`__[A-Z_]+__`)
	switch v := v.(type) {
	case string:
		for _, p := range re.FindAllString(v, -1) {
			into[p] = true
		}
	case []any:
		for _, e := range v {
			placeholders(e, into)
		}
	case map[string]any:
		for _, e := range v {
			placeholders(e, into)
		}
	}
}

func payload(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	if doc["PayloadType"] != "Configuration" || doc["PayloadScope"] != "System" {
		t.Fatalf("top level = %v %v, want a system Configuration profile", doc["PayloadType"], doc["PayloadScope"])
	}
	content, _ := doc["PayloadContent"].([]any)
	if len(content) != 1 {
		t.Fatalf("PayloadContent holds %d payloads, want 1", len(content))
	}
	p, _ := content[0].(map[string]any)
	return p
}

// Both MDM templates are well-formed property lists holding the payload they are for, and every value the
// organisation fills in is a placeholder, so a template can't be installed unfilled by mistake.
func TestMDMTemplatesAreWellFormedWithTheirPlaceholders(t *testing.T) {
	common := []string{"__IDENTIFIER_PREFIX__", "__ORGANIZATION__", "__PAYLOAD_UUID__", "__PROFILE_UUID__"}

	sa := parsePlist(t, "mdm/setup-assistant.mobileconfig.template")
	p := payload(t, sa)
	if p["PayloadType"] != "com.apple.SetupAssistant.managed" {
		t.Errorf("setup assistant payload type = %v", p["PayloadType"])
	}
	items, _ := p["SkipSetupItems"].([]any)
	for _, want := range []string{"AppleID", "Privacy", "ScreenTime", "Siri", "OSShowcase", "UpdateCompleted", "Intelligence"} {
		if !slices.Contains(items, any(want)) {
			t.Errorf("SkipSetupItems lacks %q: %v", want, items)
		}
	}
	got := map[string]bool{}
	placeholders(sa, got)
	for _, want := range common {
		if !got[want] {
			t.Errorf("setup-assistant template lacks %s", want)
		}
	}

	fda := parsePlist(t, "mdm/full-disk-access.mobileconfig.template")
	p = payload(t, fda)
	if p["PayloadType"] != "com.apple.TCC.configuration-profile-policy" {
		t.Errorf("full disk access payload type = %v", p["PayloadType"])
	}
	services, _ := p["Services"].(map[string]any)
	rows, _ := services["SystemPolicyAllFiles"].([]any)
	if len(services) != 1 || len(rows) != 1 {
		t.Fatalf("Services = %v, want one SystemPolicyAllFiles row", services)
	}
	row, _ := rows[0].(map[string]any)
	if row["IdentifierType"] != "path" || row["Authorization"] != "Allow" || row["Identifier"] != "__AGENT_PATH__" || row["CodeRequirement"] != "__CODE_REQUIREMENT__" {
		t.Errorf("row = %v, want path, Allow and the two placeholders", row)
	}
	got = map[string]bool{}
	placeholders(fda, got)
	for _, want := range append(common, "__AGENT_PATH__", "__CODE_REQUIREMENT__") {
		if !got[want] {
			t.Errorf("full-disk-access template lacks %s", want)
		}
	}
}
