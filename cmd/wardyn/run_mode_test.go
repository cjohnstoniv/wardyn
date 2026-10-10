// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"strings"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestApplyRunModeFlags(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  sdk.CreateRunRequest
		flags runModeFlags
		want  sdk.CreateRunRequest
		err   string
	}{
		{"no run-mode flag leaves the older request alone", sdk.CreateRunRequest{Agent: "claude-code", Task: "t", Interactive: false}, runModeFlags{},
			sdk.CreateRunRequest{Agent: "claude-code", Task: "t"}, ""},
		{"a background agent task", sdk.CreateRunRequest{Agent: "claude-code", Task: "fix", ModelProvider: "bedrock-team"}, runModeFlags{experience: "background", noRepos: true},
			sdk.CreateRunRequest{Experience: sdk.ExperienceBackground, NoRepositoriesOrDrives: true,
				Workload: &sdk.RunWorkload{Kind: sdk.WorkloadAgentTask, Agent: "claude-code", Task: "fix"},
				Tools:    []sdk.IncludedTool{{ID: "claude-code", Kind: sdk.IncludedToolHarness, ModelProvider: "bedrock-team"}}}, ""},
		{"a background command", sdk.CreateRunRequest{}, runModeFlags{experience: "background", command: "make test"},
			sdk.CreateRunRequest{Experience: sdk.ExperienceBackground, Workload: &sdk.RunWorkload{Kind: sdk.WorkloadCommand, Command: "make test"}}, ""},
		{"an interactive tool and its startup", sdk.CreateRunRequest{Agent: "claude-code"}, runModeFlags{experience: "interactive", startup: "harness:claude-code", startFolder: "image-default"},
			sdk.CreateRunRequest{Experience: sdk.ExperienceInteractive, Tools: []sdk.IncludedTool{{ID: "claude-code", Kind: sdk.IncludedToolHarness}},
				Startup: &sdk.RunStartup{Kind: sdk.StartupHarness, Tool: "claude-code"}, StartFolder: &sdk.StartFolder{Kind: sdk.StartFolderImageDefault}}, ""},
		{"tool flags carry their own provider", sdk.CreateRunRequest{}, runModeFlags{experience: "interactive", tools: []string{"claude-code=bedrock-team", "codex-cli"}, startup: "none"},
			sdk.CreateRunRequest{Experience: sdk.ExperienceInteractive, Startup: &sdk.RunStartup{Kind: sdk.StartupNone},
				Tools: []sdk.IncludedTool{{ID: "claude-code", Kind: sdk.IncludedToolHarness, ModelProvider: "bedrock-team"}, {ID: "codex-cli", Kind: sdk.IncludedToolHarness}}}, ""},
		{"a startup command", sdk.CreateRunRequest{Agent: "claude-code"}, runModeFlags{experience: "interactive", startup: "command:npm run dev: now"},
			sdk.CreateRunRequest{Experience: sdk.ExperienceInteractive, Tools: []sdk.IncludedTool{{ID: "claude-code", Kind: sdk.IncludedToolHarness}},
				Startup: &sdk.RunStartup{Kind: sdk.StartupCommand, Command: "npm run dev: now"}}, ""},
		{"a start folder inside an attachment", sdk.CreateRunRequest{}, runModeFlags{experience: "interactive", tools: []string{"claude-code"}, startFolder: "drive:src/app"},
			sdk.CreateRunRequest{Experience: sdk.ExperienceInteractive, Tools: []sdk.IncludedTool{{ID: "claude-code", Kind: sdk.IncludedToolHarness}},
				StartFolder: &sdk.StartFolder{Kind: sdk.StartFolderAttachment, Attachment: "drive", Subpath: "src/app"}}, ""},

		{"the mode is never inferred", sdk.CreateRunRequest{}, runModeFlags{command: "make"}, sdk.CreateRunRequest{}, "never inferred"},
		{"no experience, a startup", sdk.CreateRunRequest{}, runModeFlags{startup: "none"}, sdk.CreateRunRequest{}, "never inferred"},
		{"the older mode flags are replaced, not mixed", sdk.CreateRunRequest{Interactive: true}, runModeFlags{experience: "interactive"}, sdk.CreateRunRequest{}, "replaces"},
		{"unknown experience", sdk.CreateRunRequest{}, runModeFlags{experience: "batch"}, sdk.CreateRunRequest{}, "not one of"},
		{"a command runs no agent", sdk.CreateRunRequest{Agent: "claude-code"}, runModeFlags{experience: "background", command: "make"}, sdk.CreateRunRequest{}, "runs no agent"},
		{"a background run has no startup", sdk.CreateRunRequest{}, runModeFlags{experience: "background", startup: "none"}, sdk.CreateRunRequest{}, "interactive"},
		{"an interactive run starts a command with --startup", sdk.CreateRunRequest{}, runModeFlags{experience: "interactive", command: "make"}, sdk.CreateRunRequest{}, "--startup"},
		{"agent and tool are one choice", sdk.CreateRunRequest{Agent: "a"}, runModeFlags{experience: "interactive", tools: []string{"b"}}, sdk.CreateRunRequest{}, "one or the other"},
		{"a startup kind is closed", sdk.CreateRunRequest{}, runModeFlags{experience: "interactive", startup: "both:x"}, sdk.CreateRunRequest{}, "not none"},
		{"a start folder needs an attachment", sdk.CreateRunRequest{}, runModeFlags{startFolder: ":src"}, sdk.CreateRunRequest{}, "ATTACHMENT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.body
			err := applyRunModeFlags(&got, tc.flags)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one naming %q", err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v (%v)\nwant %+v", got, err, tc.want)
			}
		})
	}
}
