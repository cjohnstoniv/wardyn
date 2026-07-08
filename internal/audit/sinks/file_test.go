// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestFileSink_WritesJSONLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	s, err := sinks.NewFileSink(sinks.FileConfig{
		Path:     path,
		MaxBytes: 1 * 1024 * 1024, // 1 MiB — no rotation expected
		Keep:     3,
	})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	const n = 5
	for i := 0; i < n; i++ {
		ev := makeEvent(fmt.Sprintf("file.write.%d", i))
		if err := s.Emit(ctx, ev); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	var count int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev types.AuditEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Errorf("unmarshal line %d: %v", count, err)
		}
		count++
	}
	if count != n {
		t.Errorf("got %d lines, want %d", count, n)
	}
}

func TestFileSink_Rotation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	// MaxBytes is tiny so rotation happens after the first event.
	s, err := sinks.NewFileSink(sinks.FileConfig{
		Path:     path,
		MaxBytes: 1, // rotate after 1 byte
		Keep:     3,
	})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	// Emit 4 events: the active file + up to Keep=3 rotated files.
	for i := 0; i < 4; i++ {
		if err := s.Emit(ctx, makeEvent(fmt.Sprintf("rotate.%d", i))); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}

	// Active file must exist.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("active file missing: %v", err)
	}

	// At least one rotated file must exist.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("rotated file .1 missing: %v", err)
	}
}

func TestFileSink_KeepBound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	const keep = 2
	s, err := sinks.NewFileSink(sinks.FileConfig{
		Path:     path,
		MaxBytes: 1,
		Keep:     keep,
	})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	// Emit enough events to trigger more rotations than Keep.
	for i := 0; i < keep+5; i++ {
		if err := s.Emit(ctx, makeEvent(fmt.Sprintf("keep.%d", i))); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}

	// Files beyond Keep should not exist.
	tooOld := fmt.Sprintf("%s.%d", path, keep+1)
	if _, err := os.Stat(tooOld); err == nil {
		t.Errorf("file beyond Keep limit still exists: %s", tooOld)
	}
}
