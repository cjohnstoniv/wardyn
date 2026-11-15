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
	"runtime"
	"strings"
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

// actionsIn returns the Action of every event in a rotated log file, or nil if
// the file does not exist.
func actionsIn(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var ev types.AuditEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("unmarshal a line of %s: %v", path, err)
		}
		out = append(out, ev.Action)
	}
	return out
}

// TestFileSink_RotationShiftsAndDropsOldest pins what the rotate loop is FOR,
// which neither test above reaches: that each generation shifts one slot older
// (.1 is the most recently rotated, .2 the one before it) and that the event past
// Keep is genuinely gone rather than lingering in a slot nothing overwrites.
//
// TestFileSink_KeepBound only asserts that <path>.Keep+1 does not exist — a
// slot the loop never creates in the first place, so it passes even with the
// removal deleted outright. This one reads the CONTENT of every slot.
func TestFileSink_RotationShiftsAndDropsOldest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	const keep = 2
	s, err := sinks.NewFileSink(sinks.FileConfig{Path: path, MaxBytes: 1, Keep: keep})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// MaxBytes=1 rotates before every write, so event i lands in the active
	// file and is pushed one slot older by each event after it.
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := s.Emit(ctx, makeEvent(fmt.Sprintf("shift.%d", i))); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}

	for _, c := range []struct {
		file string
		want string
	}{
		{path, "shift.3"},        // active
		{path + ".1", "shift.2"}, // most recently rotated
		{path + ".2", "shift.1"}, // one older
	} {
		got := actionsIn(t, c.file)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s holds %v, want exactly [%s]", filepath.Base(c.file), got, c.want)
		}
	}

	// Past Keep: no slot, and the event that fell off the end is gone from
	// every file rather than stranded in one nothing overwrites.
	if got := actionsIn(t, fmt.Sprintf("%s.%d", path, keep+1)); got != nil {
		t.Errorf("slot beyond Keep exists and holds %v", got)
	}
	for _, f := range []string{path, path + ".1", path + ".2"} {
		for _, a := range actionsIn(t, f) {
			if a == "shift.0" {
				t.Errorf("%s still holds shift.0, which should have fallen past Keep=%d", filepath.Base(f), keep)
			}
		}
	}
}

// A rotation that cannot move the old files out of the way used to be ignored:
// the active file was reopened and the event appended past MaxBytes, reported as
// written. A non-empty directory in the .Keep slot fails its removal whatever
// the user, root included. The event is refused, a smaller one that still fits
// is appended to the reopened file, and the next rotation after the slot is
// cleared works.
func TestFileSink_FailedRotationIsReportedAndRetried(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	blocker := path + ".2"
	if err := os.MkdirAll(filepath.Join(blocker, "in-the-way"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := sinks.NewFileSink(sinks.FileConfig{Path: path, MaxBytes: 1024, Keep: 2})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	big := strings.Repeat("b", 1000)
	if err := s.Emit(ctx, makeEvent("first")); err != nil {
		t.Fatalf("Emit first: %v", err)
	}
	err = s.Emit(ctx, makeEvent(big))
	if err == nil || !strings.Contains(err.Error(), "rotate") {
		t.Fatalf("Emit past MaxBytes with the .2 slot blocked = %v, want a rotate error", err)
	}
	if err := s.Emit(ctx, makeEvent("small")); err != nil {
		t.Fatalf("Emit of an event that still fits after the failed rotation: %v", err)
	}
	if got := actionsIn(t, path); len(got) != 2 || got[0] != "first" || got[1] != "small" {
		t.Fatalf("active file holds %v, want [first small]: the refused event must not be appended", got)
	}

	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(ctx, makeEvent(big)); err != nil {
		t.Fatalf("Emit once the slot is clear: %v", err)
	}
	if got := actionsIn(t, path+".1"); len(got) != 2 || got[0] != "first" || got[1] != "small" {
		t.Errorf("audit.log.1 holds %v, want [first small]", got)
	}
	if got := actionsIn(t, path); len(got) != 1 || got[0] != big {
		t.Errorf("active file holds %d events, want only the retried one", len(got))
	}
}

// A log file that is writable in a directory that is not: renaming it to .1
// fails. Root ignores the mode, and Windows has no such directory mode, so both
// skip.
func TestFileSink_RenameFailureIsReported(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop a rename here")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	s, err := sinks.NewFileSink(sinks.FileConfig{Path: path, MaxBytes: 1, Keep: 1})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err = s.Emit(context.Background(), makeEvent("refused"))
	if err == nil || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("Emit with an unrenamable log = %v, want a rename error", err)
	}
	if got := actionsIn(t, path); got != nil {
		t.Errorf("active file holds %v, want nothing: the event must not be appended past MaxBytes", got)
	}
}
