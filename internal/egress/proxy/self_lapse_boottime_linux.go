// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"math"
	"time"

	"golang.org/x/sys/unix"
)

// sysBootClock reads CLOCK_BOOTTIME. A failed read answers the far future,
// which lapses the gate: the fail-closed direction.
type sysBootClock struct{}

func (sysBootClock) Now() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return math.MaxInt64
	}
	return time.Duration(ts.Nano())
}
