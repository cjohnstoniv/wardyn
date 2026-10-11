// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import "time"

const (
	// RunTokenTTL bounds the embedded identity and sidecar renewal give-up window.
	RunTokenTTL = time.Hour
	// RunTokenLapseAfter includes the control-plane stamp/skew allowance. Both
	// the org sweeper and offline runner contain egress after this same window.
	RunTokenLapseAfter = RunTokenTTL + 5*time.Minute
)
