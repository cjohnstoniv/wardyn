// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDeviceOrigin_CarriesNoSalt: the provenance folded into a federated row's data names the claimed format
// of a format 2 claim and never its salt; a format 1 claim keeps the shape it always had.
func TestDeviceOrigin_CarriesNoSalt(t *testing.T) {
	salt := strings.Repeat("cd", 32)
	row := types.FederatedAuditRow{HashFormat: 2, HashSalt: salt}
	row.ID, row.Seq, row.RowHash, row.Data = uuid.New(), 4, strings.Repeat("a", 64), json.RawMessage(`{"k": 1}`)
	got, err := mergeDeviceOrigin(uuid.New(), row, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), salt) || strings.Contains(string(got), "salt") {
		t.Fatalf("device_origin carries the salt: %s", got)
	}
	var m struct {
		Origin map[string]json.RawMessage `json:"device_origin"`
	}
	if err := json.Unmarshal(got, &m); err != nil || string(m.Origin["hash_format"]) != "2" {
		t.Fatalf("device_origin = %s (%v), want hash_format 2", got, err)
	}
	row.HashFormat, row.HashSalt = 0, ""
	got, err = mergeDeviceOrigin(uuid.New(), row, "")
	if err != nil || strings.Contains(string(got), "hash_format") {
		t.Fatalf("a format 1 claim's device_origin = %s (%v), want no hash_format", got, err)
	}
}

// TestFederatedRowProblem_FormatAndSalt: a format outside 1 and 2, a format 2 row without a 64-hex salt and a
// salt on a format 1 row are refused; a good format 2 row, a format 1 row and a format-less row are not.
func TestFederatedRowProblem_FormatAndSalt(t *testing.T) {
	good := strings.Repeat("0123456789abcdef", 4)
	for name, c := range map[string]struct {
		format int
		salt   string
		bad    bool
	}{
		"format 3":              {3, good, true},
		"format -1":             {-1, "", true},
		"format 2, 63 chars":    {2, good[:63], true},
		"format 2, upper case":  {2, strings.ToUpper(good), true},
		"format 2, no salt":     {2, "", true},
		"salt on a format 1":    {1, good, true},
		"salt on a format-less": {0, good, true},
		"good format 2":         {2, good, false},
		"format 1":              {1, "", false},
		"format-less":           {0, "", false},
	} {
		r := types.FederatedAuditRow{HashFormat: c.format, HashSalt: c.salt}
		p := FederatedRowProblem(r)
		if (p != "") != c.bad {
			t.Errorf("%s: problem %q, want refused=%v", name, p, c.bad)
		}
		if c.salt != "" && strings.Contains(p, c.salt) {
			t.Errorf("%s: the refusal names the salt: %q", name, p)
		}
	}
}
