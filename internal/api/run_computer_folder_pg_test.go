// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A folder on the person's computer (#2982) against a real Postgres: browsing a claimed runner's
// folders is the owner's alone, the runner's folders flag is on their list, and the run page shows the
// folder's passes by name, never its path on that computer. Guarded by WARDYN_TEST_PG: skipped cleanly
// when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_ComputerFolder_BrowseListAndRunPage(t *testing.T) {
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

	w := e.call(e.person, http.MethodGet, "/api/v1/me/runners/"+id.String()+"/folders?path="+filepath.Dir(app), "")
	var list computerFolderView
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != http.StatusOK || err != nil || len(list.Folders) != 1 || list.Folders[0].Name != "my-app" {
		t.Fatalf("the owner's browse = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.other, http.MethodGet, "/api/v1/me/runners/"+id.String()+"/folders", ""); w.Code != http.StatusNotFound {
		t.Fatalf("another person's browse = %d %s, want 404", w.Code, w.Body)
	}
	w = e.call(e.person, http.MethodGet, "/api/v1/me/runners", "")
	var mine []types.RunnerView
	if err := json.Unmarshal(w.Body.Bytes(), &mine); err != nil || len(mine) != 1 || !mine[0].Folders {
		t.Fatalf("GET /me/runners = %d %s, want the runner with folders", w.Code, w.Body)
	}

	run := e.boundRun(func(r *types.AgentRun) {
		r.State = types.RunCompleted
		r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &id, Path: app, Direction: types.ComputerFolderBoth}
	})
	if ok, err := e.pg.SetRunLocalFolderSeed(ctx, run, ""); !ok || err != nil {
		t.Fatalf("seed: %v %v", ok, err)
	}
	at := time.Now().UTC()
	if ok, err := e.pg.SetRunLocalFolderStatus(ctx, run, types.RunLocalFolderStatus{
		Seed: &types.FolderPassRecord{At: at, Files: 2, Bytes: 18, Refused: []types.FolderRefusal{}},
		Back: &types.FolderPassRecord{At: at, Files: 1, Bytes: 5, Refused: []types.FolderRefusal{}},
	}); !ok || err != nil {
		t.Fatalf("status: %v %v", ok, err)
	}
	detail := e.runDetail(run)
	raw, _ := json.Marshal(detail["computer_folder"])
	var view computerFolderRunView
	if err := json.Unmarshal(raw, &view); err != nil || view.State != computerFolderSynced || view.Runner != "laptop" || view.Seed == nil || view.Back == nil || view.Back.Files != 1 {
		t.Fatalf("computer_folder = %s", raw)
	}
	if body, _ := json.Marshal(detail); strings.Contains(string(body), app) {
		t.Errorf("the run page shows the folder's path on the person's computer: %s", body)
	}
}

// pgSampledRunner is fakeRunner with a quiet CPU reading for every sandbox.
type pgSampledRunner struct{ *fakeRunner }

func (pgSampledRunner) BatchSample() bool { return true }

func (pgSampledRunner) SampleCPU(_ context.Context, refs []string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, ref := range refs {
		out[ref] = 1
	}
	return out, nil
}

// A kill and a bring-back race on Postgres: exactly one moves the run from RUNNING.
func TestPG_ComputerFolder_BringBackAndKillHaveOneWinner(t *testing.T) {
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
	e.srv.cfg.Runner = pgSampledRunner{&fakeRunner{}}

	for i := range 10 {
		run := e.boundRun(func(r *types.AgentRun) {
			r.State, r.SandboxRef, r.CreatedAt = types.RunRunning, "sbx-"+r.ID.String(), time.Now().Add(-time.Hour)
			r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &id, Path: app, Direction: types.ComputerFolderBoth}
		})
		if ok, err := e.pg.SetRunLocalFolderSeed(ctx, run, ""); !ok || err != nil {
			t.Fatalf("seed: %v %v", ok, err)
		}
		var kill, back int
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			kill = e.call(e.person, http.MethodPost, "/api/v1/runs/"+run.String()+"/kill", "").Code
		})
		wg.Go(func() {
			<-start
			back = e.call(e.person, http.MethodPost, "/api/v1/runs/"+run.String()+"/computer-folder/bring-back", "").Code
		})
		close(start)
		wg.Wait()
		got, err := e.pg.GetRun(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		killWon, backWon := kill == http.StatusAccepted, back == http.StatusOK
		if killWon == backWon {
			t.Fatalf("round %d: kill %d, bring-back %d: want exactly one winner", i, kill, back)
		}
		if want := map[bool]types.RunState{true: types.RunKilled, false: types.RunStopped}[killWon]; got.State != want {
			t.Fatalf("round %d: kill %d, bring-back %d, state %s, want %s", i, kill, back, got.State, want)
		}
	}
}
