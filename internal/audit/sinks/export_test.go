// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"context"
	"time"
)

// PushOTLPMetrics sends one export now, as one tick of Run does, so a test
// reads the result without waiting on the ticker.
func PushOTLPMetrics(ctx context.Context, m *OTLPMetrics, collect func(context.Context) []byte) {
	m.push(ctx, collect)
}

// SetOTLPMetricsInterval lets a test tick faster than the 1s floor allows.
func SetOTLPMetricsInterval(m *OTLPMetrics, d time.Duration) { m.interval = d }
