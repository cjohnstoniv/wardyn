// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package foldersync

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// idSettlesOnSync gives the copy into name another file number until its sync, as exFAT gives a file
// a new number once its bytes land: the handle reports a decoy's number before the copy's sync, and
// its own after. With after set, the handle's stat fails after the sync instead.
func idSettlesOnSync(t *testing.T, name string, after error) {
	t.Helper()
	decoy := filepath.Join(t.TempDir(), "decoy")
	putSyncFile(t, decoy, "decoy", 0o644)
	oldS, oldF := statCopy, fsyncFile
	t.Cleanup(func() { statCopy, fsyncFile = oldS, oldF })
	synced := false
	fsyncFile = func(f *os.File) error {
		if filepath.Base(f.Name()) == name {
			synced = true
		}
		return oldF(f)
	}
	statCopy = func(f *os.File) (os.FileInfo, error) {
		switch {
		case filepath.Base(f.Name()) != name:
			return oldS(f)
		case !synced:
			return os.Lstat(decoy)
		case after != nil:
			return nil, after
		}
		return oldS(f)
	}
}

// putBackCase is copyCase's --pull-deletes put-back, with save as the laptop's save the put-back copies.
func putBackCase(t *testing.T, save string) (*Syncer, string, string) {
	t.Helper()
	sy, root, name := copyCase(t, true)
	deleteLooked = func() { editorSave(t, root, name, save) }
	return sy, root, name
}

// putBackKept checks a put-back that copied the save back: the laptop holds it, a conflict is listed
// and no temp is left.
func putBackKept(t *testing.T, rep *Report, root, name, save string) {
	t.Helper()
	temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if got := getSyncFile(t, filepath.Join(root, name)); got != save || len(temps) != 0 || !slices.Contains(rep.Conflicts, name) {
		t.Errorf("laptop %s = %q, temps %v, report = %+v: want the save copied back, a conflict listed and no temp", name, got, temps, rep)
	}
}

// A put-back on a filesystem that numbers a file anew once its bytes land copies the save back and
// leaves no second copy of it: the copy is told by the number its handle reports after the sync.
// Catches a copy told by the number it had when it was made.
func TestPass_APutBackWhoseFileIDChangesLeavesNoDuplicate(t *testing.T) {
	sy, root, name := putBackCase(t, "laptop save")
	idSettlesOnSync(t, name, nil)
	putBackKept(t, pass(t, sy), root, name, "laptop save")
}

// An empty save is put back the same way: no bytes written is a size the copy proves.
func TestPass_APutBackOfAnEmptySaveWhoseFileIDChangesLeavesNoDuplicate(t *testing.T) {
	sy, root, name := putBackCase(t, "")
	idSettlesOnSync(t, name, nil)
	putBackKept(t, pass(t, sy), root, name, "")
}

// An editor's save renamed over the name while a put-back copies into it, the copy's own length, is
// never taken for the copy where the file number changes after the write: the laptop keeps the editor's
// save and the refusal names the first save it keeps. Catches a copy proved by its size alone.
func TestPass_ACopyWhoseFileIDChangesNeverTakesAnEditorsSave(t *testing.T) {
	sy, root, name := putBackCase(t, "laptop save")
	idSettlesOnSync(t, name, nil)
	old := setModTime
	t.Cleanup(func() { setModTime = old })
	setModTime = func(f *os.File, at time.Time) error {
		if filepath.Base(f.Name()) == name {
			editorSave(t, root, name, "editor save") // the copy's length
		}
		return old(f, at)
	}
	reason := refusedFor(pass(t, sy), name)
	kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if got := getSyncFile(t, filepath.Join(root, name)); got != "editor save" || len(kept) != 1 || getSyncFile(t, kept[0]) != "laptop save" ||
		!strings.Contains(reason, "it is kept as "+filepath.Base(kept[0])) {
		t.Errorf("laptop %s = %q, kept %v, refusal %q: want the editor's save in place and the first save kept under the name the refusal gives", name, got, kept, reason)
	}
}

// A copy that fails where the file number changes after the write leaves nothing unnamed at the
// person's name: proved to be the copy, it is removed; not proved, as when its handle cannot be read, it
// stays and the refusal names it. Catches a failed copy left at the name with a refusal that never says so.
func TestPass_AFailedCopyWhoseFileIDChangesLeavesNoUnnamedPartial(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after error
	}{{name: "proved and removed"}, {name: "not proved and named", after: syscall.EIO}} {
		t.Run(tc.name, func(t *testing.T) {
			sy, root, name := copyCase(t, false)
			failCopySync(t, name)
			idSettlesOnSync(t, name, tc.after)
			reason := refusedFor(pass(t, sy), name)
			temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
			_, err := os.Lstat(filepath.Join(root, name))
			if tc.after == nil && (!errors.Is(err, os.ErrNotExist) || len(temps) != 0 || reason == "") {
				t.Errorf("laptop %s: %v, temps %v, refusal %q: want the partial copy removed, no temp and %s refused", name, err, temps, reason, name)
			}
			if tc.after != nil && (err != nil || len(temps) != 0 || !strings.Contains(reason, "its partial copy is left at "+name)) {
				t.Errorf("laptop %s: %v, temps %v, refusal %q: want the partial copy kept, no temp and the refusal naming it", name, err, temps, reason)
			}
		})
	}
}

// A failed copy someone wrote into in place, through its name, is the same file but no longer the
// bytes the copy wrote: it stays at the name and the refusal names it. Catches a copy proved by its
// file number alone.
func TestPass_AFailedCopyWrittenIntoInPlaceIsNamedNotRemoved(t *testing.T) {
	sy, root, name := copyCase(t, false)
	sync := fsyncFile
	onSync(t, func(f *os.File) error {
		if filepath.Base(f.Name()) != name {
			return sync(f)
		}
		g, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			_, err = g.WriteString(" and an edit")
			err = errors.Join(err, g.Close())
		}
		if err != nil {
			t.Fatal(err)
		}
		return syscall.EIO
	}, nil)
	reason := refusedFor(pass(t, sy), name)
	temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if got := getSyncFile(t, filepath.Join(root, name)); got != "from the sandbox and an edit" || len(temps) != 0 ||
		!strings.Contains(reason, "its partial copy is left at "+name) {
		t.Errorf("laptop %s = %q, temps %v, refusal %q: want the written-into copy kept and the refusal naming it", name, got, temps, reason)
	}
}

// A --pull-deletes put-back whose copy fails and is not proved, as when its handle cannot be read, names
// both files: the save it keeps and the partial copy at the laptop's name. Catches a put-back refusal
// that names only the save.
func TestPass_AFailedPutBackCopyNotProvedNamesItsPartialCopy(t *testing.T) {
	sy, root, name := copyCase(t, true)
	failCopySync(t, name)
	idSettlesOnSync(t, name, syscall.EIO)
	reason := refusedFor(pass(t, sy), name)
	kept, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if _, err := os.Lstat(filepath.Join(root, name)); err != nil || len(kept) != 1 || getSyncFile(t, kept[0]) != "laptop save" ||
		!strings.Contains(reason, "it is kept as "+filepath.Base(kept[0])) || !strings.Contains(reason, "its partial copy is left at "+name) {
		t.Errorf("laptop %s: %v, kept %v, refusal %q: want the save kept and both files named", name, err, kept, reason)
	}
}

// An editor's save renamed over a copy that then fails is never named as the copy's partial copy: the
// copy's handle has no name left. Catches a copy told from a save by its file number alone.
func TestPass_AnEditorsSaveOverAFailedCopyIsNotNamedAsItsPartialCopy(t *testing.T) {
	sy, root, name := copyCase(t, false)
	sync := fsyncFile
	onSync(t, func(f *os.File) error {
		if filepath.Base(f.Name()) != name {
			return sync(f)
		}
		editorSave(t, root, name, "editor save")
		return syscall.EIO
	}, nil)
	reason := refusedFor(pass(t, sy), name)
	if got := getSyncFile(t, filepath.Join(root, name)); got != "editor save" || reason == "" || strings.Contains(reason, "its partial copy is left at") {
		t.Errorf("laptop %s = %q, refusal %q: want the editor's save in place and a refusal that does not call it the partial copy", name, got, reason)
	}
}
