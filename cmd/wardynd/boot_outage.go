// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"
)

// maxLapseRecoveryGrace is the longest WARDYN_LAPSE_RECOVERY_GRACE boot accepts.
const maxLapseRecoveryGrace = 2 * time.Hour

// maxOrphanSweepHold is the longest WARDYN_ORPHAN_SWEEP_HOLD boot accepts.
const maxOrphanSweepHold = 24 * time.Hour

// outageFlags are the control-plane outage behaviours (docs/ENV.md). A bootFlags built without
// registering them, as a test does, leaves the pointers nil, which reads as off.
type outageFlags struct {
	lapseRecoveryGrace *time.Duration
	lapseMassShare     *int
	lapseMassMin       *int
	orphanSweepHold    *time.Duration
}

func registerOutageFlags() outageFlags {
	return outageFlags{
		lapseRecoveryGrace: flagDuration("lapse-recovery-grace", "WARDYN_LAPSE_RECOVERY_GRACE", 25*time.Minute,
			"after the database is reachable again, lapsed server runs are kept, not ended, and idle stops wait, for this long (duration; 0 = off; at most 2h)"),
		lapseMassShare:  flagIntEnv("lapse-mass-share", "WARDYN_LAPSE_MASS_SHARE", 25, "percent of the server's own runs whose lapse inside 30 minutes marks an outage (1-100)"),
		lapseMassMin:    flagIntEnv("lapse-mass-min", "WARDYN_LAPSE_MASS_MIN", 5, "fewest lapsed server runs that can mark an outage (at least 1)"),
		orphanSweepHold: flagDuration("orphan-sweep-hold", "WARDYN_ORPHAN_SWEEP_HOLD", 0, "after a database restore, keep a server Docker sandbox with no run row for this long, stopped with no network, before the orphan sweep removes it (duration; 0 = off; at most 24h)"),
	}
}

// validate refuses a WARDYN_LAPSE_RECOVERY_GRACE that is negative or above 2h, a WARDYN_LAPSE_MASS_SHARE outside
// 1..100, a WARDYN_LAPSE_MASS_MIN below 1 and a WARDYN_ORPHAN_SWEEP_HOLD that is negative or above 24h. An
// unregistered field is skipped.
func (o outageFlags) validate() error {
	if o.lapseRecoveryGrace != nil {
		if g := *o.lapseRecoveryGrace; g < 0 || g > maxLapseRecoveryGrace {
			return fmt.Errorf("refusing to start: WARDYN_LAPSE_RECOVERY_GRACE is %s; it must be 0 (off) or at most 2h", g)
		}
	}
	if o.lapseMassShare != nil {
		if n := *o.lapseMassShare; n < 1 || n > 100 {
			return fmt.Errorf("refusing to start: WARDYN_LAPSE_MASS_SHARE is %d; want an integer from 1 to 100", n)
		}
	}
	if o.lapseMassMin != nil {
		if n := *o.lapseMassMin; n < 1 {
			return fmt.Errorf("refusing to start: WARDYN_LAPSE_MASS_MIN is %d; want an integer of at least 1", n)
		}
	}
	if o.orphanSweepHold != nil {
		if h := *o.orphanSweepHold; h < 0 || h > maxOrphanSweepHold {
			return fmt.Errorf("refusing to start: WARDYN_ORPHAN_SWEEP_HOLD is %s; it must be 0 (off) or at most 24h", h)
		}
	}
	return nil
}
