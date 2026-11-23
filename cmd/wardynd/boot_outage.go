// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"
)

// maxLapseRecoveryGrace is the longest WARDYN_LAPSE_RECOVERY_GRACE boot accepts.
const maxLapseRecoveryGrace = 2 * time.Hour

// outageFlags are the control-plane outage behaviours (docs/ENV.md). A bootFlags built without
// registering them, as a test does, leaves the pointers nil, which reads as off.
type outageFlags struct {
	lapseRecoveryGrace *time.Duration
}

func registerOutageFlags() outageFlags {
	return outageFlags{
		lapseRecoveryGrace: flagDuration("lapse-recovery-grace", "WARDYN_LAPSE_RECOVERY_GRACE", 25*time.Minute,
			"after the database is reachable again, lapsed server runs are kept, not ended, and idle stops wait, for this long (duration; 0 = off; at most 2h)"),
	}
}

// validate refuses a WARDYN_LAPSE_RECOVERY_GRACE that is negative or above 2h. An unregistered field is skipped.
func (o outageFlags) validate() error {
	if o.lapseRecoveryGrace != nil {
		if g := *o.lapseRecoveryGrace; g < 0 || g > maxLapseRecoveryGrace {
			return fmt.Errorf("refusing to start: WARDYN_LAPSE_RECOVERY_GRACE is %s; it must be 0 (off) or at most 2h", g)
		}
	}
	return nil
}
