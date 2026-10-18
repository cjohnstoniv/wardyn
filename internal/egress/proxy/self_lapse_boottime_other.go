// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package proxy

import (
	"math"
	"time"
)

// sysBootClock has no CLOCK_BOOTTIME off Linux, where no k8s runner proxy
// runs: it answers the far future, so a self-lapsing proxy lapses at once.
type sysBootClock struct{}

func (sysBootClock) Now() time.Duration { return math.MaxInt64 }
