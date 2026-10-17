// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The proxy envelope grows only for a run whose rendered config carries an Azure route gate, which is
// what startProxy reads on create and on a proxy revive alike.
func TestProxyResources_AzureGateRaisesOnlyItsOwnRun(t *testing.T) {
	t.Cleanup(func() { runner.SetProxyLimits(0, 0) })
	runner.SetProxyLimits(0, 0)
	if got := proxyResources(false).Memory; got != runner.DefaultProxyMemoryMiB*1024*1024 {
		t.Errorf("no gate: memory = %d, want the default", got)
	}
	cfg := []byte(`{"azure_gates":[{"host":"h.test","route":"anthropic","models":["m"]}]}`)
	want := runner.AzureProxyMemoryMiB * 1024 * 1024
	if got := proxyResources(runner.ConfigHasAzureGates(cfg)).Memory; got != want {
		t.Errorf("gate: memory = %d, want %d", got, want)
	}
	if r := proxyResources(true); r.MemorySwap != r.Memory {
		t.Errorf("gate: MemorySwap = %d, want == Memory %d", r.MemorySwap, r.Memory)
	}
}
