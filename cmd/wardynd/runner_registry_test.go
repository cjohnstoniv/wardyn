// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Tag-free behavior of the self-registering substrate seam: "none" and an
// unknown -runner behave identically in BOTH build flavors. The per-flavor
// registry expectations live in runner_registry_docker_test.go (docker tag:
// "docker" resolves) and runner_registry_nodocker_test.go (tagless: "docker" is
// not registered — which doubles as the counterfactual for removing
// internal/runner/docker/register.go, since the tagless build IS the build
// without that init()).

package main

import (
	"strings"
	"testing"
)

func rrFlags(runnerSel string) *bootFlags {
	sel, cmap, img := runnerSel, "", "wardyn-proxy:test"
	id, sec, rec := "embedded", "pg", "fs" // the defaults; componentsInfo derefs them
	return &bootFlags{
		runnerSel: &sel, confinementMap: &cmap, proxyImage: &img,
		identitySel: &id, secretStoreSel: &sec, recordingSel: &rec,
	}
}

// -runner none (and "") stays a nil runner with target "none", no registry hit.
func TestBuildRunnerFromFlags_None(t *testing.T) {
	for _, sel := range []string{"none", ""} {
		r, target, err := buildRunnerFromFlags(rrFlags(sel), nil)
		if err != nil {
			t.Fatalf("-runner %q: unexpected error: %v", sel, err)
		}
		if r != nil {
			t.Fatalf("-runner %q: want nil runner, got %T", sel, r)
		}
		if target != "none" {
			t.Fatalf("-runner %q: want target \"none\", got %q", sel, target)
		}
	}
}

// An unknown -runner FAILS CLOSED with the registry-miss error naming the flag.
func TestBuildRunnerFromFlags_UnknownFailsClosed(t *testing.T) {
	r, _, err := buildRunnerFromFlags(rrFlags("bogus"), nil)
	if err == nil {
		t.Fatalf("want error for -runner bogus, got runner %T", r)
	}
	if !strings.Contains(err.Error(), `-runner "bogus"`) || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("want fail-closed registry-miss error naming -runner, got: %v", err)
	}
}
