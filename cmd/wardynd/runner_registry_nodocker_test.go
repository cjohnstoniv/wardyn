// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !docker && !k8s

package main

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// A truly tagless build (neither docker nor k8s) never imports
// internal/runner/docker or internal/runner/k8s, so neither substrate's
// register.go init() runs and the registry is EMPTY: -runner docker fails
// closed at resolve — the not-compiled-in error class, now produced by the
// registry miss instead of a hardcoded switch. (This is the live
// counterfactual for removing docker/register.go: this build IS the build
// without that init().) Scoped to !docker && !k8s (not merely !docker) since
// a -tags k8s build also satisfies !docker but registers "k8s", which would
// make the len(names)==0 assertion below false — see runner_registry_k8s_test.go
// for the k8s-tag counterpart of this same counterfactual.
func TestBuildRunnerFromFlags_DockerNotCompiledInFailsClosed(t *testing.T) {
	if names := substrate.Names(); len(names) != 0 {
		t.Fatalf("tagless build must register no substrates, have %v", names)
	}
	r, _, err := buildRunnerFromFlags(rrFlags("docker"), nil)
	if err == nil {
		t.Fatalf("want fail-closed error for -runner docker in a tagless build, got runner %T", r)
	}
	for _, want := range []string{"not registered", "-tags docker"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %q (the not-compiled-in class), got: %v", want, err)
		}
	}
	// And /healthz stays truthful: no substrate is available in this build.
	if avail := componentsInfo(rrFlags("none"), "none")["sandbox"].Available; len(avail) != 0 {
		t.Fatalf("tagless sandbox.available must be empty, got %v", avail)
	}
}
