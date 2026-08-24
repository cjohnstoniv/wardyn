// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
)

// TestSourceStamp is the #10 regression for WARDYN_AUDIT_SOURCE: when
// sinks.Source is set, every sink-serialized event gains a top-level
// "source" field with that value; when unset (the default), the payload is
// byte-identical to before the field existed — no "source" key at all.
//
// Not t.Parallel(): sinks.Source is process-global (set once at boot in
// real wardynd), so this test owns it exclusively and restores it after.
func TestSourceStamp(t *testing.T) {
	orig := sinks.Source
	t.Cleanup(func() { sinks.Source = orig })

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	s, err := sinks.NewFileSink(sinks.FileConfig{Path: path, MaxBytes: 1024 * 1024, Keep: 1})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sinks.Source = ""
	if err := s.Emit(context.Background(), makeEvent("stamp.off")); err != nil {
		t.Fatalf("Emit (unset Source): %v", err)
	}

	sinks.Source = "prod-us-east"
	if err := s.Emit(context.Background(), makeEvent("stamp.on")); err != nil {
		t.Fatalf("Emit (set Source): %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if _, has := lines[0]["source"]; has {
		t.Errorf("unset Source: payload has a \"source\" key, want none: %+v", lines[0])
	}
	if got := lines[1]["source"]; got != "prod-us-east" {
		t.Errorf("set Source: payload[\"source\"] = %v, want %q", got, "prod-us-east")
	}
}
