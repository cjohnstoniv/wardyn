// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestBuildTracker_LogRingBounded pins maxBuildLogLines: a build that logs
// more than the cap keeps only the most recent lines, oldest dropped first.
func TestBuildTracker_LogRingBounded(t *testing.T) {
	var tr buildTracker
	id := uuid.New()
	tr.begin(id, time.Now())
	for i := 0; i < maxBuildLogLines+10; i++ {
		tr.appendLog(id, "line")
	}
	got := tr.get(id)
	if len(got.Log) != maxBuildLogLines {
		t.Fatalf("Log length = %d, want %d (bounded)", len(got.Log), maxBuildLogLines)
	}
}

// TestBuildTracker_FinishCarriesLogForward pins that the accumulated log
// survives the Building=true -> terminal transition — the log IS the
// debugging story for a failed build, so finish() must not discard it.
func TestBuildTracker_FinishCarriesLogForward(t *testing.T) {
	var tr buildTracker
	id := uuid.New()
	tr.begin(id, time.Now())
	tr.appendLog(id, "step 1")
	tr.appendLog(id, "step 2 failed")
	tr.finish(id, "", "build failed with exit code 1")

	got := tr.get(id)
	if got.Building {
		t.Fatal("finish() left Building=true")
	}
	if len(got.Log) != 2 || got.Log[0] != "step 1" || got.Log[1] != "step 2 failed" {
		t.Fatalf("Log after finish() = %v, want [step 1, step 2 failed]", got.Log)
	}
}

// TestBuildTracker_BeginResetsLog pins that a retried build starts with a
// clean pane rather than appending onto the replaced attempt's tail.
func TestBuildTracker_BeginResetsLog(t *testing.T) {
	var tr buildTracker
	id := uuid.New()
	tr.begin(id, time.Now())
	tr.appendLog(id, "attempt 1 line")
	tr.finish(id, "", "failed")

	tr.begin(id, time.Now()) // retry
	got := tr.get(id)
	if len(got.Log) != 0 {
		t.Fatalf("Log after retry begin() = %v, want empty", got.Log)
	}
}

// TestBuildTracker_GetSnapshotDoesNotAliasTracker pins the defensive copy in
// get(): a snapshot returned to a caller must not change underfoot when the
// tracker keeps appending under its own lock.
func TestBuildTracker_GetSnapshotDoesNotAliasTracker(t *testing.T) {
	var tr buildTracker
	id := uuid.New()
	tr.begin(id, time.Now())
	tr.appendLog(id, "first")
	snap := tr.get(id)
	tr.appendLog(id, "second")
	if len(snap.Log) != 1 || snap.Log[0] != "first" {
		t.Fatalf("snapshot mutated after a later appendLog: %v", snap.Log)
	}
}

// TestBuildLogWriter_SplitsCompleteLinesOnly pins buildLogWriter's line
// buffering: a chunk ending mid-line holds the partial line back until a
// later Write completes it, and blank lines are dropped.
func TestBuildLogWriter_SplitsCompleteLinesOnly(t *testing.T) {
	var tr buildTracker
	id := uuid.New()
	tr.begin(id, time.Now())
	w := &buildLogWriter{t: &tr, id: id}

	if _, err := w.Write([]byte("hello\nwor")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tr.get(id).Log; len(got) != 1 || got[0] != "hello" {
		t.Fatalf("Log after partial write = %v, want [hello] (the partial 'wor' line held back)", got)
	}
	if _, err := w.Write([]byte("ld\n\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tr.get(id).Log; len(got) != 2 || got[1] != "world" {
		t.Fatalf("Log after completing the line = %v, want [hello world] (blank line dropped)", got)
	}
}
