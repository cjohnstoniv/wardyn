// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneridentity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// An identity a later runner wrote, with a key this release does not know, still loads after a
// rollback.
func TestIdentityALaterRunnerWroteStillLoads(t *testing.T) {
	dir, want := identityFixture(t)
	path := filepath.Join(dir, "runner.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	top["from_a_later_runner"] = json.RawMessage(`true`)
	later, _ := json.Marshal(top)
	if err := os.WriteFile(path, later, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "https://org.example.com")
	if err != nil || got.RunnerID != want.RunnerID {
		t.Fatalf("identity with an unknown key refused: %v", err)
	}
}
