// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Audit rows are immutable, so a row a 0.9 daemon wrote still carries "profile" where 0.10.0 writes
// "unit": the readers accept both spellings, and a legacy layer reads as a unit.
func TestLegacyAuditSpellingsStillRead(t *testing.T) {
	row := func(data string) types.AuditEvent {
		return types.AuditEvent{Action: "run.ceiling.reassert", Outcome: "success", Data: json.RawMessage(data)}
	}
	for _, c := range []struct{ name, data string }{
		{"0.9 row", `{"profile":"walled","max_ephemeral_disk_mib":512}`},
		{"0.10.0 row", `{"unit":"walled","max_ephemeral_disk_mib":512}`},
	} {
		ev := collectEvidence([]types.AuditEvent{row(c.data)}, nil)
		if ev.profile != "walled" || ev.profileMaxDisk != 512 {
			t.Errorf("%s: unit %q max disk %d, want walled 512", c.name, ev.profile, ev.profileMaxDisk)
		}
	}

	ref := &policyref.Ref{Source: policyref.SourceUnit, Name: "team"}
	got := knobProvenance{"allowed_domains": {Layer: "profile", Name: "team"}}.withContacts(ref, nil)
	if got["allowed_domains"].Layer != policyref.LayerUnit {
		t.Errorf("legacy layer read as %q, want %q", got["allowed_domains"].Layer, policyref.LayerUnit)
	}
}
