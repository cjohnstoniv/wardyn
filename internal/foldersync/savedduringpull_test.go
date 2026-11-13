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
	"time"
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

// refusedFor is the reason a pass refused rel, or "" when it did not.
func refusedFor(rep *Report, rel string) string {
	for _, r := range rep.Refused {
		if r.Path == rel {
			return r.Reason
		}
	}
	return ""
}

// copyCase is a pass whose copy of a file into the laptop's name copies (links refused, so copyNew):
// a pull of new.txt, or a --pull-deletes put-back of a save to x.txt (the first save, "laptop save",
// lands after the delete's Trash copy).
func copyCase(t *testing.T, putBack bool) (sy *Syncer, root, name string) {
	t.Helper()
	sy, box, root := runnerSyncer(t, true)
	name = "new.txt"
	if putBack {
		name = "x.txt"
		putSyncFile(t, filepath.Join(root, "x.txt"), "laptop", 0o644)
		putSyncFile(t, filepath.Join(root, "keep.txt"), "kept", 0o644)
		pass(t, sy)
		if err := os.Remove(filepath.Join(box, "x.txt")); err != nil {
			t.Fatal(err)
		}
		sy.Opts.PullDeletes, sy.Opts.MaxPullDeletes, sy.Opts.Trash = true, 5, t.TempDir()
		old := deleteLooked
		t.Cleanup(func() { deleteLooked = old })
		deleteLooked = func() { editorSave(t, root, "x.txt", "laptop save") }
	} else {
		putSyncFile(t, filepath.Join(box, "new.txt"), "from the sandbox", 0o644)
	}
	onLink(t, syscall.EXDEV)
	return sy, root, name
}

// A copy into the laptop's name that fails part-way, on a filesystem without hard links, removes only
// its own part copy: an editor's save renamed over the name during the copy stays there, and one that
// lands as the part copy is moved off is kept under a name the refusal gives. Catches a failed copy
// that removes whatever is at its name, for a pull of a new file and for a --pull-deletes put-back.
func TestPass_AFailedCopyNeverRemovesAnEditorsSave(t *testing.T) {
	for _, tc := range []struct {
		name          string
		putBack, drop bool
		laptop, kept  string
	}{
		{name: "pull of a new file", laptop: "editor save"},
		{name: "delete put-back", putBack: true, laptop: "editor save", kept: "laptop save"},
		{name: "saved as the part copy is moved off", drop: true, kept: "editor save"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sy, root, name := copyCase(t, tc.putBack)
			save := func() { editorSave(t, root, name, "editor save") }
			sync, oldD := fsyncFile, dropLooked
			t.Cleanup(func() { dropLooked = oldD })
			synced := false
			onSync(t, func(f *os.File) error {
				if filepath.Base(f.Name()) != name || synced {
					return sync(f)
				}
				if synced = true; !tc.drop {
					save()
				}
				return syscall.EIO
			}, nil)
			if tc.drop {
				dropLooked = save
			}
			rep := pass(t, sy)
			if tc.laptop != "" {
				if got := getSyncFile(t, filepath.Join(root, name)); got != tc.laptop {
					t.Errorf("laptop %s = %q, want %q", name, got, tc.laptop)
				}
			} else if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("laptop %s: %v, want it moved off with the part copy", name, err)
			}
			kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
			reason := refusedFor(rep, name)
			if tc.kept == "" {
				if len(kept) != 0 || reason == "" {
					t.Errorf("temps %v, refusal %q, want none left and %s refused", kept, reason, name)
				}
			} else if len(kept) != 1 || getSyncFile(t, kept[0]) != tc.kept || !strings.Contains(reason, "it is kept as "+filepath.Base(kept[0])) {
				t.Errorf("kept %v, refusal %q, want %q kept under the name the refusal gives", kept, reason, tc.kept)
			}
			if _, ok := sy.State.Files[name]; slices.Contains(rep.Pulled, name) || len(rep.DeletedLocal) != 0 || (ok && !tc.putBack) {
				t.Errorf("report = %+v, recorded %v: want nothing pulled, deleted or recorded", rep, ok)
			}
		})
	}
}

// A file saved at the laptop's name while a copy into it is made, on a filesystem without hard links,
// is neither stamped with the copy's time nor taken for the copy: a pull of a new file lists a
// conflict, keeping the sandbox's copy beside it, and records nothing; a --pull-deletes put-back
// keeps its save under a name the refusal gives. Catches a copy that sets its time by the name, and
// one that never checks that its name still holds it.
func TestPass_ACopyNeverStampsOrClaimsAnEditorsSave(t *testing.T) {
	for _, putBack := range []bool{false, true} {
		t.Run(map[bool]string{false: "pull of a new file", true: "delete put-back"}[putBack], func(t *testing.T) {
			sy, root, name := copyCase(t, putBack)
			old := setModTime
			t.Cleanup(func() { setModTime = old })
			var saved os.FileInfo
			setModTime = func(f *os.File, at time.Time) error {
				if filepath.Base(f.Name()) == name && saved == nil {
					// The sandbox's length, at a time no other file here has.
					editorSave(t, root, name, "editor's version")
					p, own := filepath.Join(root, name), time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
					var err error
					if err = os.Chtimes(p, own, own); err == nil {
						saved, err = os.Lstat(p)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				return old(f, at)
			}
			rep := pass(t, sy)
			got, err := os.Lstat(filepath.Join(root, name))
			if err != nil || saved == nil {
				t.Fatalf("laptop %s: %v, saved %v", name, err, saved != nil)
			}
			if body := getSyncFile(t, filepath.Join(root, name)); body != "editor's version" || !got.ModTime().Equal(saved.ModTime()) {
				t.Errorf("laptop %s = %q at %v, want the editor's save at its own time %v", name, body, got.ModTime(), saved.ModTime())
			}
			if putBack {
				kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
				if len(kept) != 1 || getSyncFile(t, kept[0]) != "laptop save" || !strings.Contains(refusedFor(rep, name), "it is kept as "+filepath.Base(kept[0])) {
					t.Errorf("kept %v, report = %+v, want the first save kept under the name the refusal gives", kept, rep)
				}
				return
			}
			_, recorded := sy.State.Files[name]
			if slices.Contains(rep.Pulled, name) || !slices.Contains(rep.Conflicts, name) || recorded ||
				getSyncFile(t, filepath.Join(root, conflictNames(name, testTag)[0])) != "from the sandbox" {
				t.Errorf("report = %+v, recorded %v: want a listed conflict with the sandbox copy kept, nothing recorded", rep, recorded)
			}
		})
	}
}

// failCopySync fails the sync of a copy into the laptop's name with EIO, so the copy is dropped.
func failCopySync(t *testing.T, name string) {
	t.Helper()
	sync := fsyncFile
	onSync(t, func(f *os.File) error {
		if filepath.Base(f.Name()) == name {
			return syscall.EIO
		}
		return sync(f)
	}, nil)
}

// onDropLooked runs save between a failed copy's look at its name and its move aside (dropLooked).
func onDropLooked(t *testing.T, save func()) {
	t.Helper()
	old := dropLooked
	t.Cleanup(func() { dropLooked = old })
	dropLooked = save
}

// Two saves that land as a failed copy's partial copy is moved off: the first takes the copy's place,
// which frees the copy's inode number unless the copy is still open, and the second can then be given
// that number. The second save is kept under a name the refusal gives. Catches a drop that tells its
// copy by a number another file may hold; on a filesystem that never gives a number out twice the
// case passes either way, so it runs four times.
func TestPass_ASecondSaveIsNeverTakenForTheFailedCopy(t *testing.T) {
	for range 4 {
		sy, root, name := copyCase(t, false)
		failCopySync(t, name)
		onDropLooked(t, func() {
			editorSave(t, root, name, "first save")
			editorSave(t, root, name, "second save")
		})
		rep := pass(t, sy)
		kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
		if reason := refusedFor(rep, name); len(kept) != 1 || getSyncFile(t, kept[0]) != "second save" || !strings.Contains(reason, "it is kept as "+filepath.Base(kept[0])) {
			t.Fatalf("kept %v, refusal %q, want the second save kept under the name the refusal gives", kept, reason)
		}
	}
}

// A --pull-deletes put-back whose copy fails, with a save landing as the partial copy is moved off,
// names both files it keeps: the first save and the later one.
func TestPass_APutBackNamesALaterSaveItsFailedCopyMovedAside(t *testing.T) {
	sy, root, name := copyCase(t, true)
	failCopySync(t, name)
	onDropLooked(t, func() { editorSave(t, root, name, "editor save") })
	reason := refusedFor(pass(t, sy), name)
	kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	var bodies []string
	for _, k := range kept {
		bodies = append(bodies, getSyncFile(t, k))
		if !strings.Contains(reason, "it is kept as "+filepath.Base(k)) {
			t.Errorf("refusal %q does not name %s", reason, k)
		}
	}
	if slices.Sort(bodies); !slices.Equal(bodies, []string{"editor save", "laptop save"}) {
		t.Errorf("kept %q, want the first save and the later one", bodies)
	}
}

// A partial copy that cannot be moved aside stays at its name and the refusal says so; the empty
// temp it was to be moved onto is removed. A directory made at the name stands in for the failure:
// it cannot be moved onto a file.
func TestPass_APartialCopyThatCannotBeMovedAsideIsNamed(t *testing.T) {
	sy, root, name := copyCase(t, false)
	failCopySync(t, name)
	onDropLooked(t, func() {
		p := filepath.Join(root, name)
		if err := errors.Join(os.Remove(p), os.Mkdir(p, 0o755)); err != nil {
			t.Fatal(err)
		}
	})
	reason := refusedFor(pass(t, sy), name)
	if temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*")); len(temps) != 0 || !strings.Contains(reason, "its partial copy is left at "+name) {
		t.Errorf("temps %v, refusal %q, want no temp and the partial copy named", temps, reason)
	}
}

// A laptop name that is gone when a put-back's copy into it is done, as when the file is deleted
// while it is copied, is not taken for the copy: the save stays under the name the refusal gives.
func TestPass_APutBackCopyWhoseNameIsGoneKeepsTheSave(t *testing.T) {
	sy, root, name := copyCase(t, true)
	old := setModTime
	t.Cleanup(func() { setModTime = old })
	setModTime = func(f *os.File, at time.Time) error {
		if filepath.Base(f.Name()) == name {
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
		return old(f, at)
	}
	reason := refusedFor(pass(t, sy), name)
	kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if len(kept) != 1 || getSyncFile(t, kept[0]) != "laptop save" || !strings.Contains(reason, "it is kept as "+filepath.Base(kept[0])) {
		t.Errorf("kept %v, refusal %q, want the save kept under the name the refusal gives", kept, reason)
	}
}
