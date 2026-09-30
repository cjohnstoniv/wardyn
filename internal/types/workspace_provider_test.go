// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestADOTokenModesAreClosed pins the three modes and their stable order.
func TestADOTokenModesAreClosed(t *testing.T) {
	want := []string{"bearer", "minted_pat", "own_pat"}
	if got := ClosedADOTokenModeList(); !slices.Equal(got, want) {
		t.Fatalf("ClosedADOTokenModeList() = %v, want %v", got, want)
	}
	for _, m := range []ADOTokenMode{ADOTokenModeBearer, ADOTokenModeMintedPAT, ADOTokenModeOwnPAT} {
		if !m.Valid() {
			t.Errorf("%q.Valid() = false, want true", m)
		}
	}
	if ADOTokenMode("oauth").Valid() || ADOTokenMode("").Valid() {
		t.Error("an invented or empty mode reads as valid; the empty mode is read as bearer by the caller, not by Valid")
	}
}

// TestADOEntraConfigPATLifetimes: 0 reads as the default, a set value is kept,
// a nil block reads as the default, and the two fields keep their wire names.
func TestADOEntraConfigPATLifetimes(t *testing.T) {
	var nilCfg *ADOEntraConfig
	if nilCfg.PATHours() != 8 || nilCfg.PATDays() != 30 {
		t.Errorf("nil config = %d h / %d d, want 8 / 30", nilCfg.PATHours(), nilCfg.PATDays())
	}
	if c := (&ADOEntraConfig{}); c.PATHours() != 8 || c.PATDays() != 30 {
		t.Errorf("unset config = %d h / %d d, want 8 / 30", c.PATHours(), c.PATDays())
	}
	if c := (&ADOEntraConfig{PATMaxHours: 24, PATMaxDays: 7}); c.PATHours() != 24 || c.PATDays() != 7 {
		t.Errorf("set config = %d h / %d d, want 24 / 7", c.PATHours(), c.PATDays())
	}
	raw, err := json.Marshal(ADOEntraConfig{PATMaxHours: 24, PATMaxDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["pat_max_hours"] != float64(24) || m["pat_max_days"] != float64(7) {
		t.Errorf("wire form = %s, want pat_max_hours 24 and pat_max_days 7", raw)
	}
	if raw, _ := json.Marshal(ADOEntraConfig{}); string(raw) != `{"tenant_id":"","client_id":""}` {
		t.Errorf("an unset config marshals as %s, want no pat_* fields", raw)
	}
}
