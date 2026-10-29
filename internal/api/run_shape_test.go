// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

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
