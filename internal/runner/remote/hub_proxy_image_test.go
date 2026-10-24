// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A claimed runner's AUTH_OK carries the proxy image the organisation chose for that runner, and says
// nothing when no chooser is set.
func TestHubAuthOKCarriesTheProxyImage(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		for _, chooser := range []func(types.Runner) string{nil, func(rn types.Runner) string { return "org/proxy-" + rn.Arch }} {
			f := newInventoryFixture(t, realWire)
			f.row.Arch = "arm64"
			f.hub.cfg.ProxyImage = chooser
			want := ""
			if chooser != nil {
				want = "org/proxy-arm64"
			}
			if _, ok, _ := f.open(t, nil); ok.ProxyImage != want {
				t.Fatalf("AUTH_OK named %q, want %q", ok.ProxyImage, want)
			}
		}
	})
}

// A claimed runner's AUTH_OK carries the per-run CPU and memory its declared limits name, and nothing
// when it has declared none (the runner then sizes as before).
func TestHubAuthOKCarriesThePerRunLimits(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		for _, limits := range []*types.RunnerLimits{nil,
			{RunsAtOnce: 2, PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192, DiskMiB: 25 << 10}}} {
			f := newInventoryFixture(t, realWire)
			f.row.Limits = limits
			var cpu, mem int64
			if limits != nil {
				cpu, mem = 4000, 8192
			}
			if _, ok, _ := f.open(t, nil); ok.PerRunCPUMillis != cpu || ok.PerRunMemoryMiB != mem {
				t.Fatalf("AUTH_OK named %d milli-CPU and %d MiB, want %d and %d", ok.PerRunCPUMillis, ok.PerRunMemoryMiB, cpu, mem)
			}
		}
	})
}
