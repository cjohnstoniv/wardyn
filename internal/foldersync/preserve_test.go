// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

// failLaptopDirs makes every sync of a directory on the laptop fail with errno, and nothing else.
func failLaptopDirs(t *testing.T, root string, errno syscall.Errno) {
	t.Helper()
	onSync(t, nil, func(d *os.File) error {
		if fi, err := d.Stat(); err == nil {
			if r, rerr := os.Stat(root); rerr == nil && os.SameFile(fi, r) {
				return &os.PathError{Op: "sync", Path: d.Name(), Err: errno}
			}
		}
		return nil
	})
}

// newInSandbox is a runner pass back and a file only the sandbox has, not yet recorded.
func newInSandbox(t *testing.T) (*Syncer, string, string) {
	t.Helper()
	sy, box, root := runnerSyncer(t, true)
	putSyncFile(t, filepath.Join(box, "new.txt"), "from the sandbox", 0o644)
	return sy, box, root
}

func pass(t *testing.T, sy *Syncer) *Report {
	t.Helper()
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func notKept(t *testing.T, name string, sy *Syncer, rep *Report, why string) {
	t.Helper()
	if !strings.Contains(rep.NotPreserved, why) || rep.Unsynced != 1 || !slices.Equal(rep.UnsyncedPaths, []string{"new.txt"}) || len(rep.Refused) != 1 || rep.Refused[0].Path != "new.txt" {
		t.Errorf("%s: report = %+v, want new.txt refused, not brought back and %q not preserved", name, rep, why)
	}
	if _, ok := sy.State.Files["new.txt"]; ok {
		t.Errorf("%s: a pull not proven kept was recorded", name)
	}
}

// T1, T2, T3: a pull whose directory sync fails is not preserved. Its retry finds the same bytes on
// both sides and records them only once the directory is synced: with the fault gone it is in step,
// with no conflict copy; with the fault still there it is still not preserved.
func TestPass_APullWhoseDirectorySyncFailsIsNotPreserved(t *testing.T) {
	unixDirSync(t)
	for _, retryFails := range []bool{false, true} {
		sy, _, root := newInSandbox(t)
		failLaptopDirs(t, root, syscall.EIO)
		notKept(t, "first pass", sy, pass(t, sy), "input/output error")
		if retryFails {
			rep := pass(t, sy)
			notKept(t, "retry, still failing", sy, rep, "keep the local file new.txt")
			continue
		}
		onSync(t, nil, func(*os.File) error { return nil })
		rep := pass(t, sy)
		if _, ok := sy.State.Files["new.txt"]; !ok || rep.NotPreserved != "" || len(rep.Conflicts) != 0 {
			t.Errorf("retry: report = %+v, recorded = %v", rep, ok)
		}
		if _, err := os.Stat(filepath.Join(root, conflictNames("new.txt", testTag)[0])); err == nil {
			t.Error("retry: a conflict copy of a file already in step")
		}
	}
}

// T1b: a directory sync the filesystem does not offer is a warning, not a change left unkept.
func TestPass_AnUnsupportedDirectorySyncIsPreservedWithAWarning(t *testing.T) {
	unixDirSync(t)
	sy, _, root := newInSandbox(t)
	failLaptopDirs(t, root, syscall.EINVAL)
	rep := pass(t, sy)
	if _, ok := sy.State.Files["new.txt"]; !ok || rep.NotPreserved != "" || rep.DurabilityWarning == "" || !slices.Equal(rep.Pulled, []string{"new.txt"}) {
		t.Errorf("report = %+v, recorded = %v", rep, ok)
	}
}

// T4: a pull whose file sync fails is not preserved.
func TestPass_APullWhoseFileSyncFailsIsNotPreserved(t *testing.T) {
	sy, _, _ := newInSandbox(t)
	onSync(t, func(f *os.File) error {
		if laptopTemp(f) {
			return errFault
		}
		return nil
	}, nil)
	notKept(t, "pull", sy, pass(t, sy), "replace the local file new.txt")
}

// blockState puts a non-empty directory at the state file's path, so a save cannot rename onto it.
func blockState(t *testing.T, sy *Syncer) {
	t.Helper()
	putSyncFile(t, filepath.Join(sy.StateFile, "x"), "x", 0o644)
}

// T5: a runner pass whose state save fails still reports what it did, with the save's error, and is
// not preserved; so does a pass cut short, whose first reason is the pull it cut.
func TestPass_ARunnerPassWhoseSaveFailsIsNotPreserved(t *testing.T) {
	sy, _, _ := newInSandbox(t)
	blockState(t, sy)
	rep, err := sy.Pass(context.Background())
	if rep == nil || err == nil || !strings.HasPrefix(rep.NotPreserved, "save sync state:") || !slices.Equal(rep.Pulled, []string{"new.txt"}) {
		t.Errorf("pass: report = %+v, err = %v", rep, err)
	}

	sy, box, _ := newInSandbox(t)
	putSyncFile(t, filepath.Join(box, "second.txt"), "more", 0o644)
	blockState(t, sy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onSync(t, func(f *os.File) error {
		if laptopTemp(f) {
			cancel()
			return errFault
		}
		return nil
	}, nil)
	rep, err = sy.Pass(ctx)
	if rep == nil || err == nil || !strings.Contains(err.Error(), "save sync state:") || !strings.HasPrefix(rep.NotPreserved, "replace the local file new.txt:") || rep.Unsynced == 0 {
		t.Errorf("cut pass: report = %+v, err = %v", rep, err)
	}
}

// T6: a conflict whose copy cannot be kept, every name being taken, is not preserved.
func TestPass_AConflictWithEveryNameTakenIsNotPreserved(t *testing.T) {
	sy, box, root := runnerSyncer(t, true)
	bothChanged(t, sy, box, root)
	for i := 1; i <= maxConflictCopies; i++ {
		putSyncFile(t, filepath.Join(root, copyName(i)), "another copy", 0o644)
	}
	rep := pass(t, sy)
	if rep.NotPreserved == "" || rep.Unsynced != 1 || getSyncFile(t, filepath.Join(box, "x.txt")) != "sandbox edit!" {
		t.Errorf("report = %+v", rep)
	}
}

// T7: a pull refused by a limit is a refusal only, not a change left unkept.
func TestPass_APullOverTheFileLimitIsOnlyRefused(t *testing.T) {
	sy, box, _ := runnerSyncer(t, true)
	big := filepath.Join(box, "big.bin")
	putSyncFile(t, big, "", 0o644)
	if err := os.Truncate(big, MaxPullFile+1); err != nil {
		t.Fatal(err)
	}
	rep := pass(t, sy)
	if rep.NotPreserved != "" || len(rep.Refused) != 1 || rep.Refused[0].Path != "big.bin" {
		t.Errorf("report = %+v", rep)
	}
}

// T8: `wardyn sync` keeps its rules: the same faults refuse, fail the pass or push, and mark nothing
// not preserved.
func TestSyncer_WithoutRunnerPassNothingIsMarkedNotPreserved(t *testing.T) {
	t.Run("pull directory sync", func(t *testing.T) {
		unixDirSync(t)
		sy, box, root := pipeSyncer(t, true)
		putSyncFile(t, filepath.Join(box, "new.txt"), "from the sandbox", 0o644)
		failLaptopDirs(t, root, syscall.EIO)
		rep := pass(t, sy)
		if rep.NotPreserved != "" || rep.Unsynced != 0 || len(rep.Refused) != 1 {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("save", func(t *testing.T) {
		sy, box, _ := pipeSyncer(t, true)
		putSyncFile(t, filepath.Join(box, "new.txt"), "from the sandbox", 0o644)
		blockState(t, sy)
		if rep, err := sy.Pass(context.Background()); rep != nil || err == nil {
			t.Errorf("report = %+v, err = %v, want no report and the error", rep, err)
		}
	})
	t.Run("every conflict name taken", func(t *testing.T) {
		sy, box, root := pipeSyncer(t, true)
		bothChanged(t, sy, box, root)
		for i := 1; i <= maxConflictCopies; i++ {
			putSyncFile(t, filepath.Join(root, copyName(i)), "another copy", 0o644)
		}
		rep := pass(t, sy)
		if rep.NotPreserved != "" || getSyncFile(t, filepath.Join(box, "x.txt")) != "laptop edit" {
			t.Errorf("report = %+v", rep)
		}
	})
}

// sentThenSandboxChanged is x.txt (and, with keep, keep.txt) sent by a first pass, then x.txt
// deleted on the laptop and changed in the sandbox.
func sentThenSandboxChanged(t *testing.T, sy *Syncer, box, root string, keep bool) {
	t.Helper()
	putSyncFile(t, filepath.Join(root, "x.txt"), "base", 0o644)
	if keep {
		putSyncFile(t, filepath.Join(root, "keep.txt"), "kept", 0o644)
	}
	pass(t, sy)
	if err := os.Remove(filepath.Join(root, "x.txt")); err != nil {
		t.Fatal(err)
	}
	putSyncFile(t, filepath.Join(box, "x.txt"), "sandbox edit!", 0o644)
	later := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(box, "x.txt"), later, later); err != nil {
		t.Fatal(err)
	}
}

// T7: a conflict a runner pass back only lists keeps the sandbox's copy beside the laptop's: a file
// the laptop deleted and the sandbox changed (with the laptop folder emptied, or not), and one the
// laptop changed while the pass fetched it. One whose copy cannot be kept is not preserved.
func TestPass_AListedConflictKeepsTheSandboxCopy(t *testing.T) {
	for _, keep := range []bool{false, true} {
		sy, box, root := runnerSyncer(t, true)
		sentThenSandboxChanged(t, sy, box, root, keep)
		rep := pass(t, sy)
		if got := getSyncFile(t, filepath.Join(root, copyName(1))); got != "sandbox edit!" || rep.NotPreserved != "" || !slices.Contains(rep.Conflicts, "x.txt") {
			t.Errorf("laptop delete (folder kept %v): copy = %q, report = %+v", keep, got, rep)
		}
		if _, err := os.Stat(filepath.Join(root, "x.txt")); !os.IsNotExist(err) {
			t.Errorf("laptop delete (folder kept %v): the deleted file came back", keep)
		}
	}

	t.Run("every name taken", func(t *testing.T) {
		sy, box, root := runnerSyncer(t, true)
		sentThenSandboxChanged(t, sy, box, root, true)
		for i := 1; i <= maxConflictCopies; i++ {
			putSyncFile(t, filepath.Join(root, copyName(i)), "another copy", 0o644)
		}
		rep := pass(t, sy)
		if rep.NotPreserved == "" || !slices.Equal(rep.UnsyncedPaths, []string{"x.txt"}) {
			t.Errorf("report = %+v", rep)
		}
	})

	t.Run("sandbox changed after the walk", func(t *testing.T) {
		sy, box, root := runnerSyncer(t, true)
		putSyncFile(t, filepath.Join(root, "x.txt"), "base", 0o644)
		putSyncFile(t, filepath.Join(root, "keep.txt"), "kept", 0o644)
		pass(t, sy)
		if err := os.Remove(filepath.Join(root, "x.txt")); err != nil {
			t.Fatal(err)
		}
		// a.txt, new in the sandbox, is pulled before x.txt; while it is, the agent changes x.txt.
		putSyncFile(t, filepath.Join(box, "a.txt"), "new", 0o644)
		onSync(t, func(f *os.File) error {
			if laptopTemp(f) && !strings.Contains(f.Name(), "sandbox conflict") {
				if getSyncFile(t, filepath.Join(box, "x.txt")) == "base" {
					putSyncFile(t, filepath.Join(box, "x.txt"), "late edit", 0o644)
				}
			}
			return nil
		}, nil)
		rep := pass(t, sy)
		if got := getSyncFile(t, filepath.Join(root, copyName(1))); got != "late edit" || getSyncFile(t, filepath.Join(box, "x.txt")) != "late edit" || rep.NotPreserved != "" {
			t.Errorf("copy = %q, sandbox = %q, report = %+v", got, getSyncFile(t, filepath.Join(box, "x.txt")), rep)
		}
	})

	t.Run("laptop changed during the pull", func(t *testing.T) {
		sy, box, root := runnerSyncer(t, true)
		putSyncFile(t, filepath.Join(root, "x.txt"), "base", 0o644)
		pass(t, sy)
		putSyncFile(t, filepath.Join(box, "x.txt"), "sandbox edit!", 0o644)
		later := time.Now().Add(time.Hour).Truncate(time.Second)
		if err := os.Chtimes(filepath.Join(box, "x.txt"), later, later); err != nil {
			t.Fatal(err)
		}
		onSync(t, func(f *os.File) error {
			if laptopTemp(f) && getSyncFile(t, filepath.Join(root, "x.txt")) == "base" {
				putSyncFile(t, filepath.Join(root, "x.txt"), "laptop save", 0o644)
			}
			return nil
		}, nil)
		rep := pass(t, sy)
		if got := getSyncFile(t, filepath.Join(root, copyName(1))); got != "sandbox edit!" || getSyncFile(t, filepath.Join(root, "x.txt")) != "laptop save" || rep.NotPreserved != "" {
			t.Errorf("copy = %q, laptop = %q, report = %+v", got, getSyncFile(t, filepath.Join(root, "x.txt")), rep)
		}
	})

	t.Run("wardyn sync lists it only", func(t *testing.T) {
		sy, box, root := pipeSyncer(t, true)
		sentThenSandboxChanged(t, sy, box, root, true)
		rep := pass(t, sy)
		if _, err := os.Stat(filepath.Join(root, copyName(1))); err == nil || rep.NotPreserved != "" || !slices.Contains(rep.Conflicts, "x.txt") {
			t.Errorf("report = %+v, copy stat err = %v", rep, err)
		}
	})
}

// onCopy makes a pull's copy into the laptop write half its bytes, then fail with err.
func onCopy(t *testing.T, err error) {
	t.Helper()
	old := copyToLaptop
	t.Cleanup(func() { copyToLaptop = old })
	copyToLaptop = func(dst *os.File, src io.Reader) (int64, error) {
		n, _ := io.CopyN(dst, src, 4)
		return n, err
	}
}

// A pull whose copy into the laptop fails, on the write (a full disk) or on the read from the
// sandbox, is not preserved on a runner pass, and `wardyn sync` still only refuses it.
func TestPass_AFailedCopyIntoTheLaptopIsNotPreserved(t *testing.T) {
	for name, err := range map[string]error{
		"write": &os.PathError{Op: "write", Path: "tmp", Err: syscall.ENOSPC},
		"read":  &sftp.StatusError{Code: 4},
	} {
		t.Run(name, func(t *testing.T) {
			sy, _, _ := newInSandbox(t)
			onCopy(t, err)
			notKept(t, name, sy, pass(t, sy), "copy into the laptop new.txt")

			cli, box, _ := pipeSyncer(t, true)
			putSyncFile(t, filepath.Join(box, "new.txt"), "from the sandbox", 0o644)
			rep := pass(t, cli)
			if rep.NotPreserved != "" || rep.Unsynced != 0 || len(rep.Refused) != 1 {
				t.Errorf("wardyn sync: report = %+v", rep)
			}
		})
	}
}

// A copy into the laptop that ends the pass (a lost connection, a short read) is not preserved on a
// runner pass either: the pass still ends with that error, and new.txt is counted once among the
// files not brought back. `wardyn sync` only ends with the error.
func TestPass_AFailedCopyThatEndsThePassIsNotPreserved(t *testing.T) {
	for name, cause := range map[string]error{
		"short read":      io.ErrUnexpectedEOF,
		"connection lost": &sftp.StatusError{Code: uint32(sftp.ErrSSHFxConnectionLost)},
	} {
		t.Run(name, func(t *testing.T) {
			sy, _, _ := newInSandbox(t)
			onCopy(t, cause)
			rep, err := sy.Pass(context.Background())
			if !errors.Is(err, cause) || rep == nil {
				t.Fatalf("pass = %+v, %v: want the report and the error", rep, err)
			}
			if !strings.Contains(rep.NotPreserved, "copy into the laptop new.txt") || rep.Unsynced != 1 || !slices.Equal(rep.UnsyncedPaths, []string{"new.txt"}) {
				t.Errorf("report = %+v: want new.txt not brought back and not preserved", rep)
			}
			if _, ok := sy.State.Files["new.txt"]; ok {
				t.Errorf("a pull not kept was recorded")
			}

			cli, box, _ := pipeSyncer(t, true)
			putSyncFile(t, filepath.Join(box, "new.txt"), "from the sandbox", 0o644)
			rep, err = cli.Pass(context.Background())
			if !errors.Is(err, cause) || rep == nil || rep.NotPreserved != "" {
				t.Errorf("wardyn sync: pass = %+v, %v", rep, err)
			}
		})
	}
}
