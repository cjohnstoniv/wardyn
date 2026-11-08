// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

const testSum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestFileDownloadScopeNameSizeHashOnly: a reviewed download's approval row shows its deciders the file's name,
// size and hash, and who asked; no field can carry the file's content.
func TestFileDownloadScopeNameSizeHashOnly(t *testing.T) {
	raw, err := json.Marshal(FileDownloadScope{Name: "report.csv", Bytes: 17, SHA256: testSum, RequestedBy: "sub-a"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(m))
	if !slices.Equal(keys, []string{"bytes", "name", "requested_by", "sha256"}) {
		t.Fatalf("scope keys = %v, want name, bytes, sha256 and requested_by only", keys)
	}
}

// TestParseFileDownloadScope: a stored scope reads back only when it is exactly that shape and well formed.
func TestParseFileDownloadScope(t *testing.T) {
	good := `{"name":"report.csv","bytes":17,"sha256":"` + testSum + `","requested_by":"sub-a"}`
	sc, ok := ParseFileDownloadScope(json.RawMessage(good))
	if !ok || sc.Name != "report.csv" || sc.Bytes != 17 || sc.SHA256 != testSum || sc.RequestedBy != "sub-a" {
		t.Fatalf("ParseFileDownloadScope(good) = %+v, %v", sc, ok)
	}
	for name, raw := range map[string]string{
		"extra key":      strings.Replace(good, `}`, `,"content":"x"}`, 1),
		"no name":        strings.Replace(good, `"report.csv"`, `""`, 1),
		"negative bytes": strings.Replace(good, `17`, `-1`, 1),
		"over the cap":   strings.Replace(good, `17`, `268435457`, 1),
		"short hash":     strings.Replace(good, testSum, "abc", 1),
		"upper hash":     strings.Replace(good, testSum, strings.ToUpper(testSum), 1),
		"no requester":   strings.Replace(good, `"sub-a"`, `""`, 1),
		"not an object":  `[]`,
	} {
		if _, ok := ParseFileDownloadScope(json.RawMessage(raw)); ok {
			t.Errorf("%s: parsed", name)
		}
	}
}
