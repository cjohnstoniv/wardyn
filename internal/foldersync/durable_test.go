// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var errFault = &os.PathError{Op: "fsync", Path: "x", Err: syscall.EIO}

// onSync replaces the two sync calls for one test.
func onSync(t *testing.T, file, dir func(*os.File) error) {
	t.Helper()
	oldF, oldD := fsyncFile, fsyncDir
	t.Cleanup(func() { fsyncFile, fsyncDir = oldF, oldD })
	if file != nil {
		fsyncFile = file
	}
	if dir != nil {
		fsyncDir = dir
	}
}

// isDir reports whether f is the directory at p.
func isDir(f *os.File, p string) bool {
	a, aerr := f.Stat()
	b, berr := os.Stat(p)
	return aerr == nil && berr == nil && os.SameFile(a, b)
}

// laptopTemp reports whether f is a pull's temp file on the laptop (not the state's).
func laptopTemp(f *os.File) bool { return strings.Contains(f.Name(), ".wardyn-sync-") }

// changedInSandbox is x.txt sent by a first pass, then changed in the sandbox only.
func changedInSandbox(t *testing.T, sy *Syncer, box, root string) {
	t.Helper()
	putSyncFile(t, filepath.Join(root, "x.txt"), "laptop", 0o644)
	if _, err := sy.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	putSyncFile(t, filepath.Join(box, "x.txt"), "sandbox edit", 0o644)
	later := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(box, "x.txt"), later, later); err != nil {
		t.Fatal(err)
	}
}

func TestInstall_PullSyncFailureKeepsTheLaptopFile(t *testing.T) {
	sy, box, root := pipeSyncer(t, true)
	changedInSandbox(t, sy, box, root)
	before := sy.State.Files["x.txt"]
	onSync(t, func(f *os.File) error {
		if laptopTemp(f) {
			return errFault
		}
		return nil
	}, nil)
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "laptop" || len(rep.Pulled) != 0 || len(rep.Refused) != 1 {
		t.Errorf("laptop = %q, report = %+v", got, rep)
	}
	if sy.State.Files["x.txt"] != before {
		t.Error("a pull that never reached the disk was recorded")
	}
	if ents, _ := os.ReadDir(root); len(ents) != 1 {
		t.Errorf("laptop holds %d entries, want x.txt alone (the temp file removed)", len(ents))
	}
}

// After the rename the new file is in place, but a failed directory sync leaves it unproven: the
// pull is refused and not recorded, so the next pass looks again.
func TestInstall_PullDirSyncFailureIsRefusedAndNotRecorded(t *testing.T) {
	sy, box, root := pipeSyncer(t, true)
	changedInSandbox(t, sy, box, root)
	before := sy.State.Files["x.txt"]
	onSync(t, nil, func(d *os.File) error {
		if isDir(d, root) {
			return errFault
		}
		return nil
	})
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pulled) != 0 || len(rep.Refused) != 1 || sy.State.Files["x.txt"] != before {
		t.Errorf("report = %+v, record changed = %v", rep, sy.State.Files["x.txt"] != before)
	}
	if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "sandbox edit" {
		t.Errorf("laptop = %q, want the installed (unproven) copy", got)
	}
}

// tempBeside is a root on a new directory holding dst ("old") and an open temp file ("new").
func tempBeside(t *testing.T) (string, *os.Root, string, *os.File) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	putSyncFile(t, filepath.Join(dir, "dst"), "old", 0o644)
	name, f, err := createTemp(root, ".", "t.", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("new")
	return dir, root, name, f
}

func TestInstall_PreInstallFailureLeavesDst(t *testing.T) {
	for _, mode := range []InstallMode{InstallReplace, InstallNew} {
		dir, root, name, f := tempBeside(t)
		onSync(t, func(*os.File) error { return errFault }, nil)
		_, err := installDurable(root, f, name, mode, "dst")
		var ie *InstallError
		if !errors.As(err, &ie) || ie.Installed || ie.Step != "sync-file" || errors.Is(err, ErrNotDurable) {
			t.Errorf("mode %d: err = %v", mode, err)
		}
		if got := getSyncFile(t, filepath.Join(dir, "dst")); got != "old" {
			t.Errorf("mode %d: dst = %q", mode, got)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 1 {
			t.Errorf("mode %d: %d entries left, want dst alone", mode, len(ents))
		}
	}
}

func TestInstall_PostInstallDirSyncFailureIsInstalledNotDurable(t *testing.T) {
	dir, root, name, f := tempBeside(t)
	onSync(t, func(*os.File) error { return nil }, func(*os.File) error { return errFault })
	_, err := installDurable(root, f, name, InstallReplace, "dst")
	var ie *InstallError
	if !errors.As(err, &ie) || !ie.Installed || ie.Step != "sync-dir" || !errors.Is(err, ErrNotDurable) {
		t.Errorf("err = %v", err)
	}
	if got := getSyncFile(t, filepath.Join(dir, "dst")); got != "new" {
		t.Errorf("dst = %q, want the new content visible", got)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("%d entries left, want dst alone", len(ents))
	}
}

func TestInstall_StateSaveFailureKeepsThePreviousState(t *testing.T) {
	file := filepath.Join(t.TempDir(), "run.json")
	save := func(n int) error {
		st := &State{Local: "/l", Remote: "/r", Files: map[string]FileSeen{fmt.Sprint(n): {}}}
		return st.Save(file)
	}
	saved := func() string {
		st, err := LoadState(file, "/l", "/r")
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(keysOf(st.Files), ",")
	}
	if err := save(1); err != nil {
		t.Fatal(err)
	}
	onSync(t, func(*os.File) error { return errFault }, nil)
	if err := save(2); err == nil || errors.Is(err, ErrNotDurable) || saved() != "1" {
		t.Errorf("file sync failure: err = %v, saved = %s, want the previous state", err, saved())
	}
	onSync(t, func(*os.File) error { return nil }, func(*os.File) error { return errFault })
	if err := save(3); !errors.Is(err, ErrNotDurable) || saved() != "3" {
		t.Errorf("directory sync failure: err = %v, saved = %s", err, saved())
	}
	if ents, _ := os.ReadDir(filepath.Dir(file)); len(ents) != 1 {
		t.Errorf("%d entries beside the state, want none but it", len(ents)-1)
	}
}

func TestSave_ConcurrentSavesLeaveALoadableState(t *testing.T) {
	file := filepath.Join(t.TempDir(), "run.json")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			st := &State{Local: "/l", Remote: "/r", Files: map[string]FileSeen{fmt.Sprint(i): {Local: Side{Size: int64(i)}}}}
			for range 20 {
				_ = st.Save(file)
			}
		})
	}
	wg.Wait()
	st, err := LoadState(file, "/l", "/r")
	if err != nil || len(st.Files) != 1 {
		t.Fatalf("state = %+v, err = %v", st, err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(file)); len(ents) != 1 {
		t.Errorf("%d temp files left", len(ents)-1)
	}
}

func TestLoadState_RemovesTheTempFilesOfAStoppedSave(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "run.json")
	for _, n := range []string{"run.json.0123456789abcdef.tmp", "run.json.tmp", "run.json.notes", "other.json.0123456789abcdef.tmp"} {
		putSyncFile(t, filepath.Join(dir, n), "x", 0o600)
	}
	if _, err := LoadState(file, "/l", "/r"); err != nil {
		t.Fatal(err)
	}
	var left []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		left = append(left, e.Name())
	}
	if want := []string{"other.json.0123456789abcdef.tmp", "run.json.notes"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

// A pulled file in new directories: once it is installed, its directory and every one above it are
// synced, the root last.
func TestInstall_NewDirectoriesSyncEveryDirectoryUpToTheRoot(t *testing.T) {
	sy, box, root := pipeSyncer(t, true)
	putSyncFile(t, filepath.Join(box, "a", "b", "c", "new.txt"), "from the sandbox", 0o644)
	var synced []string
	onSync(t, nil, func(d *os.File) error {
		for _, p := range []string{".", "a", "a/b", "a/b/c"} {
			if isDir(d, filepath.Join(root, p)) {
				synced = append(synced, p)
			}
		}
		return nil
	})
	if _, err := sy.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b/c", "a/b", "a", "."}; !slices.Equal(synced, want) {
		t.Errorf("synced %v, want %v", synced, want)
	}
}

// A state save whose root sync fails is not durable. A retry finds the state's directory already
// made and still syncs every directory up to the root; a failure that repeats fails it again.
func TestSaveUnder_ARetrySyncsEveryDirectoryUpToTheRoot(t *testing.T) {
	top := t.TempDir()
	file := filepath.Join(top, "folders", "run.json")
	st := &State{Local: "/l", Remote: "/r", Files: map[string]FileSeen{}}
	var synced []string
	failTop := true
	onSync(t, nil, func(d *os.File) error {
		switch {
		case isDir(d, filepath.Join(top, "folders")):
			synced = append(synced, "folders")
		case isDir(d, top):
			synced = append(synced, "top")
			if failTop {
				return errFault
			}
		}
		return nil
	})
	for i, fail := range []bool{true, true, false} {
		failTop, synced = fail, nil
		err := st.SaveUnder(file, top)
		if fail != errors.Is(err, ErrNotDurable) || (!fail && err != nil) {
			t.Errorf("save %d: err = %v", i+1, err)
		}
		if want := []string{"folders", "top"}; !slices.Equal(synced, want) {
			t.Errorf("save %d: synced %v, want %v", i+1, synced, want)
		}
	}
	if err := st.SaveUnder(file, filepath.Join(top, "other")); err == nil {
		t.Error("a state outside its root was saved")
	}
}
