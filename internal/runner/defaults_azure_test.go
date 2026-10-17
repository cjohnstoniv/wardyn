// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "testing"

// A run with an Azure route gate gets the larger proxy envelope; every other run keeps the deployment's
// value, and a deployment value above the Azure floor wins for an Azure run.
func TestProxyLimitsFor_AzureRaisesOnlyItsOwnRun(t *testing.T) {
	t.Cleanup(func() { SetProxyLimits(0, 0) })
	SetProxyLimits(0, 0)
	if _, mem := ProxyLimitsFor(false); mem != DefaultProxyMemoryMiB {
		t.Errorf("a run without a gate gets %d MiB, want the default %d", mem, DefaultProxyMemoryMiB)
	}
	if _, mem := ProxyLimitsFor(true); mem != AzureProxyMemoryMiB {
		t.Errorf("a run with a gate gets %d MiB, want %d", mem, AzureProxyMemoryMiB)
	}
	if _, mem := ProxyLimits(); mem != DefaultProxyMemoryMiB {
		t.Errorf("the deployment-wide default moved to %d MiB", mem)
	}
	SetProxyLimits(0, 1024)
	if _, mem := ProxyLimitsFor(true); mem != 1024 {
		t.Errorf("a deployment value above the floor: %d MiB, want 1024", mem)
	}
	SetProxyLimits(0, 512)
	if _, mem := ProxyLimitsFor(false); mem != 512 {
		t.Errorf("a run without a gate gets %d MiB, want the configured 512", mem)
	}
}

func TestConfigHasAzureGates(t *testing.T) {
	for cfg, want := range map[string]bool{
		`{"run_id":"x","azure_gates":[{"host":"h","route":"anthropic","models":["m"]}]}`: true,
		`{"run_id":"x","azure_gates":[]}`:                                                false,
		`{"run_id":"x"}`:                                                                 false,
		`not json`:                                                                       false,
	} {
		if got := ConfigHasAzureGates([]byte(cfg)); got != want {
			t.Errorf("ConfigHasAzureGates(%s) = %v, want %v", cfg, got, want)
		}
	}
}
