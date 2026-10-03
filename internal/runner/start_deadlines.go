// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

// CapacityBlockerReasons are the scheduler reasons that mean "no machine has room yet": a pod stuck
// on one is worth waiting for, because room frees as other runs finish. Tagless, like
// TerminalWaitingReasons, because the control plane reads it too.
var CapacityBlockerReasons = map[string]bool{"Unschedulable": true}

const (
	// DefaultSandboxStartTimeout is how long a sandbox has to start (WARDYN_SANDBOX_START_TIMEOUT).
	DefaultSandboxStartTimeout = 3 * time.Minute
	// DefaultSandboxCapacityWait is how long an unplaceable sandbox waits for room, on top of
	// the start timeout's own clock (WARDYN_SANDBOX_CAPACITY_WAIT). 0 turns the wait off.
	DefaultSandboxCapacityWait = 15 * time.Minute
)

// SandboxStartDeadlines reads the two start deadlines from the environment. A malformed value exits 2
// at boot (cliutil.EnvDuration); a non-positive start timeout falls back to the default and a negative
// capacity wait to 0.
func SandboxStartDeadlines() (start, capacity time.Duration) {
	start = cliutil.EnvDuration("WARDYN_SANDBOX_START_TIMEOUT", DefaultSandboxStartTimeout)
	if start <= 0 {
		start = DefaultSandboxStartTimeout
	}
	capacity = max(cliutil.EnvDuration("WARDYN_SANDBOX_CAPACITY_WAIT", DefaultSandboxCapacityWait), 0)
	return start, capacity
}
