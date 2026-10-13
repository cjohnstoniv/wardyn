// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneractivity

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/spoolowner"
)

func TestMarkerCapturesAdmissionEpochAndReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	old, next := uuid.New(), uuid.New()
	if err := Publish(dir, old); err != nil {
		t.Fatal(err)
	}
	admission := Capture(dir)
	if err := Publish(dir, next); err != nil {
		t.Fatal(err)
	}
	if err := Record(dir, admission); err != nil {
		t.Fatal(err)
	}
	stale, err := Read(dir)
	if err != nil || stale.Epoch != old || Capture(dir) != next {
		t.Fatalf("old request acquired new epoch %+v %v", stale, err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if err := Record(dir, next); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	fresh, err := Read(dir)
	if err != nil || fresh.Epoch != next || fresh.ID == stale.ID {
		t.Fatalf("torn replacement %+v %v", fresh, err)
	}
	if err := Publish(dir, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if Capture(dir) != uuid.Nil {
		t.Fatal("offline epoch remained")
	}
	if err := Record(dir, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	same, err := Read(dir)
	if err != nil || same != fresh {
		t.Fatal("offline request emitted activity")
	}
}

func TestMarkerRejectsInvalidSchemaSizeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	for _, data := range []string{`{}`, `{"epoch":"bad","id":"bad"}`, `{"extra":true}`, string(make([]byte, maxMarker+1))} {
		if err := os.WriteFile(filepath.Join(dir, signalFile), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(dir); err == nil {
			t.Fatal("invalid marker accepted")
		}
	}
	if err := os.Remove(filepath.Join(dir, signalFile)); err != nil {
		t.Fatal(err)
	}
	if err := Publish(dir, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(epochFile, filepath.Join(dir, signalFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("symlink marker accepted")
	}
}

// The activity files live in the run's spool directory, which wardyn-runnerd and the proxy share as
// different users on a rootful daemon: an epoch wardyn-runnerd publishes is given to the directory's
// owner before it replaces anything and stays 0600, so the proxy can read it at request admission, and
// the proxy's own signal is written the same way.
func TestMarkersKeepTheSpoolOwner(t *testing.T) {
	dir := t.TempDir()
	owner := os.Geteuid()
	var handed []string
	spoolowner.Chown = func(f *os.File, uid, gid int) error {
		handed = append(handed, fmt.Sprintf("%s %d", strings.SplitN(filepath.Base(f.Name()), ".json", 2)[0], uid))
		return f.Chown(uid, gid)
	}
	t.Cleanup(func() {
		spoolowner.ProcessUID, spoolowner.Chown = os.Geteuid, func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
	})

	// wardyn-runnerd, another user than the spool's owner, publishes the epoch.
	spoolowner.ProcessUID = func() int { return owner + 1 }
	epoch := uuid.New()
	if err := Publish(dir, epoch); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(handed, fmt.Sprintf("live-epoch %d", owner)) {
		t.Fatalf("the published epoch was handed to %v, want its new file given to the spool's owner %d before the rename", handed, owner)
	}
	st, err := os.Stat(filepath.Join(dir, epochFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := int(st.Sys().(*syscall.Stat_t).Uid); got != owner || st.Mode().Perm() != 0o600 {
		t.Fatalf("the epoch file: owner %d mode %v, want the spool's owner %d and 0600", got, st.Mode().Perm(), owner)
	}

	// The proxy, the spool's owner, reads it at admission and records activity without any hand-over.
	spoolowner.ProcessUID = os.Geteuid
	handed = nil
	if got := Capture(dir); got != epoch {
		t.Fatalf("the proxy captured %s, want the published epoch %s", got, epoch)
	}
	if err := Record(dir, epoch); err != nil || len(handed) != 0 {
		t.Fatalf("the proxy's own signal: %v, handed %v", err, handed)
	}
	if m, err := Read(dir); err != nil || m.Epoch != epoch {
		t.Fatalf("wardyn-runnerd read %+v, %v", m, err)
	}
}
