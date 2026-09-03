// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// Under -tags k8s the blank import in runner_k8s.go pulls in
// internal/runner/k8s/register.go, whose init() self-registers "k8s".
//
// COUNTERFACTUAL: delete internal/runner/k8s/register.go (or runner_k8s.go's
// blank import) and this fails with `component "k8s" is not registered` —
// exactly what the tagless build gets, proving registration (not a
// hardcoded switch) is what makes -runner k8s work. Mirrors
// runner_registry_docker_test.go's TestSubstrateRegistry_* shape.
//
// Deliberately does NOT call substrate.New("k8s", ...) with a real
// ProxyImage the way the docker counterpart does: the docker driver's
// constructor is a lazy client (no daemon round-trip at New time), but the
// k8s driver's constructor is eager — it loads a rest.Config and runs the
// boot-time egress canary against a real cluster before returning. This
// test suite owns no cluster (see the k8s lane's brief: live conformance is
// a separate lane), and a host's ambient kubeconfig (if any) must never be
// dialed by a unit test. Passing an EMPTY ProxyImage makes newWithClient
// return errProxyImageUnset BEFORE any cluster I/O (see internal/runner/k8s
// driver.go), which is enough to prove the registry reaches the REAL k8s
// constructor rather than short-circuiting on a registry miss.
func TestSubstrateRegistry_K8sResolvesUnderK8sTag(t *testing.T) {
	if names := substrate.Names(); !slices.Contains(names, "k8s") {
		t.Fatalf("substrate registry: want \"k8s\" registered under -tags k8s, have %v", names)
	}
	_, err := substrate.New("k8s", substrate.Deps{})
	if err == nil {
		t.Fatal("substrate.New(k8s) with no ProxyImage: want an error, got nil (a live-cluster side effect this test must never risk)")
	}
	if strings.Contains(err.Error(), "not registered") {
		t.Fatalf("substrate.New(k8s) returned a registry-miss error — \"k8s\" is not actually wired: %v", err)
	}
}

// TestBuildRunnerFromFlags_K8sConstructFailureNotMislabeled is the W27-S1-3
// regression: a REGISTERED substrate (k8s) that fails to CONSTRUCT — the
// canary's flagship refuse-to-construct chief among such failures — must not
// be printed under the "unknown -runner ... requires -tags docker" headline
// meant for a typo'd or not-compiled-in -runner; that headline sent every k8s
// boot refusal down the wrong troubleshooting path. Empty ProxyImage forces
// newWithClient's errProxyImageUnset before any cluster I/O (the same
// zero-live-cluster-risk trick the test above uses directly on substrate.New),
// a real construction failure this unit test can trigger safely.
func TestBuildRunnerFromFlags_K8sConstructFailureNotMislabeled(t *testing.T) {
	sel, cmap, img := "k8s", "", ""
	f := &bootFlags{runnerSel: &sel, confinementMap: &cmap, proxyImage: &img}

	_, _, err := buildRunnerFromFlags(f, nil, nil)
	if err == nil {
		t.Fatal("buildRunnerFromFlags(k8s) with no ProxyImage: want an error, got nil")
	}
	if strings.Contains(err.Error(), "-tags docker") || strings.Contains(err.Error(), "unknown -runner") {
		t.Fatalf("k8s construct failure printed under the not-compiled-in/unknown-runner headline: %v", err)
	}
	if !strings.Contains(err.Error(), `-runner "k8s" failed to start`) {
		t.Fatalf("error = %q, want it discriminated as a construct failure (registered, but failed to start), not a registry miss", err.Error())
	}
}
