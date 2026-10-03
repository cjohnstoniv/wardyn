// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sizing owns the deployment's default sandbox size and nothing else.
//
// It is a leaf (standard library only) so that internal/composer can read the
// effective default without importing internal/runner, which imports the whole
// egress proxy. internal/runner re-exports every name here, so a caller that
// already speaks runner.EffectiveLimits keeps doing so.
package sizing

import "sync/atomic"

// Conservative platform resource defaults, applied when a Resources field is
// zero, so EVERY agent sandbox is capped even when policy sets nothing:
// without them one runaway or prompt-injected agent can OOM-kill the host,
// fork-bomb the host PID space, or fill host storage and take sibling runs
// down with it (see types.ResourceLimits). A policy value always overrides.
const (
	DefaultCPUMillis int64 = 2000 // 2 vCPU
	DefaultMemoryMiB int64 = 4096 // 4 GiB hard memory cap
	DefaultPidsLimit int64 = 512  // max processes/threads (fork-bomb guard)
)

// The deployment's sandbox sizing, set once at boot by cmd/wardynd from
// WARDYN_SANDBOX_DEFAULT_CPU_MILLIS / WARDYN_SANDBOX_DEFAULT_MEMORY_MIB. Zero
// means "unset": the compiled-in constants apply, so a daemon with no knob
// keeps its old size.
var (
	cfgCPUMillis atomic.Int64
	cfgMemoryMiB atomic.Int64
)

// Limits is the effective default size of one sandbox.
type Limits struct {
	CPUMillis int64
	MemoryMiB int64
	PidsLimit int64
}

// SetDefaultLimits records the deployment's default sandbox size; a value <= 0
// leaves that field at its compiled-in default.
func SetDefaultLimits(cpuMillis, memoryMiB int64) {
	cfgCPUMillis.Store(cpuMillis)
	cfgMemoryMiB.Store(memoryMiB)
}

// EffectiveLimits is the default sandbox size every fallback site uses when a
// Resources field is zero: the deployment's configured value, else
// DefaultCPUMillis/DefaultMemoryMiB. PidsLimit is always the compiled-in default.
func EffectiveLimits() Limits {
	r := Limits{CPUMillis: DefaultCPUMillis, MemoryMiB: DefaultMemoryMiB, PidsLimit: DefaultPidsLimit}
	if v := cfgCPUMillis.Load(); v > 0 {
		r.CPUMillis = v
	}
	if v := cfgMemoryMiB.Load(); v > 0 {
		r.MemoryMiB = v
	}
	return r
}
