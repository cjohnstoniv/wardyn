// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "sync"

// deviceRouteState is the device routes' process state (devices_auth.go,
// device_audit_bounds.go), embedded in Server the way runLeaseState is.
type deviceRouteState struct {
	// enrolLimiter bounds the ONE anonymous device route, POST
	// /devices/enrol, per TCP peer (devices_auth.go's peerKey), with per-entry
	// eviction so the map cannot grow without bound. Configured in New.
	enrolLimiter principalLimiter
	// registerLimiter bounds the anonymous runner registration route, POST /runners/register, per TCP
	// peer, on its own: a flood of device enrolments cannot block runner registration, or the reverse.
	// A registration that redeemed a valid token is refunded, so only failed attempts are charged.
	registerLimiter principalLimiter
	// The device routes' failure-row bounds (device_audit_bounds.go):
	// enrolFailures is the anonymous route's one stream, ingestFailures one
	// stream per device, ingestFailureLimiter that stream's per-device bucket.
	enrolFailures        failureStreams
	ingestFailures       failureStreams
	ingestFailureLimiter principalLimiter
	// ingestInFlight holds the id of every device with a push in progress —
	// handleDeviceAuditIngest's one-push-per-device cap. Process-local like
	// the limiters above; an entry lives only as long as its request.
	ingestInFlight sync.Map
}

// newDeviceRouteState is the anonymous device and runner doors' limiters, as New configures them.
func newDeviceRouteState() deviceRouteState {
	return deviceRouteState{
		enrolLimiter:         principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers},
		registerLimiter:      principalLimiter{rate: enrolRatePerSec, burst: enrolBurst, max: enrolLimiterMaxPeers},
		ingestFailureLimiter: principalLimiter{rate: ingestFailureRatePerSec, burst: ingestFailureBurst, max: ingestFailureMaxDevices},
	}
}
