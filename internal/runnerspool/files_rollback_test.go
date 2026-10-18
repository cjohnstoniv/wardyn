// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A spool a later runner wrote, with keys this release does not know in its manifest, still opens
// after a rollback and keeps its records.
func TestSpoolALaterRunnerWroteStillOpens(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	if err := s.Append(ctx, decision(id)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
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
	reopened, err := Open(dir, id)
	if err != nil {
		t.Fatalf("spool with an unknown manifest key refused: %v", err)
	}
	defer reopened.Close()
	n := 0
	if err := reopened.Replay(ctx, each(func(Record) error { n++; return nil })); err != nil || n != 1 {
		t.Fatalf("replayed %d records, err=%v", n, err)
	}
}
