// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A promoted run already holds its place on its Mac when its start asks whether the Mac has room for it:
// PENDING, on the runner, with its kind. A Mac offering 2 with one other VM running has room for it, a
// direct create at that count does not, and a promoted run behind two other VMs is still refused.
func TestPG_PromotedMacOSVMDoesNotCountItself(t *testing.T) {
	e := newPlacementEnv(t)
	ctx := context.Background()
	p := e.createPool("Macs", types.RunnerPoolUser)
	mac := e.laptop(e.personP, 4000, 8192, types.CC1)
	e.addToPool(e.person, p.ID, mac)
	e.stream(mac, types.CC1)
	caps := laptopCaps(types.CC1)
	caps.MacOSVM = readyMac()
	e.srv.runnerEndpoint(mac).sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: mustJSON(caps)})
	if _, _, err := e.pg.SetRunnerTrustClass(ctx, mac, types.RunnerTrustManagedDevice, e.personP, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.RecordRunnerPosture(ctx, mac, types.RunnerPosture{MDMManaged: true, Evidence: types.PostureEvidenceMDMFile}, time.Now()); err != nil {
		t.Fatal(err)
	}
	vm := func(state types.RunState) uuid.UUID {
		t.Helper()
		id := uuid.New()
		now := time.Now().UTC()
		if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: e.personP, Agent: "claude-code",
			ConfinementClass: types.CC1, State: state, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker", MachineKind: types.MachineMacOSVM,
			RunnerID: &mac, RunnerPoolID: &p.ID, RunnerPoolRevision: e.revision(p.ID), RunnerPoolSelection: types.RunnerPoolSelectedExplicit}); err != nil {
			t.Fatalf("create a %s macOS VM run: %v", state, err)
		}
		return id
	}
	owner := withOIDCHuman(ctx, e.personP)
	const atCap = "already runs 2 macOS VMs"
	refusal := func(c context.Context) *runRefusal {
		return e.srv.placementCapabilityRefusal(c, macReq(mac), runPlacement{}, governanceCeiling{}, 0)
	}

	vm(types.RunRunning)
	promoted := vm(types.RunPending)
	start := withPromotedStart(owner, promoted, &mac, "", false)
	if ref := refusal(start); ref != nil {
		t.Fatalf("a promoted VM beside one other was refused: %+v", ref.body)
	}
	if ref := refusal(owner); ref == nil || !strings.Contains(ref.body.Error, atCap) {
		t.Fatalf("a direct create beside two VMs = %+v, want %q", ref, atCap)
	}
	vm(types.RunRunning)
	if ref := refusal(start); ref == nil || !strings.Contains(ref.body.Error, atCap) {
		t.Fatalf("a promoted VM beside two others = %+v, want %q", ref, atCap)
	}
}
