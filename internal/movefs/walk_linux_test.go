// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movefs

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// readAll reads a stream through movepack's reader and returns its headers in order.
func readAll(t *testing.T, stream []byte, set movepack.Set) ([]movepack.Header, movepack.Manifest) {
	t.Helper()
	rd, err := movepack.NewReader(bytes.NewReader(stream), movepack.Options{Set: set})
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	var hs []movepack.Header
	for {
		h, _, err := rd.Next()
		if err == io.EOF {
			return hs, rd.Manifest()
		}
		if err != nil {
			t.Fatalf("the reader refused the walk's stream: %v", err)
		}
		hs = append(hs, h)
	}
}

func paths(hs []movepack.Header) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.Path)
	}
	return out
}

// exportOf exports set from base and returns the stream.
func exportOf(t *testing.T, set movepack.Set, base string) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	_, err := Export(&buf, set, base)
	return buf.Bytes(), err
}

func mustWrite(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// The walk's order is the stream's: "a" sorts before "a/b", which sorts before "a-b", though a byte sort of the
// whole paths would put "a-b" first.
func TestMoveFS_WalkOrderIsStreamOrder(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "work/a/b"), "1", 0o644)
	mustWrite(t, filepath.Join(base, "work/a-b"), "2", 0o644)
	mustWrite(t, filepath.Join(base, "work/a.c"), "3", 0o644)
	set := movepack.Set{Roots: []string{"/work"}}
	stream, err := exportOf(t, set, base)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	hs, _ := readAll(t, stream, set)
	want := []string{"work", "work/a", "work/a/b", "work/a-b", "work/a.c"}
	if got := paths(hs); !slices.Equal(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestMoveFS_WalkNeverFollowsLinks(t *testing.T) {
	set := movepack.Set{Roots: []string{"/work"}}
	t.Run("a link to a file outside the root is carried as its text", func(t *testing.T) {
		base := t.TempDir()
		mustWrite(t, filepath.Join(base, "outside/secret"), "s3cret", 0o644)
		mustWrite(t, filepath.Join(base, "work/f"), "x", 0o644)
		if err := os.Symlink("../outside/secret", filepath.Join(base, "work/l")); err != nil {
			t.Fatal(err)
		}
		stream, err := exportOf(t, set, base)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		hs, _ := readAll(t, stream, set)
		i := slices.IndexFunc(hs, func(h movepack.Header) bool { return h.Path == "work/l" })
		if i < 0 || hs[i].Type != movepack.TypeSymlink || hs[i].Link != "../outside/secret" {
			t.Fatalf("the link entry = %+v, want a symlink to ../outside/secret", hs)
		}
		if bytes.Contains(stream, []byte("s3cret")) {
			t.Fatal("the link's target content is in the stream")
		}
	})
	t.Run("a root that is a symlink refuses", func(t *testing.T) {
		base := t.TempDir()
		if err := os.MkdirAll(filepath.Join(base, "elsewhere"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("elsewhere", filepath.Join(base, "work")); err != nil {
			t.Fatal(err)
		}
		if _, err := exportOf(t, set, base); !errors.Is(err, movepack.ErrArchiveRefused) {
			t.Fatalf("Export of a symlinked root = %v, want ErrArchiveRefused", err)
		}
	})
	t.Run("a hardlink is carried as a file", func(t *testing.T) {
		base := t.TempDir()
		mustWrite(t, filepath.Join(base, "work/a"), "same", 0o644)
		if err := os.Link(filepath.Join(base, "work/a"), filepath.Join(base, "work/b")); err != nil {
			t.Fatal(err)
		}
		stream, err := exportOf(t, set, base)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		hs, _ := readAll(t, stream, set)
		for _, h := range hs[1:] {
			if h.Type != movepack.TypeFile || h.Size != 4 {
				t.Fatalf("entry %+v, want a 4-byte file", h)
			}
		}
	})
	t.Run("a fifo is skipped and listed", func(t *testing.T) {
		base := t.TempDir()
		mustWrite(t, filepath.Join(base, "work/f"), "x", 0o644)
		if err := syscall.Mkfifo(filepath.Join(base, "work/pipe"), 0o644); err != nil {
			t.Fatal(err)
		}
		stream, err := exportOf(t, set, base)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		hs, m := readAll(t, stream, set)
		if slices.Contains(paths(hs), "work/pipe") || !slices.Equal(m.Skipped, []string{"work/pipe"}) {
			t.Fatalf("entries %v, skipped %v; want the fifo skipped and listed", paths(hs), m.Skipped)
		}
	})
	t.Run("a socket is left out", func(t *testing.T) {
		base, err := os.MkdirTemp("", "mfs") // a short path: a socket's is bounded
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(base) })
		mustWrite(t, filepath.Join(base, "work/f"), "x", 0o644)
		l, err := net.Listen("unix", filepath.Join(base, "work/s"))
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		stream, err := exportOf(t, set, base)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		hs, m := readAll(t, stream, set)
		if slices.Contains(paths(hs), "work/s") || len(m.Skipped) != 0 {
			t.Fatalf("entries %v, skipped %v; want the socket neither carried nor listed", paths(hs), m.Skipped)
		}
	})
	t.Run("setuid is cleared", func(t *testing.T) {
		base := t.TempDir()
		p := filepath.Join(base, "work/tool")
		mustWrite(t, p, "x", 0o755)
		if err := os.Chmod(p, 0o4755); err != nil {
			t.Fatal(err)
		}
		stream, err := exportOf(t, set, base)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		hs, _ := readAll(t, stream, set)
		if hs[1].Mode != 0o755 {
			t.Fatalf("mode = %o, want 0755", hs[1].Mode)
		}
	})
}

func TestMoveFS_WalkSkipsRootLostFound(t *testing.T) {
	for _, tc := range []struct {
		name          string
		isDir         bool
		uid           uint32
		parentIsMount bool
		want          bool
	}{
		{"lost+found", true, 0, true, true},
		{"lost+found", true, 1000, true, false},
		{"lost+found", false, 0, true, false},
		{"lost+found", true, 0, false, false},
		{"lost+found2", true, 0, true, false},
	} {
		if got := skipsRootLostFound(tc.name, tc.isDir, tc.uid, tc.parentIsMount); got != tc.want {
			t.Errorf("skipsRootLostFound(%q, dir %v, uid %d, mount %v) = %v, want %v", tc.name, tc.isDir, tc.uid, tc.parentIsMount, got, tc.want)
		}
	}
}
