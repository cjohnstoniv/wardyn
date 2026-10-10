// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// A run always HAS a placement, so `placement` is on the wire even when nothing
// recorded one; the three fields that describe a filled local run are absent
// until they are true. A `placement` sent as `null`, or a `runner_id` sent as
// "", would read as a choice the run never made.
func TestAgentRun_PlacementWireShape(t *testing.T) {
	b, err := json.Marshal(AgentRun{ID: uuid.New()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := raw["placement"]; !ok || v != "" {
		t.Errorf("placement = %v (present=%v), want the empty string always on the wire", v, ok)
	}
	for _, k := range []string{"placement_filled", "runner_id", "evidence_source"} {
		if _, present := raw[k]; present {
			t.Errorf("%q is on the wire for a run with no placement recorded", k)
		}
	}

	runnerID := uuid.New()
	b, err = json.Marshal(AgentRun{ID: uuid.New(), Placement: PlacementLocal, PlacementFilled: true,
		RunnerID: &runnerID, EvidenceSource: RunEvidenceRunnerAsserted})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw = nil
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["placement"] != string(PlacementLocal) || raw["placement_filled"] != true ||
		raw["runner_id"] != runnerID.String() || raw["evidence_source"] != string(RunEvidenceRunnerAsserted) {
		t.Errorf("a local run marshalled as %v, want every placement field set", raw)
	}
}

// The closed value sets the migration's CHECKs accept, pinned here so a value
// added to a constant without the matching DDL (or the reverse) shows up as a
// test failure rather than as a run that cannot be stored.
func TestPlacementValueSets(t *testing.T) {
	if got := slices.Sorted(maps.Keys(map[string]Placement{
		string(PlacementRemote): PlacementRemote, string(PlacementLocal): PlacementLocal, "": "",
	})); !slices.Equal(got, []string{"", "local", "remote"}) {
		t.Errorf("placement values = %v, want the empty string plus local and remote", got)
	}
	if RunEvidenceSubstrate != "substrate" || RunEvidenceRunnerAsserted != "runner_asserted" {
		t.Error("evidence_source values are substrate and runner_asserted, and the empty string")
	}
}
