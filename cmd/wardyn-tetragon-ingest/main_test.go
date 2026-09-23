// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
)

// TestTailExport_ReadsRotatedFileFromStart pins rotation handling: a rotation
// reopen starts at offset 0 so the new file's beginning (unread data) is read.
// Reopening the new file and seeking to the end (io.SeekEnd) would silently drop
// every ground-truth event written to it before the rotation was noticed — a
// security-signal loss.
//
// With a SeekEnd-on-reopen tailer the post-rotation event never reaches the
// sink, so the final assertion fails.
func TestTailExport_ReadsRotatedFileFromStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tetragon.log")

	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newEventSink(srv.URL, "tok", 64, 8, 20*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})
	mapper := groundtruth.NewMapper(nil) // unmapped is fine; we only need ok=true

	// A long event on file A so its post-read offset exceeds file B's size —
	// that shrink is how rotated() detects the rotation (new size < our offset).
	const binA = "/usr/bin/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const binB = "/x-post-rotation-marker"
	lineA := `{"process_exec":{"process":{"binary":"` + binA + `"}}}` + "\n"
	lineB := `{"process_exec":{"process":{"binary":"` + binB + `"}}}` + "\n"

	bodyContains := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bodies {
			if strings.Contains(b, want) {
				return true
			}
		}
		return false
	}
	waitFor := func(cond func() bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal(msg)
	}

	// Empty file first: tailExport's INITIAL open seeks to END, so event A must
	// be appended AFTER the tailer is running to be read live.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailExport(ctx, path, mapper, sink)

	// Append event A to the live file until it is posted. Retrying absorbs the
	// initial-open race (the very first open legitimately seeks to END, so an
	// append that lands before the seek is skipped); a landed A also proves the
	// tailer is attached and advances its read offset past file B's size, which
	// is what makes rotated() fire on the shrink below.
	deadline := time.Now().Add(3 * time.Second)
	for !bodyContains(binA) && time.Now().Before(deadline) {
		appendLine(t, path, lineA)
		time.Sleep(20 * time.Millisecond)
	}
	if !bodyContains(binA) {
		t.Fatal("event A on the pre-rotation file was never read (tailer not attached)")
	}

	// Rotate: write a smaller new file and atomically rename it over path (the
	// rename-rotation pattern). New size < our current offset => rotated()==true.
	tmp := filepath.Join(dir, "tetragon.log.new")
	if err := os.WriteFile(tmp, []byte(lineB), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	// The event at the START of the rotated file must be read (offset 0), not
	// skipped by a seek-to-end.
	waitFor(func() bool { return bodyContains(binB) },
		"post-rotation event B was dropped: tailer seeked to END of the rotated file instead of START")
}

// TestTailExport_LogsOnUnopenableExportFile pins that openFile logs its
// os.Open error: a typo'd/unreadable -export path must not leave the sensor
// silently blind forever while heartbeats and the stats loop keep printing as
// if it were healthy.
//
// If openFile returns false silently, nothing in this window names the bad
// path, so the assertion below fails.
func TestTailExport_LogsOnUnopenableExportFile(t *testing.T) {
	dir := t.TempDir()
	// Parent directory does not exist, so every os.Open(path) fails for the
	// whole run — this is the "typo'd or unreadable path" from the finding.
	path := filepath.Join(dir, "does-not-exist", "tetragon.log")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	sink := newEventSink("http://127.0.0.1:1", "tok", 8, 8, 20*time.Millisecond, http.DefaultClient)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})
	mapper := groundtruth.NewMapper(nil)

	// tailExport retries the open every second; run past that so at least one
	// failed attempt has had the chance to log.
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	tailExport(ctx, path, mapper, sink)

	out := buf.String()
	if !strings.Contains(out, path) {
		t.Fatalf("expected a log line naming the unopenable export path %q; got:\n%s", path, out)
	}
	if !strings.Contains(strings.ToLower(out), "cannot open") {
		t.Fatalf("expected a log line describing the open failure; got:\n%s", out)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

// TestTailExport_ReassemblesLineSplitAcrossEOF pins that a Tetragon JSON line
// whose bytes straddle an EOF read boundary (the writer flushes it in two
// syscalls) is reassembled, not split into two undecodable fragments that both
// drop. Processing the first half on io.EOF fails the JSON parse (dropped), and
// the later-arriving remainder then fails as its own fragment (also dropped), so
// the event would be silently lost from the tamper-proof stream.
func TestTailExport_ReassemblesLineSplitAcrossEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tetragon.log")

	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newEventSink(srv.URL, "tok", 64, 8, 20*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})
	mapper := groundtruth.NewMapper(nil) // unmapped is fine; we only need ok=true

	bodyContains := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bodies {
			if strings.Contains(b, want) {
				return true
			}
		}
		return false
	}

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailExport(ctx, path, mapper, sink)

	// Prime: land one COMPLETE line so the tailer is provably attached (its
	// initial open seeks to END) and its read offset sits at EOF with an empty
	// pending buffer. Retry absorbs the initial-open race.
	const primeBin = "/usr/bin/prime-marker"
	primeLine := `{"process_exec":{"process":{"binary":"` + primeBin + `"}}}` + "\n"
	deadline := time.Now().Add(3 * time.Second)
	for !bodyContains(primeBin) && time.Now().Before(deadline) {
		appendLine(t, path, primeLine)
		time.Sleep(20 * time.Millisecond)
	}
	if !bodyContains(primeBin) {
		t.Fatal("prime event never read (tailer not attached)")
	}

	// Write ONE line in two halves straddling an EOF poll: append the first half
	// with NO newline, wait past the 250ms EOF poll so the tailer definitely
	// consumes it at EOF, then append the remainder + newline.
	const stradBin = "/x-straddle-marker"
	full := `{"process_exec":{"process":{"binary":"` + stradBin + `"}}}`
	half := len(full) / 2
	appendLine(t, path, full[:half])
	time.Sleep(300 * time.Millisecond)
	appendLine(t, path, full[half:]+"\n")

	waitDeadline := time.Now().Add(3 * time.Second)
	for !bodyContains(stradBin) && time.Now().Before(waitDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !bodyContains(stradBin) {
		t.Fatal("straddling event dropped: a line split across an EOF boundary was not reassembled")
	}
}

// TestTailExport_CapsPendingLineAt1MiB pins the bound on pending: an
// unterminated export line must not grow tailExport's own memory without
// limit when a stretch of writes carries no '\n' (a corrupted write, or
// Tetragon itself emitting one abnormally long record) — the tail loop
// cannot refuse to read the file it is handed.
// maxPendingExportLine caps it at 1 MiB: past the cap the accumulated bytes
// are dropped (counted on the sink, the same "lost event, never silent"
// contract as every other drop this sidecar already counts) and pending
// resets to empty, so a well-formed line written afterward is still read on
// its own instead of being appended onto — and permanently lost behind — an
// unbounded backlog.
func TestTailExport_CapsPendingLineAt1MiB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tetragon.log")

	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newEventSink(srv.URL, "tok", 64, 8, 20*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})
	mapper := groundtruth.NewMapper(nil) // unmapped is fine; we only need ok=true

	bodyContains := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bodies {
			if strings.Contains(b, want) {
				return true
			}
		}
		return false
	}

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailExport(ctx, path, mapper, sink)

	// Prime: land one COMPLETE line so the tailer is provably attached past
	// its initial SeekEnd. Retry absorbs the initial-open race.
	const primeBin = "/usr/bin/prime-marker"
	primeLine := `{"process_exec":{"process":{"binary":"` + primeBin + `"}}}` + "\n"
	deadline := time.Now().Add(3 * time.Second)
	for !bodyContains(primeBin) && time.Now().Before(deadline) {
		appendLine(t, path, primeLine)
		time.Sleep(20 * time.Millisecond)
	}
	if !bodyContains(primeBin) {
		t.Fatal("prime event never read (tailer not attached)")
	}

	before := sink.droppedCount()

	// Feed 2 MiB with NO newline — past the 1 MiB cap, with the buffer never
	// terminated.
	appendLine(t, path, strings.Repeat("x", 2<<20))

	dropDeadline := time.Now().Add(3 * time.Second)
	for sink.droppedCount() == before && time.Now().Before(dropDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := sink.droppedCount(); got == before {
		t.Fatalf("droppedCount did not move after a 2 MiB no-newline feed (still %d) — pending grew past the cap with nothing counting it", got)
	}

	// The buffer must have RESET, not merely stopped growing — a well-formed
	// line written afterward has to be read eventually, not appended onto —
	// and lost behind — the dropped backlog forever.
	//
	// R-06: this 2 MiB feed lands in ONE single io.EOF read (never returns
	// mid-line), so tailExport cannot tell it apart from a line that is still
	// STREAMING — the far more likely reason a real line would ever hit this
	// cap — and resyncing (see main.go) treats the first newline-terminated
	// content after ANY drop as that (possibly-imagined) line's remaining
	// tail, discarding it rather than risk re-splicing a genuinely torn
	// write's second half onto a fresh line. A sacrificial write absorbs
	// that presumed tail; the SECOND write is the one actually proven read.
	sacrificial := `{"sacrificial":"absorbs-the-presumed-resync-tail"}` + "\n"
	appendLine(t, path, sacrificial)

	const afterBin = "/x-after-cap-marker"
	afterLine := `{"process_exec":{"process":{"binary":"` + afterBin + `"}}}` + "\n"
	appendLine(t, path, afterLine)

	waitDeadline := time.Now().Add(3 * time.Second)
	for !bodyContains(afterBin) && time.Now().Before(waitDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !bodyContains(afterBin) {
		t.Fatal("a well-formed line written two writes after the oversized feed was never read — pending did not reset after the drop")
	}
}

// TestTailExport_ResyncsPastStillStreamingOversizedLine covers what
// TestTailExport_CapsPendingLineAt1MiB cannot: that test writes its whole
// 2 MiB feed in ONE syscall, so tailExport sees it all in a single io.EOF
// read and the drop's reset lands on a clean boundary — it cannot see the
// hazard the surrounding code comment names. A line that is still
// STREAMING when it trips the cap (a writer mid-write, so its remainder
// arrives over a LATER read) behaves differently: without resyncing, that
// remainder would be appended onto a fresh (post-drop) pending, and the
// first real '\n' after it would hand processLine a garbage fragment — the
// exact "split a line into two fragments" hazard pending itself exists to
// prevent — while a multi-MiB line would also cost one `dropped` increment
// per 1 MiB of itself instead of one per line.
func TestTailExport_ResyncsPastStillStreamingOversizedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tetragon.log")

	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newEventSink(srv.URL, "tok", 64, 8, 20*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})
	mapper := groundtruth.NewMapper(nil)

	bodyContains := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bodies {
			if strings.Contains(b, want) {
				return true
			}
		}
		return false
	}

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailExport(ctx, path, mapper, sink)

	const primeBin = "/usr/bin/prime-marker"
	primeLine := `{"process_exec":{"process":{"binary":"` + primeBin + `"}}}` + "\n"
	deadline := time.Now().Add(3 * time.Second)
	for !bodyContains(primeBin) && time.Now().Before(deadline) {
		appendLine(t, path, primeLine)
		time.Sleep(20 * time.Millisecond)
	}
	if !bodyContains(primeBin) {
		t.Fatal("prime event never read (tailer not attached)")
	}

	before := sink.droppedCount()

	// 1.5 MiB, NO newline: past the 1 MiB cap. Written alone and waited on
	// (below) so tailExport genuinely hits io.EOF on ITS OWN read call before
	// the second half exists on disk — a real torn-write, separate-syscalls
	// shape, not one lucky read that happens to swallow both halves at once
	// (writing them back to back with no wait risks exactly that: a bufio
	// Read loop that has not yet returned when the second write lands would
	// fold both halves into ONE chunk, ending at the real '\n' inside the
	// second half, and never exercise the multi-read resync path at all).
	appendLine(t, path, strings.Repeat("x", 3*(1<<20)/2))

	dropDeadline := time.Now().Add(3 * time.Second)
	for sink.droppedCount() == before && time.Now().Before(dropDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := sink.droppedCount(); got == before {
		t.Fatalf("droppedCount did not move after the 1.5 MiB no-newline feed (still %d)", got)
	}
	afterFirstDrop := sink.droppedCount()

	// The line's REMAINDER arrives now, on a SEPARATE write (a later,
	// independent read call): ANOTHER 1.5 MiB (itself over the cap on its
	// own, the way a genuinely huge line's continuation would be), completing
	// with a real '\n', immediately followed (same write — bufio already has
	// these bytes buffered together, no further read needed) by a well-formed
	// record. Without resyncing, this remainder is appended onto a fresh
	// (post-first-drop) pending and trips the cap A SECOND TIME — one
	// `dropped` increment per 1 MiB-sized chunk of what is really ONE
	// oversized line, exactly the "⌈N⌉ dropped increments, not one" defect
	// R-06 names. With resyncing (which — unlike a quiet-poll timeout — never
	// auto-clears on its own; only a real '\n' clears it, so this wait is
	// safe no matter how long it takes), the whole remainder is discarded
	// without ever being appended to pending or re-checked against the cap —
	// exactly one increment for the whole line — and only the well-formed
	// line after it is read.
	const interleaveBin = "/x-interleave-marker"
	remainder := strings.Repeat("y", 3*(1<<20)/2) + "\n"
	wellFormed := `{"process_exec":{"process":{"binary":"` + interleaveBin + `"}}}` + "\n"
	appendLine(t, path, remainder+wellFormed)

	waitDeadline := time.Now().Add(3 * time.Second)
	for !bodyContains(interleaveBin) && time.Now().Before(waitDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !bodyContains(interleaveBin) {
		t.Fatal("the well-formed line immediately after the streaming line's remainder was never read")
	}
	if got := sink.droppedCount(); got != afterFirstDrop {
		t.Fatalf("droppedCount moved from %d to %d processing the SAME oversized line's still-streaming remainder — want exactly one drop for the whole line, not one per cap-sized chunk (R-06)", afterFirstDrop, got)
	}
}
