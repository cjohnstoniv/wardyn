// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmitEnvAsCode(t *testing.T) {
	p := WorkspaceProfile{
		Languages: []string{"Go", "JavaScript"}, PackageManagers: []string{"go", "pnpm"},
		ServicesNeeded: []string{"postgres"}, Confidence: "high", Source: "deterministic",
		SetupCommands: []SetupCommand{
			{Stage: "install", Command: "pnpm install --frozen-lockfile"},
			{Stage: "build", Command: "go build ./..."},
			{Stage: "test", Command: "go test ./..."},
		},
	}
	files, err := EmitEnvAsCode(p, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	dc, ok := files[".devcontainer/devcontainer.json"]
	if !ok {
		t.Fatal("no devcontainer.json emitted")
	}
	var parsed struct {
		Build *struct {
			Dockerfile string `json:"dockerfile"`
		} `json:"build"`
		Features     map[string]any    `json:"features"`
		ContainerEnv map[string]string `json:"containerEnv"`
	}
	if err := json.Unmarshal([]byte(dc), &parsed); err != nil {
		t.Fatalf("devcontainer.json not valid JSON: %v", err)
	}
	// No postCreateCommand, ever: there is no verify step to prove a detected
	// install/build command actually works, so nothing is auto-run, unattended,
	// on container create — it's prose in AGENTS.md instead (asserted below).
	if strings.Contains(dc, "postCreateCommand") {
		t.Errorf("devcontainer.json must never carry postCreateCommand (no verify step to prove it works): %s", dc)
	}
	// The standard agent-tool install is unconditional: every emit points
	// build.dockerfile at the generated Dockerfile rather than naming `image`.
	if parsed.Build == nil || parsed.Build.Dockerfile != "Dockerfile" {
		t.Errorf("expected build.dockerfile pointing at the generated Dockerfile: %+v", parsed)
	}
	if df := files[".devcontainer/Dockerfile"]; !strings.Contains(df, "downloads.claude.ai") {
		t.Errorf("the exported Dockerfile must carry claude-code's install: %q", df)
	}
	if len(parsed.Features) != 2 {
		t.Errorf("expected go+node features, got %v", parsed.Features)
	}
	// Go detected => containerEnv carries GOTMPDIR (the fidelity fix runs.go's
	// sandboxEnv applies at dispatch, replicated for a workspace built outside
	// Wardyn from this exported env-as-code).
	if parsed.ContainerEnv["GOTMPDIR"] == "" {
		t.Errorf("containerEnv missing GOTMPDIR for a Go workspace: %v", parsed.ContainerEnv)
	}
	agents := files["AGENTS.md"]
	if !strings.Contains(agents, "go test ./...") || !strings.Contains(agents, "postgres") {
		t.Errorf("AGENTS.md missing setup commands or services: %s", agents)
	}
	if !strings.Contains(agents, "GOTMPDIR") {
		t.Errorf("AGENTS.md missing the GOTMPDIR fidelity note: %s", agents)
	}
	if strings.Contains(agents, "Maven proxy") {
		t.Errorf("AGENTS.md should not mention Maven for a non-Maven workspace: %s", agents)
	}
}

func TestEmitEnvAsCode_MavenNoteAndNoGoNoise(t *testing.T) {
	p := WorkspaceProfile{Languages: []string{"Java"}, PackageManagers: []string{"maven"}}
	files, err := EmitEnvAsCode(p, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	dc := files[".devcontainer/devcontainer.json"]
	var parsed struct {
		ContainerEnv map[string]string `json:"containerEnv"`
	}
	if err := json.Unmarshal([]byte(dc), &parsed); err != nil {
		t.Fatalf("devcontainer.json not valid JSON: %v", err)
	}
	// No Go detected => no GOTMPDIR noise.
	if len(parsed.ContainerEnv) != 0 {
		t.Errorf("containerEnv should be empty for a non-Go workspace, got %v", parsed.ContainerEnv)
	}
	agents := files["AGENTS.md"]
	if !strings.Contains(agents, "Maven proxy") {
		t.Errorf("AGENTS.md missing the Maven proxy note: %s", agents)
	}
	if strings.Contains(agents, "GOTMPDIR") {
		t.Errorf("AGENTS.md should not mention GOTMPDIR for a non-Go workspace: %s", agents)
	}
}

// TestEmitEnvAsCode_ResolvedBaseRef pins WSPIPE-9: a workspace pinned to a
// non-recommended base image (registry/custom/byo) must export a devcontainer
// whose generated Dockerfile FROMs THAT image, not the universal genBaseImage
// default — otherwise a committed devcontainer.json describes an environment
// nobody actually boots.
func TestEmitEnvAsCode_ResolvedBaseRef(t *testing.T) {
	const custom = "mycorp/build:2024.3"
	p := WorkspaceProfile{Languages: []string{"Go"}, Confidence: "high", Source: "deterministic"}

	files, err := EmitEnvAsCode(p, nil, custom)
	if err != nil {
		t.Fatal(err)
	}
	if df := files[".devcontainer/Dockerfile"]; !strings.HasPrefix(df, "FROM "+custom+"\n") {
		t.Errorf("generated Dockerfile must FROM the resolved base ref, got: %s", df)
	}
}

// TestEmitEnvAsCode_AlwaysBakesAgentTool pins the unconditional bake: every
// emit carries the generated Dockerfile (never a lifecycle command, which
// would bake nothing into the pushed image anyway) — independent of anything
// about the workspace's integrations.
func TestEmitEnvAsCode_AlwaysBakesAgentTool(t *testing.T) {
	p := WorkspaceProfile{Languages: []string{"Go"}, Confidence: "high", Source: "deterministic"}
	files, err := EmitEnvAsCode(p, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	dc := files[".devcontainer/devcontainer.json"]
	if _, ok := files[".devcontainer/Dockerfile"]; !ok {
		t.Error("every emit must carry the generated Dockerfile — the standard agent-tool install is unconditional")
	}
	if strings.Contains(dc, "CreateCommand") {
		t.Errorf("a lifecycle command bakes nothing into the pushed image: %s", dc)
	}
}
