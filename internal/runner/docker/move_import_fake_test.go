// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// importStream is a movepack stream of the root /work holding one file of n bytes.
func importStream(t *testing.T, n int) (*bytes.Buffer, runner.CarriedSet) {
	t.Helper()
	set := runner.CarriedSet{Roots: []string{"/work"}}
	var buf bytes.Buffer
	w, err := movepack.NewWriter(&buf, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(movepack.Header{Path: "work", Type: movepack.TypeDir, Mode: 0o755}, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(movepack.Header{Path: "work/f", Type: movepack.TypeFile, Mode: 0o644, Size: int64(n)}, strings.NewReader(strings.Repeat("x", n))); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, set
}

// importSpec is testSpec without the proxy config an import refuses.
func importSpec() runner.SandboxSpec {
	s := testSpec()
	s.ProxyConfig = runner.ProxyConfig{}
	return s
}

// TestMoveImport_ReadsWithTheMoveLimit: the import reads with its move's own bound, not the reader's default:
// a stream past it is refused and the sandbox it created is removed, and a missing or zero bound is refused
// before anything is created.
func TestMoveImport_ReadsWithTheMoveLimit(t *testing.T) {
	ctx := context.Background()
	for _, limit := range []int64{0, -1} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		stream, set := importStream(t, 10)
		_, _, err := newTestDriver(f).ImportSandbox(ctx, importSpec(), set, limit, stream)
		if !errors.Is(err, runner.ErrMoveCreate) || len(f.containers) != 0 {
			t.Errorf("import with bound %d = %v, %d containers; want ErrMoveCreate and nothing created", limit, err, len(f.containers))
		}
	}
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	stream, set := importStream(t, 64<<10)
	_, _, err := newTestDriver(f).ImportSandbox(ctx, importSpec(), set, 16<<10, stream)
	if !errors.Is(err, movepack.ErrCarriedTooLarge) || !errors.Is(err, runner.ErrMoveFiles) {
		t.Fatalf("import of 64 KiB under a 16 KiB bound = %v; want ErrCarriedTooLarge at the files stage", err)
	}
	if c := f.containers[agentContainerName(importSpec().RunID)]; c != nil && !c.removed {
		t.Error("the refused import's container was kept")
	}
	stream, set = importStream(t, 10)
	if _, _, err := newTestDriver(newImportFake()).ImportSandbox(ctx, importSpec(), set, 16<<10, stream); err != nil {
		t.Fatalf("import within its bound = %v; want nil", err)
	}
}

func newImportFake() *fakeDocker {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	return f
}
