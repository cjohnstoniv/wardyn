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
