// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Kept machines: one person's macOS guest disk, kept on that person's own Mac
// between runs (migration kept_machines). Every session is a new run; run_id
// names the run that has the machine, or last had it.
package types

import (
	"time"

	"github.com/google/uuid"
)

// KeptMachineState is where a kept machine is in its life.
type KeptMachineState string

const (
	// KeptMachineCreating is a keep run's machine before its sandbox is up.
	KeptMachineCreating KeptMachineState = "creating"
	// KeptMachineInUse is a machine a run holds.
	KeptMachineInUse KeptMachineState = "in_use"
	// KeptMachineKept is a stopped machine, kept on its Mac.
	KeptMachineKept KeptMachineState = "kept"
	// KeptMachineDeleting is a machine whose delete has started (the tombstone).
	KeptMachineDeleting KeptMachineState = "deleting"
)

// KeptMachine is the organisation's record of one kept machine.
type KeptMachine struct {
	ID              uuid.UUID        `json:"id"`
	Name            string           `json:"name"`
	OwnerSubject    string           `json:"owner_subject"`
	RunnerID        uuid.UUID        `json:"runner_id"`
	Base            string           `json:"base"`
	BasePreparedAt  time.Time        `json:"base_prepared_at"`
	State           KeptMachineState `json:"state"`
	RunID           *uuid.UUID       `json:"run_id,omitempty"`
	Scrubbed        bool             `json:"scrubbed"`
	DiskBytes       int64            `json:"disk_bytes"`
	CreatedAt       time.Time        `json:"created_at"`
	LastUsedAt      time.Time        `json:"last_used_at"`
	ExpiresAt       time.Time        `json:"expires_at"`
	DeleteStartedAt *time.Time       `json:"delete_started_at,omitempty"`
}
