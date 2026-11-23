// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
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

// TestMoveImport_MarkerBeforeLinks: the archive the daemon extracts writes the booted marker before every
// symlink, and the symlinks last, so no carried link can stand where the import writes a file.
func TestMoveImport_MarkerBeforeLinks(t *testing.T) {
	set := runner.CarriedSet{Roots: []string{"/work"}}
	var buf bytes.Buffer
	w, err := movepack.NewWriter(&buf, set)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []movepack.Header{
		{Path: "work", Type: movepack.TypeDir, Mode: 0o755},
		{Path: "work/a", Type: movepack.TypeSymlink, Mode: 0o777, Link: "/etc"},
		{Path: "work/b", Type: movepack.TypeDir, Mode: 0o755},
	} {
		if err := w.Add(h, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f := newImportFake()
	if _, _, err := newTestDriver(f).ImportSandbox(context.Background(), importSpec(), set, 1<<20, &buf); err != nil {
		t.Fatalf("ImportSandbox: %v", err)
	}
	if len(f.copies) != 1 {
		t.Fatalf("archive calls = %d; want 1", len(f.copies))
	}
	var names []string
	marker, firstLink := -1, -1
	tr := tar.NewReader(bytes.NewReader(f.copies[0].archive))
	for {
		th, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if th.Name == strings.TrimPrefix(bootedMarker, "/") {
			marker = len(names)
		}
		if th.Typeflag == tar.TypeSymlink && firstLink < 0 {
			firstLink = len(names)
		}
		if th.Typeflag != tar.TypeSymlink && firstLink >= 0 {
			t.Errorf("%s follows a symlink in the archive", th.Name)
		}
		names = append(names, th.Name)
	}
	if marker < 0 || firstLink < 0 || marker > firstLink {
		t.Errorf("archive order %q; want the marker before the symlink", names)
	}
}

// TestLastHost: a moved run's proxy takes its subnet's last host, below the broadcast address, whatever bits
// the prefix carries past its mask; a subnet smaller than a run network may be, or not IPv4, is refused.
func TestLastHost(t *testing.T) {
	small := netip.PrefixFrom(netip.MustParseAddr("10.0.0.0"), maxRunSubnetBits+1)
	for _, row := range []struct {
		in, want string
	}{
		{"10.88.0.0/16", "10.88.255.254"},
		{"10.200.0.16/28", "10.200.0.30"},
		{"10.200.0.21/28", "10.200.0.30"},
		{"192.168.1.0/24", "192.168.1.254"},
		{netip.PrefixFrom(netip.MustParseAddr("10.0.0.8"), maxRunSubnetBits).String(), "10.0.0.14"},
		{small.String(), ""},
		{"10.0.0.1/32", ""},
		{"fd00::/64", ""},
	} {
		got, err := lastHost(netip.MustParsePrefix(row.in))
		switch {
		case row.want == "" && err == nil:
			t.Errorf("lastHost(%s) = %s; want a refusal", row.in, got)
		case row.want != "" && (err != nil || got.String() != row.want):
			t.Errorf("lastHost(%s) = %s, %v; want %s", row.in, got, err, row.want)
		}
	}
}

// TestMoveImport_ManyShortLinksTripTheBound: each symlink an import holds back counts its record as well as
// its two strings, so many short links reach the bound on held links though their text alone would not
// (900,000 links of 14 bytes are about 12 MiB of text).
func TestMoveImport_ManyShortLinksTripTheBound(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a stream of 900,000 entries")
	}
	set := runner.CarriedSet{Roots: []string{"/work"}}
	pr, pw := io.Pipe()
	go func() {
		w, err := movepack.NewWriter(pw, set)
		if err == nil {
			err = w.Add(movepack.Header{Path: "work", Type: movepack.TypeDir, Mode: 0o755}, nil)
		}
		for i := 0; err == nil && i < 900_000; i++ {
			err = w.Add(movepack.Header{Path: fmt.Sprintf("work/l%07d", i), Type: movepack.TypeSymlink, Mode: 0o777, Link: "x"}, nil)
		}
		if err == nil {
			_, err = w.Close()
		}
		pw.CloseWithError(err)
	}()
	rd, err := movepack.NewReader(pr, movepack.Options{Set: set})
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	err = reemit(io.Discard, rd)
	_ = pr.CloseWithError(errors.New("test done"))
	if !errors.Is(err, movepack.ErrCarriedTooLarge) || !strings.Contains(err.Error(), "of symlinks") {
		t.Fatalf("reemit of 900,000 short links = %v; want the held-link bound", err)
	}
}
