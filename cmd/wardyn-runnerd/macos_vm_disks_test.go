// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import "testing"

// A runner that keeps a person's disk drives on its Mac says so: the organisation sends a disk drive,
// and its reclaim_drive, only to a runner that does.
func TestMacOSVMOffersDiskDrives(t *testing.T) {
	m := newMacLedgerRig(t).open(t)
	m.recompute()
	if c := m.current(); !c.DiskDrives {
		t.Fatal("caps: disk_drives=false, want true")
	}
}
