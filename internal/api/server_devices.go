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
