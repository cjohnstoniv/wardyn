// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// ProxyFence is advertised for a substrate that can cut an agent off its networks, never for the process: a
// substrate with the Kubernetes driver's method set, which has no FenceProxy, does not send it.
func TestHostCaps_ProxyFenceFollowsTheSubstrate(t *testing.T) {
	ctx := context.Background()
	fencing := runnertest.NewFake()
	caps, err := hostCaps(ctx, fencing, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !caps.ProxyFence {
		t.Error("a substrate that implements runner.ProxyFencer: ProxyFence = false; want true")
	}
	// Embedding the interface leaves only its methods, as the Kubernetes driver's method set does.
	caps, err = hostCaps(ctx, struct{ substrate.Substrate }{fencing}, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if caps.ProxyFence {
		t.Error("a substrate without FenceProxy advertised ProxyFence; want it off")
	}
}
