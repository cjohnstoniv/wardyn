// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// seamRemoveAll replaces removeAll for one test.
func seamRemoveAll(t *testing.T, f func(string) error) {
	t.Helper()
	old := removeAll
	removeAll = f
	t.Cleanup(func() { removeAll = old })
}

// interruptedDelete runs discard with a delete that stops before removing
// anything, as a crash would, then restarts on the same directory: no run
// directory is left for New to refuse, and New removes the tomb.
func interruptedDelete(t *testing.T, discard func(*Server, uuid.UUID) error) {
	dir := t.TempDir()
	s, err := New(runnertest.NewFake(), uuid.NewString(), dir)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	w, err := s.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("interrupted")
	seamRemoveAll(t, func(string) error { return crash })
	if err := discard(s, id); !errors.Is(err, crash) {
		t.Fatalf("discard = %v, want the interrupted delete", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "output", id.String())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory after an interrupted delete: %v, want gone", err)
	}
	removeAll = os.RemoveAll
	restarted, err := New(runnertest.NewFake(), uuid.NewString(), dir)
	if err != nil {
		t.Fatalf("restart after an interrupted delete: %v", err)
	}
	if restarted.outputs[id] != nil {
		t.Fatal("the restart resurrected a deleted run's output")
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "output")); err != nil || len(entries) != 0 {
		t.Fatalf("output directory after the restart = %v %v, want empty", entries, err)
	}
}

func TestForgetOutput_InterruptedDeleteLeavesNoRunDir(t *testing.T) {
	interruptedDelete(t, (*Server).ForgetOutput)
}

func TestEraseOutput_InterruptedDeleteLeavesNoRunDir(t *testing.T) {
	interruptedDelete(t, (*Server).EraseOutput)
}

// Two deletes of one run, the first interrupted and its tomb still on disk,
// each get their own tomb: the second never fails on the first's name.
func TestReleaseOutput_TombNameIsUnique(t *testing.T) {
	dir := t.TempDir()
	s, err := New(runnertest.NewFake(), uuid.NewString(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var tombs []string
	seamRemoveAll(t, func(path string) error { tombs = append(tombs, path); return nil })
	id := uuid.New()
	for range 2 {
		completedOutput(t, s, id, 4)
		if err := s.ReleaseOutput(id, "local-"+id.String()); err != nil {
			t.Fatalf("release %d: %v", len(tombs)+1, err)
		}
	}
	if len(tombs) != 2 || tombs[0] == tombs[1] {
		t.Fatalf("tombs = %q, want two distinct names", tombs)
	}
	for _, tomb := range tombs {
		if !strings.HasPrefix(filepath.Base(tomb), releasedPrefix+id.String()+"-") {
			t.Fatalf("tomb %q is not hidden under the run's prefix", tomb)
		}
		if _, err := os.Stat(tomb); err != nil {
			t.Fatalf("tomb %q: %v", tomb, err)
		}
	}
	removeAll = os.RemoveAll
	if _, err := New(runnertest.NewFake(), uuid.NewString(), dir); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "output")); err != nil || len(entries) != 0 {
		t.Fatalf("output directory after the restart = %v %v, want empty", entries, err)
	}
}

// ForgetOutput deletes outside the buffer's lock: a writer still holding the
// buffer is answered while the delete is held, not blocked behind it.
func TestForgetOutput_DeletesOutsideTheLock(t *testing.T) {
	s, err := New(runnertest.NewFake(), uuid.NewString(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	w, err := s.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	seamRemoveAll(t, func(path string) error {
		once.Do(func() { close(entered) })
		<-release
		return os.RemoveAll(path)
	})
	forgot := make(chan error, 1)
	go func() { forgot <- s.ForgetOutput(id) }()
	<-entered
	wrote := make(chan error, 1)
	go func() { _, err := w.Write([]byte("late")); wrote <- err }()
	select {
	case err := <-wrote:
		if err == nil {
			t.Fatal("a write to forgotten output succeeded")
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a writer waited on the buffer lock while the delete ran")
	}
	close(release)
	if err := <-forgot; err != nil {
		t.Fatal(err)
	}
}
