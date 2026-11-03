// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "github.com/google/uuid"

// KeptMachineSpec names the kept machine a macos_vm create is the session of. It carries no
// credential: the owner is named by a hash, and the guest's content stays on the Mac.
type KeptMachineSpec struct {
	ID        uuid.UUID
	Mode      string // "new" | "resume" | "fresh"
	OwnerHash string // types.DriveSubjectHash(owner)
	IdleDays  int
}

// KeptScrub is the outcome of one scrub of a kept machine's disk: counts and certificate digests,
// never content.
type KeptScrub struct {
	When    string         `json:"when"`    // "stop" | "boot"
	Outcome string         `json:"outcome"` // "done" | "failed" | "not_run"
	Removed map[string]int `json:"removed,omitempty"`
	CAs     []string       `json:"ca_sha256,omitempty"`
}
