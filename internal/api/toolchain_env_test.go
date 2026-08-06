// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// The owner's rule under test: what lands in a container follows from the
// workspace's ACTUAL requirements. The toolchain-fidelity env is
// requirements-driven — a scanned workspace gets exactly what its scans
// detected; only a run with no workspace context keeps the full set.
func TestBuildBaseSandboxEnv_ToolchainEnvIsRequirementsDriven(t *testing.T) {
	run := types.AgentRun{CreatedBy: "op", Agent: "claude-code"}
	keys := func(env map[string]string, want map[string]bool) {
		t.Helper()
		for k, present := range want {
			if got := env[k] != ""; got != present {
				t.Errorf("env[%q] present=%v, want %v", k, got, present)
			}
		}
	}

	// nil needs — no workspace context (ad-hoc/BYO/scan/login): full set.
	keys(buildBaseSandboxEnv(run, "http://p:3128", nil),
		map[string]bool{"GOTMPDIR": true, "GOCACHE": true, "MAVEN_OPTS": true, "GRADLE_OPTS": true})

	// Go detected, no JVM: Go group only.
	keys(buildBaseSandboxEnv(run, "http://p:3128", &toolchainNeeds{goTools: true}),
		map[string]bool{"GOTMPDIR": true, "GOCACHE": true, "MAVEN_OPTS": false, "GRADLE_OPTS": false})

	// Maven/Gradle detected, no Go: JVM group only.
	keys(buildBaseSandboxEnv(run, "http://p:3128", &toolchainNeeds{jvmTools: true}),
		map[string]bool{"GOTMPDIR": false, "GOCACHE": false, "MAVEN_OPTS": true, "GRADLE_OPTS": true})

	// Scanned and NEITHER detected: a workspace run's env states what its
	// requirements ground — nothing more.
	keys(buildBaseSandboxEnv(run, "http://p:3128", &toolchainNeeds{}),
		map[string]bool{"GOTMPDIR": false, "GOCACHE": false, "MAVEN_OPTS": false, "GRADLE_OPTS": false})
}

func TestRunToolchainNeeds(t *testing.T) {
	wsWith := func(p workspacescan.WorkspaceProfile) types.Workspace {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return types.Workspace{Profile: raw}
	}

	if got := runToolchainNeeds(nil); got != nil {
		t.Fatalf("no workspaces must mean UNKNOWN (nil), got %+v", got)
	}
	// A workspace with no profile yet declares nothing.
	if got := runToolchainNeeds([]types.Workspace{{}}); got == nil || got.goTools || got.jvmTools {
		t.Fatalf("profileless workspace: want empty needs, got %+v", got)
	}
	// Union across attachments: one Go workspace + one Maven workspace.
	got := runToolchainNeeds([]types.Workspace{
		wsWith(workspacescan.WorkspaceProfile{Languages: []string{"Go", "TypeScript"}}),
		wsWith(workspacescan.WorkspaceProfile{PackageManagers: []string{"maven"}}),
	})
	if got == nil || !got.goTools || !got.jvmTools {
		t.Fatalf("union of Go+maven profiles: want both, got %+v", got)
	}
	// Gradle counts as the JVM signal too.
	if got := runToolchainNeeds([]types.Workspace{wsWith(workspacescan.WorkspaceProfile{PackageManagers: []string{"gradle"}})}); got == nil || !got.jvmTools {
		t.Fatalf("gradle profile: want jvmTools, got %+v", got)
	}
}
