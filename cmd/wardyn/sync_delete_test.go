// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSyncGateway_LaptopDeletesReachTheSandbox(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, sess, root := g.syncer(syncRemote, false)
	putSyncFile(t, filepath.Join(root, "gone.txt"), "x", 0o644)
	putSyncFile(t, filepath.Join(root, "kept.txt"), "k", 0o644) // an all-empty tree deletes nothing
	putSyncFile(t, filepath.Join(root, "old/dir/a.txt"), "a", 0o644)
	putSyncFile(t, filepath.Join(root, "edited.txt"), "laptop", 0o644)
	putSyncFile(t, filepath.Join(box, "theirs.txt"), "the agent's own file", 0o644)
	mustPass(t, sy)

	// The agent edits one file; the laptop then deletes all three.
	putSyncFile(t, filepath.Join(box, "edited.txt"), "the agent's edit", 0o644)
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(box, "edited.txt"), later, later)
	for _, p := range []string{"gone.txt", "old", "edited.txt"} {
		if err := os.RemoveAll(filepath.Join(root, p)); err != nil {
			t.Fatal(err)
		}
	}
	rep := mustPass(t, sy)
	if !slices.Equal(rep.DeletedRemote, []string{"gone.txt", "old/dir/a.txt"}) {
		t.Fatalf("deleted in the sandbox = %v", rep.DeletedRemote)
	}
	if _, err := os.Stat(filepath.Join(box, "old")); err == nil {
		t.Error("the emptied directory the laptop deleted is still in the sandbox")
	}
	if got := getSyncFile(t, filepath.Join(box, "edited.txt")); got != "the agent's edit" || !slices.Contains(rep.Conflicts, "edited.txt") {
		t.Errorf("a sandbox edit of a file the laptop deleted must be kept and listed as a conflict: %q, conflicts %v", got, rep.Conflicts)
	}
	if _, err := os.Stat(filepath.Join(box, "theirs.txt")); err != nil {
		t.Errorf("a sandbox file this sync never sent was deleted: %v", err)
	}

	if err := sess.close(); err != nil {
		t.Fatal(err)
	}
	_, data := g.audit.row(t)
	if n, _ := data["remove_requests"].(float64); n < 2 {
		t.Errorf("remove_requests = %v in %v: every delete is counted in the audit row", data["remove_requests"], data)
	}
}

func TestSyncGateway_ARenameIsARenameInTheSandbox(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, sess, root := g.syncer(syncRemote, false)
	putSyncFile(t, filepath.Join(root, "docs/plan.md"), "the plan", 0o644)
	mustPass(t, sy)
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "docs/plan.md"), filepath.Join(root, "notes/plan.md")); err != nil {
		t.Fatal(err)
	}
	rep := mustPass(t, sy)
	if !slices.Equal(rep.Renamed, []syncRename{{From: "docs/plan.md", To: "notes/plan.md"}}) || len(rep.Pushed)+len(rep.DeletedRemote) != 0 {
		t.Fatalf("renamed %v, pushed %v, deleted %v: want one rename and nothing else", rep.Renamed, rep.Pushed, rep.DeletedRemote)
	}
	if got := getSyncFile(t, filepath.Join(box, "notes/plan.md")); got != "the plan" {
		t.Errorf("renamed file = %q", got)
	}
	if rep := mustPass(t, sy); !rep.empty() {
		t.Errorf("the pass after a rename did something: %+v", rep)
	}
	if err := sess.close(); err != nil {
		t.Fatal(err)
	}
	if _, data := g.audit.row(t); data["rename_requests"] != float64(1) {
		t.Errorf("rename_requests = %v, want 1", data["rename_requests"])
	}
}

func TestSyncGateway_AnEmptyOrUnreadableLaptopTreeDeletesNothing(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, false)
	putSyncFile(t, filepath.Join(root, "a.txt"), "a", 0o644)
	putSyncFile(t, filepath.Join(root, "locked/b.txt"), "b", 0o644)
	mustPass(t, sy)

	if os.Getuid() != 0 { // root reads a 0o000 directory anyway
		if err := os.Chmod(filepath.Join(root, "locked"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o755) })
		if rep := mustPass(t, sy); len(rep.DeletedRemote) != 0 {
			t.Errorf("an unreadable laptop directory deleted %v in the sandbox", rep.DeletedRemote)
		}
		_ = os.Chmod(filepath.Join(root, "locked"), 0o755)
	}
	for _, p := range []string{"a.txt", "locked"} {
		_ = os.RemoveAll(filepath.Join(root, p))
	}
	if rep := mustPass(t, sy); len(rep.DeletedRemote) != 0 {
		t.Errorf("an empty laptop directory (an unmounted drive looks the same) deleted %v", rep.DeletedRemote)
	}
	if _, err := os.Stat(filepath.Join(box, "a.txt")); err != nil {
		t.Error(err)
	}
}

func TestSyncGateway_SandboxDeletesNeedPullDeletes(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(root, "keep.txt"), "k", 0o644)
	putSyncFile(t, filepath.Join(root, "drop.txt"), "d", 0o644)
	mustPass(t, sy)
	if err := os.Remove(filepath.Join(box, "drop.txt")); err != nil {
		t.Fatal(err)
	}

	sy.o.maxPullDeletes, sy.o.trash = 20, t.TempDir() // the CLI's defaults
	if rep := mustPass(t, sy); len(rep.DeletedLocal)+len(rep.Refused) != 0 {
		t.Fatalf("--pull alone deleted %v (refused %v) on the laptop", rep.DeletedLocal, rep.Refused)
	}
	if _, err := os.Stat(filepath.Join(root, "drop.txt")); err != nil {
		t.Fatalf("--pull alone removed a laptop file: %v", err)
	}

	var listed strings.Builder
	sy.o.pullDeletes, sy.o.maxPullDeletes, sy.o.notify = true, 5, &listed
	sy.o.trash = filepath.Join(t.TempDir(), "deleted")
	rep := mustPass(t, sy)
	if !slices.Equal(rep.DeletedLocal, []string{"drop.txt"}) {
		t.Fatalf("deleted on the laptop = %v", rep.DeletedLocal)
	}
	if _, err := os.Stat(filepath.Join(root, "drop.txt")); err == nil {
		t.Error("the file is still on the laptop")
	}
	if got := getSyncFile(t, filepath.Join(sy.o.trash, "drop.txt")); got != "d" {
		t.Errorf("the deleted file was not kept: %q", got)
	}
	if !strings.Contains(listed.String(), "drop.txt") {
		t.Errorf("the delete was not listed before it was applied: %q", listed.String())
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Error(err)
	}
}

func TestSyncGateway_PullDeletesAreBoundedPerPass(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	for _, p := range []string{"a", "b", "c", "keep"} {
		putSyncFile(t, filepath.Join(root, p), p, 0o644)
	}
	mustPass(t, sy)
	for _, p := range []string{"a", "b", "c"} {
		_ = os.Remove(filepath.Join(box, p))
	}
	sy.o.pullDeletes, sy.o.maxPullDeletes, sy.o.trash = true, 2, t.TempDir()
	rep := mustPass(t, sy)
	if len(rep.DeletedLocal) != 0 || len(rep.Refused) != 3 {
		t.Fatalf("over the bound: deleted %v, refused %v; want none deleted and all three refused", rep.DeletedLocal, rep.Refused)
	}
	for _, p := range []string{"a", "b", "c"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestSyncGateway_APassReportCountsLaptopDeletes(t *testing.T) {
	g := newSyncRig(t)
	g.mkSandboxDir(syncRemote)
	_, sess, _ := g.syncer(syncRemote, true)
	if !sess.report(syncPassReport{Complete: true, DeletedOnLaptop: 3}) || !sess.report(syncPassReport{Complete: true, DeletedOnLaptop: 2}) {
		t.Fatal("the gateway refused a pass report on a started sync channel")
	}
	if err := sess.close(); err != nil {
		t.Fatal(err)
	}
	if _, data := g.audit.row(t); data["laptop_deletes_reported"] != float64(5) {
		t.Errorf("laptop_deletes_reported = %v, want 5", data["laptop_deletes_reported"])
	}
}
