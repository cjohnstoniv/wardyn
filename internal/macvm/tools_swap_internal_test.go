// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// An exchange whose post-swap check fails and whose swap back fails too leaves the old generation in the
// staging dir: it is moved aside and named in the error, and neither this update's clean-up nor the next
// update's sweep deletes it.
func TestAFailedSwapBackKeepsTheOldGeneration(t *testing.T) {
	for name, swap := range map[string]func(nb, dir string) error{
		"base":         func(nb, dir string) error { return swapBase(context.Background(), nb, dir) },
		"kept machine": swapKept,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, log := context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))
			base := filepath.Join(t.TempDir(), "0")
			gen(t, base, "gen1")
			nb, done, err := beginUpdate(ctx, base, log)
			if err != nil {
				t.Fatal(err)
			}
			gen(t, nb, "gen2")
			// A file no base or kept machine holds, so the check after the exchange fails.
			if err := os.WriteFile(filepath.Join(nb, "boot.log"), []byte("stage 2"), 0o600); err != nil {
				t.Fatal(err)
			}
			old, swaps := swapFn, 0
			swapFn = func(a, b string) error {
				if swaps++; swaps > 1 {
					return unix.EIO
				}
				return old(a, b)
			}
			err = swap(nb, base)
			swapFn = old
			done()
			kept, _ := filepath.Glob(nb + ".old-*")
			if len(kept) != 1 {
				t.Fatalf("swap = %v; old generation copies %v, want one", err, kept)
			}
			if err == nil || !strings.Contains(err.Error(), kept[0]) {
				t.Fatalf("swap = %v, want it to name %s", err, kept[0])
			}
			if got := readFile(t, filepath.Join(kept[0], DiskFile)); got != "gen1-disk" {
				t.Fatalf("kept copy holds %q, want the old generation", got)
			}
			if _, done2, err := beginUpdate(ctx, base, log); err != nil {
				t.Fatal(err)
			} else {
				done2()
			}
			if got := readFile(t, filepath.Join(kept[0], DiskFile)); got != "gen1-disk" {
				t.Fatalf("after the next update's sweep the kept copy holds %q", got)
			}
		})
	}
}

// A kept machine's delete takes the old generations an update moved aside with it, and leaves another
// machine's, whose name starts with this one's.
func TestRemoveKeptTakesItsMovedAsideOldGenerations(t *testing.T) {
	root := t.TempDir()
	dir, other := filepath.Join(root, "0123456789ab"), filepath.Join(root, "0123456789ab0")
	gen(t, dir, "gen2")
	for _, d := range []string{dir, other} {
		gen(t, d+".update", "gen1")
		if msg := keepOldGeneration(d+".update", "kept machine"); !strings.Contains(msg, "is kept at") {
			t.Fatal(msg)
		}
	}
	if gone, err := RemoveKept(dir); err != nil || !gone {
		t.Fatalf("RemoveKept = %v, %v; want true, nil", gone, err)
	}
	if left, _ := filepath.Glob(dir + ".update.old-*"); len(left) != 0 {
		t.Fatalf("old generations left after the delete: %v", left)
	}
	if left, _ := filepath.Glob(other + ".update.old-*"); len(left) != 1 {
		t.Fatalf("the other machine's old generations %v, want one", left)
	}
}
