// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A disk drive's delete is asked only of a runner that says it keeps disk drives, and then goes over
// the wire as reclaim_drive (the fake runner deletes none, and says so).
func TestReclaimMacDiskAsksOnlyARunnerThatKeepsDisks(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	disk := types.DriveMount{Backend: types.DriveBackendMacDisk, ObjectName: "wardyn-drive-x"}
	if _, err := rig.Sub.ReclaimMacDisk(ctxT(t), disk); err == nil || !strings.Contains(err.Error(), "keeps no disk drives") {
		t.Fatalf("a runner with no disk drives: %v", err)
	}
	data, err := json.Marshal(runnerwire.Caps{Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC3}},
		MacOSVM: &runnerwire.MacVMCaps{DiskDrives: true}})
	if err != nil {
		t.Fatal(err)
	}
	rig.Sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	if _, err := rig.Sub.ReclaimMacDisk(ctxT(t), disk); err == nil || !strings.Contains(err.Error(), "deletes no drives") {
		t.Fatalf("reclaim_drive did not reach the runner: %v", err)
	}
}
