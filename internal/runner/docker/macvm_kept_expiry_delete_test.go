// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A delete the organisation starts because a machine expired (the delete_expired op) is rechecked against the Mac's own record under the
// machine's lock: a machine used or stopped since the organisation last heard has a later deadline here, and
// stays. A delete a person or an operator asks for is not an expiry and is not rechecked. A machine that has
// really expired is still deleted.
func TestKeptMachineExpiryDeleteRechecksTheLocalDeadline(t *testing.T) {
	r, _ := newKeptRig(t)
	ctx := context.Background()
	now := time.Now()
	keptAt(r, now)
	folder := func(rec macvm.KeptRecord) (uuid.UUID, string) {
		id := uuid.New()
		rec.ID, rec.OwnerHash, rec.RunnerID = id.String(), keptOwnerHash, "runner-a"
		return id, writeKeptFolder(t, r, rec)
	}
	refused := func(err error) bool {
		var we *runnerwire.Error
		return errors.As(err, &we) && we.Code == runnerwire.CodeRefused
	}

	// The organisation's deadline passed, but the Mac's record was extended by a later stop.
	id, dir := folder(macvm.KeptRecord{LastStopRun: "run-2", ExpiresAt: now.Add(24 * time.Hour)})
	if out, err := r.d.DeleteExpiredKeptMachine(ctx, id, runnerwire.KeptMachineByExpiry); err == nil || !refused(err) || out != "" {
		t.Fatalf("an expiry delete of a machine whose own record is extended: %q, %v; want a refused reply", out, err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("the refused expiry delete removed the folder: %v", err)
	}
	// A session holds it: a machine a run holds never expires under it.
	run := uuid.New()
	heldDir, held := keptRunning(t, r, run, true, now.Add(-time.Hour))
	if _, err := r.d.DeleteExpiredKeptMachine(ctx, held, runnerwire.KeptMachineByExpiry); err == nil || !refused(err) {
		t.Fatalf("an expiry delete of a machine a session holds: %v; want a refused reply", err)
	}
	if _, err := os.Lstat(heldDir); err != nil {
		t.Fatalf("the refused expiry delete removed a held machine: %v", err)
	}
	// An explicit delete is the person's or an operator's decision, whatever the deadline says.
	if out, err := r.d.DeleteKeptMachine(ctx, id, "owner"); err != nil || out != runnerwire.KeptMachineDeleted {
		t.Fatalf("an owner's delete of an unexpired machine: %q, %v", out, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the owner's delete left the folder: %v", err)
	}
	// Normal expiry: the Mac's own record agrees, so the delete proceeds.
	id, dir = folder(macvm.KeptRecord{LastStopRun: "run-1", ExpiresAt: now.Add(-time.Hour)})
	if out, err := r.d.DeleteExpiredKeptMachine(ctx, id, runnerwire.KeptMachineByExpiry); err != nil || out != runnerwire.KeptMachineDeleted {
		t.Fatalf("an expiry delete of an expired machine: %q, %v", out, err)
	}
	for _, p := range []string{dir, dir + ".lock"} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s outlived the expiry delete: %v", p, err)
		}
	}
	// The folder the sweep removed between the first look and the lock: nothing to remove, and the lock file the
	// wait made is not left behind.
	c := r.d.macVMConfig()
	vanished := c.keptDir(uuid.New())
	if gone, err := c.removeKeptIfExpired(vanished, uuid.New()); gone || err != nil {
		t.Fatalf("an expiry delete of a folder gone under the lock: %v, %v; want false and no error", gone, err)
	}
	if _, err := os.Lstat(vanished + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a lock file outlived the expiry delete of a vanished folder: %v", err)
	}
	// A failed tools update leaves the folder with no record beside the person's old disk moved aside. An expiry
	// delete never removes it, whether it finds the record missing at its first look or only under the lock.
	stuck := uuid.New()
	stuckDir := c.keptDir(stuck)
	oldGen := stuckDir + ".update.old-" + uuid.NewString()
	for _, d := range []string{stuckDir, oldGen, stuckDir + ".update"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(oldGen, "disk.img")
	if err := os.WriteFile(data, []byte("the person's disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := r.d.DeleteExpiredKeptMachine(ctx, stuck, runnerwire.KeptMachineByExpiry); err == nil || !refused(err) || out != "" {
		t.Fatalf("an expiry delete of a folder with no record: %q, %v; want a refused reply", out, err)
	}
	if gone, err := c.removeKeptIfExpired(stuckDir, stuck); gone || !errors.Is(err, errKeptRecordMissing) {
		t.Fatalf("under the lock, a folder with no record: %v, %v; want refused and not removed", gone, err)
	}
	for _, p := range []string{stuckDir, stuckDir + ".update", data} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("an expiry delete of a recoverable folder removed %s: %v", p, err)
		}
	}
}
