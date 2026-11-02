// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// Ten caps' worth of lines leave the live file under the cap, three full rotated files, and no fourth:
// the log on disk is bounded however long a failure lasts.
func TestRotatingLogCapsItsFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Logs", "wardyn", "runnerd.log")
	l, err := openRotatingLog(path, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	line := append(bytes.Repeat([]byte("x"), 99), '\n')
	for range 100 {
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fileSize(t, path); got == 0 || got > 1000 {
		t.Errorf("runnerd.log is %d bytes, want at most the 1000-byte cap", got)
	}
	for _, n := range []string{".1", ".2", ".3"} {
		if got := fileSize(t, path+n); got != 1000 {
			t.Errorf("runnerd.log%s is %d bytes, want a full 1000", n, got)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Errorf("runnerd.log.4 exists (%v): more than 3 older files are kept", err)
	}
}

// A log already over the cap when serve starts (an older runnerd's, which never rotated) is moved to .1
// before the first line, and a write appends to what an earlier start left under the cap.
func TestRotatingLogRotatesAnOversizedLogAtStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runnerd.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("o"), 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := openRotatingLog(path, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if got := fileSize(t, path); got != 4 {
		t.Errorf("runnerd.log is %d bytes after start, want only the new line", got)
	}
	if got := fileSize(t, path+".1"); got != 5000 {
		t.Errorf("runnerd.log.1 is %d bytes, want the old log", got)
	}
	l, err = openRotatingLog(path, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("again\n"))
	_ = l.Close()
	if got := fileSize(t, path); got != 10 {
		t.Errorf("runnerd.log is %d bytes after a second start, want both lines", got)
	}
}

// A rotation that cannot rename (here runnerd.log.1 is a directory) costs only that rotation: every write
// still lands in the live file, and once the obstacle is gone a later rotation succeeds.
func TestRotatingLogSurvivesAFailedRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runnerd.log")
	if err := os.MkdirAll(filepath.Join(path+".1", "in-the-way"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := openRotatingLog(path, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := append(bytes.Repeat([]byte("x"), 59), '\n')
	for i := range 3 {
		if n, err := l.Write(line); err != nil || n != len(line) {
			t.Fatalf("write %d after a failed rotation: %d, %v", i, n, err)
		}
	}
	if got := fileSize(t, path); got != 180 {
		t.Fatalf("runnerd.log is %d bytes, want all 3 lines kept while it could not rotate", got)
	}
	if err := os.RemoveAll(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write(line); err != nil {
		t.Fatal(err)
	}
	if got := fileSize(t, path+".1"); got != 180 {
		t.Errorf("runnerd.log.1 is %d bytes, want the old lines once a rotation could rename", got)
	}
	if got := fileSize(t, path); got != 60 {
		t.Errorf("runnerd.log is %d bytes after the rotation, want under the cap", got)
	}
}

// A crash log over its bound when serve starts is copied to .1 and emptied in place, so a file launchd holds
// open for append keeps working; one under the bound is left alone.
func TestServeBoundsTheCrashLogAtStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runnerd.log")
	crash := filepath.Join(dir, "runnerd.crash.log")
	if err := os.WriteFile(crash, bytes.Repeat([]byte("p"), crashLogMax+1), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(crash, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	restore, err := logToFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if got := fileSize(t, crash); got != 0 {
		t.Errorf("runnerd.crash.log is %d bytes after start, want it emptied", got)
	}
	if got := fileSize(t, crash+".1"); got != crashLogMax+1 {
		t.Errorf("runnerd.crash.log.1 is %d bytes, want the %d it held", got, crashLogMax+1)
	}
	if _, err := held.WriteString("panic\n"); err != nil {
		t.Fatal(err)
	}
	if got := fileSize(t, crash); got != 6 {
		t.Errorf("a write through the held handle left %d bytes, want 6", got)
	}
	restore()
	if err := os.WriteFile(crash, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore2, err := logToFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restore2()
	if got := fileSize(t, crash); got != 5 {
		t.Errorf("a crash log under the bound became %d bytes, want it kept at 5", got)
	}
}
