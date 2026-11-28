// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// Export from one root, then import into another: the reader's digests of both streams are equal.
func TestMoveHelper_ExportImportRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for p, body := range map[string]string{"work/a/b": "1", "work/a-b": "22", "work/.wardyn/booted-run": ""} {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a/b", filepath.Join(src, "work/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	set := movepack.Set{Roots: []string{"/work"}}
	setJSON, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var stream, stderr bytes.Buffer
	if rc := runMoveHelper(moveModeExport, moveArgs{set: string(setJSON), base: src}, nil, &stream, &stderr); rc != 0 {
		t.Fatalf("export rc %d: %s", rc, stderr.String())
	}
	exported := digestOf(t, stream.Bytes(), set)
	in := moveArgs{set: string(setJSON), maxBytes: 1 << 20, mounts: `["/work"]`, base: dst}
	if rc := runMoveHelper(moveModeImport, in, bytes.NewReader(stream.Bytes()), &bytes.Buffer{}, &stderr); rc != 0 {
		t.Fatalf("import rc %d: %s", rc, stderr.String())
	}
	var again bytes.Buffer
	if rc := runMoveHelper(moveModeExport, moveArgs{set: string(setJSON), base: dst}, nil, &again, &stderr); rc != 0 {
		t.Fatalf("export of the import rc %d: %s", rc, stderr.String())
	}
	if got := digestOf(t, again.Bytes(), set); got != exported {
		t.Fatalf("digest after the round trip = %s, want %s", got, exported)
	}
	if link, err := os.Readlink(filepath.Join(dst, "work/link")); err != nil || link != "a/b" {
		t.Fatalf("the imported link = %q, %v; want a/b", link, err)
	}
}

func digestOf(t *testing.T, stream []byte, set movepack.Set) string {
	t.Helper()
	rd, err := movepack.NewReader(bytes.NewReader(stream), movepack.Options{Set: set})
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	for {
		if _, _, err := rd.Next(); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("read: %v", err)
			}
			return rd.Manifest().Digest
		}
	}
}

func TestMoveHelper_FlagsRefuseMissingSet(t *testing.T) {
	for _, tc := range []struct {
		mode string
		args moveArgs
	}{
		{moveModeExport, moveArgs{base: "/"}},
		{moveModeImport, moveArgs{maxBytes: 1, mounts: `["/work"]`, base: "/"}},
		{moveModeImport, moveArgs{set: `{"roots":["/work"]}`, base: "/"}},
		{moveModeExport, moveArgs{set: "not json", base: "/"}},
	} {
		var stderr bytes.Buffer
		if rc := runMoveHelper(tc.mode, tc.args, strings.NewReader(""), &bytes.Buffer{}, &stderr); rc != 2 {
			t.Errorf("%s with %+v: rc %d, want 2 (%s)", tc.mode, tc.args, rc, stderr.String())
		}
	}
	if _, err := moveMode(true, true, false); err == nil {
		t.Error("two move modes at once were accepted")
	}
}
