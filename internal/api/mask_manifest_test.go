// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A writer whose guard turns false forwards nothing from then on, not even
// bytes it would have masked: it never emits what it cannot vouch for.
func TestLiveMaskWriter_GuardDropsChunksOnceTheRunIsUncovered(t *testing.T) {
	reg := secretmask.NewRegistry()
	runID := uuid.New()
	reg.Add(runID, []byte("registered-secret"))
	var dst bytes.Buffer
	covered := true
	w := &liveMaskWriter{reg: reg, runID: runID, dst: &dst, guard: func() bool { return covered }}

	if _, err := w.Write([]byte("a registered-secret b ")); err != nil {
		t.Fatal(err)
	}
	if got := dst.String(); !strings.Contains(got, "<secret-hidden>") || strings.Contains(got, "registered-secret") {
		t.Fatalf("a covered writer forwarded %q, want the value masked", got)
	}

	covered = false
	before := dst.Len()
	n, err := w.Write([]byte("an unmasked value the registry may not hold\n"))
	if err != nil || n == 0 {
		t.Fatalf("a dropped chunk must read as accepted so the runner keeps draining: n=%d err=%v", n, err)
	}
	if dst.Len() != before {
		t.Errorf("an uncovered writer forwarded %q", dst.String()[before:])
	}
}

// The upload reader ends with errMaskUncovered the moment its guard turns
// false, and never reads another chunk from the source.
func TestRecordingMaskReader_GuardEndsTheUpload(t *testing.T) {
	covered := true
	src := io.MultiReader(strings.NewReader("first chunk\n"), strings.NewReader("second chunk\n"))
	r := buildGuardedMaskingBody(src, secretmask.NewRegistry(), uuid.New(), func() bool { return covered })

	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "first chunk") {
		t.Fatalf("covered read = %q, %v", buf[:n], err)
	}
	covered = false
	if n, err = r.Read(buf); !errors.Is(err, errMaskUncovered) || n != 0 {
		t.Fatalf("uncovered read = %d, %v; want 0 and errMaskUncovered", n, err)
	}
}

// With no manifests configured nothing is gated: a nil Config.MaskManifests is
// the existing test and local behaviour.
func TestMaskCovered_NoManifestsGatesNothing(t *testing.T) {
	srv := New(Config{AdminToken: adminToken})
	if !srv.maskCovered(t.Context(), uuid.New()) {
		t.Error("a server keeping no manifests refused a run")
	}
	if srv.maskGuard(uuid.New()) != nil {
		t.Error("a server keeping no manifests built a guard")
	}
	if srv.openExecOutput(types.AgentRun{ID: uuid.New()}, false) == nil {
		t.Error("a server keeping no manifests refused the exec relay")
	}
}
