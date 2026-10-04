// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
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

// AzureProxyMemoryMiB is the wardyn-proxy envelope of a run that carries an Azure route gate. The gate
// holds up to 128 MiB of request bodies in flight (azureInflightBudget), beside the scan buffer
// (maxRetainedScanBytes, 64 MiB) and one extraction at the largest body (5.3 x 32 MiB, about 170 MiB):
// 128 + 64 + 170 = 362 MiB, plus the 34 MiB of headroom the default 256 MiB leaves over its own 222 MiB
// (64 + 158), is 396 MiB, rounded to 400. The deployment-wide default is never raised for it.
const AzureProxyMemoryMiB int64 = 400

// ProxyLimitsFor is ProxyLimits for one run's sidecar: a run with an Azure route gate (azure true) gets at
// least AzureProxyMemoryMiB, a deployment-configured value above that winning; every other run is
// unchanged.
func ProxyLimitsFor(azure bool) (cpuMillis, memoryMiB int64) {
	cpuMillis, memoryMiB = ProxyLimits()
	if azure && memoryMiB < AzureProxyMemoryMiB {
		memoryMiB = AzureProxyMemoryMiB
	}
	return cpuMillis, memoryMiB
}

// ConfigHasAzureGates reports whether a rendered sidecar config (the JSON BuildProxyConfig writes)
// carries an Azure route gate, for a driver that holds only the rendered bytes (a proxy revive).
func ConfigHasAzureGates(cfgJSON []byte) bool {
	var c struct {
		AzureGates []json.RawMessage `json:"azure_gates"`
	}
	return json.Unmarshal(cfgJSON, &c) == nil && len(c.AzureGates) > 0
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

// Sizing is what a driver applies to one run's agent and proxy: the agent's effective CPU
// and memory requests and limits and the proxy envelope. Both drivers and the dispatch
// record read this one result, so the recorded figure cannot drift from the applied one.
type Sizing struct {
	AgentCPURequestMillis int64
	AgentCPULimitMillis   int64
	AgentMemoryRequestMiB int64
	AgentMemoryLimitMiB   int64
	ProxyCPUMillis        int64
	ProxyMemoryMiB        int64
}

// EffectiveResources resolves res into Sizing: the default (EffectiveLimits) fills only zero
// limit fields, EffectiveRequests applies the ratio, and ProxyLimits gives the proxy envelope.
func EffectiveResources(res Resources) Sizing {
	lim := EffectiveLimits()
	s := Sizing{AgentCPULimitMillis: lim.CPUMillis, AgentMemoryLimitMiB: lim.MemoryMiB}
	if res.CPUMillis > 0 {
		s.AgentCPULimitMillis = res.CPUMillis
	}
	if res.MemoryMiB > 0 {
		s.AgentMemoryLimitMiB = res.MemoryMiB
	}
	s.AgentCPURequestMillis, s.AgentMemoryRequestMiB = EffectiveRequests(res)
	s.ProxyCPUMillis, s.ProxyMemoryMiB = ProxyLimits()
	return s
}
