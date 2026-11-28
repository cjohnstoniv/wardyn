// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBringComputerFolderBack_DenyPullRefused_PG: a run whose stored walls hold deny_pull refuses the person's
// bring-back with 403 local_folder_walled, naming the wall's layer, and audits it.
func TestBringComputerFolderBack_DenyPullRefused_PG(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	if _, err := e.pg.PutSiteConfig(ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	home, app := laptopHome(t)
	id := e.claimedRunner(e.personP)
	e.srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{id: folderRunnerEndpoint(t, id, true, home)}
	e.srv.cfg.RunnerOnline = func(r uuid.UUID) bool { return r == id }
	e.srv.cfg.ComputerFolders = true
	e.srv.cfg.Runner = pgAgentRunner{&fakeRunner{}}

	run := e.boundRun(func(r *types.AgentRun) {
		r.State, r.SandboxRef = types.RunRunning, "sbx-"+r.ID.String()
		r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &id, Path: app, Direction: types.ComputerFolderIn}
	})
	if ok, err := e.pg.SetRunLocalFolderSeed(ctx, run, ""); !ok || err != nil {
		t.Fatalf("seed: %v %v", ok, err)
	}
	if err := e.pg.KeepRunLocalFolderWalls(ctx, run, types.LocalFolderWallsRecord{
		Walls:  &types.LocalFolderRules{DenyPull: true},
		Layers: map[string]types.LocalFolderWallLayer{"deny_pull": {Layer: policyref.LayerBoundary}},
	}); err != nil {
		t.Fatal(err)
	}
	w := e.call(e.person, http.MethodPost, "/api/v1/runs/"+run.String()+"/computer-folder/bring-back", "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonLocalFolderWalled) ||
		!strings.Contains(w.Body.String(), "walled off by the Wardyn boundary") {
		t.Fatalf("bring-back under deny_pull = %d %s, want 403 %s naming the boundary", w.Code, w.Body, reasonLocalFolderWalled)
	}
	got, err := e.pg.GetRun(ctx, run)
	if err != nil || got.State != types.RunRunning {
		t.Errorf("the run after a refused bring-back: %s %v, want still RUNNING", got.State, err)
	}
}

// TestLocalFolderWalls_LockedUnitRefusesLowerWrite_PG: ua-a locks local_folders; ua-a1's admin, a team below,
// cannot set it on ua-a1 (409 unit_knob_locked naming ua-a), and the refused write leaves ua-a1 as it was.
func TestLocalFolderWalls_LockedUnitRefusesLowerWrite_PG(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	pathA, pathA1 := "/api/v1/governance/units/"+e.a.ID.String(), "/api/v1/governance/units/"+e.a1.ID.String()
	aOverlay := map[string]any{"allowed_domains": []string{"pypi.org", "github.com", "npmjs.org"}, "local_folders": map[string]any{"deny_pull": true}}
	if w := e.call(e.alice, http.MethodPut, pathA, e.restate(e.a, map[string]any{"overlay": aOverlay, "locked_knobs": []string{"local_folders"}})); w.Code != http.StatusOK {
		t.Fatalf("locking ua-a's local_folders = %d %s, want 200", w.Code, w.Body)
	}
	before := e.profile(e.a1.ID)
	body := uaUnitBody("ua-a1", e.a.ID, map[string]any{"allowed_domains": []string{"pypi.org", "github.com"}, "local_folders": map[string]any{"deny_pull": true, "max_push_mib": 10}}, nil)
	if w := e.call(e.adminA1, http.MethodPut, pathA1, body); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), reasonUnitKnobLocked) || !strings.Contains(w.Body.String(), "local_folders is locked by ua-a") {
		t.Errorf("ua-a1's admin setting local_folders on ua-a1 = %d %s, want 409 naming ua-a's lock", w.Code, w.Body)
	}
	if got := e.profile(e.a1.ID); !sameJSON(got.Overlay, before.Overlay) {
		t.Errorf("the refused write changed ua-a1's overlay to %+v", got.Overlay)
	}
}
