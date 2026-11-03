// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

func TestKeptMachineDirIsBesideTheRuns(t *testing.T) {
	l := macvm.Layout{Root: "/r"}
	id := uuid.NewString()
	if got, want := l.KeptMachineDir(id), filepath.Join("/r", "kept", macvm.RunKey(id)); got != want {
		t.Fatalf("KeptMachineDir = %s, want %s", got, want)
	}
	if filepath.Dir(l.KeptMachineDir(id)) != l.KeptDir() || filepath.Dir(l.RunDir(id)) == l.KeptDir() {
		t.Fatal("a kept machine's folder is not under runs/")
	}
}

func TestKeptRecordRoundTripChecksTheFullID(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	rec := macvm.KeptRecord{ID: id.String(), OwnerHash: "h", RunnerID: uuid.NewString(), Base: "0",
		CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := macvm.WriteKept(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := macvm.ReadKept(dir, id.String())
	if err != nil || got != rec {
		t.Fatalf("ReadKept = %+v, %v; want %+v", got, err, rec)
	}
	// Another id with the same 12-digit key reads the same folder, and is refused.
	other := id
	other[15] ^= 1
	if macvm.RunKey(other.String()) != macvm.RunKey(id.String()) {
		t.Fatal("test ids should share a key")
	}
	if _, err := macvm.ReadKept(dir, other.String()); !errors.Is(err, macvm.ErrKeptIDMismatch) {
		t.Fatalf("ReadKept(other id) = %v, want ErrKeptIDMismatch", err)
	}
	if _, err := macvm.ReadKept(filepath.Join(l.KeptDir(), "none"), id.String()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing record = %v, want ErrNotExist", err)
	}
	if err := macvm.WriteKept(dir, macvm.KeptRecord{ID: "nope"}); err == nil {
		t.Fatal("WriteKept took a non-UUID id")
	}
}

func TestKeptLockedProbe(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	dir := l.KeptMachineDir(uuid.NewString())
	if locked, err := macvm.KeptLocked(dir); locked || err != nil {
		t.Fatalf("never-booted machine: locked=%v err=%v", locked, err)
	}
	unlock, err := macvm.LockDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := macvm.KeptLocked(dir); !locked || err != nil {
		t.Fatalf("held: locked=%v err=%v", locked, err)
	}
	unlock()
	if locked, err := macvm.KeptLocked(dir); locked || err != nil {
		t.Fatalf("released: locked=%v err=%v", locked, err)
	}
}

func TestKeptAllocatedBytesCountsWrittenBlocks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, macvm.DiskFile), make([]byte, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := macvm.KeptAllocatedBytes(dir)
	if err != nil || n < 64<<10 {
		t.Fatalf("KeptAllocatedBytes = %d, %v; want at least 64 KiB", n, err)
	}
}

func TestKeptRunConfigRefusesABadKeptBlock(t *testing.T) {
	dir := macvm.Layout{Root: t.TempDir()}.RunDir(testRunID)
	for _, k := range []*macvm.KeptRun{{ID: "x", Mode: macvm.KeptNew}, {ID: uuid.NewString(), Mode: "revive"}} {
		cfg := macvm.RunConfig{RunID: testRunID, BaseDir: "/b", Kept: k}
		if err := macvm.WriteRunConfig(dir, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := macvm.ReadRunConfig(dir); err == nil {
			t.Fatalf("ReadRunConfig took kept block %+v", k)
		}
	}
	cfg := macvm.RunConfig{RunID: testRunID, BaseDir: "/b", Kept: &macvm.KeptRun{ID: uuid.NewString(), Mode: macvm.KeptResume}}
	if err := macvm.WriteRunConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := macvm.ReadRunConfig(dir); err != nil || got.Kept == nil || got.Kept.Mode != macvm.KeptResume {
		t.Fatalf("ReadRunConfig = %+v, %v", got, err)
	}
}
