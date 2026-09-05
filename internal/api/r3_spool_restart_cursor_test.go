// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSpoolRestartReplaysOnlyTheInFlightBatch is F219.
//
// Drain retires work by advancing a byte cursor and only REWRITES the file when
// the reclaim pays for itself (at least half), so between compactions the cursor
// is the only record of what already reached the store. It lived in memory, so a
// restart replayed every line back to the last compaction — and the compaction
// rule bounds that at ~50% of the spool, i.e. a 64k-event backlog can produce
// ~32k duplicate audit_events rows on ONE restart.
//
// Drain's own doc states the bound as "a crash between rec.Record succeeding and
// the on-disk trim" — the in-flight batch. This is that sentence, asserted.
func TestSpoolRestartReplaysOnlyTheInFlightBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	const total, batch = 400, 100
	for i := 0; i < total; i++ {
		if err := sp.Append(newTestEvent("run.create")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	rec := &fakeRecorder{}
	n, err := sp.Drain(context.Background(), rec, batch)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if n != batch {
		t.Fatalf("first pass replayed %d, want %d", n, batch)
	}

	// THE RESTART: a brand-new AuditSpool over the same files, which is exactly
	// what wardynd does at boot (cmd/wardynd's api.NewAuditSpool(spoolPath)).
	reopened, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, derr := reopened.Drain(context.Background(), rec, batch)
		if derr != nil {
			t.Fatalf("post-restart pass %d: %v", i, derr)
		}
		if got == 0 {
			break
		}
	}

	if got := rec.count(); got != total {
		t.Errorf("the store recorded %d events for a spool of %d — %d DUPLICATES. A restart must replay only the "+
			"lines the store has not accepted (Drain's own stated bound is the in-flight batch), not everything "+
			"back to the last compaction", got, total, got-total)
	}
	// And the spool really did drain: a test where the cursor skipped past
	// un-replayed lines would pass the count above by LOSING events, which is
	// the one direction this file never errs in.
	if got := reopened.Lines(); got != 0 {
		t.Errorf("spool still holds %d un-replayed lines after draining to exhaustion", got)
	}
}

// TestSpoolCursorRefusesAnOutOfRangeSidecar: a cursor past the end of the file
// describes a file that no longer exists — compacted, truncated or replaced
// while the sidecar was stale — and honouring it would SKIP un-replayed events.
// It must read as 0: replay from the start, at-least-once.
func TestSpoolCursorRefusesAnOutOfRangeSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := sp.Append(newTestEvent("run.create")); err != nil {
			t.Fatal(err)
		}
	}
	// A cursor from a much larger, since-compacted file.
	if err := os.WriteFile(path+".consumed", []byte("999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecorder{}
	if _, err := reopened.Drain(context.Background(), rec, 100); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(); got != 3 {
		t.Errorf("replayed %d of 3 events after a stale, out-of-range cursor — an unusable cursor must mean "+
			"'replay from the start', never 'skip ahead'", got)
	}

	for _, junk := range []string{"", "not-a-number", "-5"} {
		if got := seedSpoolCursor(filepath.Join(dir, "x"), 100); got != 0 {
			t.Errorf("a missing cursor seeded %d, want 0", got)
		}
		p := filepath.Join(dir, "junk.consumed")
		if err := os.WriteFile(p, []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := seedSpoolCursor(p, 100); got != 0 {
			t.Errorf("cursor %q seeded %d, want 0", junk, got)
		}
	}
}
