// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CreateRunStoresTheChosenExperienceOnly creates runs through the real
// door and reads them back from Postgres: a new client's run stores the mode it
// chose (and is background-only exactly when it chose Background), and an older
// client's run, whatever its legacy flags say, stores none.
func TestPG_CreateRunStoresTheChosenExperienceOnly(t *testing.T) {
	srv, pool := pgHarness(t)
	srv.cfg.AgentImages = composeAgentImages
	for _, tc := range []struct {
		name, body     string
		experience     types.RunExperience
		interactive    bool
		agent, task    string
		backgroundOnly bool
	}{
		{"background agent task", `{"title":"t","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},` +
			`"tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true}`, types.ExperienceBackground, false, "claude-code", "fix the build", true},
		{"interactive with a startup command", `{"title":"t","experience":"interactive","tools":[{"id":"claude-code","kind":"harness"}],` +
			`"startup":{"kind":"command","command":"npm run dev"},"no_repositories_or_drives":true}`, types.ExperienceInteractive, true, "claude-code", "npm run dev", false},
		{"an older client's autonomous run", `{"agent":"claude-code","repo":"acme/widgets","task":"t"}`, "", false, "claude-code", "t", false},
		{"an older client's interactive run", `{"agent":"claude-code","repo":"acme/widgets","interactive":true}`, "", true, "claude-code", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, tc.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", w.Code, w.Body.String())
			}
			var created types.AgentRun
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			stored, err := store.NewPG(pool).GetRun(context.Background(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Admission and the doors read one value: a Background-only place admits a run exactly when the
			// doors treat it as background-only, so an older client's run (stored with none) is not admitted.
			backgroundOnlyPool := types.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{}}
			if admitted := backgroundOnlyPool.AllowsRunType(types.RunnerPoolRunType(stored.Experience)); admitted != tc.backgroundOnly {
				t.Errorf("a Background-only place admitted = %v, but the run's BackgroundOnly = %v", admitted, tc.backgroundOnly)
			}
			for _, run := range []types.AgentRun{created, stored} {
				if run.Experience != tc.experience || run.BackgroundOnly() != tc.backgroundOnly || run.Interactive != tc.interactive ||
					run.Agent != tc.agent || run.Task != tc.task {
					t.Errorf("run = experience %q interactive %v agent %q task %q, want %q %v %q %q", run.Experience, run.Interactive, run.Agent, run.Task,
						tc.experience, tc.interactive, tc.agent, tc.task)
				}
			}
		})
	}
}

// TestPG_ABackgroundRunHasNoIdleStopFromAnySource creates runs through the real
// door whose idle stop comes from the deployment default and from a stored
// policy (the sources a person did not write in the request): a background run
// stores 0 and says so, an interactive run keeps what the policy gave it.
func TestPG_ABackgroundRunHasNoIdleStopFromAnySource(t *testing.T) {
	srv, pool := pgHarness(t)
	srv.cfg.AgentImages = composeAgentImages
	srv.cfg.DefaultPolicy.AutoStopAfterSec = 1800
	// A policy name is unique in the database, so a reused database must not meet last run's.
	polName := "idle-900-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	w := do(t, srv, http.MethodPost, "/api/v1/policies", adminToken,
		`{"name":"`+polName+`","spec":{"allowed_domains":["api.anthropic.com"],"min_confinement_class":"CC2","auto_stop_after_sec":900}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create policy = %d %s", w.Code, w.Body.String())
	}
	var pol struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &pol); err != nil || pol.ID == "" {
		t.Fatalf("policy id: %v %s", err, w.Body.String())
	}
	const bg = `"experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},"tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	const inter = `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	for _, tc := range []struct {
		name, body string
		want       int
		warned     bool
	}{
		{"background, deployment default", `{"title":"t",` + bg + `}`, 0, true},
		{"background, stored policy", `{"title":"t",` + bg + `,"policy_id":"` + pol.ID + `"}`, 0, true},
		{"interactive, deployment default", `{"title":"t",` + inter + `}`, 1800, false},
		{"interactive, stored policy", `{"title":"t",` + inter + `,"policy_id":"` + pol.ID + `"}`, 900, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, tc.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", w.Code, w.Body.String())
			}
			var created createRunResponse
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			stored, err := store.NewPG(pool).GetRun(context.Background(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if created.AutoStopAfterSec != tc.want || stored.AutoStopAfterSec != tc.want {
				t.Errorf("idle stop = created %d stored %d, want %d", created.AutoStopAfterSec, stored.AutoStopAfterSec, tc.want)
			}
			warned := slices.ContainsFunc(created.Warnings, func(s string) bool { return strings.Contains(s, "idle stop") })
			if warned != tc.warned {
				t.Errorf("idle-stop warning = %v, want %v (%v)", warned, tc.warned, created.Warnings)
			}
		})
	}
}
