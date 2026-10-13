// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneractivity

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
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
