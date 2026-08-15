// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
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

// TestBuildLogWriter_StripsANSIEscapes pins the M5 guard: color/cursor CSI
// codes a build tool emits must not land in the stored line verbatim — the
// pane renders plain text, not a terminal.
func TestBuildLogWriter_StripsANSIEscapes(t *testing.T) {
	var tr buildTracker
	id := uuid.New()
	tr.begin(id, time.Now())
	w := &buildLogWriter{t: &tr, id: id}

	if _, err := w.Write([]byte("\x1b[32mgreen text\x1b[0m\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := tr.get(id).Log
	if len(got) != 1 || got[0] != "green text" {
		t.Fatalf("Log = %v, want [green text] (ANSI/CSI escapes stripped)", got)
	}
}

// TestBuildLogWriter_ClampsOversizedLine pins M6: one absurdly long line must
// not bloat the ring or the /build response payload.
func TestBuildLogWriter_ClampsOversizedLine(t *testing.T) {
	var tr buildTracker
	id := uuid.New()
	tr.begin(id, time.Now())
	w := &buildLogWriter{t: &tr, id: id}

	long := strings.Repeat("x", maxBuildLogLineLen+100)
	if _, err := w.Write([]byte(long + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := tr.get(id).Log
	if len(got) != 1 {
		t.Fatalf("Log = %d lines, want exactly 1", len(got))
	}
	if len(got[0]) != maxBuildLogLineLen {
		t.Fatalf("stored line length = %d, want clamped to %d", len(got[0]), maxBuildLogLineLen)
	}
}

// TestResolveBuildView_ExplicitImageNeedsBuilder is the W7-S1-2 regression: a
// registry/byo base image is NOT "boots as-is" on a builder-less host —
// resolveWorkspaceImage wraps it with the agent runtime via FinalizeBase, the
// same wrap a devcontainer build needs, and runs_create.go's wsRefs door
// REFUSES the run outright when no builder is wired (never silently
// substitutes the convention image). Pre-fix, resolveBuildView reported
// "nothing_to_build ... boots as-is — no build involved" regardless of
// whether a builder existed — false on every bare-binary default and every
// Helm/k8s install that doesn't set WARDYN_ENVBUILD.
func TestResolveBuildView_ExplicitImageNeedsBuilder(t *testing.T) {
	ws := types.Workspace{BaseImage: &types.WorkspaceBaseImage{Kind: "byo", Image: "golang:1.26"}}

	// No builder wired: the honest "none" report, not the false "nothing_to_build".
	noBuilder := &Server{}
	view := noBuilder.resolveBuildView(ws)
	if view.State != "none" {
		t.Fatalf("state = %q, want %q (no builder wired — the base image is refused at run creation, not verbatim)", view.State, "none")
	}
	if !strings.Contains(view.Detail, "REFUSED") {
		t.Fatalf("detail = %q, want it to name that the run is REFUSED without a builder", view.Detail)
	}
	if view.Image != "golang:1.26" {
		t.Fatalf("image = %q, want the chosen base image surfaced even in the warning", view.Image)
	}

	// A wired builder DOES make it verbatim — additive: the wired case is
	// unaffected by the builder-less fix above.
	wired := &Server{}
	wired.cfg.ImageBuilder = fakeImageBuilder{}
	view = wired.resolveBuildView(ws)
	if view.State != "nothing_to_build" || view.Image != "golang:1.26" {
		t.Fatalf("wired-builder view = %+v, want nothing_to_build/golang:1.26", view)
	}
}

// TestBuildLogWriter_FlushesUnterminatedOverflow pins M6's other half: a
// chunk with NO newline at all (pathological or binary output) must not grow
// buildLogWriter.buf without bound — past maxBuildLogBufBytes it flushes as
// its own (clamped) line and resets, rather than accumulating forever.
func TestBuildLogWriter_FlushesUnterminatedOverflow(t *testing.T) {
	var tr buildTracker
	id := uuid.New()
	tr.begin(id, time.Now())
	w := &buildLogWriter{t: &tr, id: id}

	huge := strings.Repeat("y", maxBuildLogBufBytes+1)
	if _, err := w.Write([]byte(huge)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(w.buf) != 0 {
		t.Fatalf("buf len = %d after overflow flush, want reset to 0", len(w.buf))
	}
	got := tr.get(id).Log
	if len(got) != 1 || len(got[0]) != maxBuildLogLineLen {
		t.Fatalf("Log = %v, want exactly one clamped line", got)
	}
}
