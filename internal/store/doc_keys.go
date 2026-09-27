// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"reflect"
	"strings"
)

// declaredJSONKeys returns the top-level object keys encoding/json can write for
// the struct type of v: the keys THIS binary owns in a stored JSONB document.
//
// A document write replaces those keys and keeps every other one
// (`(old - declared) || new`), so a key a NEWER wardynd wrote survives an older
// binary's save. Without it, the older binary's whole-document replace silently
// dropped the newer keys — and for governance limits an absent limit means no
// limit, so the drop was a widening. Keys this binary declares still clear
// normally (an omitempty field left out is gone). Only the top level is kept:
// an unknown key nested inside a known one is still dropped.
func declaredJSONKeys(v any) []string {
	var keys []string
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
				walk(f.Type) // promoted fields marshal at this level
				continue
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			keys = append(keys, name)
		}
	}
	walk(reflect.TypeOf(v))
	return keys
}
