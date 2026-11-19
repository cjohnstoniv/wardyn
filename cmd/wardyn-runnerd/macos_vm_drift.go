// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import "time"

// driftRetry is how often checkAgent asks again for a drift tools update whose start failed (slots
// full, the ledger refused, an update already running).
const driftRetry = 10 * time.Minute

// driftNow is checkAgent's clock (tests).
var driftNow = time.Now

// macOSVMDrift is checkAgent's record of the tools updates it asked for.
type macOSVMDrift struct {
	started string    // the base agent version whose update started (startToolsUpdate returned nil)
	tried   string    // the version last asked for, at triedAt
	triedAt time.Time // when
}

// askDrift reports whether checkAgent asks now for the tools update of base agent version v: only while
// serving (a runnerd still waiting for its proxy image starts no base job, so a restart loop cannot pile
// up helpers), never again once one started for v, and at most once per driftRetry while starts fail.
func (m *macOSVMs) askDrift(v string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil || m.drift.started == v {
		return false
	}
	now := driftNow()
	if m.drift.tried == v && now.Sub(m.drift.triedAt) < driftRetry {
		return false
	}
	m.drift.tried, m.drift.triedAt = v, now
	return true
}

// driftStarted records that the tools update of base agent version v started.
func (m *macOSVMs) driftStarted(v string) {
	m.mu.Lock()
	m.drift.started = v
	m.mu.Unlock()
}
