// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A member's pool list and both dry doors state the pool's main-drive facts, the same for the same pool,
// and a pool whose executor cannot report says unknown. The enforcement word reaches a member only here.
func TestListRunnerPoolsMainDriveForMember(t *testing.T) {
	e := newPlacementEnv(t)
	e.srv.cfg.EndedRunGrace = 30 * time.Minute
	e.runner.execFacts["alpha"] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2, types.CC3},
		EphemeralDiskEnforcement: types.StorageEnforcementFilesystem, KeepsEnded: true, UserDrives: true}
	e.runner.execFacts["beta"] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2, types.CC3},
		EphemeralDiskEnforcement: types.StorageEnforcementEviction, UserDrives: true}
	remote := e.sizedPool("Remote", types.RunnerPoolAmount{Default: 1000, Cap: 2000})
	home := e.createPool("Home server", types.RunnerPoolUser)
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, home.ID, laptop)
	e.runner.runnerFacts[laptop.String()] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2},
		EphemeralDiskEnforcement: types.StorageEnforcementFilesystem, KeepsEnded: true, UserDrives: true}

	list, _ := e.list(e.person, "")
	got := entryOf(t, list, remote.ID).MainDrive
	want := &client.MainDriveFacts{Enforcement: client.MainDriveEviction, Basis: types.RunEvidenceSubstrate,
		Coverage: client.MainDriveCoverageScratch, DriveMounts: true}
	if got == nil || *got != *want {
		t.Fatalf("remote pool main_drive = %+v, want %+v (beta cannot keep an ended sandbox)", got, want)
	}
	if h := entryOf(t, list, home.ID).MainDrive; h == nil || h.Basis != types.RunEvidenceRunnerAsserted || h.DriveMounts ||
		h.Enforcement != client.MainDriveFilesystem || !h.Keep.Supported || h.Keep.GraceSec != 1800 {
		t.Errorf("self-hosted pool main_drive = %+v", h)
	}

	for _, door := range []string{"/api/v1/runs/preflight", "/api/v1/runs/policy-preview"} {
		w := e.call(e.person, http.MethodPost, door, limitsBody(fmt.Sprintf(`"runner_pool_id":%q`, remote.ID)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", door, w.Code, w.Body)
		}
		answer := decodeJSON[struct {
			RunnerPools []client.RunnerPoolChoice `json:"runner_pools"`
		}](t, w)
		if len(answer.RunnerPools) == 0 {
			t.Fatalf("%s carries no pools", door)
		}
		for _, c := range answer.RunnerPools {
			if c.MainDrive == nil || *c.MainDrive != *entryOf(t, list, c.ID).MainDrive {
				t.Errorf("%s says %+v for %s, the list %+v", door, c.MainDrive, c.Name, entryOf(t, list, c.ID).MainDrive)
			}
		}
	}

	// An executor that cannot report makes the whole pool unknown: a chip never claims what it cannot read.
	delete(e.runner.execFacts, "beta")
	e.runner.silent = map[string]bool{"beta": true}
	list, raw := e.list(e.person, "")
	if m := entryOf(t, list, remote.ID).MainDrive; m == nil || m.Enforcement != client.MainDriveUnknown || m.Coverage != "" || m.DriveMounts {
		t.Errorf("with beta silent main_drive = %+v, want unknown with no coverage", m)
	}
	if !strings.Contains(raw, `"main_drive":{"enforcement":"unknown"`) {
		t.Errorf("the unknown word is not on the wire: %s", raw)
	}
}

// A pool a manager may only manage carries no main_drive: nothing is computed for a pool the caller may not use.
func TestListRunnerPoolsMainDriveAbsentOnManagerRows(t *testing.T) {
	e := newPlacementEnv(t)
	narrowed := e.sizedPool("Narrowed", types.RunnerPoolAmount{Default: 1000, Cap: 2000})
	if w := e.call(e.alice, http.MethodPut, usePolicyPath(narrowed.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`); w.Code >= 300 {
		t.Fatalf("use policy = %d %s", w.Code, w.Body)
	}
	list, raw := e.list(e.alice, "")
	if got := entryOf(t, list, narrowed.ID); got.MainDrive != nil || strings.Contains(raw, "main_drive") {
		t.Errorf("manager row carries main_drive: %+v", got)
	}
}
