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

// kept_machine is asked only of a runner that says it keeps machines; the fake answers deleted, then
// already_absent for the same id.
func TestKeptMachineAsksOnlyARunnerThatKeepsMachines(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	args := runnerwire.KeptMachineArgs{Op: runnerwire.KeptMachineOpDelete, ID: "m1", By: "owner"}
	if _, err := rig.Sub.KeptMachine(ctxT(t), args); err == nil || !strings.Contains(err.Error(), "keeps no machines") {
		t.Fatalf("a runner with no kept machines: %v", err)
	}
	data, err := json.Marshal(runnerwire.Caps{Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC3}},
		MacOSVM: &runnerwire.MacVMCaps{KeptMachines: true}})
	if err != nil {
		t.Fatal(err)
	}
	rig.Sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	for _, want := range []string{runnerwire.KeptMachineDeleted, runnerwire.KeptMachineAlreadyAbsent} {
		res, err := rig.Sub.KeptMachine(ctxT(t), args)
		if err != nil || res.Outcome != want {
			t.Fatalf("kept_machine delete: want %q, got %+v, %v", want, res, err)
		}
	}
}
