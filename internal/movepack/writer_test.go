// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movepack_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// engineTar is a tar as a container engine archives the root /home/agent/work: names start with "work".
func engineTar(t *testing.T, hs ...*tar.Header) *tar.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = io.WriteString(tw, strings.Repeat("c", int(h.Size)))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return tar.NewReader(&buf)
}

// fromTar runs FromTar over hs into a fresh stream and reads that stream back.
func fromTar(t *testing.T, flatten func(string) (io.ReadCloser, int64, error), hs ...*tar.Header) (map[string]movepack.Header, movepack.Manifest, error) {
	t.Helper()
	var buf bytes.Buffer
	w, err := movepack.NewWriter(&buf, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := movepack.FromTar(w, engineTar(t, hs...), "/home/agent/work", "work", flatten); err != nil {
		return nil, movepack.Manifest{}, err
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := movepack.NewReader(&buf, movepack.Options{Set: set})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := map[string]movepack.Header{}
	for {
		h, content, err := r.Next()
		if err == io.EOF {
			return got, r.Manifest(), nil
		}
		if err != nil {
			t.Fatalf("reading what FromTar wrote: %v", err)
		}
		if content != nil {
			_, _ = io.Copy(io.Discard, content)
		}
		got[h.Path] = h
	}
}

// TestMovePack_FromTarCarriesAnEngineArchive: FromTar, with no daemon, maps an engine's archive of a root onto
// the stream: a directory, a file and a symlink as themselves, a hardlink as a file its target's bytes fill, an
// excluded path passed over, and fifos and devices skipped and listed. An entry outside the root, a hardlink
// outside it or with nothing to flatten it, and any other type are refused.
func TestMovePack_FromTarCarriesAnEngineArchive(t *testing.T) {
	flatten := func(abs string) (io.ReadCloser, int64, error) {
		if abs != "/home/agent/work/a" {
			return nil, 0, fmt.Errorf("unexpected %s", abs)
		}
		return io.NopCloser(strings.NewReader("target")), 6, nil
	}
	dirOf := func(n string) *tar.Header { return &tar.Header{Name: n + "/", Typeflag: tar.TypeDir, Mode: 0o755} }
	regOf := func(n string) *tar.Header { return &tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: 3} }
	got, m, err := fromTar(t, flatten,
		dirOf("work"),
		regOf("work/a"),
		&tar.Header{Name: "work/dev", Typeflag: tar.TypeChar, Mode: 0o644},
		&tar.Header{Name: "work/fifo", Typeflag: tar.TypeFifo, Mode: 0o644},
		&tar.Header{Name: "work/h", Typeflag: tar.TypeLink, Linkname: "work/a"},
		&tar.Header{Name: "work/l", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
		dirOf("work/x"),
		regOf("work/x/.credentials.json"),
	)
	if err != nil {
		t.Fatalf("FromTar = %v", err)
	}
	const w = "home/agent/work"
	switch {
	case got[w].Type != movepack.TypeDir, got[w+"/a"].Type != movepack.TypeFile, got[w+"/l"].Link != "/etc":
		t.Errorf("entries = %+v; want the directory, file and symlink as themselves", got)
	case got[w+"/h"].Type != movepack.TypeFile || got[w+"/h"].Size != 6:
		t.Errorf("hardlink = %+v; want a file of its target's 6 bytes", got[w+"/h"])
	case got[w+"/x/.credentials.json"] != (movepack.Header{}):
		t.Error("an excluded path was carried")
	case strings.Join(m.Skipped, ",") != w+"/dev,"+w+"/fifo":
		t.Errorf("skipped = %q; want the device and the fifo", m.Skipped)
	}

	for _, row := range []struct {
		name    string
		flatten func(string) (io.ReadCloser, int64, error)
		hs      []*tar.Header
	}{
		{"an entry outside the root", flatten, []*tar.Header{dirOf("work"), regOf("other/a")}},
		{"a hardlink outside the root", flatten, []*tar.Header{dirOf("work"), {Name: "work/h", Typeflag: tar.TypeLink, Linkname: "etc/passwd"}}},
		{"a hardlink with nothing to flatten it", nil, []*tar.Header{dirOf("work"), regOf("work/a"), {Name: "work/h", Typeflag: tar.TypeLink, Linkname: "work/a"}}},
		{"a PAX global header", flatten, []*tar.Header{dirOf("work"), {Name: "g", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, _, err := fromTar(t, row.flatten, row.hs...); !errors.Is(err, movepack.ErrArchiveRefused) {
				t.Fatalf("FromTar = %v; want ErrArchiveRefused", err)
			}
		})
	}
}

// TestMovePack_SkipListsAndBounds: Writer.Skip lists a file in the manifest, which the reader passes on, and
// refuses past the bound on skipped files.
func TestMovePack_SkipListsAndBounds(t *testing.T) {
	var buf bytes.Buffer
	w, err := movepack.NewWriter(&buf, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Skip("home/agent/work/sock"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := readAll(&buf, movepack.Options{})
	if err != nil || len(m.Skipped) != 1 || m.Skipped[0] != "home/agent/work/sock" {
		t.Fatalf("read = %+v, %v; want the skipped file listed", m, err)
	}

	w, err = movepack.NewWriter(io.Discard, set)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10_000 {
		if err := w.Skip(fmt.Sprintf("home/agent/work/s%05d", i)); err != nil {
			t.Fatalf("skip %d = %v; want it listed", i, err)
		}
	}
	if err := w.Skip("home/agent/work/one-more"); !errors.Is(err, movepack.ErrCarriedTooLarge) {
		t.Fatalf("skip past the bound = %v; want ErrCarriedTooLarge", err)
	}
}
