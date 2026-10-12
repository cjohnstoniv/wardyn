// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// addLocalRun puts a RUNNING run on a runner into the fake store.
func addLocalRun(f *runnerRegistrationFake, runner *types.Runner) types.AgentRun {
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunRunning, Placement: types.PlacementLocal, CreatedAt: time.Now().UTC()}
	if runner != nil {
		id := runner.ID
		run.RunnerID = &id
	}
	f.authzStore.mu.Lock()
	f.authzStore.runs[run.ID] = run
	f.authzStore.mu.Unlock()
	return run
}

// runnerFactsOf reads a run's three runner facts off the wire, keeping which keys were present.
func runnerFactsOf(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("decode run: %v (%s)", err, raw)
	}
	facts := map[string]json.RawMessage{}
	for _, k := range []string{"runner_name", "runner_online", "runner_last_seen_at"} {
		if v, ok := all[k]; ok {
			facts[k] = v
		}
	}
	return facts
}

// A local run names its runner on every route that serves a run: the detail, the list, and the filtered list
// the Runs board uses. A remote run carries none of the three keys.
func TestRunRunnerFacts_LocalRunNamesItsRunnerOnEveryRoute(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	seen := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	runner := f.addRunner(srv, "alice", "laptop-7", types.RunnerClaimed, 24*time.Hour)
	runner.LastSeenAt = &seen
	f.rows[runner.ID] = runner
	srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return id == runner.ID }
	local := addLocalRun(f, &runner)
	// A remote run names no runner; even a stray runner_id on one must not make it read as a local run.
	remote := types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunRunning, Placement: types.PlacementRemote, RunnerID: &runner.ID, CreatedAt: time.Now().UTC()}
	f.authzStore.mu.Lock()
	f.authzStore.runs[remote.ID] = remote
	f.authzStore.mu.Unlock()

	check := func(where string, facts map[string]json.RawMessage) {
		t.Helper()
		var name string
		var online bool
		var lastSeen time.Time
		if json.Unmarshal(facts["runner_name"], &name) != nil || name != "laptop-7" ||
			json.Unmarshal(facts["runner_online"], &online) != nil || !online ||
			json.Unmarshal(facts["runner_last_seen_at"], &lastSeen) != nil || !lastSeen.Equal(seen) {
			t.Fatalf("%s: a local run must carry its runner's name, liveness and last seen: %v", where, facts)
		}
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+local.ID.String(), adminToken, "")
	check("detail", runnerFactsOf(t, w.Body.Bytes()))
	if w := do(t, srv, http.MethodGet, "/api/v1/runs/"+remote.ID.String(), adminToken, ""); len(runnerFactsOf(t, w.Body.Bytes())) != 0 {
		t.Fatalf("a remote run's detail carries runner facts: %s", w.Body.String())
	}
	for _, path := range []string{"/api/v1/runs", "/api/v1/runs?status=active"} {
		w := do(t, srv, http.MethodGet, path, adminToken, "")
		var rows []json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 2 {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
		for _, raw := range rows {
			var id struct{ ID uuid.UUID }
			_ = json.Unmarshal(raw, &id)
			switch id.ID {
			case local.ID:
				check(path, runnerFactsOf(t, raw))
			case remote.ID:
				if len(runnerFactsOf(t, raw)) != 0 {
					t.Fatalf("%s: a remote run carries runner facts: %s", path, raw)
				}
			}
		}
	}
}

// Liveness is a fact either way: an offline runner is "runner_online": false, present, so the page can say so;
// a runner whose row cannot be read leaves all three facts off and the response whole.
func TestRunRunnerFacts_OfflineIsAFactAndUnreadableIsAbsent(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	runner := f.addRunner(srv, "alice", "desk-2", types.RunnerClaimed, 24*time.Hour)
	srv.cfg.RunnerOnline = func(uuid.UUID) bool { return false }
	offline := addLocalRun(f, &runner)
	gone := types.Runner{ID: uuid.New()}
	unreadable := addLocalRun(f, &gone)

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+offline.ID.String(), adminToken, "")
	if got := runnerFactsOf(t, w.Body.Bytes()); string(got["runner_online"]) != "false" || string(got["runner_name"]) != `"desk-2"` {
		t.Fatalf("an offline runner must read runner_online:false with its name: %v", got)
	}
	w = do(t, srv, http.MethodGet, "/api/v1/runs/"+unreadable.ID.String(), adminToken, "")
	if w.Code != http.StatusOK || len(runnerFactsOf(t, w.Body.Bytes())) != 0 {
		t.Fatalf("an unreadable runner must leave the facts off and the read whole: %d %s", w.Code, w.Body.String())
	}
}

// A runner is never renamed and a revoked row is never deleted, so a revoked runner's run keeps the name it
// was created under; it reads offline.
func TestRunRunnerFacts_ARevokedRunnersRunKeepsItsName(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	runner := f.addRunner(srv, "alice", "old-laptop", types.RunnerRevoked, 48*time.Hour)
	srv.cfg.RunnerOnline = func(uuid.UUID) bool { return true }
	run := addLocalRun(f, &runner)

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), adminToken, "")
	if got := runnerFactsOf(t, w.Body.Bytes()); string(got["runner_name"]) != `"old-laptop"` || string(got["runner_online"]) != "false" {
		t.Fatalf("a revoked runner's run must keep its name and read offline: %v", got)
	}
}

// Under WARDYN_HA a replica that does not hold the runner's session answers "offline" for a runner connected
// to another replica, so there false is a guess: liveness stays unset (unknown) and the page claims nothing.
// A runner this replica does hold is still reported online, and the name and last seen are unaffected.
func TestRunRunnerFacts_UnderHAOfflineIsUnknownNotFalse(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	srv.cfg.HA = true
	held := f.addRunner(srv, "alice", "held-here", types.RunnerClaimed, 24*time.Hour)
	elsewhere := f.addRunner(srv, "alice", "held-elsewhere", types.RunnerClaimed, 24*time.Hour)
	srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return id == held.ID }
	heldRun := addLocalRun(f, &held)
	elsewhereRun := addLocalRun(f, &elsewhere)

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+heldRun.ID.String(), adminToken, "")
	if got := runnerFactsOf(t, w.Body.Bytes()); string(got["runner_online"]) != "true" {
		t.Fatalf("a runner this replica holds is a fact: %v", got)
	}
	for _, path := range []string{"/api/v1/runs/" + elsewhereRun.ID.String(), "/api/v1/runs"} {
		w := do(t, srv, http.MethodGet, path, adminToken, "")
		body := w.Body.Bytes()
		if path == "/api/v1/runs" {
			var rows []json.RawMessage
			if json.Unmarshal(body, &rows) != nil {
				t.Fatalf("decode list: %s", body)
			}
			body = nil
			for _, raw := range rows {
				var id struct{ ID uuid.UUID }
				_ = json.Unmarshal(raw, &id)
				if id.ID == elsewhereRun.ID {
					body = raw
				}
			}
		}
		got := runnerFactsOf(t, body)
		if _, present := got["runner_online"]; present || string(got["runner_name"]) != `"held-elsewhere"` {
			t.Fatalf("%s: under HA an unheld runner must keep its name and leave liveness unset: %v", path, got)
		}
	}
}
