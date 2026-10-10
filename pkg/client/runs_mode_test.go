// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEffectiveExperienceIsTheDocumentedMapping pins the older-client mapping:
// interactive, or no task, is interactive; a task is background; an explicit
// experience is its own answer.
func TestEffectiveExperienceIsTheDocumentedMapping(t *testing.T) {
	for _, tc := range []struct {
		req  CreateRunRequest
		want Experience
	}{
		{CreateRunRequest{Agent: "claude-code", Task: "fix"}, ExperienceBackground},
		{CreateRunRequest{Agent: "codex-cli", Task: "echo", TaskMode: "exec"}, ExperienceBackground},
		{CreateRunRequest{Agent: "claude-code", Interactive: true, Task: "seed"}, ExperienceInteractive},
		{CreateRunRequest{Agent: "claude-code"}, ExperienceInteractive},
		{CreateRunRequest{Agent: "claude-code", Task: "  "}, ExperienceInteractive},
		{CreateRunRequest{Experience: ExperienceBackground, Interactive: true}, ExperienceBackground},
		{CreateRunRequest{Experience: ExperienceInteractive, Task: "t"}, ExperienceInteractive},
	} {
		if got := tc.req.EffectiveExperience(); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.req, got, tc.want)
		}
	}
}

func TestUsesRunModeIsTheNewClientTest(t *testing.T) {
	for name, req := range map[string]CreateRunRequest{
		"experience":   {Experience: ExperienceBackground},
		"workload":     {Workload: &RunWorkload{}},
		"tools":        {Tools: []IncludedTool{{}}},
		"startup":      {Startup: &RunStartup{}},
		"start folder": {StartFolder: &StartFolder{}},
		"no repos":     {NoRepositoriesOrDrives: true},
	} {
		if !req.UsesRunMode() {
			t.Errorf("%s does not mark a new client", name)
		}
	}
	for _, req := range []CreateRunRequest{{}, {Agent: "a", Task: "t", Interactive: true, TaskMode: "exec", InteractiveStart: "agent"}} {
		if req.UsesRunMode() {
			t.Errorf("%+v is an older client's", req)
		}
	}
}

// TestRunModeWireShape pins the JSON names the server and the console share.
func TestRunModeWireShape(t *testing.T) {
	req := CreateRunRequest{
		Experience:             ExperienceInteractive,
		Tools:                  []IncludedTool{{ID: "claude-code", Kind: IncludedToolHarness, ModelProvider: "bedrock-team"}},
		Startup:                &RunStartup{Kind: StartupHarness, Tool: "claude-code"},
		StartFolder:            &StartFolder{Kind: StartFolderAttachment, Attachment: StartFolderDrive, Subpath: "src"},
		Workload:               &RunWorkload{Kind: WorkloadAgentTask, Agent: "a", Task: "t"},
		NoRepositoriesOrDrives: true,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"experience":"interactive"`, `"tools":[{"id":"claude-code","kind":"harness","model_provider":"bedrock-team"}]`,
		`"startup":{"kind":"harness","tool":"claude-code"}`, `"start_folder":{"kind":"attachment","attachment":"drive","subpath":"src"}`,
		`"workload":{"kind":"agent_task","agent":"a","task":"t"}`, `"no_repositories_or_drives":true`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s missing %s", raw, want)
		}
	}
	var none CreateRunRequest
	raw, _ = json.Marshal(none)
	for _, key := range []string{"experience", "workload", "tools", "startup", "start_folder", "no_repositories_or_drives"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("an older client's body carries %s: %s", key, raw)
		}
	}
	if k := (PushRuleOverride{Provider: "github", Org: "Acme"}).PushRuleKey(); k != "github/acme" {
		t.Errorf("push key = %q", k)
	}
}
