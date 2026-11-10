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

func TestInstall_ErrorsTellAnUnprovenInstallFromAnUntouchedOne(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	put := func() (string, *os.File) {
		name, f, err := createTemp(root, ".", "t.", "")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("new")
		return name, f
	}
	putSyncFile(t, filepath.Join(dir, "dst"), "old", 0o644)

	onSync(t, func(*os.File) error { return errFault }, nil)
	name, f := put()
	_, err = installDurable(root, f, name, InstallReplace, "dst")
	var ie *InstallError
	if !errors.As(err, &ie) || ie.Installed || errors.Is(err, ErrNotDurable) || getSyncFile(t, filepath.Join(dir, "dst")) != "old" {
		t.Errorf("file sync failure: err = %v, dst = %q", err, getSyncFile(t, filepath.Join(dir, "dst")))
	}

	onSync(t, func(*os.File) error { return nil }, func(*os.File) error { return errFault })
	name, f = put()
	_, err = installDurable(root, f, name, InstallReplace, "dst")
	if !errors.As(err, &ie) || !ie.Installed || !errors.Is(err, ErrNotDurable) || getSyncFile(t, filepath.Join(dir, "dst")) != "new" {
		t.Errorf("directory sync failure: err = %v, dst = %q", err, getSyncFile(t, filepath.Join(dir, "dst")))
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

// A pulled file in new directories: the parent of each new directory is synced, up to the first
// that existed, and the file's own directory once it is installed.
func TestInstall_NewDirectoriesSyncTheirParents(t *testing.T) {
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
	if want := []string{"a/b", "a", ".", "a/b/c"}; !slices.Equal(synced, want) {
		t.Errorf("synced %v, want %v", synced, want)
	}
}
