// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A cast replaced by a save after retention read its age is kept, whether the save renames a fresh
// cast over it or rewrites it in place. Catches a sweep that unlinks what its first stat said was old
// without asking again under the run's lock.
func TestFSStore_SweepKeepsACastReplacedAfterItsStat(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		root := t.TempDir()
		s, err := NewFSStore(root)
		if err != nil {
			t.Fatal(err)
		}
		cast := filepath.Join(root, "run.cast")
		if err := os.WriteFile(cast, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		aged := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(cast, aged, aged); err != nil {
			t.Fatal(err)
		}
		replaced := false
		sweepAfterStat = func(name string) {
			if name != "run.cast" || replaced {
				return
			}
			replaced = true
			save := s.SaveCast
			if inPlace {
				save = func(ctx context.Context, key string, r io.Reader) error {
					return s.SaveRecordingFile(ctx, key+".cast", r)
				}
			}
			if err := save(t.Context(), "run", strings.NewReader("fresh")); err != nil {
				t.Errorf("in place %v: the save inside the sweep = %v", inPlace, err)
			}
		}
		n, err := s.Sweep(24 * time.Hour)
		sweepAfterStat = func(string) {}
		if err != nil || !replaced {
			t.Fatalf("in place %v: sweep = %d, %v (replaced %v)", inPlace, n, err, replaced)
		}
		if got, err := os.ReadFile(cast); err != nil || string(got) != "fresh" {
			t.Errorf("in place %v: the cast saved after the sweep's stat = %q, %v; want it kept (%d removed)", inPlace, got, err, n)
		}
	}
}

// A save after the run's erasure is refused before it makes a staging file or reads a byte of its
// body, so a crash part way through that upload leaves no new file of the erased run behind. Catches a
// save that streams the body to disk first and checks the erasure only afterwards.
func TestFSStore_SaveAfterErasureMakesNoStagingFileAndReadsNoBody(t *testing.T) {
	root := t.TempDir()
	s, err := NewFSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRun(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	body := &staged{t: t, root: root}
	for _, key := range []string{"run", "run~attach"} {
		if err := s.SaveCast(t.Context(), key, body); !errors.Is(err, ErrErased) {
			t.Errorf("%s: save after the erasure = %v, want ErrErased", key, err)
		}
	}
	if body.reads != 0 {
		t.Errorf("the refused saves read their body %d times (a staging file held it meanwhile)", body.reads)
	}
}

// staged is a body that records each read and fails the test if a staging file exists while it is read.
type staged struct {
	t     *testing.T
	root  string
	reads int
}

func (b *staged) Read(p []byte) (int, error) {
	b.reads++
	ents, _ := os.ReadDir(b.root)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), tmpCastPrefix) {
			b.t.Errorf("staging file %s holds the erased run's upload", e.Name())
		}
	}
	return copy(p, "late"), errors.New("end")
}
