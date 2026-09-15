// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/setup"
)

// Host-proxy sweep memo. setup.DetectHostProxy's OS tier shells out to the
// platform's proxy configuration (registry/scutil/gsettings) and measured ~450ms
// per call on a WSL host — 90%+ of handleSetupStatus's cost, on an endpoint the
// console polls every 5s, so a single open Getting-started tab spent most of a
// core on re-reading a host setting that changes about never.
//
// The sweep runs in the BACKGROUND and a caller never waits for it. The memo
// alone was not enough: DetectHostProxy's WSL tier runs powershell.exe and then
// netsh.exe, each bounded by its own 3s probeTimeout, so on a host whose interop
// is wedged the FIRST caller after every daemon boot paid SIX seconds — and the
// console's first paint is that caller. It cost the 0.7.3 e2e suite 17 spec
// files, whose opening page-render assertion timed out at 5s with nothing on
// screen. A readiness snapshot must never block on a subprocess; a sweep that
// has not answered yet reports what the memo holds and refreshes behind the
// request.
//
// Memoized here rather than on Server (the way githubRefRulesetCheck's cache is)
// on purpose: the answer is a property of the HOST, not of any one Server, so
// two Servers in one process would only duplicate the sweep. hostProxyDetect is
// the seam the memo test swaps; hostProxyCacheReset drops the memo (tests, and
// the Re-check path if one is ever wired to force a re-detect).
const hostProxyTTL = 30 * time.Second

var (
	hostProxyMu sync.Mutex
	hostProxyAt time.Time
	// hostProxyVal is the last sweep that ANSWERED — the zero value until the
	// first one lands (a window of one sweep after boot, in which the host_proxy
	// check reads "none detected"). ponytail: a compose/`make setup` install —
	// the corporate-network case this detection exists for — is seeded instead
	// (setup.HostProxySeeded), so it never sees that window; give the check its
	// own "still looking" state only if a real deployment is ever caught by it.
	hostProxyVal setup.HostProxyDetection
	// hostProxySweeping: at most one sweep in flight, so a burst of polls (the
	// console opens with two concurrent /setup/status calls) starts one.
	hostProxySweeping bool
	// hostProxyGen is bumped by hostProxyCacheReset so an in-flight sweep from
	// before the reset — a test's real DetectHostProxy, swapped out mid-flight —
	// cannot land on top of the fresh memo afterwards.
	hostProxyGen    int
	hostProxyDetect = setup.DetectHostProxy
)

// cachedHostProxy returns the last-known host-proxy sweep and NEVER runs one on
// the caller's goroutine — a stale or not-yet-taken memo starts a background
// refresh and returns immediately.
func cachedHostProxy() setup.HostProxyDetection {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	if hostProxyAt.IsZero() || time.Since(hostProxyAt) >= hostProxyTTL {
		startHostProxySweepLocked()
	}
	return hostProxyVal
}

// startHostProxySweepLocked starts the one background sweep. The caller holds
// hostProxyMu; the sweep itself runs outside it, so nothing on a request path
// can ever block behind the subprocesses DetectHostProxy spawns.
func startHostProxySweepLocked() {
	if hostProxySweeping {
		return
	}
	hostProxySweeping = true
	detect, gen := hostProxyDetect, hostProxyGen
	go func() {
		val := detect()
		hostProxyMu.Lock()
		defer hostProxyMu.Unlock()
		hostProxySweeping = false
		if gen != hostProxyGen {
			return
		}
		hostProxyVal, hostProxyAt = val, time.Now()
	}()
}

// hostProxyCacheReset forgets the memo so the next caller re-detects.
func hostProxyCacheReset() {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	hostProxyGen++
	hostProxyAt = time.Time{}
	hostProxyVal = setup.HostProxyDetection{}
}
