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
//
// The -spec JSON contract (image required, run_id parsed-or-generated, field
// mapping into runner.SandboxSpec) is covered against the REAL production
// loadSpec by TestStandalone_SpecContract_FullMapping/_Defaults in
// standalone_docker_test.go (docker-tagged, daemon-free) rather than here.

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
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

// res2-06: the -spec JSON cannot deliver a user drive, which is the fact three
// comments in internal/runner leaned on while saying the opposite.
//
// Those comments justified drive defence-in-depth checks by naming "a
// hand-written -spec for the standalone runner" as a reachable
// types.DriveMount input. fileSpec declares no drive field and loadSpec never
// assigns SandboxSpec.Drive, so the JSON decodes it into nothing: the reachable
// inputs are a control-plane bug and an in-process caller that builds a
// SandboxSpec itself. The checks are correct and stay; the evidence a reader
// had for keeping them was not, and evidence that does not survive a check is
// how a correct check gets deleted.
//
// READ FROM THE SOURCE rather than exercised, because loadSpec is behind
// `//go:build docker` and this pin has to run in the DEFAULT build — the one
// `go test ./cmd/wardyn-runner/...` runs. Adding a drive field to either place
// fails this and sends whoever adds it to re-derive the three comments.
func TestSpecJSON_CannotCarryADrive(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	start := strings.Index(text, "type fileSpec struct {")
	if start < 0 {
		t.Fatal("main.go declares no fileSpec — the -spec decoder was renamed; re-derive this guard and the comments citing it")
	}
	end := strings.Index(text[start:], "\n}")
	if end < 0 {
		t.Fatal("fileSpec's declaration did not close — the struct scan regressed")
	}
	decl := text[start : start+end]
	// Vacuity guard: the scan must really be looking at the decoded fields.
	if !strings.Contains(decl, `json:"image"`) {
		t.Fatalf("fileSpec scan found no image field, so it would pass vacuously:\n%s", decl)
	}
	if strings.Contains(strings.ToLower(decl), "drive") {
		t.Errorf("fileSpec now decodes a drive field:\n%s\n"+
			"A hand-written -spec can then deliver a types.DriveMount, which internal/runner's drive comments state it cannot.", decl)
	}
	if strings.Contains(text, "Drive:") || strings.Contains(text, ".Drive =") {
		t.Error("main.go assigns SandboxSpec.Drive — the standalone runner can now produce a drive, and internal/runner's drive comments say it cannot")
	}
}
