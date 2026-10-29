// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A laptop file that appears as another disappears, with the same size and
// mtime but other bytes, is not a rename: the sandbox gets the new bytes.
func TestSync_ADifferentFileWithTheSameSizeAndMtimeIsNotARename(t *testing.T) {
	sy, box, root := pipeSyncer(t, false)
	putSyncFile(t, filepath.Join(root, "old.txt"), "aaaa", 0o644)
	putSyncFile(t, filepath.Join(root, "keep.txt"), "k", 0o644) // an all-empty tree deletes nothing
	if _, err := sy.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "old.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "old.txt")); err != nil {
		t.Fatal(err)
	}
	putSyncFile(t, filepath.Join(root, "new.txt"), "bbbb", 0o644)
	if err := os.Chtimes(filepath.Join(root, "new.txt"), fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := getSyncFile(t, filepath.Join(box, "new.txt")); got != "bbbb" {
		t.Errorf("new.txt in the sandbox = %q, want the laptop's bytes", got)
	}
	if _, err := os.Lstat(filepath.Join(box, "old.txt")); err == nil {
		t.Error("old.txt is still in the sandbox")
	}
	if len(rep.Renamed) != 0 || !slices.Equal(rep.Pushed, []string{"new.txt"}) || !slices.Equal(rep.DeletedRemote, []string{"old.txt"}) {
		t.Errorf("renamed %v, pushed %v, deleted %v: want a push and a delete", rep.Renamed, rep.Pushed, rep.DeletedRemote)
	}
}

// A sandbox copy the agent wrote after the walk is not renamed away: both paths
// go to the normal delete and push, and the delete keeps the edit as a conflict.
func TestSync_ASandboxEditAfterTheWalkIsNotRenamed(t *testing.T) {
	for _, edit := range []string{"the agent's edit", "the plan"} { // other bytes; the laptop's bytes at a new mtime
		sy, box, root := pipeSyncer(t, false)
		putSyncFile(t, filepath.Join(root, "old.txt"), "the plan", 0o644)
		putSyncFile(t, filepath.Join(root, "keep.txt"), "k", 0o644)
		if _, err := sy.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "new.txt")); err != nil {
			t.Fatal(err)
		}
		// The first half of a pass: both sides listed, nothing acted on yet.
		ctx, rep := context.Background(), NewReport()
		sy.ig = newSyncIgnore(nil)
		local, _, tree, err := sy.walkLocal(rep)
		if err != nil {
			t.Fatal(err)
		}
		remote, err := sy.walkRemote(ctx, rep)
		if err != nil {
			t.Fatal(err)
		}
		v := &passView{local: local, localTree: tree, remote: remote}

		putSyncFile(t, filepath.Join(box, "old.txt"), edit, 0o644)
		later := time.Now().Add(time.Hour)
		_ = os.Chtimes(filepath.Join(box, "old.txt"), later, later)
		done, err := sy.renames(ctx, []string{"keep.txt", "new.txt", "old.txt"}, v, func(string) bool { return true }, rep)
		if err != nil {
			t.Fatal(err)
		}
		if len(done) != 0 || len(rep.Renamed) != 0 {
			t.Errorf("edit %q: renamed %v, handled %v: a changed sandbox copy must not be renamed", edit, rep.Renamed, done)
		}
		if got := getSyncFile(t, filepath.Join(box, "old.txt")); got != edit {
			t.Errorf("edit %q: old.txt in the sandbox = %q", edit, got)
		}

		rep2, err := sy.Pass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := getSyncFile(t, filepath.Join(box, "old.txt")); got != edit || !slices.Contains(rep2.Conflicts, "old.txt") {
			t.Errorf("edit %q: the sandbox edit must be kept as a conflict: %q, conflicts %v", edit, got, rep2.Conflicts)
		}
		if got := getSyncFile(t, filepath.Join(box, "new.txt")); got != "the plan" {
			t.Errorf("edit %q: new.txt in the sandbox = %q", edit, got)
		}
	}
}

// A real rename, with the same bytes, stays a rename in the sandbox.
func TestSync_ARenameWithTheSameBytesStaysARename(t *testing.T) {
	sy, box, root := pipeSyncer(t, false)
	putSyncFile(t, filepath.Join(root, "docs/plan.md"), "the plan", 0o644)
	putSyncFile(t, filepath.Join(root, "keep.txt"), "k", 0o644)
	if _, err := sy.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "docs/plan.md"), filepath.Join(root, "notes/plan.md")); err != nil {
		t.Fatal(err)
	}
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Renamed, []Rename{{From: "docs/plan.md", To: "notes/plan.md"}}) || len(rep.Pushed)+len(rep.DeletedRemote) != 0 {
		t.Errorf("renamed %v, pushed %v, deleted %v: want one rename and nothing else", rep.Renamed, rep.Pushed, rep.DeletedRemote)
	}
	if got := getSyncFile(t, filepath.Join(box, "notes/plan.md")); got != "the plan" {
		t.Errorf("renamed file = %q", got)
	}
}
