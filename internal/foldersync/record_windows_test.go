// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// onOpen makes every open of the laptop file name for writing fail with err; a nil err keeps the
// real open.
func onOpen(t *testing.T, name string, err error) {
	t.Helper()
	old := openFileIn
	t.Cleanup(func() { openFileIn = old })
	openFileIn = func(root *os.Root, n string, flag int) (*os.File, error) {
		if err != nil && path.Base(n) == name && flag&os.O_RDWR != 0 {
			return nil, &os.PathError{Op: "open", Path: n, Err: err}
		}
		return old(root, n, flag)
	}
}

// failFlush fails every flush of the laptop file name, not of its temp: a read-only file recorded
// must not have been flushed.
func failFlush(t *testing.T, name string, flushed *int) {
	t.Helper()
	onSync(t, func(f *os.File) error {
		if !laptopTemp(f) && filepath.Base(f.Name()) == name {
			*flushed++
			return &os.PathError{Op: "sync", Path: f.Name(), Err: syscall.EIO}
		}
		return nil
	}, nil)
}

// readOnlyAtRecord sets the read-only attribute on the laptop file name just before its first open
// for writing, the record's: a pull installs a new file writable (0644), so a read-only one is
// recorded only when the attribute is set between its install and its record.
func readOnlyAtRecord(t *testing.T, name string) {
	t.Helper()
	old := openFileIn
	t.Cleanup(func() { openFileIn = old })
	set := false
	openFileIn = func(root *os.Root, n string, flag int) (*os.File, error) {
		if !set && path.Base(n) == name && flag&os.O_RDWR != 0 {
			set = true
			if err := os.Chmod(filepath.Join(root.Name(), filepath.FromSlash(n)), 0o444); err != nil {
				t.Errorf("set read-only: %v", err)
			}
		}
		return old(root, n, flag)
	}
}

func readOnly(t *testing.T, p string) {
	t.Helper()
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
}

// A pulled file made read-only before it is recorded: Windows refuses a handle for writing to it,
// so it is recorded without a flush, with the pass's durability warning in the report (what
// `wardyn sync` prints) and logged once with its reason (the runner's log), and its attribute kept.
func TestPass_AReadOnlyPullIsRecordedWithAWarning(t *testing.T) {
	buf := logged(t)
	sy, box, root := runnerSyncer(t, true)
	putSyncFile(t, filepath.Join(box, "new.txt"), "from the sandbox", 0o644)
	readOnlyAtRecord(t, "new.txt")
	var flushed int
	failFlush(t, "new.txt", &flushed)
	rep := pass(t, sy)
	laptop := filepath.Join(root, "new.txt")
	fi, err := os.Stat(laptop)
	if err != nil || getSyncFile(t, laptop) != "from the sandbox" || fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("laptop file = %v, %v: want the sandbox's bytes, read-only", fi, err)
	}
	if !slices.Contains(rep.Pulled, "new.txt") || rep.NotPreserved != "" || sy.State.Files["new.txt"].Local != localSide(fi) || flushed != 0 {
		t.Errorf("report = %+v, record = %+v, flushed %d times: want it pulled and recorded unflushed", rep, sy.State.Files["new.txt"], flushed)
	}
	if rep.DurabilityWarning != durabilityWarning || strings.Count(buf.String(), durabilityWarning) != 1 || !strings.Contains(buf.String(), "path=new.txt") || !strings.Contains(buf.String(), reasonReadOnly) {
		t.Errorf("report warning %q, log:\n%s", rep.DurabilityWarning, buf)
	}
}

// Only a refusal that the read-only attribute explains is passed over, and only its flush: the
// file is still opened, checked and recorded. A refusal of a writable file (an access list) or any
// other open error is not preserved, as before.
func TestPass_OnlyAReadOnlyRefusalSkipsTheFlush(t *testing.T) {
	cases := []struct {
		name     string
		readOnly bool
		open     error
		recorded bool
	}{
		{"read-only, real open", true, nil, true},
		{"read-only, access denied", true, windows.ERROR_ACCESS_DENIED, true},
		{"writable, access denied", false, windows.ERROR_ACCESS_DENIED, false},
		{"read-only, sharing violation", true, windows.ERROR_SHARING_VIOLATION, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := logged(t)
			sy, box, root := runnerSyncer(t, true)
			bothChanged(t, sy, box, root)
			identicalOnBoth(t, box, root)
			laptop := filepath.Join(root, "x.txt")
			if c.readOnly {
				readOnly(t, laptop)
			}
			before := sy.State.Files["x.txt"]
			onOpen(t, "x.txt", c.open)
			var flushed int
			failFlush(t, "x.txt", &flushed)
			rep := pass(t, sy)
			fi, err := os.Stat(laptop)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, copyName(1))); err == nil || getSyncFile(t, filepath.Join(box, "x.txt")) != "the same edit" {
				t.Errorf("a conflict copy, or the sandbox's file changed")
			}
			if c.recorded {
				if sy.State.Files["x.txt"].Local != localSide(fi) || rep.NotPreserved != "" || flushed != 0 {
					t.Errorf("record = %+v, report = %+v, flushed %d times: want it recorded unflushed", sy.State.Files["x.txt"], rep, flushed)
				}
				if rep.DurabilityWarning != durabilityWarning || strings.Count(buf.String(), durabilityWarning) != 1 || !strings.Contains(buf.String(), reasonReadOnly) {
					t.Errorf("report warning %q, log:\n%s", rep.DurabilityWarning, buf)
				}
				return
			}
			if sy.State.Files["x.txt"] != before || !strings.Contains(rep.NotPreserved, "keep the local file x.txt") || rep.DurabilityWarning != "" {
				t.Errorf("record = %+v, report = %+v: want it not preserved, with no warning", sy.State.Files["x.txt"], rep)
			}
		})
	}
}
