// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"time"

	"github.com/google/uuid"
)

// KeptMachineSpec names the kept machine a macos_vm create is the session of. It carries no
// credential: the owner is named by a hash, and the guest's content stays on the Mac.
type KeptMachineSpec struct {
	ID        uuid.UUID
	Mode      string // "new" | "resume" | "fresh"
	OwnerHash string // types.DriveSubjectHash(owner)
	IdleDays  int
	// Name is the machine's name as the organisation knows it; the Mac records it in kept.json (and a
	// later boot's different name replaces it) so the machine can be listed by name with no network.
	Name string `json:"name,omitempty"`
}

// KeptScrub is the outcome of one scrub of a kept machine's disk: counts and certificate digests,
// never content.
type KeptScrub struct {
	When    string         `json:"when"`    // "stop" | "boot"
	Outcome string         `json:"outcome"` // "done" | "failed" | "not_run"
	Removed map[string]int `json:"removed,omitempty"`
	CAs     []string       `json:"ca_sha256,omitempty"`
}

// KeptBase is the base a kept machine's disk was cloned from, as its Mac's record says after this boot:
// for a fresh start, the clone the reset just made, so its prepared time is the clone's own, not the
// runner's current base's. Sandbox.KeptBase carries it: nil for any other sandbox, or a runner that
// predates it.
type KeptBase struct {
	Name       string    `json:"name"`
	PreparedAt time.Time `json:"prepared_at"`
}
