// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestStoredRunShape: the run keeps the request's shape, else the one shape
// runShapeRefusal lets the request carry, else none.
func TestStoredRunShape(t *testing.T) {
	harness := []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness}}
	server := createRunRequest{Experience: client.ExperienceInteractive,
		Startup: &client.RunStartup{Kind: client.StartupCommand, Command: "npm run dev"}, PolicyOverlay: json.RawMessage(`{"ui_apps":[{"name":"web","port":3000}]}`)}
	for _, tc := range []struct {
		name string
		req  createRunRequest
		want client.RunShape
	}{
		{"the request's own", createRunRequest{Shape: client.RunShapeEnvironment, Experience: client.ExperienceInteractive, Tools: harness}, client.RunShapeEnvironment},
		{"a background agent task", createRunRequest{Experience: client.ExperienceBackground,
			Workload: &client.RunWorkload{Kind: client.WorkloadAgentTask, Agent: "claude-code", Task: "t"}, Tools: harness}, client.RunShapeAgentTask},
		{"a background command", createRunRequest{Experience: client.ExperienceBackground,
			Workload: &client.RunWorkload{Kind: client.WorkloadCommand, Command: "make"}}, client.RunShapeApplication},
		{"an interactive run with no harness", createRunRequest{Experience: client.ExperienceInteractive}, client.RunShapeEnvironment},
		{"an interactive run with a harness fits agent_task and environment", createRunRequest{Experience: client.ExperienceInteractive, Tools: harness}, ""},
		{"an application server fits environment and application", server, ""},
		{"an older client's run names no experience", createRunRequest{Agent: "claude-code", Task: "t", Interactive: true}, ""},
	} {
		if got := storedRunShape(tc.req); got != tc.want {
			t.Errorf("%s: stored shape %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An application that names a stored policy_id is read against that policy's own
// ui_apps (#3796): the shape check lets it through, the policy must declare an app
// once it resolves, and an unnamed shape is never read off a policy the check
// cannot see.
func TestApplicationOnStoredPolicy(t *testing.T) {
	id := uuid.New()
	app := createRunRequest{Shape: client.RunShapeApplication, Experience: client.ExperienceInteractive, PolicyID: &id,
		Startup: &client.RunStartup{Kind: client.StartupCommand, Command: "npm run dev"}}

	if got := runShapeRefusal(app); got != nil {
		t.Errorf("an interactive command on a stored policy_id was refused before the policy could be read: %+v", got)
	}
	noCommand := app
	noCommand.Startup = nil
	if got := runShapeRefusal(noCommand); got == nil || got.body.Reason != reasonRunShapeConflict {
		t.Errorf("a stored policy_id with no startup command = %+v, want %s", got, reasonRunShapeConflict)
	}

	withApp := types.RunPolicySpec{UIApps: []types.UIApp{{Name: "web", Port: 3000}}}
	if got := storedPolicyApplicationRefusal(app, withApp); got != nil {
		t.Errorf("a policy that declares an app was refused: %+v", got)
	}
	got := storedPolicyApplicationRefusal(app, types.RunPolicySpec{})
	if got == nil || got.status != http.StatusUnprocessableEntity || got.body.Reason != reasonRunShapeConflict {
		t.Errorf("a policy with no app = %+v, want 422 %s", got, reasonRunShapeConflict)
	}
	app.Shape = ""
	if got := storedPolicyApplicationRefusal(app, types.RunPolicySpec{}); got != nil {
		t.Errorf("a run that names no shape was held to declaring an app: %+v", got)
	}
	if got := storedRunShape(app); got != client.RunShapeEnvironment {
		t.Errorf("an unnamed shape on a stored policy_id stores %q, want environment", got)
	}
}
