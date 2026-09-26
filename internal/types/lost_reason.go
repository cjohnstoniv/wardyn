// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// LostReason says why a kept run lost its sandbox (AgentRun.LostReason).
type LostReason string

const (
	// LostEnded is a run whose lease ran out (AgentRun.EndsAt passed): stopped
	// and kept for the ended-run grace.
	LostEnded LostReason = "ended"
	// LostReboot is an interactive run whose agent container exited under it
	// (a host reboot, a Docker Desktop restart, a long suspend) but still
	// exists: kept with its files and its proxy stopped.
	LostReboot LostReason = "reboot"
	// LostOutage is an interactive run whose run token lapsed because the
	// control plane was unreachable past the token's life: its proxy is stopped
	// so it has no egress, and its agent is left running.
	LostOutage LostReason = "outage"
)
