// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A tool outside the daemon (sync, backup) can remove the directory this Open just made, before the lock
// is created in it. Open makes it again, once.
func TestOpenSurvivesItsNewDirectoryRemovedBeforeTheLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	removed := 0
	afterOpenRoot = func() {
		if removed == 0 {
			removed++
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { afterOpenRoot = func() {} })
	s, err := Open(dir, uuid.New())
	if err != nil {
		t.Fatalf("open after its new directory was removed: %v", err)
	}
	s.Close()
	if removed != 1 {
		t.Fatalf("the directory was removed %d times, want once", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("no manifest in the made-again directory: %v", err)
	}

	t.Run("an existing stream is never recreated", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spool")
		id := uuid.New()
		first, err := Open(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		first.Close()
		afterOpenRoot = func() {
			afterOpenRoot = func() {}
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}
		_, err = Open(dir, id)
		if err == nil || !strings.Contains(err.Error(), "removed or replaced while it was opened") && !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("open of an erased existing stream = %v, want an error", err)
		}
		if _, statErr := os.Lstat(dir); statErr == nil {
			t.Fatal("an erased existing stream was recreated")
		}
	})
}

// The daemon's activity markers are not stream data: a marker beside the lock, before the manifest, is
// still a new spool.
func TestOpenIgnoresActivityMarkersBesideTheLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"live-epoch.json", "live-activity.json", "live-epoch.json." + uuid.NewString()} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(dir, uuid.New())
	if err != nil {
		t.Fatalf("open beside activity markers: %v", err)
	}
	s.Close()
	if err := os.WriteFile(filepath.Join(dir, "stray"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, uuid.New()); err == nil {
		t.Fatal("a stray file beside the lock was accepted as a new spool")
	}
}
