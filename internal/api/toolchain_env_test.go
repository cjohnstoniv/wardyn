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
	// An attached workspace with NO DECODABLE profile (unattached/unscanned/
	// malformed) is UNKNOWN too — nil, so dispatch keeps the full accommodation
	// set. Empty needs is reserved for a workspace that actually scanned and
	// decoded a profile with neither signal.
	if got := runToolchainNeeds([]types.Workspace{{}}); got != nil {
		t.Fatalf("profileless workspace: want UNKNOWN (nil), got %+v", got)
	}
	if got := runToolchainNeeds([]types.Workspace{wsWith(workspacescan.WorkspaceProfile{Languages: []string{"Python"}})}); got == nil || got.goTools || got.jvmTools {
		t.Fatalf("scanned workspace with neither signal: want empty needs (not nil), got %+v", got)
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
	// MIXED: one scanned workspace + one unscanned workspace. The unscanned
	// one must dominate the whole set to UNKNOWN (nil) — dropping it from the
	// union (as if it declared nothing) would ship a Go-only env for a run
	// that also attached a workspace nobody has scanned yet.
	if got := runToolchainNeeds([]types.Workspace{
		wsWith(workspacescan.WorkspaceProfile{Languages: []string{"Go"}}),
		{}, // no profile: unattached/unscanned/malformed
	}); got != nil {
		t.Fatalf("mixed scanned+unscanned attachment: want UNKNOWN (nil), got %+v", got)
	}
}

// TestApplyEphemeralDirsEnv is the dispatch-level assertion for audit row 56:
// applyEphemeralDirsEnv is the exact function dispatchRun calls to land a
// run's ephemeral workspace-source targets in sandboxEnv as
// WARDYN_EPHEMERAL_DIRS — nil/empty dirs (the ordinary case) must add
// nothing, and non-empty dirs must land comma-joined, in order.
func TestApplyEphemeralDirsEnv(t *testing.T) {
	env := map[string]string{}
	applyEphemeralDirsEnv(env, nil)
	if _, ok := env["WARDYN_EPHEMERAL_DIRS"]; ok {
		t.Errorf("no ephemeral dirs must add nothing, got env = %v", env)
	}

	applyEphemeralDirsEnv(env, []string{"/home/agent/scratch-a", "/home/agent/scratch-b"})
	if got, want := env["WARDYN_EPHEMERAL_DIRS"], "/home/agent/scratch-a,/home/agent/scratch-b"; got != want {
		t.Errorf("WARDYN_EPHEMERAL_DIRS = %q, want %q", got, want)
	}
}
