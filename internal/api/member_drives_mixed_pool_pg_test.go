// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mixedPoolMemberDrive is the person's own drive and a pool holding her own runner (idle, room for many
// runs) and the remote-hosted runner alpha.
func mixedPoolMemberDrive(t *testing.T, e *queueEnv) (types.UserDrive, types.RunnerPool, uuid.UUID) {
	t.Helper()
	e.srv.cfg.RunnerTarget = "docker"
	mine, err := e.pg.CreateMemberDrive(context.Background(), types.UserDrive{Name: "Mine", Backend: types.DriveBackendDockerVolume,
		OwnerSubject: types.CanonicalUserSubject(e.personP), CreatedBy: e.personP}, 3)
	if err != nil {
		t.Fatal(err)
	}
	pool, laptop := e.mixedPool("Mixed")
	return mine, pool, laptop
}

func memberDriveLaunch(pool types.RunnerPool, drive types.UserDrive, extra string) string {
	x := `"runner_pool_id":"` + pool.ID.String() + `"`
	if extra != "" {
		x += "," + extra
	}
	return strings.Replace(endBody("background", x), `"no_repositories_or_drives":true`, fmt.Sprintf(`"drives":[{"drive_id":%q}]`, drive.ID), 1)
}

func (e *queueEnv) noRunOfHersOnHerOwnRunner(t *testing.T, what string) {
	t.Helper()
	if n := e.count(`SELECT count(*) FROM agent_runs WHERE created_by = $1 AND state <> 'FAILED' AND (placement = 'local' OR runner_id IS NOT NULL)`, e.personP); n != 0 {
		t.Errorf("%s: %d live run(s) with her drive on a person's own runner", what, n)
	}
}

// A person's own drive launched into a pool that holds her own runner and a remote-hosted one runs only
// on the remote-hosted runner: it goes there when that runner can take it, and is refused, named, when
// it cannot or when she asks for her own runner. The same pool without a drive does use her runner.
func TestPG_MemberDriveInAMixedPoolNeverRunsOnHerOwnRunner(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(e *queueEnv, laptop uuid.UUID) string // the extra request fields
		code   int
		reason string // the refusal's reason, or "" for a run placed on the remote-hosted runner
	}{
		{"the remote-hosted runner is free", func(*queueEnv, uuid.UUID) string { return "" }, http.StatusCreated, ""},
		{"the remote-hosted runner is too small for the run", func(e *queueEnv, _ uuid.UUID) string {
			e.runner.execFacts["alpha"] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}, CapacityAdvertised: true, CPUMillisMax: 1, MemoryMiBMax: 1}
			return ""
		}, http.StatusCreated, ""},
		{"the remote-hosted runner is no longer offered to her", func(e *queueEnv, _ uuid.UUID) string { e.setPeople("alpha", ""); return "" },
			http.StatusUnprocessableEntity, "drive_not_in_pool"},
		{"she asks for her own runner", func(*queueEnv, uuid.UUID) string { return `"placement":"local"` },
			http.StatusUnprocessableEntity, "drive_not_in_pool"},
		{"she pins her own runner", func(_ *queueEnv, laptop uuid.UUID) string { return `"runner_id":"` + laptop.String() + `"` },
			http.StatusBadRequest, "placement_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newQueueEnv(t)
			mine, pool, laptop := mixedPoolMemberDrive(t, e)
			w := e.call(e.person, http.MethodPost, "/api/v1/runs", memberDriveLaunch(pool, mine, tc.setup(e, laptop)))
			var out struct {
				queuedLaunchAnswer
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			if w.Code != tc.code || out.Reason != tc.reason {
				t.Fatalf("launch = %d %q, want %d %q: %.300s", w.Code, out.Reason, tc.code, tc.reason, w.Body.String())
			}
			if w.Code == http.StatusCreated {
				if r := e.run(out.ID); r.Placement != types.PlacementRemote || r.RunnerID != nil {
					t.Errorf("the run is placed %s on runner %v; want the remote-hosted runner", r.Placement, r.RunnerID)
				}
			}
			e.noRunOfHersOnHerOwnRunner(t, tc.name)
		})
	}
	e := newQueueEnv(t)
	_, pool, _ := mixedPoolMemberDrive(t, e)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`))
	var plain queuedLaunchAnswer
	_ = json.Unmarshal(w.Body.Bytes(), &plain)
	if w.Code != http.StatusCreated || e.run(plain.ID).Placement != types.PlacementLocal {
		t.Errorf("control: the same pool without a drive = %d, placement %s; want her own runner", w.Code, e.run(plain.ID).Placement)
	}
}

// A run with her drive that waits in the mixed pool starts on the remote-hosted runner when the slot
// frees, and is ended at its start, with nothing minted, when that runner was withdrawn while it waited.
func TestPG_MemberDriveWaitingInAMixedPoolNeverStartsOnHerOwnRunner(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote-hosted runner withdrawn while it waited=%v", gone), func(t *testing.T) {
			e := newQueueEnv(t)
			mine, pool, _ := mixedPoolMemberDrive(t, e)
			e.exec(`UPDATE runner_pools SET limits = '{"background":{"cpu_millis":{"default":500,"cap":4000},"memory_mib":{"default":512,"cap":8192},"lifetime":{"default_sec":3600,"max_sec":7200}},"barriers":["CC1","CC2","CC3"],"max_concurrent_runs":1}'::jsonb, revision = revision + 1 WHERE id = $1`, pool.ID)
			pool = e.reread(pool)
			occupant := e.occupy(pool)
			w := e.call(e.person, http.MethodPost, "/api/v1/runs", memberDriveLaunch(pool, mine, ""))
			var out queuedLaunchAnswer
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated || out.State != types.RunQueued {
				t.Fatalf("launch into the full pool = %d %.300s; want 201 QUEUED", w.Code, w.Body.String())
			}
			if gone {
				e.setPeople("alpha", "")
			}
			e.free(occupant)
			e.eventually("the waiting run to start or fail", func() bool {
				r := e.run(out.ID)
				return r.State == types.RunFailed || (r.AdmittedAt != nil && r.SPIFFEID != "" && r.State != types.RunPending)
			})
			r := e.run(out.ID)
			switch {
			case !gone && (r.State == types.RunFailed || r.Placement != types.PlacementRemote || r.RunnerID != nil):
				t.Errorf("the run is %s %q placed %s on runner %v; want started on the remote-hosted runner", r.State, r.StatusDetail, r.Placement, r.RunnerID)
			case gone && (r.State != types.RunFailed || r.SPIFFEID != "" || r.StatusDetail != "queue: drive_not_in_pool"):
				t.Errorf("the run is %s %q minted=%v; want FAILED queue: drive_not_in_pool with nothing minted", r.State, r.StatusDetail, r.SPIFFEID != "")
			}
			e.noRunOfHersOnHerOwnRunner(t, "waiting run")
		})
	}
}
