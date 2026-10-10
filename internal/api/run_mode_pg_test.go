// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CreateRunStoresTheChosenExperienceOnly creates runs through the real
// door and reads them back from Postgres: a new client's run stores the mode it
// chose (and is background-only exactly when it chose Background), and an older
// client's run, whatever its legacy flags say, stores none.
func TestPG_CreateRunStoresTheChosenExperienceOnly(t *testing.T) {
	srv, pool := pgHarness(t)
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
