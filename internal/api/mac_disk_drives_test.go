// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A disk drive is bound to the Mac it is kept on: it is on a pool only when the pool holds that Mac, a
// run with it goes to that Mac, a macOS VM's kept work folder follows it there, and its mount is
// verified for that Mac alone.
func TestDiskDriveIsBoundToItsMac(t *testing.T) {
	mac, other := uuid.New(), uuid.New()
	disk := types.UserDrive{Name: "Notes", Backend: types.DriveBackendMacDisk, OwnerSubject: "alice", HomeRunnerID: &mac}
	withMac := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Scope: types.RunnerPoolUser}, OwnRunnerIDs: []uuid.UUID{mac}}
	withoutMac := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Scope: types.RunnerPoolUser}, OwnRunnerIDs: []uuid.UUID{other}}
	if memberDriveNeverOnPool(withMac, disk) || !memberDriveNeverOnPool(withoutMac, disk) {
		t.Fatal("a disk drive must be on a pool exactly when the pool holds its Mac")
	}
	drives := []runDrive{{resolved: types.ResolvedDrive{Drive: disk}}}
	if got := drivesBoundRunner(drives, nil); got == nil || *got != mac {
		t.Fatalf("drivesBoundRunner = %v, want the drive's Mac", got)
	}
	macKeep := &keptWorkFolder{row: types.UserDrive{Backend: types.DriveBackendMacDisk}}
	if got := drivesBoundRunner(drives, macKeep); got == nil || *got != mac {
		t.Fatalf("with a macOS VM's kept work folder, drivesBoundRunner = %v, want the drive's Mac", got)
	}
	if got := drivesBoundRunner(drives, &keptWorkFolder{row: types.UserDrive{Backend: types.DriveBackendDockerVolume}}); got != nil {
		t.Fatalf("a kept volume binds the run to %v, want none", got)
	}
	s := &Server{}
	mounts := []types.DriveMount{{Backend: types.DriveBackendMacDisk, RunnerID: &mac}}
	if got := s.drivePathsOn(mac, mounts); !got["SandboxSpec.Drives[0]"] {
		t.Fatalf("drivePathsOn(its Mac) = %v, want the disk verified", got)
	}
	if got := s.drivePathsOn(other, mounts); got["SandboxSpec.Drives[0]"] {
		t.Fatalf("drivePathsOn(another runner) = %v, want the disk unverified", got)
	}
}
