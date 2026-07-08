// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Unit tests for the wardyn-runner binary in the DEFAULT (no -tags docker) build.
//
// Build layout (see main.go / main_nodocker.go): the real standalone runner —
// flag parsing, -spec loading, the -capabilities path, and the docker driver —
// is gated behind `//go:build docker`. The default build compiles only the stub
// in main_nodocker.go, whose entire job is to be HONEST about the absent
// substrate: it must not pretend to run anything. This lane stays pure (no
// daemon, no -tags docker), so we cover:
//
//   - stub honesty: the default build refuses to run and exits non-zero with a
//     clear "rebuild with -tags docker" message (capability honesty at the
//     binary level — a no-substrate build claims no runner).
//   - -spec JSON contract: the documented -spec schema must deserialize into the
//     runner.SandboxSpec the binary feeds the driver, with the same required /
//     defaulting rules (image required, run_id parsed-or-generated).

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// --- stub binary honesty -----------------------------------------------------

// TestStubMain_HonestlyRefusesWithoutDocker builds the default (no -tags docker)
// wardyn-runner and runs it. The stub must fail closed: exit code 2 and a message
// pointing at the docker build tag. This is the binary-level analogue of
// "capability honesty" — a build with no runner substrate must NOT behave as if
// it could create sandboxes.
//
// Pattern: re-exec a freshly built copy of THIS package so we exercise the real
// main() (which calls os.Exit) without the test process itself exiting.
func TestStubMain_HonestlyRefusesWithoutDocker(t *testing.T) {
	bin := buildStub(t)

	cmd := exec.Command(bin)
	out, err := cmd.CombinedOutput()

	// Expect a non-zero exit (os.Exit(2)); err must be a *exec.ExitError.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("stub should exit non-zero, got err=%v output=%q", err, out)
	}
	if code := exitErr.ExitCode(); code != 2 {
		t.Fatalf("stub exit code = %d, want 2", code)
	}
	if got := string(out); !strings.Contains(got, "docker driver not compiled in") ||
		!strings.Contains(got, "-tags docker") {
		t.Fatalf("stub message %q should explain the docker driver is absent and how to rebuild", got)
	}
}

// buildStub compiles the default-build wardyn-runner (no -tags docker) to a temp
// path and returns it. A build failure fails the test outright (the no-docker
// build is the control-plane parity build and must always compile).
func buildStub(t *testing.T) string {
	t.Helper()
	bin := t.TempDir() + "/wardyn-runner-stub"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build default wardyn-runner: %v", err)
	}
	return bin
}

// --- -spec JSON contract -----------------------------------------------------

// specContract mirrors the wardyn-runner -spec JSON schema (fileSpec in main.go,
// which is docker-tagged and therefore not importable in this no-docker lane).
// We assert the DOCUMENTED schema deserializes into the runner.SandboxSpec the
// binary builds, exercising the real public runner/types so drift between the
// -spec schema and the runner contract is caught.
type specContract struct {
	RunID            string                 `json:"run_id,omitempty"`
	Image            string                 `json:"image"`
	ConfinementClass types.ConfinementClass `json:"confinement_class,omitempty"`
	Env              map[string]string      `json:"env,omitempty"`
	Resources        struct {
		CPUMillis int64 `json:"cpu_millis,omitempty"`
		MemoryMiB int64 `json:"memory_mib,omitempty"`
		DiskMiB   int64 `json:"disk_mib,omitempty"`
	} `json:"resources,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// toSandboxSpec applies the same required/defaulting rules main.go's loadSpec
// applies: image is required, run_id is parsed when present else a fresh UUID is
// generated, and the resource/env/label fields map straight through.
func toSandboxSpec(raw []byte) (runner.SandboxSpec, error) {
	var fs specContract
	if err := json.Unmarshal(raw, &fs); err != nil {
		return runner.SandboxSpec{}, err
	}
	if fs.Image == "" {
		return runner.SandboxSpec{}, errors.New("spec.image is required")
	}
	runID := uuid.New()
	if fs.RunID != "" {
		id, err := uuid.Parse(fs.RunID)
		if err != nil {
			return runner.SandboxSpec{}, err
		}
		runID = id
	}
	return runner.SandboxSpec{
		RunID:            runID,
		Image:            fs.Image,
		ConfinementClass: fs.ConfinementClass,
		Env:              fs.Env,
		Resources: runner.Resources{
			CPUMillis: fs.Resources.CPUMillis,
			MemoryMiB: fs.Resources.MemoryMiB,
			DiskMiB:   fs.Resources.DiskMiB,
		},
		Labels: fs.Labels,
	}, nil
}

// TestSpecContract_FullMapping verifies a fully-populated -spec JSON document
// deserializes into the corresponding runner.SandboxSpec field-for-field.
func TestSpecContract_FullMapping(t *testing.T) {
	id := uuid.New()
	raw := []byte(`{
		"run_id": "` + id.String() + `",
		"image": "ghcr.io/acme/agent:v1",
		"confinement_class": "CC2",
		"env": {"FOO": "bar", "BAZ": "qux"},
		"resources": {"cpu_millis": 1500, "memory_mib": 512, "disk_mib": 2048},
		"labels": {"team": "research"}
	}`)

	spec, err := toSandboxSpec(raw)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if spec.RunID != id {
		t.Errorf("RunID = %s, want parsed %s", spec.RunID, id)
	}
	if spec.Image != "ghcr.io/acme/agent:v1" {
		t.Errorf("Image = %q, want ghcr.io/acme/agent:v1", spec.Image)
	}
	if spec.ConfinementClass != types.CC2 {
		t.Errorf("ConfinementClass = %q, want CC2", spec.ConfinementClass)
	}
	if spec.Env["FOO"] != "bar" || spec.Env["BAZ"] != "qux" {
		t.Errorf("Env = %v, want FOO=bar BAZ=qux", spec.Env)
	}
	if spec.Resources.CPUMillis != 1500 || spec.Resources.MemoryMiB != 512 || spec.Resources.DiskMiB != 2048 {
		t.Errorf("Resources = %+v, want {1500 512 2048}", spec.Resources)
	}
	if spec.Labels["team"] != "research" {
		t.Errorf("Labels = %v, want team=research", spec.Labels)
	}
}

// TestSpecContract_ImageRequired pins the only hard-required field: a spec with
// no image must be rejected (the driver cannot create a sandbox without one).
func TestSpecContract_ImageRequired(t *testing.T) {
	_, err := toSandboxSpec([]byte(`{"confinement_class":"CC2"}`))
	if err == nil {
		t.Fatal("want error for missing image, got nil")
	}
	if !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("error %q should mention image is required", err.Error())
	}
}

// TestSpecContract_RunIDOmittedGeneratesFresh verifies an omitted run_id yields
// a fresh, non-nil UUID (standalone mode lets run_id be omitted), and two parses
// of the same image produce DIFFERENT run ids (genuinely fresh, not a constant).
func TestSpecContract_RunIDOmittedGeneratesFresh(t *testing.T) {
	a, err := toSandboxSpec([]byte(`{"image":"img"}`))
	if err != nil {
		t.Fatalf("parse a: %v", err)
	}
	b, err := toSandboxSpec([]byte(`{"image":"img"}`))
	if err != nil {
		t.Fatalf("parse b: %v", err)
	}
	if a.RunID == uuid.Nil {
		t.Fatal("omitted run_id must generate a non-nil UUID")
	}
	if a.RunID == b.RunID {
		t.Fatalf("omitted run_id must be freshly generated each parse; got identical %s", a.RunID)
	}
}

// TestSpecContract_RunIDInvalidRejected verifies a present-but-malformed run_id
// is a hard error rather than being silently replaced by a fresh UUID (which
// would mask an operator typo and detach the sandbox from its intended run).
func TestSpecContract_RunIDInvalidRejected(t *testing.T) {
	_, err := toSandboxSpec([]byte(`{"image":"img","run_id":"not-a-uuid"}`))
	if err == nil {
		t.Fatal("want error for malformed run_id, got nil")
	}
}

// TestSpecContract_MalformedJSONRejected verifies invalid JSON surfaces an error
// (the binary wraps this as "parse spec").
func TestSpecContract_MalformedJSONRejected(t *testing.T) {
	if _, err := toSandboxSpec([]byte(`{not json`)); err == nil {
		t.Fatal("want error for malformed JSON, got nil")
	}
}
