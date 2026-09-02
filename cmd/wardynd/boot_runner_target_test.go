// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// The gap this flag closes: a daemon booted with -runner none resolves the
// runner target "none", api.Config.RunnerTarget carries it, and
// types.ValidateUserDrive compares every drive's
// types.DriveBackend.RunnerTarget ("docker" or "k8s") against it — so a
// runner-less daemon refuses EVERY backend with a 400 and no drive can be
// registered there by any route. That is exactly the daemon the Playwright
// backend boots (scripts/e2e-backend.sh), which is how the lane found it.
//
// WARDYN_RUNNER_TARGET moves the REGISTRATION boundary only. The four legs
// below are the whole contract: the default is unchanged, the override applies
// only where no runner exists, a configured runner is never overridden, and an
// unknown value refuses at boot instead of advertising a target no stored
// object could ever match.
func TestBuildRunnerFromFlags_RunnerTargetOverride(t *testing.T) {
	withOverride := func(sel, target string) *bootFlags {
		f := rrFlags(sel)
		f.runnerTargetOverride = &target
		return f
	}

	// 1. No runner, no override: byte-identical to before the flag existed.
	t.Run("none without an override stays none", func(t *testing.T) {
		r, target, err := buildRunnerFromFlags(rrFlags("none"), nil)
		if err != nil || r != nil || target != "none" {
			t.Fatalf("got (%T, %q, %v), want (nil, \"none\", nil)", r, target, err)
		}
	})

	// 2. No runner + a known override: the daemon advertises that target, and
	//    the runner is STILL nil — registration widened, dispatch untouched.
	for _, want := range knownRunnerTargets() {
		t.Run("none with override "+want, func(t *testing.T) {
			r, target, err := buildRunnerFromFlags(withOverride("none", want), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if target != want {
				t.Errorf("target = %q, want %q", target, want)
			}
			if r != nil {
				t.Errorf("runner = %T, want nil — the override must never make this daemon dispatch", r)
			}
		})
	}
	// Surrounding whitespace is the shape a compose/env passthrough produces;
	// an all-blank value is the unset default, not a refusal.
	t.Run("none with a whitespace-padded override", func(t *testing.T) {
		_, target, err := buildRunnerFromFlags(withOverride("none", "  docker\n"), nil)
		if err != nil || target != "docker" {
			t.Fatalf("got (%q, %v), want (\"docker\", nil)", target, err)
		}
	})
	t.Run("none with a blank override is the unset default", func(t *testing.T) {
		_, target, err := buildRunnerFromFlags(withOverride("none", "   "), nil)
		if err != nil || target != "none" {
			t.Fatalf("got (%q, %v), want (\"none\", nil)", target, err)
		}
	})

	// 3. A CONFIGURED runner ignores the override entirely: the resolved
	//    substrate's own name is the only truthful target, so the override is
	//    not even consulted. Which half of that is observable depends on the
	//    test build's tag set, so the case branches on the registry rather
	//    than on a build tag: with the docker substrate registered (-tags
	//    docker) the runner resolves and keeps its own name; in the tagless
	//    build (runner_registry_nodocker_test.go registers nothing) -runner
	//    docker fails at the registry, and the override neither rescues that
	//    failure nor becomes the target.
	t.Run("a configured runner ignores the override", func(t *testing.T) {
		r, target, err := buildRunnerFromFlags(withOverride("docker", "k8s"), nil)
		if slices.Contains(substrate.Names(), "docker") {
			if err != nil {
				t.Fatalf("unexpected error resolving the registered docker substrate: %v", err)
			}
			if r == nil {
				t.Error("runner = nil: -runner docker resolved, so the daemon must dispatch")
			}
			if target != "docker" {
				t.Errorf("target = %q, want \"docker\": the override was consulted for a configured runner", target)
			}
			return
		}
		if err == nil {
			t.Fatal("want the registry-miss refusal; the override must not rescue an unresolvable -runner")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Errorf("err = %v, want the registry-miss error", err)
		}
		if target == "k8s" {
			t.Error("target = \"k8s\": the override was consulted for a configured runner")
		}
	})

	// 4. An unknown override fails boot CLOSED, naming the flag and the set.
	for _, bad := range []string{"docker,k8s", "none", "kubernetes", "DOCKER"} {
		t.Run("unknown override "+bad, func(t *testing.T) {
			_, _, err := buildRunnerFromFlags(withOverride("none", bad), nil)
			if err == nil {
				t.Fatalf("-runner-target %q was accepted; an unmatchable target must refuse at boot", bad)
			}
			for _, want := range []string{"-runner-target", bad} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, must name %q", err, want)
				}
			}
		})
	}
}

// knownRunnerTargets is DERIVED from types.DriveBackends rather than restated,
// so this pins the derivation itself: the flag's accepted set must be exactly
// the targets a stored drive can name, with no duplicates and no empty entry
// from an unknown backend.
func TestKnownRunnerTargetsAreTheStorableTargets(t *testing.T) {
	got := knownRunnerTargets()
	want := []string{"docker", "k8s"}
	if len(got) != len(want) {
		t.Fatalf("knownRunnerTargets() = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("knownRunnerTargets() = %v, want %v", got, want)
		}
	}
}
