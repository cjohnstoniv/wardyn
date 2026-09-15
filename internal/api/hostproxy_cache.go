// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
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
// netsh.exe, and probeTimeout bounds each CHILD but not the call — exec's
// Output() waits for EOF on the stdout pipe, which a grandchild the kill did not
// reach can hold open indefinitely (measured: 30s against a 3s context). So on a
// host whose interop is wedged the FIRST caller after every daemon boot paid six
// seconds at best and unboundedly at worst — and the console's first paint is
// that caller. It cost the 0.7.3 e2e suite 17 spec files, whose opening
// page-render assertion timed out at 5s with nothing on screen.
//
// A readiness snapshot must never block on a subprocess, so a sweep that has not
// answered yet reports what the memo holds and refreshes behind the request. The
// three things that keeps honest:
//
//   - SEEDED INSTALLS NEVER SEE THE WINDOW. A compose/`make setup` install —
//     the corporate-network case this detection exists for — carries its answer
//     in WARDYN_HOST_PROXY_B64, which DetectHostProxy decodes in-process with no
//     exec at all, so cachedHostProxy resolves it SYNCHRONOUSLY on the first
//     call rather than reporting an empty detection the operator would read as
//     the confident "nothing is there" (hostProxyCheck's blind flag is false on
//     a seeded install — the honesty rule at setup_checks.go's own doc comment).
//   - A HANGING SWEEP IS BOUNDED, LOGGED AND RETRIED. hostProxySweepDeadline
//     abandons it, releases the in-flight flag so the next poll tries again, and
//     says so once in the journal — rather than leaving a permanently blind
//     daemon reading as "none detected" forever with nothing to see.
//   - RE-CHECK FORCES A RE-DETECT. GET /setup/status?recheck=1 (operator-only)
//     drops the memo first, so the console's Re-check button is a real re-read of
//     the host and not a refetch of the same 30s-old answer.
//
// Memoized here rather than on Server (the way githubRefRulesetCheck's cache is)
// on purpose: the answer is a property of the HOST, not of any one Server, so
// two Servers in one process would only duplicate the sweep. hostProxyDetect is
// the seam the memo tests swap.
const hostProxyTTL = 30 * time.Second

// hostProxySweepDeadline is how long a sweep may run before it is abandoned. The
// honest worst case is DetectHostProxy's two interop probes at probeTimeout each
// (3s + 3s, now that probeWaitDelay actually bounds them) plus the cheap
// env/shell/git/file tiers, so 10s is comfortably above "slow but working" and
// far below "nobody is coming". Past it the answer would be too stale to be
// worth the wait anyway: the next poll is 5s away.
//
// A var, not a const, only so the abandonment tests need not sleep ten seconds
// to watch it work — and read/written ONLY under hostProxyMu (see
// setHostProxySweepDeadline), because a test that swapped it while a previous
// test's sweep goroutine was still waiting on it is a real data race.
var hostProxySweepDeadline = 10 * time.Second

// setHostProxySweepDeadline swaps the deadline under the memo's own lock and
// returns the previous value. Test-only seam; the production value never moves.
func setHostProxySweepDeadline(d time.Duration) time.Duration {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	prev := hostProxySweepDeadline
	hostProxySweepDeadline = d
	return prev
}

var (
	hostProxyMu sync.Mutex
	hostProxyAt time.Time
	// hostProxyVal is the last sweep that ANSWERED — the zero value until the
	// first one lands. See the SEEDED INSTALLS bullet above for why the install
	// this detection exists for never reads that zero.
	hostProxyVal setup.HostProxyDetection
	// hostProxySweeping: at most one sweep in flight, so a burst of polls (the
	// console opens with two concurrent /setup/status calls) starts one.
	hostProxySweeping bool
	// hostProxySeq names the CURRENT sweep. hostProxyCacheReset bumps it, and so
	// does an abandonment (deadline or contained panic) — so a sweep still
	// running after either can neither land its answer on a memo that has moved
	// on, nor clear a NEWER sweep's in-flight flag.
	hostProxySeq uint64
	// hostProxySweepWarned keeps the abandonment warning to ONE line per process:
	// a wedged host abandons every poll, and a journal full of the same sentence
	// is how the sentence stops being read.
	hostProxySweepWarned bool
	hostProxyDetect      = setup.DetectHostProxy
)

// cachedHostProxy returns the last-known host-proxy sweep and NEVER runs a
// SUBPROCESS on the caller's goroutine — a stale or not-yet-taken memo starts a
// background refresh and returns immediately.
//
// The one exception is the seeded install, which is not a sweep at all: the
// answer is already in this process's environment and decoding it is microseconds
// with no exec, so the first caller resolves it in line rather than being told
// "none detected" for a window.
func cachedHostProxy() setup.HostProxyDetection {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	if !hostProxyAt.IsZero() && time.Since(hostProxyAt) < hostProxyTTL {
		return hostProxyVal
	}
	if hostProxyAt.IsZero() && setup.HostProxySeeded() {
		hostProxyVal, hostProxyAt = hostProxyDetect(), time.Now()
		return hostProxyVal
	}
	startHostProxySweepLocked()
	return hostProxyVal
}

// startHostProxySweepLocked starts the one background sweep. The caller holds
// hostProxyMu; the sweep itself runs outside it, so nothing on a request path
// can ever block behind the subprocesses DetectHostProxy spawns.
//
// TWO goroutines, not one, and both are load-bearing: the detector may never
// return (see the package comment's Output()-pipe note), so the waiter that owns
// the deadline cannot be the same goroutine that calls it.
func startHostProxySweepLocked() {
	if hostProxySweeping {
		return
	}
	hostProxySweeping = true
	hostProxySeq++
	detect, seq, deadline := hostProxyDetect, hostProxySeq, hostProxySweepDeadline

	done := make(chan setup.HostProxyDetection, 1) // buffered: an abandoned sweep must not block on the send
	go func() {
		// Contain a panic in the detached sweep so it cannot crash the daemon —
		// the same law sshGo and the run watcher state, and this one parses
		// whatever a host .exe printed (JSON/regexp over foreign output), which is
		// exactly the shape those recovers exist for. Pre-fix the same panic ran
		// on the request goroutine, where net/http recovers it.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("wardynd: PANIC in host-proxy sweep (contained)", slog.Any("panic", r))
				// Release the flag, or a panic strands it forever and the memo is
				// blind for the life of the process.
				abandonHostProxySweep(seq)
			}
		}()
		done <- detect()
	}()

	go func() {
		timer := time.NewTimer(deadline)
		defer timer.Stop()
		select {
		case val := <-done:
			hostProxyMu.Lock()
			defer hostProxyMu.Unlock()
			if seq != hostProxySeq {
				return // reset or abandoned meanwhile: this answer is not ours to store
			}
			hostProxySweeping = false
			hostProxyVal, hostProxyAt = val, time.Now()
		case <-timer.C:
			if abandonHostProxySweep(seq) {
				slog.Warn("wardynd: host-proxy sweep exceeded its deadline; reporting last-known host proxy",
					slog.Duration("deadline", deadline))
			}
		}
	}()
}

// abandonHostProxySweep retires the sweep named by seq: the in-flight flag is
// released so the NEXT poll retries, and the sequence moves on so the abandoned
// goroutine — which may still be inside a wedged powershell.exe — can never land
// its answer or clear a successor's flag.
//
// It reports whether this call is the one that should be spoken about, which is
// at most once per process (see hostProxySweepWarned).
func abandonHostProxySweep(seq uint64) bool {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	if seq != hostProxySeq {
		return false
	}
	hostProxySeq++
	hostProxySweeping = false
	if hostProxySweepWarned {
		return false
	}
	hostProxySweepWarned = true
	return true
}

// hostProxyForceRedetect is the Re-check door (handleSetupStatus's recheck
// param): it invalidates the memo's FRESHNESS without forgetting its answer, so
// the next read re-detects — starting a sweep, or, on a seeded install,
// resolving in line — while still reporting the last-known value.
//
// Keeping the value is the difference between a re-check and a downgrade: a full
// forget would answer the very press that asked with an empty detection, and the
// operator would have to press again to see what they already had on screen.
//
// SINGLE-FLIGHT IS DELIBERATELY YIELDED HERE, so do not "fix" it back: clearing
// hostProxySweeping without stopping the sweep it displaces means N presses
// inside one deadline start N overlapping sweeps. That is the point — the case
// Re-check exists for is a sweep that is WEDGED, and keeping the flag set would
// make the button a no-op exactly then. The cost is bounded and operator-only:
// isOperator gates the door, the button is disabled while a check is in flight,
// each probe is capped at probeTimeout + probeWaitDelay and each waiter at
// hostProxySweepDeadline.
func hostProxyForceRedetect() {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	hostProxySeq++
	hostProxySweeping = false
	hostProxyAt = time.Time{}
}

// hostProxyCacheReset forgets the memo ENTIRELY — value included — so the next
// caller re-detects from nothing. The test door; the Re-check door above
// deliberately keeps the last-known answer.
//
// It clears hostProxySweeping, which is the whole point: without that, a reset
// taken while a sweep is in flight left startHostProxySweepLocked early-returning
// on a stale flag and "the next caller re-detects" was simply false.
func hostProxyCacheReset() {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	hostProxySeq++
	hostProxySweeping = false
	hostProxySweepWarned = false
	hostProxyAt = time.Time{}
	hostProxyVal = setup.HostProxyDetection{}
}
