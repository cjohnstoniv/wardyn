// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "sync/atomic"

// The deployment's sandbox sizing, set once at boot by cmd/wardynd from
// WARDYN_SANDBOX_DEFAULT_CPU_MILLIS / WARDYN_SANDBOX_DEFAULT_MEMORY_MIB and
// WARDYN_PROXY_CPU_MILLIS / WARDYN_PROXY_MEMORY_MIB. Zero means "unset": the
// compiled-in constants apply, so a daemon with no knob keeps its old size.
var (
	cfgCPUMillis      atomic.Int64
	cfgMemoryMiB      atomic.Int64
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
func SetDefaultLimits(cpuMillis, memoryMiB int64) {
	cfgCPUMillis.Store(cpuMillis)
	cfgMemoryMiB.Store(memoryMiB)
}

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
	r := Resources{CPUMillis: DefaultCPUMillis, MemoryMiB: DefaultMemoryMiB, PidsLimit: DefaultPidsLimit}
	if v := cfgCPUMillis.Load(); v > 0 {
		r.CPUMillis = v
	}
	if v := cfgMemoryMiB.Load(); v > 0 {
		r.MemoryMiB = v
	}
	return r
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
