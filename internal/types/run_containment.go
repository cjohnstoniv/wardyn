// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Containment-pending kinds a Kubernetes runner reports (design K1 §3.1 Q2), the values of
// runner.ContainmentAgentOnly and runner.ContainmentPendingDelete.
const (
	ContainmentPendingAgentOnly = "agent_only"
	ContainmentPendingDelete    = "pending_delete"
)

// Node-record attestations (design K1 §3.1 (b)), the values of runner.AttestNodePoweredOff and
// runner.AttestNoPriorHolder.
const (
	NodeRecordPoweredOff    = "node_powered_off"
	NodeRecordNoPriorHolder = "no_prior_holder"
)

// Where the operators confirmed what a node record attests.
const (
	NodeRecordSourceProviderConsole      = "provider_console"
	NodeRecordSourceManagementController = "management_controller"
	NodeRecordSourceOther                = "other"
)

// Node-record states: sent to the runner, accepted by it, or refused with its reason.
const (
	NodeRecordSent     = "sent"
	NodeRecordAccepted = "accepted"
	NodeRecordRefused  = "refused"
)

// RunContainment is GET /runs/{id}/containment: whether the run's runner holds it as containment
// pending, and the node records made for it, newest first.
type RunContainment struct {
	Pending *RunContainmentPending `json:"pending,omitempty"`
	Records []RunNodeRecord        `json:"records"`
}

// RunContainmentPending is a run its Kubernetes runner has deleted but cannot yet prove stopped. Its
// proxy's egress ends by BoundUntil (the last token renewal plus the token's life); past it, egress is
// bounded but whether the processes stopped is unknown. It is never plain containment.
type RunContainmentPending struct {
	Kind                string    `json:"kind"`
	Since               time.Time `json:"since"`
	BoundUntil          time.Time `json:"bound_until"`
	BoundPassed         bool      `json:"bound_passed"`
	NameReused          bool      `json:"name_reused"`
	NoCausalAssociation bool      `json:"no_causal_association"`
	// TwoPeople: a node record for this run needs two people: four-eyes is on, or the runner saw a name
	// reuse or could not tie the run to one machine.
	TwoPeople bool `json:"two_people"`
	// Tuple is the runner's record of the run's pods and nodes that a node record must name, shown to
	// security operators only.
	Tuple json.RawMessage `json:"tuple,omitempty"`
}

// RunNodeRecord is one node record made for a run and what its runner answered.
type RunNodeRecord struct {
	ID          uuid.UUID  `json:"id"`
	Attestation string     `json:"attestation"`
	Source      string     `json:"source"`
	SourceID    string     `json:"source_id"`
	State       string     `json:"state"`
	Reason      string     `json:"reason,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
}

// RunNodeRecordRequest is POST /runs/{id}/node-record. Tuple is the tuple the form showed; the record
// is refused when the runner's record has changed since.
type RunNodeRecordRequest struct {
	Attestation string          `json:"attestation"`
	Source      string          `json:"source"`
	SourceID    string          `json:"source_id"`
	Tuple       json.RawMessage `json:"tuple"`
}
