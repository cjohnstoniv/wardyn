// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// DiffWorkloadDocuments lists the fields proposed changes from stored, as RFC 6901 paths over the
// document's JSON, in key order. A nil stored is a workload that does not exist yet: every field of
// proposed is an add. The result is never nil.
func DiffWorkloadDocuments(stored *WorkloadDocument, proposed WorkloadDocument) []WorkloadChange {
	changes := []WorkloadChange{}
	to := workloadJSON(proposed)
	var from any
	if stored != nil {
		from = workloadJSON(*stored)
	} else {
		from = map[string]any{"api_version": to["api_version"], "kind": to["kind"], "name": to["name"], "intent": map[string]any{}}
	}
	diffJSON(&changes, "", from, to)
	return changes
}

// workloadJSON is d as decoded JSON, numbers kept as written.
func workloadJSON(d WorkloadDocument) map[string]any {
	raw, err := json.Marshal(d)
	if err != nil {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return map[string]any{}
	}
	return out
}

func diffJSON(out *[]WorkloadChange, path string, from, to any) {
	fm, fok := from.(map[string]any)
	tm, tok := to.(map[string]any)
	if fok && tok {
		keys := slices.Sorted(maps.Keys(fm))
		for _, k := range slices.Sorted(maps.Keys(tm)) {
			if _, seen := fm[k]; !seen {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			p := path + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
			f, inFrom := fm[k]
			t, inTo := tm[k]
			switch {
			case !inFrom:
				*out = append(*out, WorkloadChange{Path: p, Op: "add", To: jsonRaw(t)})
			case !inTo:
				*out = append(*out, WorkloadChange{Path: p, Op: "remove", From: jsonRaw(f)})
			default:
				diffJSON(out, p, f, t)
			}
		}
		return
	}
	if !reflect.DeepEqual(from, to) {
		*out = append(*out, WorkloadChange{Path: path, Op: "change", From: jsonRaw(from), To: jsonRaw(to)})
	}
}

func jsonRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}
