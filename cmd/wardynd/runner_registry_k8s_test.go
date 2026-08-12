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
