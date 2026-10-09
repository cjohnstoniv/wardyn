// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"crypto/sha256"
	"reflect"
)

// leafHashes is a set of SHA-256 digests. A delivered runner_resident value is
// kept only as its digest (and its whitespace-trimmed digest, for a file that
// gained a newline), so the org holds no plaintext copy beyond the call.
type leafHashes map[[sha256.Size]byte]struct{}

func (h leafHashes) add(v []byte) {
	h[sha256.Sum256(v)] = struct{}{}
	h[sha256.Sum256(bytes.TrimSpace(v))] = struct{}{}
}

func (h leafHashes) has(leaf []byte) bool {
	if len(leaf) == 0 {
		return false
	}
	_, ok := h[sha256.Sum256(leaf)]
	if !ok {
		_, ok = h[sha256.Sum256(bytes.TrimSpace(leaf))]
	}
	return ok
}

// walkLeaves calls fn for every string, []byte and map value (and string map
// key) reachable from v through exported, marshalled fields, and reports
// whether fn returned true for one. It sees what encoding/json would write, in
// whatever encoding json would give it: a []byte is compared as raw bytes, not
// as the base64 it marshals to.
func walkLeaves(v reflect.Value, fn func([]byte) bool) bool { return walk(v, 0, fn) }

// maxWalkDepth bounds the recursion. SandboxSpec's type graph is far shallower
// and has no cycle, so the cap is insurance; past it the walk reports a hit,
// failing closed: a value that cannot be inspected is not sent.
const maxWalkDepth = 32

func walk(v reflect.Value, depth int, fn func([]byte) bool) bool {
	if depth > maxWalkDepth {
		return true
	}
	switch v.Kind() {
	case reflect.String:
		return fn([]byte(v.String()))
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && walk(v.Elem(), depth+1, fn)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return fn(v.Bytes())
		}
		fallthrough
	case reflect.Array:
		for i := range v.Len() {
			if walk(v.Index(i), depth+1, fn) {
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if walk(it.Key(), depth+1, fn) || walk(it.Value(), depth+1, fn) {
				return true
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() || f.Tag.Get("json") == "-" {
				continue
			}
			if walk(v.Field(i), depth+1, fn) {
				return true
			}
		}
	}
	return false
}
