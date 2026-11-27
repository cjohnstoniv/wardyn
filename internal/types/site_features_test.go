// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Every key of the features block is a pointer to a struct whose first field is its switch, an Enabled *bool that
// reads off when absent, and every key and switch is omitempty, so a deployment with no block reads the GET it always
// did.
func TestSiteFeatureSwitchesShape(t *testing.T) {
	block := reflect.TypeFor[SiteFeatureSwitches]()
	if block.NumField() == 0 {
		t.Fatal("the features block has no keys")
	}
	for i := range block.NumField() {
		f := block.Field(i)
		if tag := f.Tag.Get("json"); !strings.HasSuffix(tag, ",omitempty") {
			t.Errorf("%s: json tag %q, want omitempty", f.Name, tag)
		}
		if f.Type.Kind() != reflect.Pointer || f.Type.Elem().Kind() != reflect.Struct {
			t.Errorf("%s: type %s, want a pointer to a struct", f.Name, f.Type)
			continue
		}
		key := f.Type.Elem()
		if key.NumField() == 0 {
			t.Errorf("%s: %s has no fields", f.Name, key)
			continue
		}
		first := key.Field(0)
		if first.Name != "Enabled" || first.Type != reflect.TypeFor[*bool]() || first.Tag.Get("json") != "enabled,omitempty" {
			t.Errorf("%s: first field %s %s %q, want Enabled *bool `json:\"enabled,omitempty\"`", f.Name, first.Name, first.Type, first.Tag)
		}
	}
	raw, err := json.Marshal(SiteFeatureSwitches{})
	if err != nil || string(raw) != "{}" {
		t.Errorf("json.Marshal(SiteFeatureSwitches{}) = %s (%v), want {}", raw, err)
	}
	raw, err = json.Marshal(SiteConfig{})
	if err != nil || strings.Contains(string(raw), `"features"`) {
		t.Errorf("json.Marshal(SiteConfig{}) = %s (%v), want no features key", raw, err)
	}
}
