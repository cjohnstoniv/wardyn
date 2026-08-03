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
	files, err := EmitEnvAsCode(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	dc, ok := files[".devcontainer/devcontainer.json"]
	if !ok {
		t.Fatal("no devcontainer.json emitted")
	}
	var parsed struct {
		Image        string            `json:"image"`
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
	files, err := EmitEnvAsCode(p, nil)
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
