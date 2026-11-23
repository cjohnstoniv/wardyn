// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movepack_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// nopDetector scans nothing; it turns the reader's git tracking on.
type nopDetector struct{}

func (nopDetector) Write(p []byte) (int, error) { return len(p), nil }
func (nopDetector) Found() bool                 { return false }

// TestMoveReader_MemoryBoundedByDepth: 20,000 long-path symlinks, and 20,000 directories each holding a HEAD
// file, leave the reader's live heap within a small fixed bound. A reader that keeps state per entry holds
// about 4 KB for each, 80 MB here.
func TestMoveReader_MemoryBoundedByDepth(t *testing.T) {
	if testing.Short() {
		t.Skip("streams about 200 MB")
	}
	const n = 20000
	const bound = 8 << 20
	deep := "home/agent/work"
	var dirs []string
	for i := range 14 {
		deep += "/" + strings.Repeat(string(rune('a'+i)), 250)
		dirs = append(dirs, deep)
	}
	rows := []struct {
		name    string
		entries func(w *movepack.Writer) error
		o       movepack.Options
	}{
		{"symlinks", func(w *movepack.Writer) error {
			for i := range n {
				h := movepack.Header{Path: fmt.Sprintf("%s/%s%06d", deep, strings.Repeat("l", 230), i), Type: movepack.TypeSymlink, Mode: 0o777, Link: "x"}
				if err := w.Add(h, nil); err != nil {
					return err
				}
			}
			return nil
		}, movepack.Options{Set: set}},
		{"HEAD files", func(w *movepack.Writer) error {
			for i := range n {
				d := fmt.Sprintf("%s/%s%06d", deep, strings.Repeat("g", 230), i)
				if err := w.Add(movepack.Header{Path: d, Type: movepack.TypeDir, Mode: 0o755}, nil); err != nil {
					return err
				}
				if err := w.Add(movepack.Header{Path: d + "/HEAD", Type: movepack.TypeFile, Mode: 0o644, Size: 2}, strings.NewReader("x\n")); err != nil {
					return err
				}
			}
			return nil
		}, movepack.Options{Set: set, Detect: func() movepack.Detector { return nopDetector{} }}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pr, pw := io.Pipe()
			go func() {
				w, err := movepack.NewWriter(pw, set)
				if err == nil {
					err = w.Add(movepack.Header{Path: "home/agent/work", Type: movepack.TypeDir, Mode: 0o755}, nil)
				}
				for _, d := range dirs {
					if err == nil {
						err = w.Add(movepack.Header{Path: d, Type: movepack.TypeDir, Mode: 0o755}, nil)
					}
				}
				if err == nil {
					err = row.entries(w)
				}
				if err == nil {
					_, err = w.Close()
				}
				pw.CloseWithError(err)
			}()
			before := liveHeap()
			r, err := movepack.NewReader(pr, row.o)
			if err != nil {
				t.Fatal(err)
			}
			for {
				_, content, err := r.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if content != nil {
					_, _ = io.Copy(io.Discard, content)
				}
			}
			grew := int64(liveHeap()) - int64(before)
			runtime.KeepAlive(r)
			if grew > bound {
				t.Errorf("the reader's live heap grew %d KiB over %d entries; want under %d KiB", grew>>10, r.Manifest().Entries, bound>>10)
			}
		})
	}
}

func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestMoveReader_RefusedStreamLeaksNoGoroutine: 50 streams cut inside a loose git object whose bytes the
// caller skipped, each refused and closed, leave no git decoder running.
func TestMoveReader_RefusedStreamLeaksNoGoroutine(t *testing.T) {
	const o = "home/agent/work/.git/objects"
	// Incompressible, so the object's stored bytes fill most of the stream and the cut lands inside them.
	big := make([]byte, 64<<10)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(big)
	stream := gitTree(t, dir(o+"/ab"), reg(o+"/ab/"+strings.Repeat("ab", 19), looseObject(t, len(big), big)))
	// Inside the object's bytes: past the headers, well before the archive's end.
	cut := stream[:len(stream)/2]
	before := runtime.NumGoroutine()
	for range 50 {
		// Next alone, so the cut is met as Next finishes the object for a caller that skipped its bytes.
		r, err := movepack.NewReader(bytes.NewReader(cut), scanning(movepack.Limits{}))
		if err != nil {
			t.Fatal(err)
		}
		for err == nil {
			_, _, err = r.Next()
		}
		r.Close()
		if err == io.EOF {
			t.Fatal("a cut stream read clean")
		}
	}
	after := runtime.NumGoroutine()
	for deadline := time.Now().Add(2 * time.Second); after > before+2 && time.Now().Before(deadline); after = runtime.NumGoroutine() {
		time.Sleep(10 * time.Millisecond)
	}
	if after > before+2 {
		t.Errorf("goroutines %d before 50 refused streams, %d after; want no decoder left running", before, after)
	}
}
