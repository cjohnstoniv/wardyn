// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDriveSecondPlaceSaveRefusal: a second place beside a managed volume, or one
// that is not a share on a runner, is refused with the board's wire sentence and
// nothing is written.
func TestDriveSecondPlaceSaveRefusal(t *testing.T) {
	runner := uuid.New().String()
	for name, body := range map[string]string{
		"beside a managed volume": `{"name":"scratch","backend":"docker_volume","alt_backend":"host_path","alt_host_root":"/Volumes/x","alt_runner_id":"` + runner + `"}`,
		"with no runner":          `{"name":"team-share","backend":"docker_volume","alt_backend":"host_path","alt_host_root":"/Volumes/x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := newDriveCRUDStore()
			srv, rec := driveAdminServer(st, nil)
			w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body, nil)
			var out struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			var req struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal([]byte(body), &req)
			want := `user_drives: "` + req.Name + `": the second place must also be a share, and somewhere the first is not`
			if w.Code != http.StatusBadRequest || out.Error != want {
				t.Fatalf("create = %d %q, want 400 %q", w.Code, out.Error, want)
			}
			if len(st.drives) != 0 || len(rec.events) != 0 {
				t.Fatalf("a refused second place wrote %d drive(s), %d audit row(s)", len(st.drives), len(rec.events))
			}
		})
	}
}

// TestDriveOnRunner: on a person's own runner a drive mounts only from a second
// place bound to that runner; otherwise the refusal says which case, and never
// names the runner it is bound to.
func TestDriveOnRunner(t *testing.T) {
	mine, other := uuid.New(), uuid.New()
	bound := types.UserDrive{Name: "team-share", AltBackend: types.DriveBackendHostPath, AltHostRoot: "/Volumes/team-share", AltRunnerID: &mine}
	if msg := driveOnRunnerMsg(bound, &mine); msg != "" {
		t.Fatalf("bound to this runner: %q, want it to mount", msg)
	}
	if msg := driveOnRunnerMsg(bound, &other); msg != `drive: "team-share" is bound to a different runner — choose an organisation pool, or launch without drive` {
		t.Fatalf("bound elsewhere: %q", msg)
	}
	if msg := driveOnRunnerMsg(bound, nil); msg != driveBoundElsewhereMsg("team-share") {
		t.Fatalf("no one runner: %q", msg)
	}
	if msg := driveOnRunnerMsg(types.UserDrive{Name: "scratch"}, &mine); msg != `drive: "scratch" mounts only from the organisation's runners — choose an organisation pool, or launch without drive` {
		t.Fatalf("no second place: %q", msg)
	}
	f := (&Server{}).driveBindFailureHere(t.Context(), types.ResolvedDrive{Drive: types.UserDrive{Name: "scratch"}}, drivePlace{runner: &mine})
	if f == nil || f.body() != driveOrgRunnersOnlyMsg("scratch") {
		t.Fatalf("driveBindFailureHere on a runner = %+v, want the org-runners-only refusal", f)
	}
}

// TestDrivesBoundRunner: a run is held to one runner only when every drive is
// bound to it and it keeps no work folder.
func TestDrivesBoundRunner(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	on := func(id *uuid.UUID) runDrive {
		return runDrive{resolved: types.ResolvedDrive{Drive: types.UserDrive{AltRunnerID: id}}}
	}
	if got := drivesBoundRunner([]runDrive{on(&a), on(&a)}, nil); got == nil || *got != a {
		t.Fatalf("both bound to a = %v", got)
	}
	for name, c := range map[string]struct {
		drives []runDrive
		keep   *keptWorkFolder
	}{
		"one unbound":        {[]runDrive{on(&a), on(nil)}, nil},
		"two runners":        {[]runDrive{on(&a), on(&b)}, nil},
		"a kept work folder": {[]runDrive{on(&a)}, &keptWorkFolder{}},
		"no drive":           {nil, nil},
	} {
		if got := drivesBoundRunner(c.drives, c.keep); got != nil {
			t.Errorf("%s: %v, want none", name, got)
		}
	}
	targets := []poolTarget{{id: a.String(), runnerID: &a}, {id: b.String(), runnerID: &b}, {id: "alpha", executor: "alpha"}}
	if got := targetsOn(targets, &b); len(got) != 1 || got[0].runnerID == nil || *got[0].runnerID != b {
		t.Fatalf("targetsOn(b) = %+v", got)
	}
	if got := targetsOn(targets, nil); len(got) != 0 {
		t.Fatalf("targetsOn(nil) = %+v, want none", got)
	}
}

// TestSecondPlaceResolved: the second place keeps the home and moves the root.
func TestSecondPlaceResolved(t *testing.T) {
	runner := uuid.New()
	r := types.ResolvedDrive{HomeName: "alice", ObjectName: "/srv/shares/team/alice", Drive: types.UserDrive{Backend: types.DriveBackendHostPath, HostRoot: "/srv/shares/team",
		AltBackend: types.DriveBackendHostPath, AltHostRoot: "/Volumes/team-share", AltRunnerID: &runner}}
	got := secondPlaceResolved(r)
	if got.ObjectName != "/Volumes/team-share/alice" || got.Drive.HostRoot != "/Volumes/team-share" || got.HomeName != "alice" || got.Enforcement != types.StorageEnforcementExternal {
		t.Fatalf("second place = %+v", got)
	}
}
