// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"reflect"
	"strings"
)

// declaredJSONKeys returns the top-level JSON keys encoding/json can write for
// v's struct type: the keys THIS binary owns in a stored JSONB document. A
// write merges as (old - declared) || new, so a key a NEWER binary wrote
// survives an older binary's save — critical for governance limits, where an
// absent limit means no limit and a silent drop would be a widening. Only the
// top level is preserved; a nested unknown key is still dropped.
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
