// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"math"
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
	cfgRequestRatio   atomic.Uint64 // math.Float64bits; 0 = unset (requests = limits)
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

// SetRequestRatio records the deployment's agent-pod request ratio, set from
// WARDYN_SANDBOX_REQUEST_RATIO. 0 means unset (requests equal limits); otherwise the
// ratio must be in (0, 1]. It is deployment-wide on purpose: a per-run request would let
// a member under-request and push contention onto co-tenants.
func SetRequestRatio(ratio float64) error {
	if ratio != 0 && !(ratio > 0 && ratio <= 1) { // also refuses NaN
		return fmt.Errorf("sandbox request ratio %v is outside (0, 1]", ratio)
	}
	cfgRequestRatio.Store(math.Float64bits(ratio))
	return nil
}

// EffectiveRequests is the one place the request ratio is applied: the CPU (milli) and
// memory (MiB) requests an agent pod carries. An explicit Resources request field wins;
// else the ratio times the effective limit, rounded down and at least 1; with no ratio the
// request equals the limit. Never above the limit. The proxy envelope does not use it
// (it stays Guaranteed).
func EffectiveRequests(res Resources) (cpuMillis, memoryMiB int64) {
	lim := EffectiveLimits()
	cpuLimit, memLimit := lim.CPUMillis, lim.MemoryMiB
	if res.CPUMillis > 0 {
		cpuLimit = res.CPUMillis
	}
	if res.MemoryMiB > 0 {
		memLimit = res.MemoryMiB
	}
	ratio := math.Float64frombits(cfgRequestRatio.Load())
	scale := func(limit, explicit int64) int64 {
		switch {
		case explicit > 0:
			return min(explicit, limit)
		case ratio > 0:
			return min(limit, max(1, int64(float64(limit)*ratio)))
		}
		return limit
	}
	return scale(cpuLimit, res.CPURequestMillis), scale(memLimit, res.MemoryRequestMiB)
}
