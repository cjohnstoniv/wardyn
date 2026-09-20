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
// that caller.
//
// A readiness snapshot must never block on a subprocess, so a sweep that has not
// answered yet reports what the memo holds and refreshes behind the request. The
// three things that keeps honest:
//
//   - Seeded installs never see the window. A compose/`make setup` install —
//     the corporate-network case this detection exists for — carries its answer
//     in WARDYN_HOST_PROXY_B64, which DetectHostProxy decodes in-process with no
//     exec at all, so cachedHostProxy resolves it SYNCHRONOUSLY on the first
//     call rather than reporting an empty detection the operator would read as
//     the confident "nothing is there" (hostProxyCheck's blind flag is false on
//     a seeded install — the honesty rule at setup_checks.go's own doc comment).
//   - A hanging sweep is bounded, logged and retried. hostProxySweepDeadline
//     abandons it, releases the in-flight flag so the next poll tries again, and
//     says so once in the journal — rather than leaving a permanently blind
//     daemon reading as "none detected" forever with nothing to see.
//   - Re-check forces a re-detect. GET /setup/status?recheck=1 (operator-only)
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

// hostProxyRecheckWait is how long the FORCED Re-check door waits for the sweep
// it started before answering with last-known. Only the forced path ever waits:
// the unforced poll must never block on a subprocess (that is the whole point of
// the memo), but a button whose one job is "look at the host again" and which
// answers from the memo it just invalidated is always one press behind — and
// stamps "checked just now" over the old value. Two seconds covers the
// env/shell/git/file tiers and a healthy OS tier; a wedged interop blows through
// it and gets the same last-known answer a poll gets.
const hostProxyRecheckWait = 2 * time.Second

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
	// hostProxySettled is closed when the CURRENT sweep stops being in flight —
	// answered, abandoned or panicked. Only the forced Re-check door waits on it
	// (bounded), so a sweep nobody is waiting for costs nothing.
	hostProxySettled chan struct{}
	// hostProxyForcedAt is when Re-check last FORCED a re-detect. One forced
	// re-detect per hostProxySweepDeadline is the aggregate bound single-flight
	// deliberately yields: the wedged sweep Re-check exists for is
	// unstuck by one, and N presses inside one deadline otherwise started N
	// overlapping sweeps with up to two host subprocesses each.
	hostProxyForcedAt time.Time
	hostProxyDetect   = setup.DetectHostProxy
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
//
// "Nothing on a request path blocks behind it" holds for every POLL; the one
// exception is the operator-only forced Re-check, which waits a bounded
// hostProxyRecheckWait for the sweep it started (hostProxyRecheck).
func startHostProxySweepLocked() {
	if hostProxySweeping {
		return
	}
	hostProxySweeping = true
	hostProxySeq++
	detect, seq, deadline := hostProxyDetect, hostProxySeq, hostProxySweepDeadline

	settled := make(chan struct{})
	hostProxySettled = settled

	done := make(chan setup.HostProxyDetection, 1) // buffered: an abandoned sweep must not block on the send
	go func() {
		// Contain a panic in the detached sweep so it cannot crash the daemon —
		// the same law sshGo and the run watcher state, and this one parses
		// whatever a host .exe printed (JSON/regexp over foreign output), which is
		// exactly the shape those recovers exist for.
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
		// Closed on EVERY exit — stored, deadline, or a detector that panicked
		// and left this waiter to time out — so a forced Re-check waiting on it
		// is released by the sweep ending, whichever way it ends.
		defer close(settled)
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

// hostProxyRecheck is the whole Re-check door (handleSetupStatus's recheck
// param): force a re-detect (at most one per hostProxySweepDeadline),
// start the sweep, and wait a BOUNDED moment for it so the answer this press
// returns is the one it asked for. A press inside the bound starts
// nothing, but still waits on the sweep already in flight — which is what the
// operator is waiting for anyway.
//
// It invalidates the memo's FRESHNESS without forgetting its answer, and that
// is the difference between a re-check and a downgrade: a full forget would
// answer the very press that asked with an empty detection, and the operator
// would have to press again to see what they already had on screen. (The
// aggregate bound REPLACED the old unbounded force, which yielded single-flight
// outright so that N presses inside one deadline started N overlapping sweeps —
// the wedged sweep that motivated it is still unstuck, once per deadline.)
func hostProxyRecheck() setup.HostProxyDetection {
	hostProxyMu.Lock()
	if hostProxyForcedAt.IsZero() || time.Since(hostProxyForcedAt) >= hostProxySweepDeadline {
		hostProxyForcedAt = time.Now()
		hostProxySeq++
		hostProxySweeping = false
		hostProxyAt = time.Time{}
	}
	hostProxyMu.Unlock()

	val := cachedHostProxy() // starts the sweep, or resolves a seeded install in line
	hostProxyMu.Lock()
	settled, sweeping := hostProxySettled, hostProxySweeping
	hostProxyMu.Unlock()
	if !sweeping || settled == nil {
		return val
	}
	timer := time.NewTimer(hostProxyRecheckWait)
	defer timer.Stop()
	select {
	case <-settled:
	case <-timer.C: // a wedged host answers with last-known, exactly like a poll
	}
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	return hostProxyVal
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
	hostProxyForcedAt = time.Time{}
	hostProxySettled = nil
	hostProxyVal = setup.HostProxyDetection{}
}
