// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package foldersync

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// editorSave saves body at the laptop's name the way editors do: a file written beside it and
// renamed over it.
func editorSave(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	putSyncFile(t, p+".editor", body, 0o644)
	if err := os.Rename(p+".editor", p); err != nil {
		t.Fatal(err)
	}
}

// editorSaves has an editor save at the laptop's name (editorSave) at each install step of a pull on
// name: after the pull's last look at the laptop file. saves holds one body per step in order, the
// swap, then the swap back. failBack fails the swap back.
func editorSaves(t *testing.T, root, name string, saves []string, failBack bool) {
	t.Helper()
	save := func(body string) { editorSave(t, root, name, body) }
	oldX, oldL := exchangeFiles, linkFile
	t.Cleanup(func() { exchangeFiles, linkFile = oldX, oldL })
	steps := 0
	exchangeFiles = func(r *os.Root, a, b string) error {
		if path.Base(b) != name {
			return oldX(r, a, b)
		}
		if steps++; steps <= len(saves) {
			save(saves[steps-1])
		}
		if failBack && steps == 2 {
			return &os.LinkError{Op: "renameat", Old: a, New: b, Err: syscall.EIO}
		}
		return oldX(r, a, b)
	}
	linkFile = func(r *os.Root, from, to string) error {
		if to == name && steps == 0 {
			steps++
			save(saves[0])
		}
		return oldL(r, from, to)
	}
}

// keptSave is the body of the one file a pull kept a laptop save in, or "" when there is none.
func keptSave(t *testing.T, root string) string {
	t.Helper()
	kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-save-*"))
	switch len(kept) {
	case 0:
		return ""
	case 1:
		return getSyncFile(t, kept[0])
	}
	t.Fatalf("%d kept saves: %v", len(kept), kept)
	return ""
}

// A laptop save that lands after a pull's last check and before its install is never replaced: the
// pull swaps instead of renaming over, finds the save in what it displaced, puts it back and lists a
// conflict, keeping the sandbox's copy beside it. A new file saved there is never linked over. A save
// that cannot be put back is kept under a name the report gives. Catches a pull that renames over a
// file it no longer looks at, and one that deletes a displaced save.
func TestPass_ALaptopSaveAfterThePullsCheckIsKept(t *testing.T) {
	for _, tc := range []struct {
		name, file   string
		saves        []string
		failBack     bool
		laptop, kept string
	}{
		{name: "save over a listed file", file: "x.txt", saves: []string{"laptop save"}, laptop: "laptop save"},
		{name: "new file saved", file: "new.txt", saves: []string{"laptop save"}, laptop: "laptop save"},
		{name: "saved again during the swap back", file: "x.txt", saves: []string{"first save", "second save"}, laptop: "first save", kept: "second save"},
		{name: "swap back fails", file: "x.txt", saves: []string{"laptop save"}, failBack: true, laptop: "sandbox edit", kept: "laptop save"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sy, box, root := runnerSyncer(t, true)
			sandbox := "sandbox edit"
			if tc.file == "new.txt" {
				sandbox = "from the sandbox"
				putSyncFile(t, filepath.Join(box, "new.txt"), sandbox, 0o644)
			} else {
				changedInSandbox(t, sy, box, root)
			}
			editorSaves(t, root, tc.file, tc.saves, tc.failBack)
			rep := pass(t, sy)
			if got := getSyncFile(t, filepath.Join(root, tc.file)); got != tc.laptop {
				t.Errorf("laptop %s = %q, want %q", tc.file, got, tc.laptop)
			}
			if got := keptSave(t, root); got != tc.kept {
				t.Errorf("kept save = %q, want %q", got, tc.kept)
			}
			if slices.Contains(rep.Pulled, tc.file) {
				t.Errorf("%s reported pulled: %+v", tc.file, rep)
			}
			if tc.kept != "" {
				if !strings.Contains(rep.NotPreserved, "kept as .wardyn-sync-save-") {
					t.Errorf("report = %+v, want not preserved naming the kept save", rep)
				}
				return
			}
			got := getSyncFile(t, filepath.Join(root, conflictNames(tc.file, testTag)[0]))
			if got != sandbox || !slices.Contains(rep.Conflicts, tc.file) || rep.NotPreserved != "" {
				t.Errorf("sandbox copy = %q, report = %+v, want a listed conflict with the sandbox copy kept", got, rep)
			}
			if temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*")); len(temps) != 0 {
				t.Errorf("temp files left: %v", temps)
			}
		})
	}
}

// Where the swap is not offered (Windows, or a filesystem without it), a pull renames over the
// file as before. Catches a pull that fails on such a filesystem.
func TestPass_APullWithoutTheSwapStillReplaces(t *testing.T) {
	sy, box, root := runnerSyncer(t, true)
	changedInSandbox(t, sy, box, root)
	old := exchangeFiles
	t.Cleanup(func() { exchangeFiles = old })
	exchangeFiles = func(*os.Root, string, string) error { return errors.ErrUnsupported }
	rep := pass(t, sy)
	if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "sandbox edit" || !slices.Contains(rep.Pulled, "x.txt") || rep.NotPreserved != "" {
		t.Errorf("laptop = %q, report = %+v", got, rep)
	}
}

// A laptop delete that lands after a pull's last check is kept: the swap finds no file, nothing is
// installed and the file is listed as a conflict. Catches a pull that puts a deleted file back.
func TestPass_ALaptopDeleteAfterThePullsCheckIsKept(t *testing.T) {
	sy, box, root := runnerSyncer(t, true)
	changedInSandbox(t, sy, box, root)
	old := exchangeFiles
	t.Cleanup(func() { exchangeFiles = old })
	exchangeFiles = func(r *os.Root, a, b string) error {
		if path.Base(b) == "x.txt" {
			if err := os.Remove(filepath.Join(root, "x.txt")); err != nil {
				t.Fatal(err)
			}
		}
		return old(r, a, b)
	}
	rep := pass(t, sy)
	_, err := os.Lstat(filepath.Join(root, "x.txt"))
	if !errors.Is(err, fs.ErrNotExist) || slices.Contains(rep.Pulled, "x.txt") || !slices.Contains(rep.Conflicts, "x.txt") {
		t.Errorf("x.txt: %v, report = %+v, want it still deleted and listed as a conflict", err, rep)
	}
}

// A sandbox delete pulled with --pull-deletes removes only the laptop file the walk listed: it is
// copied into Trash where it is, then moved aside and what was moved is looked at, so a save made
// after the look is moved back and listed as a conflict, and Trash keeps nothing. Catches a delete
// that removes a file it no longer looks at, and one that moves the file before its Trash copy is whole.
func TestPass_ALaptopSaveAfterAPullDeletesLookIsKept(t *testing.T) {
	for _, save := range []bool{false, true} {
		sy, box, root := pipeSyncer(t, true)
		putSyncFile(t, filepath.Join(root, "x.txt"), "laptop", 0o644)
		putSyncFile(t, filepath.Join(root, "keep.txt"), "kept", 0o644) // a sandbox emptied whole deletes nothing
		pass(t, sy)
		if err := os.Remove(filepath.Join(box, "x.txt")); err != nil {
			t.Fatal(err)
		}
		sy.Opts.PullDeletes, sy.Opts.MaxPullDeletes, sy.Opts.Trash = true, 5, t.TempDir()
		old := deleteLooked
		t.Cleanup(func() { deleteLooked = old })
		deleteLooked = func() {
			temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
			if getSyncFile(t, filepath.Join(sy.Opts.Trash, "x.txt")) != "laptop" || getSyncFile(t, filepath.Join(root, "x.txt")) != "laptop" || len(temps) != 0 {
				t.Errorf("save %v: the laptop file moved before its Trash copy was whole (temp files %v)", save, temps)
			}
			if save {
				editorSave(t, root, "x.txt", "laptop save")
			}
		}
		rep := pass(t, sy)
		trash, _ := os.ReadDir(sy.Opts.Trash)
		if temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*")); len(temps) != 0 {
			t.Errorf("save %v: temp files left: %v", save, temps)
		}
		if !save {
			if _, err := os.Lstat(filepath.Join(root, "x.txt")); err == nil || !slices.Equal(rep.DeletedLocal, []string{"x.txt"}) ||
				getSyncFile(t, filepath.Join(sy.Opts.Trash, "x.txt")) != "laptop" {
				t.Errorf("plain delete: report = %+v, want x.txt moved into Trash", rep)
			}
			continue
		}
		if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "laptop save" || len(rep.DeletedLocal) != 0 || !slices.Contains(rep.Conflicts, "x.txt") || len(trash) != 0 {
			t.Errorf("save after the look: laptop = %q, Trash holds %d, report = %+v", got, len(trash), rep)
		}
	}
}

// A file made at the laptop's name after a --pull-deletes move-aside is never replaced when a save is
// moved back: the save stays under its temp name and the refusal names it. Catches a move back that
// renames over the name, and one that fails without saying where the save is.
func TestPass_ALaptopFileMadeDuringAPullDeletesMoveBackIsKept(t *testing.T) {
	sy, box, root := pipeSyncer(t, true)
	putSyncFile(t, filepath.Join(root, "x.txt"), "laptop", 0o644)
	putSyncFile(t, filepath.Join(root, "keep.txt"), "kept", 0o644)
	pass(t, sy)
	if err := os.Remove(filepath.Join(box, "x.txt")); err != nil {
		t.Fatal(err)
	}
	sy.Opts.PullDeletes, sy.Opts.MaxPullDeletes, sy.Opts.Trash = true, 5, t.TempDir()
	oldD, oldL := deleteLooked, linkFile
	t.Cleanup(func() { deleteLooked, linkFile = oldD, oldL })
	deleteLooked = func() { editorSave(t, root, "x.txt", "laptop save") }
	linkFile = func(r *os.Root, from, to string) error {
		if to == "x.txt" {
			putSyncFile(t, filepath.Join(root, "x.txt"), "made since", 0o644)
		}
		return oldL(r, from, to)
	}
	rep := pass(t, sy)
	kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "made since" || len(kept) != 1 || getSyncFile(t, kept[0]) != "laptop save" ||
		len(rep.DeletedLocal) != 0 || len(rep.Refused) != 1 || !strings.Contains(rep.Refused[0].Reason, "it is kept as "+filepath.Base(kept[0])) {
		t.Errorf("x.txt = %q, kept = %v, report = %+v", got, kept, rep)
	}
}

// A laptop file the person deletes while its Trash copy is made is not moved aside, and the Trash
// copy of it is dropped, so Trash keeps nothing of a file nobody removed through the sync.
func TestPass_ATrashCopyIsDroppedWhenTheMoveAsideFails(t *testing.T) {
	sy, box, root := pipeSyncer(t, true)
	putSyncFile(t, filepath.Join(root, "x.txt"), "laptop", 0o644)
	putSyncFile(t, filepath.Join(root, "keep.txt"), "kept", 0o644)
	pass(t, sy)
	if err := os.Remove(filepath.Join(box, "x.txt")); err != nil {
		t.Fatal(err)
	}
	sy.Opts.PullDeletes, sy.Opts.MaxPullDeletes, sy.Opts.Trash = true, 5, t.TempDir()
	old := deleteLooked
	t.Cleanup(func() { deleteLooked = old })
	deleteLooked = func() { _ = os.Remove(filepath.Join(root, "x.txt")) }
	pass(t, sy)
	if trash, _ := filepath.Glob(filepath.Join(sy.Opts.Trash, "*")); len(trash) != 0 {
		t.Errorf("Trash holds %v after the move aside failed", trash)
	}
	if temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*")); len(temps) != 0 {
		t.Errorf("a temp file stays after the move aside failed: %v", temps)
	}
}
