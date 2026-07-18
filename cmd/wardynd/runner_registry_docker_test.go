// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// Under -tags docker the blank import in runner_docker.go pulls in
// internal/runner/docker/register.go, whose init() self-registers "docker".
//
// COUNTERFACTUAL: delete internal/runner/docker/register.go and this fails with
// `component "docker" is not registered` — exactly what the tagless build gets
// (runner_registry_nodocker_test.go), proving registration (not a hardcoded
// switch) is what makes -runner docker work.
func TestSubstrateRegistry_DockerResolvesUnderDockerTag(t *testing.T) {
	if names := substrate.Names(); !slices.Contains(names, "docker") {
		t.Fatalf("substrate registry: want \"docker\" registered under -tags docker, have %v", names)
	}
	sub, err := substrate.New("docker", substrate.Deps{ProxyImage: "wardyn-proxy:test"})
	if err != nil {
		t.Fatalf("substrate.New(docker): %v", err)
	}
	if got := sub.Name(); got != "docker" {
		t.Fatalf("substrate name: want docker, got %q", got)
	}
}

// The boot path end to end: -runner docker resolves via the registry, wraps in
// the orchestrator, and advertises the ACTUAL substrate name.
func TestBuildRunnerFromFlags_DockerEnabled(t *testing.T) {
	r, target, err := buildRunnerFromFlags(rrFlags("docker"), nil)
	if err != nil {
		t.Fatalf("buildRunnerFromFlags(docker): %v", err)
	}
	if r == nil || target != "docker" {
		t.Fatalf("want orchestrated docker runner with target \"docker\", got %T / %q", r, target)
	}
	if !strings.Contains(strings.Join(componentsInfo(rrFlags("docker"), target)["sandbox"].Available, ","), "docker") {
		t.Fatalf("healthz sandbox.available must list the registered docker substrate")
	}
}
