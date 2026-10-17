// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// A runner rolled back to this release keeps the state a later release wrote: keys it does not
// know, at the top and inside a run, are ignored, while trailing data is still refused.
func TestStateALaterRunnerWroteStillLoads(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.dir, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var runs map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["runs"], &runs); err != nil {
		t.Fatal(err)
	}
	runs[id.String()]["from_a_later_runner"] = json.RawMessage(`true`)
	top["runs"], _ = json.Marshal(runs)
	top["from_a_later_runner"] = json.RawMessage(`{"kept":1}`)
	later, _ := json.Marshal(top)
	if err := os.WriteFile(path, later, 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatalf("state with unknown keys refused: %v", err)
	}
	if restarted.state.Runs[id].Ref != id.String() {
		t.Fatal("run lost on reload")
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(later, []byte("{}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if again, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase); err == nil {
		again.Close()
		t.Fatal("trailing state accepted")
	}
}
