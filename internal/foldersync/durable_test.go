// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var errFault = &os.PathError{Op: "fsync", Path: "x", Err: syscall.EIO}

// dirSyncs is whether directories are synced through fsyncDir here. Windows has no directory sync:
// the write-through move stands for it (durable_windows.go).
const dirSyncs = runtime.GOOS != "windows"

// unixDirSync skips a test, or a subtest, that is only about directory syncs where there are none.
func unixDirSync(t *testing.T) {
	t.Helper()
	if !dirSyncs {
		t.Skip("no directory sync on Windows: the write-through move stands for it")
	}
}

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
	unixDirSync(t)
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
		_, err := installDurable(root, nil, f, name, mode, "dst")
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
	unixDirSync(t)
	dir, root, name, f := tempBeside(t)
	onSync(t, func(*os.File) error { return nil }, func(*os.File) error { return errFault })
	_, err := installDurable(root, nil, f, name, InstallReplace, "dst")
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
		return st.SaveUnder(file, filepath.Dir(file))
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
	if err := save(3); dirSyncs && (!errors.Is(err, ErrNotDurable) || saved() != "3") {
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
				_ = st.SaveUnder(file, filepath.Dir(file))
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
	unixDirSync(t)
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
	unixDirSync(t)
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

// The `wardyn sync` state path: its first save makes .local/state/wardyn/sync under the home. A save
// that cannot sync the home fails; a retry finds every directory made and still syncs each one up
// to the home, and a failure that repeats keeps failing. A syncer with no state root never saves.
func TestSaveState_TheCLIStatePathSyncsUpToTheHomeOnEverySave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	file, err := StatePath("run-1")
	if err != nil {
		t.Fatal(err)
	}
	root, err := StateRoot()
	if err != nil || root != home {
		t.Fatalf("StateRoot = %q, %v, want %q", root, err, home)
	}
	sy := &Syncer{State: &State{Local: "/l", Remote: "/r", Files: map[string]FileSeen{}}, StateFile: file, StateRoot: root}
	dirs := []string{".local/state/wardyn/sync", ".local/state/wardyn", ".local/state", ".local", "."}
	var synced []string
	failHome := false
	onSync(t, nil, func(d *os.File) error {
		for _, p := range dirs {
			if isDir(d, filepath.Join(home, p)) {
				synced = append(synced, p)
				if p == "." && failHome {
					return errFault
				}
			}
		}
		return nil
	})
	for i, fail := range []bool{true, true, false} {
		if !dirSyncs {
			break
		}
		failHome, synced = fail, nil
		err := sy.saveState()
		if fail != errors.Is(err, ErrNotDurable) || (!fail && err != nil) {
			t.Errorf("save %d: err = %v", i+1, err)
		}
		if !slices.Equal(synced, dirs) {
			t.Errorf("save %d: synced %v, want %v", i+1, synced, dirs)
		}
	}
	sy.StateRoot = ""
	if err := sy.saveState(); err == nil {
		t.Error("a state with no root was saved")
	}
}

// logged sends the default logger to a buffer for one test.
func logged(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// A directory sync the filesystem does not offer (EINVAL, ENOTSUP or EOPNOTSUPP, however wrapped) is
// best effort: the conflict copy is kept, the push goes ahead, the new file is pulled and the state
// is saved. However many directories hit it, the state's own included, the pass warns once, naming
// the folder, and its report says so. The same errno from a file sync, or EIO from a directory
// sync, still refuses the push and fails the save.
func TestInstall_AnUnsupportedDirSyncIsBestEffortAndWarnedOnce(t *testing.T) {
	wrap := func(e error) error { return fmt.Errorf("sync: %w", &os.PathError{Op: "sync", Path: "d", Err: e}) }
	cases := []struct {
		name      string
		file, dir error
		ok        bool
	}{
		{"dir EINVAL", nil, syscall.EINVAL, true},
		{"dir ENOTSUP", nil, syscall.ENOTSUP, true},
		{"dir EOPNOTSUPP", nil, syscall.EOPNOTSUPP, true},
		{"file EINVAL", syscall.EINVAL, nil, false},
		{"dir EIO", nil, syscall.EIO, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.dir != nil {
				unixDirSync(t)
			}
			sy, box, root := runnerSyncer(t, true)
			bothChanged(t, sy, box, root)
			putSyncFile(t, filepath.Join(box, "a", "b", "new.txt"), "from the sandbox", 0o644)
			buf := logged(t)
			stateDirs := 0
			onSync(t, func(*os.File) error {
				if c.file != nil {
					return wrap(c.file)
				}
				return nil
			}, func(d *os.File) error {
				if isDir(d, filepath.Dir(sy.StateFile)) {
					stateDirs++
				}
				if c.dir != nil {
					return wrap(c.dir)
				}
				return nil
			})
			rep, err := sy.Pass(context.Background())
			sandbox := getSyncFile(t, filepath.Join(box, "x.txt"))
			warned := strings.Count(buf.String(), dirSyncWarning)
			if !c.ok {
				if err == nil || sandbox != "sandbox edit!" || warned != 0 {
					t.Errorf("%s: err = %v, sandbox = %q, warned %d times: want the push refused and the save failed", c.name, err, sandbox, warned)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if sandbox != "laptop edit" || getSyncFile(t, filepath.Join(root, copyName(1))) != "sandbox edit!" || !slices.Contains(rep.Pulled, "a/b/new.txt") {
				t.Errorf("%s: sandbox = %q, report = %+v", c.name, sandbox, rep)
			}
			if warned != 1 || !strings.Contains(buf.String(), "folder="+root) || rep.DurabilityWarning != dirSyncWarning || stateDirs == 0 {
				t.Errorf("%s: warned %d times, state dir synced %d times, report warning %q, log:\n%s", c.name, warned, stateDirs, rep.DurabilityWarning, buf)
			}
			if st, err := LoadState(sy.StateFile, root, box); err != nil || len(st.Files) == 0 {
				t.Errorf("%s: saved state = %+v, %v", c.name, st, err)
			}
		})
	}
}
