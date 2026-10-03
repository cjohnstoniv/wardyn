// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"sync/atomic"

	"github.com/cjohnstoniv/wardyn/internal/runner/sizing"
)

// The sandbox default size lives in internal/runner/sizing (a leaf, so the
// composer can read it without importing this package); the proxy sidecar's
// envelope below stays here. Both are set once at boot by cmd/wardynd from
// WARDYN_SANDBOX_DEFAULT_CPU_MILLIS / WARDYN_SANDBOX_DEFAULT_MEMORY_MIB and
// WARDYN_PROXY_CPU_MILLIS / WARDYN_PROXY_MEMORY_MIB. Zero means "unset": the
// compiled-in constants apply, so a daemon with no knob keeps its old size.
var (
	cfgProxyCPUMillis atomic.Int64
	cfgProxyMemoryMiB atomic.Int64
)

// Compiled-in wardyn-proxy sidecar envelope, used when no knob is set.
const (
	DefaultProxyCPUMillis int64 = 500
	DefaultProxyMemoryMiB int64 = 256
)

// SetDefaultLimits records the deployment's default sandbox size; a value <= 0
// leaves that field at its compiled-in default.
func SetDefaultLimits(cpuMillis, memoryMiB int64) { sizing.SetDefaultLimits(cpuMillis, memoryMiB) }

// SetProxyLimits records the deployment's wardyn-proxy sidecar envelope; a value
// <= 0 leaves that field at its compiled-in default.
func SetProxyLimits(cpuMillis, memoryMiB int64) {
	cfgProxyCPUMillis.Store(cpuMillis)
	cfgProxyMemoryMiB.Store(memoryMiB)
}

// EffectiveLimits is the default sandbox size every fallback site uses when a
// Resources field is zero: the deployment's configured value, else
// DefaultCPUMillis/DefaultMemoryMiB. PidsLimit is always the compiled-in default.
func EffectiveLimits() Resources {
	l := sizing.EffectiveLimits()
	return Resources{CPUMillis: l.CPUMillis, MemoryMiB: l.MemoryMiB, PidsLimit: l.PidsLimit}
}

// ProxyLimits is the wardyn-proxy sidecar's cgroup envelope on every substrate:
// the deployment's configured value, else DefaultProxyCPUMillis/DefaultProxyMemoryMiB.
func ProxyLimits() (cpuMillis, memoryMiB int64) {
	cpuMillis, memoryMiB = DefaultProxyCPUMillis, DefaultProxyMemoryMiB
	if v := cfgProxyCPUMillis.Load(); v > 0 {
		cpuMillis = v
	}
	if v := cfgProxyMemoryMiB.Load(); v > 0 {
		memoryMiB = v
	}
	return cpuMillis, memoryMiB
}
