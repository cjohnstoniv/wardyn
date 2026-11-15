// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package foldersync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A failed copy someone rewrote in place to its own length, through its name, is the same file with the same size
// but not the bytes the copy wrote: it is never removed. Where the volume shows it as the copy it is moved aside and
// kept, and the refusal names the aside; where it does not, it stays at the name and the refusal names that.
// Catches a failed copy removed on its file number and size alone.
func TestPass_AFailedCopyRewrittenInPlaceToItsLengthIsKept(t *testing.T) {
	sy, root, name := copyCase(t, false)
	sync := fsyncFile
	onSync(t, func(f *os.File) error {
		if filepath.Base(f.Name()) != name {
			return sync(f)
		}
		g, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.WriteAt([]byte("edited in place!"), 0); err != nil { // the copy's own 16 bytes, rewritten
			t.Fatal(err)
		}
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
		return syscall.EIO
	}, nil)
	reason := refusedFor(pass(t, sy), name)
	temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	atName, nameErr := os.ReadFile(filepath.Join(root, name))
	switch {
	case len(temps) == 1:
		if got := getSyncFile(t, temps[0]); got != "edited in place!" || nameErr == nil ||
			!strings.Contains(reason, "it is kept as") || !strings.Contains(reason, filepath.Base(temps[0])) {
			t.Errorf("aside %q, laptop %s %q (%v), refusal %q: want the rewritten copy kept at the aside the refusal names", got, name, atName, nameErr, reason)
		}
	case len(temps) == 0:
		if nameErr != nil || string(atName) != "edited in place!" || !strings.Contains(reason, "its partial copy is left at "+name) {
			t.Errorf("laptop %s %q (%v), refusal %q: want the rewritten copy kept at its name and the refusal naming it", name, atName, nameErr, reason)
		}
	default:
		t.Errorf("temps %v: want at most one", temps)
	}
}

// A failed copy of an empty file is never removed: its size proves nothing, and a filesystem may give every empty
// file the same number, so an empty save at the name would pass for it. It stays at the name and the refusal names
// it. Catches an empty failed copy removed on its number and size alone.
func TestPass_AFailedEmptyCopyIsNamedNotRemoved(t *testing.T) {
	sy, box, root := runnerSyncer(t, true)
	putSyncFile(t, filepath.Join(box, "new.txt"), "", 0o644)
	onLink(t, syscall.EXDEV)
	failCopySync(t, "new.txt")
	reason := refusedFor(pass(t, sy), "new.txt")
	temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if got := getSyncFile(t, filepath.Join(root, "new.txt")); got != "" || len(temps) != 0 ||
		!strings.Contains(reason, "its partial copy is left at new.txt") {
		t.Errorf("laptop new.txt = %q, temps %v, refusal %q: want the empty copy kept at its name, no temp and the refusal naming it", got, temps, reason)
	}
}

// An editor that renames a failed copy to its backup name and saves at the name leaves its save there, and the
// refusal never calls that save the copy's partial copy: the copy's file number held from its create through its
// write, so the save at the name is another file. Catches a refusal that names whatever is at the name while the
// copy still has a name of its own.
func TestPass_AnEditorsBackupRenameOfAFailedCopyIsNotNamedAsItsPartialCopy(t *testing.T) {
	sy, root, name := copyCase(t, false)
	sync := fsyncFile
	onSync(t, func(f *os.File) error {
		if filepath.Base(f.Name()) != name {
			return sync(f)
		}
		if err := os.Rename(filepath.Join(root, name), filepath.Join(root, name+"~")); err != nil {
			t.Fatal(err)
		}
		editorSave(t, root, name, "editor save")
		return syscall.EIO
	}, nil)
	reason := refusedFor(pass(t, sy), name)
	temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	if got, backup := getSyncFile(t, filepath.Join(root, name)), getSyncFile(t, filepath.Join(root, name+"~")); got != "editor save" ||
		backup != "from the sandbox" || len(temps) != 0 || reason == "" || strings.Contains(reason, "its partial copy is left at") {
		t.Errorf("laptop %s = %q, backup %q, temps %v, refusal %q: want the editor's save and backup kept, no temp, and a refusal that names no partial copy",
			name, got, backup, temps, reason)
	}
}

func failCopyStamp(t *testing.T, name string) {
	t.Helper()
	old := setModTime
	t.Cleanup(func() { setModTime = old })
	setModTime = func(f *os.File, at time.Time) error {
		if filepath.Base(f.Name()) == name {
			return syscall.EIO
		}
		return old(f, at)
	}
}

// Where a volume has the handle report another number than the copy's name until the copy's sync, a copy that fails
// before that sync is still at its name, so the refusal names it.
// Catches a number from before the sync trusted as proof the file at the name is not the copy.
func TestPass_AFailedCopyBeforeItsNumberSettlesIsNamed(t *testing.T) {
	sy, root, name := copyCase(t, false)
	idSettlesOnSync(t, name, nil)
	failCopyStamp(t, name)
	reason := refusedFor(pass(t, sy), name)
	if _, err := os.Lstat(filepath.Join(root, name)); err != nil || !strings.Contains(reason, "its partial copy is left at "+name) {
		t.Errorf("laptop %s: %v, refusal %q: want the partial copy kept and named", name, err, reason)
	}
}

// A copy that fails before its sync, on whatever the volume numbers files like, is removed or kept and named, and
// leaves no temp.
func TestPass_AFailedCopyBeforeItsSyncLeavesNoUnnamedPartial(t *testing.T) {
	sy, root, name := copyCase(t, false)
	failCopyStamp(t, name)
	reason := refusedFor(pass(t, sy), name)
	temps, _ := filepath.Glob(filepath.Join(root, ".wardyn-sync-*"))
	_, err := os.Lstat(filepath.Join(root, name))
	if reason == "" || len(temps) != 0 || (!errors.Is(err, os.ErrNotExist) && !strings.Contains(reason, "its partial copy is left at "+name)) {
		t.Errorf("laptop %s: %v, temps %v, refusal %q: want the partial copy removed, or kept and named, and no temp", name, err, temps, reason)
	}
}

// The handle and the name told one number at the create, the handle told another once the copy was written, and an
// editor then renamed the copy to a backup name and saved at the name: the number did not hold, so the file at the
// name is not proved to be another file and the refusal names it.
// Catches the create-time number trusted without the handle reporting it still.
func TestPass_AnEditorsBackupRenameWhereTheCopysNumberChangedIsNamed(t *testing.T) {
	sy, root, name := copyCase(t, false)
	decoy := filepath.Join(t.TempDir(), "decoy")
	putSyncFile(t, decoy, "decoy", 0o644)
	oldS, oldF := statCopy, fsyncFile
	t.Cleanup(func() { statCopy, fsyncFile = oldS, oldF })
	created := false
	statCopy = func(f *os.File) (os.FileInfo, error) {
		if filepath.Base(f.Name()) != name || !created {
			created = created || filepath.Base(f.Name()) == name
			return oldS(f)
		}
		return os.Lstat(decoy)
	}
	fsyncFile = func(f *os.File) error {
		if filepath.Base(f.Name()) != name {
			return oldF(f)
		}
		if err := oldF(f); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(root, name), filepath.Join(root, name+"~")); err != nil {
			t.Fatal(err)
		}
		editorSave(t, root, name, "editor save")
		return syscall.EIO
	}
	reason := refusedFor(pass(t, sy), name)
	if got, backup := getSyncFile(t, filepath.Join(root, name)), getSyncFile(t, filepath.Join(root, name+"~")); got != "editor save" ||
		backup != "from the sandbox" || !strings.Contains(reason, "its partial copy is left at "+name) {
		t.Errorf("laptop %s = %q, backup %q, refusal %q: want both kept and the name named, since the number did not hold", name, got, backup, reason)
	}
}

// An editor's save renamed over a copy whose number changed after its create, and that then fails, is never named as
// the copy's partial copy: the number did not hold, but the copy's handle has no name left. Catches a save called the
// partial copy wherever the create-time number is not trusted.
func TestPass_AnEditorsSaveOverAFailedCopyWhoseNumberChangedIsNotNamedAsItsPartialCopy(t *testing.T) {
	sy, root, name := copyCase(t, false)
	idSettlesOnSync(t, name, nil)
	sync := fsyncFile
	onSync(t, func(f *os.File) error {
		if filepath.Base(f.Name()) != name {
			return sync(f)
		}
		if err := sync(f); err != nil {
			t.Fatal(err)
		}
		editorSave(t, root, name, "editor save")
		return syscall.EIO
	}, nil)
	reason := refusedFor(pass(t, sy), name)
	if got := getSyncFile(t, filepath.Join(root, name)); got != "editor save" || reason == "" || strings.Contains(reason, "its partial copy is left at") {
		t.Errorf("laptop %s = %q, refusal %q: want the editor's save in place and a refusal that does not call it the partial copy", name, got, reason)
	}
}
