// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/hostcapacity"

// HostCapacityConfig is Config's host-capacity admission, embedded so its
// wiring lives beside the feature rather than in server.go.
type HostCapacityConfig struct {
	// HostCapacity refuses every run launch while the host is over its memory
	// or load limits (WARDYN_HOST_*); nil admits everything.
	HostCapacity *hostcapacity.Guard
}
