// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

// contain needs no engine: a docker context that cannot be resolved does not stop it reaching its own checks.
func TestContain_SkipsDockerContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "")
	if err := os.Unsetenv("DOCKER_HOST"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONTEXT", "no-such-context")
	if err := followDockerContext(); err == nil {
		t.Fatal("the docker context resolved; the test proves nothing")
	}
	cmd := rootCommand()
	cmd.SetArgs([]string{"contain"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "give exactly one of --run") {
		t.Fatalf("contain with an unresolvable docker context = %v, want its own argument check", err)
	}
}
