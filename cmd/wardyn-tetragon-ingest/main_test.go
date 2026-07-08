// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
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

// TestTailExport_ReadsRotatedFileFromStart covers ITEM 27: on log rotation the
// tailer reopened the NEW file and seeked to END (io.SeekEnd), silently
// dropping every ground-truth event written to the new file before the rotation
// was noticed — a security-signal loss. A rotation reopen must start at offset 0
// so the new file's beginning (unread data) is read.
//
// Red-first: against the pre-fix SeekEnd-on-reopen behavior the post-rotation
// event never reaches the sink, so the final assertion fails.
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
